// Package request holds HTTP request body DTOs. Validation rules run
// through httperror.BindAndValidate and surface as 400
// VALIDATION_FAILED with FieldError[] details.
package request

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ------------------------------------------------------------------ auth

type LoginRequest struct {
	Phone    string `json:"phone" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type OtpRequest struct {
	Phone string `json:"phone" validate:"required"`
}

// RegisterRequest is the whole signup: the invite proves the number was
// invited, the code proves the person holds it.
type RegisterRequest struct {
	InviteToken string `json:"invite_token" validate:"required"`
	Code        string `json:"code" validate:"required"`
	DisplayName string `json:"display_name" validate:"required"`
	Password    string `json:"password" validate:"required"`
}

type ResetVerifyRequest struct {
	Phone string `json:"phone" validate:"required"`
	Code  string `json:"code" validate:"required"`
}

type ResetCompleteRequest struct {
	ResetToken string `json:"reset_token" validate:"required"`
	Password   string `json:"password" validate:"required"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"required"`
	NewPassword     string `json:"new_password" validate:"required"`
}

// ----------------------------------------------------------------- users

type InviteRequest struct {
	Phone string `json:"phone" validate:"required"`
}

// --------------------------------------------------------------- sources

type CreateSourceRequest struct {
	Name     string `json:"name" validate:"required"`
	Type     string `json:"type" validate:"required,oneof=cash bank ewallet custom"`
	IsDebt   bool   `json:"is_debt"`
	Currency string `json:"currency" validate:"omitempty,len=3"`
}

type UpdateSourceRequest struct {
	Name   *string `json:"name" validate:"omitempty,min=1"`
	Type   *string `json:"type" validate:"omitempty,oneof=cash bank ewallet custom"`
	IsDebt *bool   `json:"is_debt"`
}

type AdjustSourceRequest struct {
	AmountMinor int64   `json:"amount_minor" validate:"required"`
	Note        *string `json:"note"`
}

// ---------------------------------------------------------- transactions

type CreateTransactionRequest struct {
	Type                string     `json:"type" validate:"required,oneof=expense income transfer adjustment"`
	OccurredAt          *time.Time `json:"occurred_at"`
	OccurredDate        *string    `json:"occurred_date" validate:"omitempty,yyyy-mm-date"`
	AmountMinor         int64      `json:"amount_minor" validate:"required"`
	SourceID            uuid.UUID  `json:"source_id" validate:"required,uuid"`
	DestinationSourceID *uuid.UUID `json:"destination_source_id" validate:"omitempty,uuid"`
	CategoryID          *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
	Note                *string    `json:"note"`
	Merchant            *string    `json:"merchant"`
}

// UpdateTransactionRequest: pointer = field present. Empty string on
// Note/Merchant clears them; CategoryID cannot be unset.
type UpdateTransactionRequest struct {
	AmountMinor *int64     `json:"amount_minor" validate:"omitempty"`
	OccurredAt  *time.Time `json:"occurred_at"`
	CategoryID  *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
	Note        *string    `json:"note"`
	Merchant    *string    `json:"merchant"`
}

type ReverseTransactionRequest struct {
	Note *string `json:"note"`
}

// ------------------------------------------------------------- categories

type CreateCategoryRequest struct {
	Name      string     `json:"name" validate:"required,min=1"`
	ParentID  *uuid.UUID `json:"parent_id" validate:"omitempty,uuid"`
	Icon      *string    `json:"icon"`
	Essential bool       `json:"essential"`
}

// RenameMerchantRequest renames a canonical merchant; every row that
// points at it picks up the new display text.
type RenameMerchantRequest struct {
	Name string `json:"name" validate:"required,min=1,max=80"`
}

// MergeMerchantRequest folds the path merchant into another one.
type MergeMerchantRequest struct {
	IntoMerchantID uuid.UUID `json:"into_merchant_id" validate:"required"`
}

type UpdateCategoryRequest struct {
	Name      *string    `json:"name" validate:"omitempty,min=1"`
	ParentID  *uuid.UUID `json:"parent_id" validate:"omitempty,uuid"`
	Icon      *string    `json:"icon"`
	Essential *bool      `json:"essential"`
}

// ------------------------------------------------------------------ bills

type CreateBillRequest struct {
	Name            string     `json:"name" validate:"required,min=1"`
	AmountMinMinor  int64      `json:"amount_min_minor" validate:"required,min=1"`
	AmountMaxMinor  int64      `json:"amount_max_minor" validate:"required,min=1"`
	Cadence         string     `json:"cadence" validate:"required,oneof=once weekly monthly annual"`
	NextDueDate     string     `json:"next_due_date" validate:"required,yyyy-mm-date"`
	LinkedSourceID  *uuid.UUID `json:"linked_source_id" validate:"omitempty,uuid"`
	CategoryID      *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
	RemainingCount  *int       `json:"remaining_count" validate:"omitempty,min=1"`
	ReminderOffsets []int      `json:"reminder_offsets" validate:"omitempty,dive,oneof=0 1 3"`
}

type UpdateBillRequest struct {
	Name           *string    `json:"name" validate:"omitempty,min=1"`
	AmountMinMinor *int64     `json:"amount_min_minor" validate:"omitempty,min=1"`
	AmountMaxMinor *int64     `json:"amount_max_minor" validate:"omitempty,min=1"`
	Cadence        *string    `json:"cadence" validate:"omitempty,oneof=once weekly monthly annual"`
	NextDueDate    *string    `json:"next_due_date" validate:"omitempty,yyyy-mm-date"`
	LinkedSourceID *uuid.UUID `json:"linked_source_id" validate:"omitempty,uuid"`
	CategoryID     *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
	// RemainingCount distinguishes absent (tenor unchanged) from an
	// explicit null (tenor cleared back to open-ended).
	RemainingCount  NullableInt `json:"remaining_count"`
	ReminderOffsets *[]int      `json:"reminder_offsets" validate:"omitempty,dive,oneof=0 1 3"`
}

// NullableInt is a PATCH field that can be absent, null, or a number.
// A plain *int collapses the first two, which would make "clear the
// tenor" unexpressible.
type NullableInt struct {
	Set   bool
	Value *int
}

func (n *NullableInt) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// PayBillRequest settles one occurrence of a bill. The amount is always
// the user's: the band is an estimate, never an authority to write.
type PayBillRequest struct {
	AmountMinor  int64      `json:"amount_minor" validate:"required,min=1"`
	SourceID     *uuid.UUID `json:"source_id" validate:"omitempty,uuid"`
	OccurredDate *string    `json:"occurred_date" validate:"omitempty,yyyy-mm-date"`
}

// ---------------------------------------------------------------- budgets

type CreateBudgetRequest struct {
	CategoryID  uuid.UUID `json:"category_id" validate:"required,uuid"`
	Period      string    `json:"period" validate:"required,yyyy-mm"`
	AmountMinor int64     `json:"amount_minor" validate:"required,min=1"`
}

type UpdateBudgetRequest struct {
	AmountMinor int64 `json:"amount_minor" validate:"required,min=1"`
}

// ------------------------------------------------------------- hold queue

// ResolveHoldRequest confirms (execute) or discards a parked action.
// Overrides are the corrections the user made in the capture or receipt
// sheet before confirming; they only apply to `confirm`.
type ResolveHoldRequest struct {
	Action    string         `json:"action" validate:"required,oneof=confirm discard"`
	Overrides *HoldOverrides `json:"overrides" validate:"omitempty"`
}

type HoldOverrides struct {
	AmountMinor         *int64     `json:"amount_minor" validate:"omitempty"`
	SourceID            *uuid.UUID `json:"source_id" validate:"omitempty,uuid"`
	DestinationSourceID *uuid.UUID `json:"destination_source_id" validate:"omitempty,uuid"`
	CategoryID          *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
	Merchant            *string    `json:"merchant"`
	Note                *string    `json:"note"`
	OccurredDate        *string    `json:"occurred_date" validate:"omitempty,yyyy-mm-date"`
	// Collapse writes a receipt as one line at its printed total instead
	// of one line per item.
	Collapse *bool `json:"collapse"`
	// Items are per-line corrections for a receipt, keyed by the line's
	// `n`.
	Items []HoldItemOverride `json:"items" validate:"omitempty,dive"`
}

// HoldItemOverride corrects one parked receipt line. N is the line's
// 1-based `n` as parked, so the correction survives a re-render.
type HoldItemOverride struct {
	N          int        `json:"n" validate:"required,min=1"`
	CategoryID *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
}

// --------------------------------------------------------------- settings

// UpdateSettingsRequest carries the whole settings form. A missing or
// null photo_default_source_id is meaningful: it clears the receipt
// fallback so the bot asks which Source paid every time.
type UpdateSettingsRequest struct {
	IncomeHorizonDays    int        `json:"income_horizon_days" validate:"required,min=1,max=90"`
	PhotoDefaultSourceID *uuid.UUID `json:"photo_default_source_id"`
	// ReminderHour is optional so a client that predates the bill digest
	// cannot silently reset the user's send hour.
	ReminderHour *int `json:"reminder_hour" validate:"omitempty,min=0,max=23"`
}

// ------------------------------------------------------------ admin: llm

// UpdateLLMProviderRequest is the whole AI-provider form. APIKey is a
// three-state field and the tri-state is the point: absent/null keeps
// the stored key (so the admin can change only the model without
// re-typing a secret the API never shows back), "" clears it and turns
// AI off, and a value replaces it.
type UpdateLLMProviderRequest struct {
	BaseURL              string  `json:"base_url" validate:"required,url"`
	APIKey               *string `json:"api_key"`
	Model                string  `json:"model" validate:"required"`
	VisionModel          string  `json:"vision_model"`
	TimeoutSeconds       int     `json:"timeout_seconds" validate:"required,min=1,max=60"`
	VisionTimeoutSeconds int     `json:"vision_timeout_seconds" validate:"required,min=1,max=120"`
}

// LLMModelsRequest asks a provider what it serves. It carries the
// draft base URL and key so the admin can list models before saving;
// an absent key falls back to the stored one, then to the environment.
type LLMModelsRequest struct {
	BaseURL string  `json:"base_url" validate:"required,url"`
	APIKey  *string `json:"api_key"`
}
