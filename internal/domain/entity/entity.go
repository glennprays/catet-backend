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
	Legs                  []Leg           `json:"legs"`
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

// Bill is a recurring obligation with an estimated amount band.
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
	CreatedAt        time.Time  `json:"created_at"`
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

// AppSettings is the singleton settings row.
type AppSettings struct {
	IncomeHorizonDays int `json:"income_horizon_days"`
}
