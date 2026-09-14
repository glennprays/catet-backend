// Package request holds HTTP request body DTOs. Validation rules run
// through httperror.BindAndValidate and surface as 400
// VALIDATION_FAILED with FieldError[] details.
package request

import (
	"time"

	"github.com/google/uuid"
)

// ------------------------------------------------------------------ auth

type OtpRequest struct {
	Phone string `json:"phone" validate:"required"`
}

type OtpVerifyRequest struct {
	Phone string `json:"phone" validate:"required"`
	Code  string `json:"code" validate:"required"`
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

type UpdateCategoryRequest struct {
	Name      *string    `json:"name" validate:"omitempty,min=1"`
	ParentID  *uuid.UUID `json:"parent_id" validate:"omitempty,uuid"`
	Icon      *string    `json:"icon"`
	Essential *bool      `json:"essential"`
}

// ------------------------------------------------------------------ bills

type CreateBillRequest struct {
	Name             string     `json:"name" validate:"required,min=1"`
	AmountMinMinor   int64      `json:"amount_min_minor" validate:"required,min=1"`
	AmountMaxMinor   int64      `json:"amount_max_minor" validate:"required,min=1"`
	Cadence          string     `json:"cadence" validate:"required,oneof=weekly monthly annual"`
	NextDueDate      string     `json:"next_due_date" validate:"required,yyyy-mm-date"`
	LinkedSourceID   *uuid.UUID `json:"linked_source_id" validate:"omitempty,uuid"`
	CategoryID       *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
}

type UpdateBillRequest struct {
	Name             *string    `json:"name" validate:"omitempty,min=1"`
	AmountMinMinor   *int64     `json:"amount_min_minor" validate:"omitempty,min=1"`
	AmountMaxMinor   *int64     `json:"amount_max_minor" validate:"omitempty,min=1"`
	Cadence          *string    `json:"cadence" validate:"omitempty,oneof=weekly monthly annual"`
	NextDueDate      *string    `json:"next_due_date" validate:"omitempty,yyyy-mm-date"`
	LinkedSourceID   *uuid.UUID `json:"linked_source_id" validate:"omitempty,uuid"`
	CategoryID       *uuid.UUID `json:"category_id" validate:"omitempty,uuid"`
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

type ResolveHoldRequest struct {
	Action string `json:"action" validate:"required,oneof=confirm discard"`
}

// --------------------------------------------------------------- settings

type UpdateSettingsRequest struct {
	IncomeHorizonDays int `json:"income_horizon_days" validate:"required,min=1,max=90"`
}
