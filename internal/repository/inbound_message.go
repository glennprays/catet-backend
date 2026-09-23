package repository

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	workLeaseInterval     = "3 minutes"
	deliveryLeaseInterval = "30 seconds"
	minimumRetryDelay     = 15 * time.Second
	maximumRetryDelay     = 5 * time.Minute
	maximumWorkAttempts   = 3
)

var (
	// ErrInboundLeaseLost means another worker owns, completed, or reclaimed
	// the row. Callers must not make any state transition after this error.
	ErrInboundLeaseLost = errors.New("inbound lease no longer held")
	stageCode           = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// InboundMessage is one webhook delivery. Its message id is the durable
// financial idempotency key; a duplicate must replay a stored reply, never
// rerun ledger work.
type InboundMessage struct {
	MessageID  string
	Chat       string
	FromJID    string
	UserID     *uuid.UUID
	ReceivedAt time.Time
	OccurredAt time.Time
	Body       string
	Kind       string
	Status     string // processing | ready | replied | dropped | superseded
	ReplyText  *string

	TransactionIDs  []uuid.UUID
	Lang            string
	MediaURL        *string
	MediaMime       *string
	MediaSha256     *string
	WorkAttempts    int
	DeliveryAttempts int
	NextAttemptAt   time.Time
	LeaseUntil      *time.Time
	LastError       *string
	OutcomeReaction *string
}

// InboundClaimKind is the compact work/delivery/incomplete/replay/busy
// boundary crossed by the harness. Locking and schedule details remain
// private to this module.
type InboundClaimKind string

const (
	InboundClaimWork       InboundClaimKind = "work"
	InboundClaimDelivery   InboundClaimKind = "delivery"
	InboundClaimIncomplete InboundClaimKind = "incomplete"
	InboundClaimReplay     InboundClaimKind = "replay"
	InboundClaimBusy       InboundClaimKind = "busy"
)

// InboundLease is an opaque CAS identity. Its timestamp and monotonic attempt
// counter fence a stale worker after an expired lease has been reclaimed.
type InboundLease struct {
	messageID        string
	until            time.Time
	status           string
	workAttempts     int
	deliveryAttempts int
}

// InboundClaim is the only lifecycle state a caller needs to interpret.
type InboundClaim struct {
	Kind    InboundClaimKind
	Message *InboundMessage
	Lease   *InboundLease
}

// InboundResult is committed before any outbound reply attempt.
type InboundResult struct {
	ReplyText       string
	TransactionIDs  []uuid.UUID
	Lang            string
	OutcomeReaction string
}

type InboundMessageRepository struct {
	db *pgxpool.Pool
}

func NewInboundMessageRepository(db *pgxpool.Pool) *InboundMessageRepository {
	return &InboundMessageRepository{db: db}
}

const inboundColumns = `message_id, chat, from_jid, user_id, received_at, occurred_at, body, kind,
	status, reply_text, transaction_ids, lang, media_url, media_mime, media_sha256,
	work_attempts, delivery_attempts, next_attempt_at, lease_until, last_error, outcome_reaction`

// ClaimInbound inserts a new delivery or atomically claims due work. Existing
// terminal rows replay their saved reply, while due ready rows enter the
// delivery path without re-running parsing or ledger writes.
func (r *InboundMessageRepository) ClaimInbound(ctx context.Context, incoming *InboundMessage) (*InboundClaim, error) {
	if incoming == nil {
		return nil, errors.New("claim inbound: nil message")
	}
	if incoming.MessageID == "" {
		return nil, errors.New("claim inbound: empty message id")
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	status := incoming.Status
	if status != "dropped" {
		status = "processing"
	}
	lang := incoming.Lang
	if lang == "" {
		lang = "id"
	}

	var row InboundMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO inbound_messages (message_id, chat, from_jid, user_id, occurred_at, body, kind, status, lang,
			media_url, media_mime, media_sha256)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (message_id) DO NOTHING
		 RETURNING `+inboundColumns,
		incoming.MessageID, incoming.Chat, incoming.FromJID, incoming.UserID, incoming.OccurredAt, incoming.Body,
		incoming.Kind, status, lang, incoming.MediaURL, incoming.MediaMime, incoming.MediaSha256,
	).Scan(row.scanTargets()...)
	if err == nil {
		if status == "dropped" {
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return &InboundClaim{Kind: InboundClaimReplay, Message: &row}, nil
		}
		row, err = acquireInbound(ctx, tx, row.MessageID, "processing")
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return claimedWork(&row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	err = tx.QueryRow(ctx, `SELECT `+inboundColumns+` FROM inbound_messages WHERE message_id = $1 FOR UPDATE SKIP LOCKED`, incoming.MessageID).Scan(row.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &InboundClaim{Kind: InboundClaimBusy}, nil
	}
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if incoming.Status == "dropped" && (row.Status == "processing" || row.Status == "ready") &&
		!row.NextAttemptAt.After(now) && (row.LeaseUntil == nil || !row.LeaseUntil.After(now)) {
		_, err := tx.Exec(ctx, `UPDATE inbound_messages
			SET status = 'dropped', lease_until = NULL, next_attempt_at = now(), last_error = $2
			WHERE message_id = $1`, incoming.MessageID, "sender_unavailable")
		if err != nil {
			return nil, err
		}
		row.Status = "dropped"
		row.LeaseUntil = nil
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &InboundClaim{Kind: InboundClaimReplay, Message: &row}, nil
	}

	if row.Status != "processing" && row.Status != "ready" {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &InboundClaim{Kind: InboundClaimReplay, Message: &row}, nil
	}
	if row.NextAttemptAt.After(now) || (row.LeaseUntil != nil && row.LeaseUntil.After(now)) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &InboundClaim{Kind: InboundClaimBusy, Message: &row}, nil
	}
	superseded, err := supersedeEquivalentText(ctx, tx, row.MessageID)
	if err != nil {
		return nil, err
	}
	if superseded {
		row.Status = "superseded"
		row.LeaseUntil = nil
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &InboundClaim{Kind: InboundClaimReplay, Message: &row}, nil
	}
	if row.Status == "processing" && row.WorkAttempts >= maximumWorkAttempts {
		row, err = renewIncompleteInbound(ctx, tx, row.MessageID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return claimedIncomplete(&row), nil
	}

	row, err = acquireInbound(ctx, tx, row.MessageID, row.Status)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if row.Status == "ready" {
		return claimedDelivery(&row), nil
	}
	return claimedWork(&row), nil
}

func acquireInbound(ctx context.Context, tx pgx.Tx, messageID, status string) (InboundMessage, error) {
	var row InboundMessage
	var err error
	if status == "ready" {
		err = tx.QueryRow(ctx,
			`UPDATE inbound_messages SET lease_until = now() + $2::interval
			 WHERE message_id = $1 AND status = 'ready'
			 RETURNING `+inboundColumns,
			messageID, deliveryLeaseInterval,
		).Scan(row.scanTargets()...)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE inbound_messages SET work_attempts = work_attempts + 1, lease_until = now() + $2::interval
			 WHERE message_id = $1 AND status = 'processing' AND work_attempts < $3
			 RETURNING `+inboundColumns,
			messageID, workLeaseInterval, maximumWorkAttempts,
		).Scan(row.scanTargets()...)
	}
	return row, err
}

func renewIncompleteInbound(ctx context.Context, tx pgx.Tx, messageID string) (InboundMessage, error) {
	var row InboundMessage
	err := tx.QueryRow(ctx,
		`UPDATE inbound_messages SET lease_until = now() + $2::interval
		 WHERE message_id = $1 AND status = 'processing' AND work_attempts >= $3
		   AND next_attempt_at <= now() AND (lease_until IS NULL OR lease_until <= now())
		 RETURNING `+inboundColumns,
		messageID, workLeaseInterval, maximumWorkAttempts,
	).Scan(row.scanTargets()...)
	return row, err
}

func claimedWork(row *InboundMessage) *InboundClaim {
	return &InboundClaim{Kind: InboundClaimWork, Message: row, Lease: leaseFor(row, "processing")}
}

func claimedIncomplete(row *InboundMessage) *InboundClaim {
	return &InboundClaim{Kind: InboundClaimIncomplete, Message: row, Lease: leaseFor(row, "processing")}
}

func claimedDelivery(row *InboundMessage) *InboundClaim {
	return &InboundClaim{Kind: InboundClaimDelivery, Message: row, Lease: leaseFor(row, "ready")}
}

func leaseFor(row *InboundMessage, status string) *InboundLease {
	if row.LeaseUntil == nil {
		return nil
	}
	return &InboundLease{messageID: row.MessageID, until: *row.LeaseUntil, status: status, workAttempts: row.WorkAttempts, deliveryAttempts: row.DeliveryAttempts}
}

// StoreInboundResult atomically makes the reply durable and exchanges the
// work lease for a short delivery lease. No sender runs before this succeeds.
func (r *InboundMessageRepository) StoreInboundResult(ctx context.Context, lease *InboundLease, result InboundResult) (*InboundLease, error) {
	if !validLease(lease, "processing") {
		return nil, ErrInboundLeaseLost
	}
	ids := result.TransactionIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	lang := result.Lang
	if lang == "" {
		lang = "id"
	}
	var row InboundMessage
	err := r.db.QueryRow(ctx,
		`UPDATE inbound_messages
		 SET status = 'ready', reply_text = $1, transaction_ids = $2, lang = $3, outcome_reaction = $4,
		     lease_until = now() + $5::interval, next_attempt_at = now()
		 WHERE message_id = $6 AND status = 'processing' AND lease_until = $7 AND work_attempts = $8
		 RETURNING `+inboundColumns,
		result.ReplyText, ids, lang, nullIfEmpty(result.OutcomeReaction), deliveryLeaseInterval,
		lease.messageID, lease.until, lease.workAttempts,
	).Scan(row.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInboundLeaseLost
	}
	if err != nil {
		return nil, err
	}
	return leaseFor(&row, "ready"), nil
}

// MarkInboundReplied records only a successful gateway handoff under the
// exact delivery lease that sent it.
func (r *InboundMessageRepository) MarkInboundReplied(ctx context.Context, lease *InboundLease) error {
	if !validLease(lease, "ready") {
		return ErrInboundLeaseLost
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE inbound_messages SET status = 'replied', lease_until = NULL
		 WHERE message_id = $1 AND status = 'ready' AND lease_until = $2 AND delivery_attempts = $3`,
		lease.messageID, lease.until, lease.deliveryAttempts,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInboundLeaseLost
	}
	return nil
}

// RescheduleInbound releases a failed delivery without losing its stored
// result. Only a stable stage code enters the audit column.
func (r *InboundMessageRepository) RescheduleInbound(ctx context.Context, lease *InboundLease, stage string) error {
	if !validLease(lease, "ready") {
		return ErrInboundLeaseLost
	}
	delay := retryDelay(lease.deliveryAttempts + 1)
	tag, err := r.db.Exec(ctx,
		`UPDATE inbound_messages
		 SET delivery_attempts = delivery_attempts + 1, next_attempt_at = $1, lease_until = NULL, last_error = $2
		 WHERE message_id = $3 AND status = 'ready' AND lease_until = $4 AND delivery_attempts = $5`,
		time.Now().Add(delay), sanitizeStage(stage), lease.messageID, lease.until, lease.deliveryAttempts,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInboundLeaseLost
	}
	return nil
}

// ReleaseInboundWork makes a failed computation recoverable. On the third
// attempt it keeps the valid lease and returns final=true so the caller can
// store an explicit incomplete result with the recovered transaction ids.
func (r *InboundMessageRepository) ReleaseInboundWork(ctx context.Context, lease *InboundLease, partialIDs []uuid.UUID, stage string) (bool, error) {
	if !validLease(lease, "processing") {
		return false, ErrInboundLeaseLost
	}
	ids := partialIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	final := lease.workAttempts >= maximumWorkAttempts
	next := time.Now().Add(retryDelay(lease.workAttempts))
	var row InboundMessage
	err := r.db.QueryRow(ctx,
		`UPDATE inbound_messages
		 SET transaction_ids = $1, last_error = $2,
		     lease_until = CASE WHEN $3 THEN lease_until ELSE NULL END,
		     next_attempt_at = CASE WHEN $3 THEN next_attempt_at ELSE $4 END
		 WHERE message_id = $5 AND status = 'processing' AND lease_until = $6 AND work_attempts = $7
		 RETURNING `+inboundColumns,
		ids, sanitizeStage(stage), final, next, lease.messageID, lease.until, lease.workAttempts,
	).Scan(row.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrInboundLeaseLost
	}
	if err != nil {
		return false, err
	}
	return final, nil
}

// ListDueInbound returns a bounded recovery view; claiming remains a separate
// atomic operation, so concurrent tickers cannot execute a row twice.
func (r *InboundMessageRepository) ListDueInbound(ctx context.Context, limit int) ([]InboundMessage, error) {
	if limit <= 0 {
		return []InboundMessage{}, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+inboundColumns+` FROM inbound_messages
		 WHERE status IN ('processing', 'ready') AND next_attempt_at <= now()
		   AND (lease_until IS NULL OR lease_until <= now())
		 ORDER BY next_attempt_at, received_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboundMessage{}
	for rows.Next() {
		var row InboundMessage
		if err := rows.Scan(row.scanTargets()...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SupersedeEquivalentText protects only an unreplied duplicate from being
// replayed after a newer equivalent text has completed. Terminal messages are
// deliberately never compared for financial de-duplication.
func (r *InboundMessageRepository) SupersedeEquivalentText(ctx context.Context, messageID string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	updated, err := supersedeEquivalentText(ctx, tx, messageID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return updated, nil
}

func supersedeEquivalentText(ctx context.Context, tx pgx.Tx, messageID string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE inbound_messages AS stale
		 SET status = 'superseded', lease_until = NULL
		 WHERE stale.message_id = $1 AND stale.status IN ('processing', 'ready')
		   AND stale.kind = 'text' AND btrim(stale.body) <> ''
		   AND stale.next_attempt_at <= now() AND (stale.lease_until IS NULL OR stale.lease_until <= now())
		   AND EXISTS (
		     SELECT 1 FROM inbound_messages AS replied
		     WHERE replied.chat = stale.chat AND replied.kind = 'text' AND replied.status = 'replied'
		       AND btrim(replied.body) <> '' AND replied.received_at > stale.received_at
		       AND lower(regexp_replace(btrim(replied.body), '\s+', ' ', 'g')) =
		           lower(regexp_replace(btrim(stale.body), '\s+', ' ', 'g'))
		   )`, messageID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetTransactionIDs rewrites only the batch pointer after a chat correction.
func (r *InboundMessageRepository) SetTransactionIDs(ctx context.Context, messageID string, ids []uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	_, err = r.db.Exec(ctx, `UPDATE inbound_messages SET transaction_ids = $2 WHERE message_id = $1 AND user_id = $3`, messageID, ids, uid)
	return err
}

func (r *InboundMessageRepository) SetLang(ctx context.Context, messageID, lang string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `UPDATE inbound_messages SET lang = $2 WHERE message_id = $1 AND user_id = $3`, messageID, lang, uid)
	return err
}

func (m *InboundMessage) scanTargets() []any {
	return []any{
		&m.MessageID, &m.Chat, &m.FromJID, &m.UserID, &m.ReceivedAt, &m.OccurredAt, &m.Body, &m.Kind,
		&m.Status, &m.ReplyText, &m.TransactionIDs, &m.Lang, &m.MediaURL, &m.MediaMime, &m.MediaSha256,
		&m.WorkAttempts, &m.DeliveryAttempts, &m.NextAttemptAt, &m.LeaseUntil, &m.LastError, &m.OutcomeReaction,
	}
}

func (r *InboundMessageRepository) getUnscoped(ctx context.Context, messageID string) (*InboundMessage, error) {
	var m InboundMessage
	err := r.db.QueryRow(ctx, `SELECT `+inboundColumns+` FROM inbound_messages WHERE message_id = $1`, messageID).Scan(m.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *InboundMessageRepository) Get(ctx context.Context, messageID string) (*InboundMessage, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	m, err := r.getUnscoped(ctx, messageID)
	if err != nil || m == nil {
		return m, err
	}
	if m.UserID == nil || *m.UserID != uid {
		return nil, nil
	}
	return m, nil
}

func (r *InboundMessageRepository) SetMediaSha256(ctx context.Context, messageID, sha string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `UPDATE inbound_messages SET media_sha256 = $2 WHERE message_id = $1 AND user_id = $3`, messageID, sha, uid)
	return err
}

func (r *InboundMessageRepository) FindRepliedByMediaSha256(ctx context.Context, sha string, since time.Time) (*InboundMessage, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var m InboundMessage
	err = r.db.QueryRow(ctx,
		`SELECT `+inboundColumns+` FROM inbound_messages
		 WHERE media_sha256 = $1 AND status = 'replied' AND received_at >= $2 AND user_id = $3
		 ORDER BY received_at DESC LIMIT 1`, sha, since, uid).Scan(m.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func validLease(lease *InboundLease, status string) bool {
	return lease != nil && lease.status == status && lease.messageID != "" && !lease.until.IsZero()
}

func retryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return minimumRetryDelay
	}
	delay := minimumRetryDelay
	for i := 1; i < attempt && delay < maximumRetryDelay; i++ {
		delay *= 2
	}
	if delay > maximumRetryDelay {
		return maximumRetryDelay
	}
	return delay
}

func sanitizeStage(stage string) string {
	if stageCode.MatchString(stage) {
		return stage
	}
	return "internal_error"
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

