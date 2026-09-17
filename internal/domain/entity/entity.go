// Package entity holds the persistence-shaped domain structs shared by
// repositories and services. Money fields are int64 minor units; time
// fields are UTC instants (dates in Asia/Jakarta are handled at the
// query boundary).
package entity

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Source is a place money lives. Balance is ledger-computed, never
// stored here.
type Source struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	IsDebt     bool       `json:"is_debt"`
	Currency   string     `json:"currency"`
	ArchivedAt *time.Time `json:"archived_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Category is a user-defined spend classifier with an optional parent.
type Category struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	ParentID   *uuid.UUID `json:"parent_id"`
	Icon       *string    `json:"icon"`
	Essential  bool       `json:"essential"`
	ArchivedAt *time.Time `json:"archived_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Merchant is the brand or recurring concept money went to: one
// canonical row per user per name, pointed at by every transaction that
// named it however it was spelled.
type Merchant struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"-"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Leg is one side of a transaction: either a source change, a category
// attribution, or (both NULL) the adjustment system sink.
type Leg struct {
	ID            uuid.UUID  `json:"id"`
	TransactionID uuid.UUID  `json:"transaction_id"`
	SourceID      *uuid.UUID `json:"source_id"`
	SourceName    *string    `json:"source_name"`
	CategoryID    *uuid.UUID `json:"category_id"`
	CategoryName  *string    `json:"category_name"`
	SignedAmount  int64      `json:"signed_amount"`
}

// Transaction is one real-world money event stored as two legs
// (ADR-0001). Amounts live on the legs; AmountMinor is the unsigned
// base amount denormalized for display.
type Transaction struct {
	ID                    uuid.UUID       `json:"id"`
	OccurredAt            time.Time       `json:"occurred_at"`
	Origin                string          `json:"origin"`
	Type                  string          `json:"type"`
	Merchant              *string         `json:"merchant"`
	MerchantID            *uuid.UUID      `json:"merchant_id"`
	Note                  *string         `json:"note"`
	AmountMinor           int64           `json:"amount_minor"`
	CategoryID            *uuid.UUID      `json:"category_id"`
	CategoryName          *string         `json:"category_name"`
	CategorySource        *string         `json:"category_source"`
	CategoryConfidence    *float64        `json:"category_confidence"`
	ReviewStatus          string          `json:"review_status"`
	ExternalID            *string         `json:"external_id"`
	ContentHash           *string         `json:"-"`
	ReversesTransactionID *uuid.UUID      `json:"reverses_transaction_id"`
	EditHistory           json.RawMessage `json:"edit_history"`
	VoidedAt              *time.Time      `json:"voided_at"`
	CreatedAt             time.Time       `json:"created_at"`
	// BillID/BillName are set on the detail read when this transaction
	// settled a bill occurrence; voiding it reverts that bill.
	BillID   *uuid.UUID `json:"bill_id"`
	BillName *string    `json:"bill_name"`
	Legs     []Leg      `json:"legs"`
}

// MarshalJSON adds the derived `voided` flag the API contract requires.
// VoidedAt stays the single source of truth; a boolean column would be
// a second one that can disagree with it.
func (t Transaction) MarshalJSON() ([]byte, error) {
	type alias Transaction
	return json.Marshal(struct {
		alias
		Voided bool `json:"voided"`
	}{alias(t), t.VoidedAt != nil})
}

// Bill is an expected obligation — recurring or one-time — with an
// estimated amount band.
type Bill struct {
	ID               uuid.UUID  `json:"id"`
	Name             string     `json:"name"`
	AmountMinMinor   int64      `json:"amount_min_minor"`
	AmountMaxMinor   int64      `json:"amount_max_minor"`
	Cadence          string     `json:"cadence"`
	NextDueDate      time.Time  `json:"next_due_date"`
	LinkedSourceID   *uuid.UUID `json:"linked_source_id"`
	LinkedSourceName *string    `json:"linked_source_name"`
	CategoryID       *uuid.UUID `json:"category_id"`
	CategoryName     *string    `json:"category_name"`
	Active           bool       `json:"active"`
	// RemainingCount is an optional tenor: payments left before the
	// bill completes. nil = open-ended.
	RemainingCount *int `json:"remaining_count"`
	// CompletedAt is set when the last occurrence was paid; a completed
	// bill is inactive and cannot be reactivated.
	CompletedAt *time.Time `json:"completed_at"`
	// ReminderOffsets are days-before-due the user opted into (⊆ 3,1,0).
	// Never nil on the wire — the dashboard indexes it.
	ReminderOffsets []int `json:"reminder_offsets"`
	// AnchorDay pins a monthly/annual bill to its day-of-month so
	// February cannot permanently shorten it. Server-side only.
	AnchorDay int       `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// BillPayment records that a transaction settled one occurrence of a
// bill. Voiding the transaction reverts the bill (ADR-0006).
type BillPayment struct {
	ID            uuid.UUID  `json:"id"`
	BillID        uuid.UUID  `json:"bill_id"`
	BillName      string     `json:"bill_name"`
	TransactionID uuid.UUID  `json:"transaction_id"`
	DueDate       time.Time  `json:"due_date"`
	AdvancedTo    *time.Time `json:"advanced_to"`
	AmountMinor   int64      `json:"amount_minor"`
	Decremented   bool       `json:"decremented"`
	PaidAt        time.Time  `json:"paid_at"`
}

// BillReminderSend is one delivered reminder slot — the dedup row
// behind "a bill is named at most once per offset per due date".
type BillReminderSend struct {
	ID         uuid.UUID `json:"id"`
	BillID     uuid.UUID `json:"bill_id"`
	DueDate    time.Time `json:"due_date"`
	OffsetDays int       `json:"offset_days"`
	SendDate   time.Time `json:"send_date"`
	SentAt     time.Time `json:"sent_at"`
}

// BudgetRow is a budget joined with its category and the computed
// spend for the period.
type BudgetRow struct {
	ID           uuid.UUID `json:"id"`
	CategoryID   uuid.UUID `json:"category_id"`
	CategoryName string    `json:"category_name"`
	Essential    bool      `json:"essential"`
	AmountMinor  int64     `json:"amount_minor"`
	SpentMinor   int64     `json:"spent_minor"`
}

// PendingAction is a parked Tier 1-3 tool call waiting for explicit
// confirmation (ADR-0003).
type PendingAction struct {
	ID         uuid.UUID       `json:"id"`
	Kind       string          `json:"kind"`
	Params     json.RawMessage `json:"params"`
	Summary    string          `json:"summary"`
	Action     *string         `json:"action"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at"`
}

// OtpCode is a hashed WhatsApp login code with attempt bookkeeping.
type OtpCode struct {
	ID        uuid.UUID `json:"id"`
	Phone     string    `json:"phone"`
	CodeHash  string    `json:"code_hash"`
	ExpiresAt time.Time `json:"expires_at"`
	Attempts  int       `json:"attempts"`
	Consumed  bool      `json:"consumed"`
	CreatedAt time.Time `json:"created_at"`
}

// User is one account: its own ledger, its own WhatsApp number.
//
// Phone is the identity (E.164, normalised by auth.NormalizePhone);
// PhoneDigits is the generated digits-only form the bot matches an
// inbound JID against. WhatsAppLID is the opaque "@lid" identity the
// gateway sometimes uses instead of the number. PasswordHash is NULL
// until the account has a password: the bootstrap admin starts that way
// and sets one through the reset flow.
type User struct {
	ID            uuid.UUID  `json:"id"`
	Phone         string     `json:"phone"`
	PhoneDigits   string     `json:"phone_digits"`
	WhatsAppLID   *string    `json:"whatsapp_lid"`
	DisplayName   string     `json:"display_name"`
	PasswordHash  *string    `json:"-"`
	IsAdmin       bool       `json:"is_admin"`
	SessionEpoch  int        `json:"-"`
	FailedLogins  int        `json:"-"`
	LockedUntil   *time.Time `json:"-"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Invite is a phone-bound, single-use permission to register. The token
// itself never lands here: only its sha256 is stored, and the plaintext
// exists just long enough to be sent over WhatsApp.
type Invite struct {
	ID        uuid.UUID  `json:"id"`
	Phone     string     `json:"phone"`
	CreatedBy uuid.UUID  `json:"created_by"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// LLMProvider is the instance-wide AI provider the admin configured
// from the dashboard: the one row of llm_provider. It has no user id by
// design — one instance, one provider, one bill. APIKeyEnc is
// AES-256-GCM ciphertext (see internal/secretbox); APIKeyLast4 is the
// only fragment of the key that ever leaves the server. An empty
// APIKeyEnc is the documented "AI off" state.
type LLMProvider struct {
	BaseURL              string     `json:"base_url"`
	APIKeyEnc            string     `json:"-"`
	APIKeyLast4          string     `json:"api_key_last4"`
	Model                string     `json:"model"`
	VisionModel          string     `json:"vision_model"`
	TimeoutSeconds       int        `json:"timeout_seconds"`
	VisionTimeoutSeconds int        `json:"vision_timeout_seconds"`
	UpdatedBy            *uuid.UUID `json:"updated_by"`
	UpdatedAt            time.Time  `json:"updated_at"`
}
