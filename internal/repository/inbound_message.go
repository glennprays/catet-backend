package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InboundMessage is one webhook delivery. The row is the bot's
// idempotency key: the gateway retries deliveries, so a duplicate
// message_id must replay the stored reply instead of writing again.
type InboundMessage struct {
	MessageID string
	Chat      string
	FromJID   string
	// UserID is the ledger the message belongs to. It is nil for a
	// dropped delivery from a number that maps to nobody — such a row is
	// kept for the audit trail, not for a tenant.
	UserID     *uuid.UUID
	ReceivedAt time.Time
	OccurredAt time.Time
	Body       string
	Kind       string
	Status     string // processing | replied | dropped
	ReplyText  *string
	// TransactionIDs are the rows the message wrote or edited: a capture,
	// a reversal, a corrected row.
	TransactionIDs []uuid.UUID
	Lang           string
	// Media metadata only — the bytes are never stored. MediaSha256 is
	// filled after the photo is actually downloaded, so it is the proof
	// that these exact bytes were already read once.
	MediaURL    *string
	MediaMime   *string
	MediaSha256 *string
}

type InboundMessageRepository struct {
	db *pgxpool.Pool
}

func NewInboundMessageRepository(db *pgxpool.Pool) *InboundMessageRepository {
	return &InboundMessageRepository{db: db}
}

const inboundColumns = `message_id, chat, from_jid, user_id, received_at, occurred_at, body, kind,
	                    status, reply_text, transaction_ids, lang,
	                    media_url, media_mime, media_sha256`

// InsertProcessing claims a message for processing. inserted=false means
// the message_id was already known; existing then carries the prior row
// (and its reply, for replay).
//
// It is the one write that does not read the scope: it runs before the
// sender has been resolved to a user, and a delivery from a stranger
// never resolves to one at all, so the owner — nil for that stranger —
// comes from the struct the caller filled in.
func (r *InboundMessageRepository) InsertProcessing(ctx context.Context, m *InboundMessage) (bool, *InboundMessage, error) {
	status := m.Status
	if status == "" {
		status = "processing"
	}
	lang := m.Lang
	if lang == "" {
		lang = "id"
	}
	tag, err := r.db.Exec(ctx,
		`INSERT INTO inbound_messages (message_id, chat, from_jid, user_id, occurred_at, body, kind, status, lang,
		                              media_url, media_mime, media_sha256)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (message_id) DO NOTHING`,
		m.MessageID, m.Chat, m.FromJID, m.UserID, m.OccurredAt, m.Body, m.Kind, status, lang,
		m.MediaURL, m.MediaMime, m.MediaSha256,
	)
	if err != nil {
		return false, nil, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil, nil
	}
	existing, err := r.getUnscoped(ctx, m.MessageID)
	if err != nil {
		return false, nil, err
	}
	return false, existing, nil
}

// Finish stamps the terminal status, the reply we sent and the batch the
// message wrote.
func (r *InboundMessageRepository) Finish(ctx context.Context, messageID, status string, replyText *string, txIDs []uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	if txIDs == nil {
		txIDs = []uuid.UUID{}
	}
	_, err = r.db.Exec(ctx,
		`UPDATE inbound_messages
		 SET status = $2, reply_text = $3, transaction_ids = $4
		 WHERE message_id = $1 AND user_id = $5`,
		messageID, status, replyText, txIDs, uid,
	)
	return err
}

// SetTransactionIDs rewrites only the batch pointer of a message. A
// chat correction that reverses and re-writes a transaction has to swap
// the id the batch is addressable by without disturbing the reply text
// Finish stored for replay.
func (r *InboundMessageRepository) SetTransactionIDs(ctx context.Context, messageID string, ids []uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	_, err = r.db.Exec(ctx,
		`UPDATE inbound_messages SET transaction_ids = $2 WHERE message_id = $1 AND user_id = $3`,
		messageID, ids, uid,
	)
	return err
}

// SetLang records the language the parser detected, so the replay reply
// and later rituals stay in the same language.
func (r *InboundMessageRepository) SetLang(ctx context.Context, messageID, lang string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`UPDATE inbound_messages SET lang = $2 WHERE message_id = $1 AND user_id = $3`,
		messageID, lang, uid,
	)
	return err
}

// scanTargets lists the destinations for inboundColumns, in order. Every
// read of the table goes through it so adding a column cannot leave one
// scan site behind.
func (m *InboundMessage) scanTargets() []any {
	return []any{
		&m.MessageID, &m.Chat, &m.FromJID, &m.UserID, &m.ReceivedAt, &m.OccurredAt, &m.Body,
		&m.Kind, &m.Status, &m.ReplyText, &m.TransactionIDs, &m.Lang,
		&m.MediaURL, &m.MediaMime, &m.MediaSha256,
	}
}

// getUnscoped loads one message by id with no tenant filter. It exists
// for InsertProcessing, which replays the already-known row before a
// user has been resolved; sharing it keeps a single by-id SELECT in the
// file.
func (r *InboundMessageRepository) getUnscoped(ctx context.Context, messageID string) (*InboundMessage, error) {
	var m InboundMessage
	err := r.db.QueryRow(ctx,
		`SELECT `+inboundColumns+` FROM inbound_messages WHERE message_id = $1`, messageID,
	).Scan(m.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Get loads one message; nil when unknown, and equally nil when the row
// belongs to somebody else — another user's message_id must look exactly
// like one we have never seen.
func (r *InboundMessageRepository) Get(ctx context.Context, messageID string) (*InboundMessage, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	m, err := r.getUnscoped(ctx, messageID)
	if err != nil || m == nil {
		return nil, err
	}
	if m.UserID == nil || *m.UserID != uid {
		return nil, nil
	}
	return m, nil
}

// SetMediaSha256 records the hash of the bytes we actually downloaded.
// It is written before the photo reaches the model, so a crash mid-read
// still leaves the fingerprint behind.
func (r *InboundMessageRepository) SetMediaSha256(ctx context.Context, messageID, sha string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`UPDATE inbound_messages SET media_sha256 = $2 WHERE message_id = $1 AND user_id = $3`,
		messageID, sha, uid,
	)
	return err
}

// FindRepliedByMediaSha256 finds an earlier, already-answered delivery of
// the exact same image bytes. nil means this photo is new.
func (r *InboundMessageRepository) FindRepliedByMediaSha256(ctx context.Context, sha string, since time.Time) (*InboundMessage, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var m InboundMessage
	err = r.db.QueryRow(ctx,
		`SELECT `+inboundColumns+` FROM inbound_messages
		 WHERE media_sha256 = $1 AND status = 'replied' AND received_at >= $2 AND user_id = $3
		 ORDER BY received_at DESC LIMIT 1`,
		sha, since, uid,
	).Scan(m.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListProcessing returns messages claimed but never finished — the
// crash-recovery replay set at boot. It is deliberately unscoped: it runs
// before any user is in the context, and the harness re-resolves each
// message's owner from from_jid as it replays it.
func (r *InboundMessageRepository) ListProcessing(ctx context.Context) ([]InboundMessage, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+inboundColumns+` FROM inbound_messages WHERE status = 'processing' ORDER BY received_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []InboundMessage{}
	for rows.Next() {
		var m InboundMessage
		if err := rows.Scan(m.scanTargets()...); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
