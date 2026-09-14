// Package response holds HTTP response payloads — the `data` side of
// the envelope. Money fields are int64 minor units, ready to render.
package response

import (
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
)

// ------------------------------------------------------------------ auth

type OtpRequestData struct {
	Sent bool `json:"sent"`
}

type OtpVerifyData struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type MeData struct {
	Phone string `json:"phone"`
}

// --------------------------------------------------------------- sources

// SourceResponse is a source with its ledger-computed balance.
type SourceResponse struct {
	entity.Source
	BalanceMinor int64 `json:"balance_minor"`
}

type SourcesData struct {
	Sources            []SourceResponse `json:"sources"`
	TotalAvailableMinor int64           `json:"total_available_minor"`
	TotalDebtMinor      int64           `json:"total_debt_minor"`
}

type ArchivedData struct {
	Archived bool `json:"archived"`
}

// ---------------------------------------------------------- transactions

// LedgerItem is the flattened list row: user-perspective signed amount
// (expense −, income +, transfer −) plus display names.
type LedgerItem struct {
	TransactionID         uuid.UUID `json:"transaction_id"`
	OccurredAt            time.Time `json:"occurred_at"`
	Type                  string    `json:"type"`
	Merchant              *string   `json:"merchant"`
	Note                  *string   `json:"note"`
	AmountMinor           int64     `json:"amount_minor"`
	CategoryID            *uuid.UUID `json:"category_id"`
	CategoryName          *string   `json:"category_name"`
	SourceID              uuid.UUID `json:"source_id"`
	SourceName            string    `json:"source_name"`
	ReviewStatus          string    `json:"review_status"`
	CategorySource        *string   `json:"category_source"`
	CategoryConfidence    *float64  `json:"category_confidence"`
	Voided                bool      `json:"voided"`
	ReversesTransactionID *uuid.UUID `json:"reverses_transaction_id"`
}

type TransactionListData struct {
	Transactions []LedgerItem `json:"transactions"`
}

type VoidedData struct {
	Voided bool `json:"voided"`
}

// Transaction (detail) reuses entity.Transaction — it already carries
// legs, names, edit_history and voiding state with json tags.

// ------------------------------------------------------------- categories

type CategoriesData struct {
	Categories []entity.Category `json:"categories"`
}

// ------------------------------------------------------------------ bills

type BillsData struct {
	Bills []entity.Bill `json:"bills"`
}

// ---------------------------------------------------------------- budgets

type BudgetsData struct {
	Period  string           `json:"period"`
	Budgets []entity.BudgetRow `json:"budgets"`
}

// -------------------------------------------------------------- overview

type BadgeCounts struct {
	NeedsReview int64 `json:"needs_review"`
	HoldQueue   int64 `json:"hold_queue"`
}

type OverviewData struct {
	StsMinor               int64          `json:"sts_minor"`
	RunwayDays             int64          `json:"runway_days"`
	DailyAllowanceMinor    int64          `json:"daily_allowance_minor"`
	ReservedTomorrowMinor  int64          `json:"reserved_tomorrow_minor"`
	ReservedMinor          int64          `json:"reserved_minor"`
	AvailableMinor         int64          `json:"available_minor"`
	EssentialMinor         int64          `json:"essential_minor"`
	LifestyleMinor         int64          `json:"lifestyle_minor"`
	Recent                 []LedgerItem   `json:"recent"`
	Badges                 BadgeCounts    `json:"badges"`
}

type BadgesData = BadgeCounts

// ----------------------------------------------------------- analytics

type CategoryBreakdownNode struct {
	CategoryID  uuid.UUID               `json:"category_id"`
	Name        string                  `json:"name"`
	Essential   bool                    `json:"essential"`
	AmountMinor int64                   `json:"amount_minor"`
	Share       float64                 `json:"share"`
	Children    []CategoryBreakdownNode `json:"children,omitempty"`
}

type CategoryBreakdownData struct {
	TotalMinor int64                   `json:"total_minor"`
	Categories []CategoryBreakdownNode `json:"categories"`
}

type IncomeExpenseBucket struct {
	WeekStart    string `json:"week_start"`
	IncomeMinor  int64  `json:"income_minor"`
	ExpenseMinor int64  `json:"expense_minor"`
}

type IncomeExpenseData struct {
	Buckets []IncomeExpenseBucket `json:"buckets"`
}

type StsPoint struct {
	Date       string `json:"date"`
	StsMinor   int64  `json:"sts_minor"`
	RunwayDays int64  `json:"runway_days"`
}

type StsTrendData struct {
	Points []StsPoint `json:"points"`
}

type HeatmapDay struct {
	Date         string `json:"date"`
	OutflowMinor int64  `json:"outflow_minor"`
	Anomaly      bool   `json:"anomaly"`
}

type HeatmapData struct {
	Days      []HeatmapDay `json:"days"`
	MeanMinor int64        `json:"mean_minor"`
	StdMinor  int64        `json:"std_minor"`
}

type TopMerchant struct {
	Name        string `json:"name"`
	AmountMinor int64  `json:"amount_minor"`
	TxCount     int64  `json:"tx_count"`
}

type TopMerchantsData struct {
	Merchants []TopMerchant `json:"merchants"`
}

type EssentialLifestyleData struct {
	EssentialMinor int64 `json:"essential_minor"`
	LifestyleMinor int64 `json:"lifestyle_minor"`
}

// ------------------------------------------------------------ hold queue

type HoldQueueData struct {
	Items []entity.PendingAction `json:"items"`
}

type ResolvedData struct {
	Resolved bool `json:"resolved"`
}

// ----------------------------------------------------------------- misc

type DeletedData struct {
	Deleted bool `json:"deleted"`
}

type BillActiveData struct {
	Active bool `json:"active"`
}

type SettingsData struct {
	IncomeHorizonDays int    `json:"income_horizon_days"`
	Phone             string `json:"phone"`
}
