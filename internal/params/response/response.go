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

// SessionData is what every successful authentication returns. The
// token never reaches the browser: the Next.js BFF turns it into the
// httpOnly session cookie.
type SessionData struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InviteInfoData is what the register page may know before anybody has
// proved anything: which number this link is for.
type InviteInfoData struct {
	Phone     string    `json:"phone"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ResetTokenData struct {
	ResetToken string    `json:"reset_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type MeData struct {
	ID          uuid.UUID `json:"id"`
	Phone       string    `json:"phone"`
	DisplayName string    `json:"display_name"`
	IsAdmin     bool      `json:"is_admin"`
}

type OKData struct {
	OK bool `json:"ok"`
}

// ----------------------------------------------------------------- users

// UserData is one row of the admin users list.
type UserData struct {
	ID            uuid.UUID  `json:"id"`
	Phone         string     `json:"phone"`
	DisplayName   string     `json:"display_name"`
	IsAdmin       bool       `json:"is_admin"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

type UsersData struct {
	Users []UserData `json:"users"`
}

// InviteData never carries the token: the link exists only in the
// WhatsApp message and — for the gateway-down case — in the operator
// CLI's output.
type InviteData struct {
	ID        uuid.UUID `json:"id"`
	Phone     string    `json:"phone"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type InvitesData struct {
	Invites []InviteData `json:"invites"`
}

// --------------------------------------------------------------- sources

// SourceResponse is a source with its ledger-computed balance and
// whether any leg ever referenced it (drives permanent delete).
type SourceResponse struct {
	entity.Source
	BalanceMinor    int64 `json:"balance_minor"`
	HasTransactions bool  `json:"has_transactions"`
}

type SourcesData struct {
	Sources             []SourceResponse `json:"sources"`
	TotalAvailableMinor int64            `json:"total_available_minor"`
	TotalDebtMinor      int64            `json:"total_debt_minor"`
}

type ArchivedData struct {
	Archived bool `json:"archived"`
}

// ---------------------------------------------------------- transactions

// LedgerItem is the flattened list row: user-perspective signed amount
// (expense −, income +, transfer −) plus display names.
type LedgerItem struct {
	TransactionID         uuid.UUID  `json:"transaction_id"`
	OccurredAt            time.Time  `json:"occurred_at"`
	Type                  string     `json:"type"`
	Merchant              *string    `json:"merchant"`
	MerchantID            *uuid.UUID `json:"merchant_id"`
	Note                  *string    `json:"note"`
	AmountMinor           int64      `json:"amount_minor"`
	CategoryID            *uuid.UUID `json:"category_id"`
	CategoryName          *string    `json:"category_name"`
	SourceID              uuid.UUID  `json:"source_id"`
	SourceName            string     `json:"source_name"`
	ReviewStatus          string     `json:"review_status"`
	CategorySource        *string    `json:"category_source"`
	CategoryConfidence    *float64   `json:"category_confidence"`
	Voided                bool       `json:"voided"`
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

// BillPaymentData is the result of settling one occurrence: the bill as
// it now stands plus the transaction that paid it, so the dashboard can
// refresh both without a second round trip.
type BillPaymentData struct {
	Bill        *entity.Bill        `json:"bill"`
	Transaction *entity.Transaction `json:"transaction"`
}

type BillPaymentsData struct {
	Payments []entity.BillPayment `json:"payments"`
}

// ---------------------------------------------------------------- budgets

type BudgetsData struct {
	Period  string             `json:"period"`
	Budgets []entity.BudgetRow `json:"budgets"`
}

// -------------------------------------------------------------- overview

type BadgeCounts struct {
	NeedsReview  int64 `json:"needs_review"`
	HoldQueue    int64 `json:"hold_queue"`
	BillsOverdue int64 `json:"bills_overdue"`
}

type OverviewData struct {
	StsMinor              int64        `json:"sts_minor"`
	RunwayDays            int64        `json:"runway_days"`
	DailyAllowanceMinor   int64        `json:"daily_allowance_minor"`
	ReservedTomorrowMinor int64        `json:"reserved_tomorrow_minor"`
	ReservedMinor         int64        `json:"reserved_minor"`
	AvailableMinor        int64        `json:"available_minor"`
	EssentialMinor        int64        `json:"essential_minor"`
	LifestyleMinor        int64        `json:"lifestyle_minor"`
	Recent                []LedgerItem `json:"recent"`
	Badges                BadgeCounts  `json:"badges"`
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
	MerchantID  uuid.UUID `json:"merchant_id"`
	Name        string    `json:"name"`
	AmountMinor int64     `json:"amount_minor"`
	TxCount     int64     `json:"tx_count"`
}

type TopMerchantsData struct {
	Merchants []TopMerchant `json:"merchants"`
}

// ------------------------------------------------------------- merchants

// MerchantSummary is one Settings → Merchants line.
type MerchantSummary struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	TxCount    int64      `json:"tx_count"`
	TotalMinor int64      `json:"total_minor"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type MerchantListData struct {
	Merchants []MerchantSummary `json:"merchants"`
}

type MerchantData struct {
	Merchant entity.Merchant `json:"merchant"`
}

type MergedMerchantData struct {
	Merged         bool      `json:"merged"`
	IntoMerchantID uuid.UUID `json:"into_merchant_id"`
}

type EssentialLifestyleData struct {
	EssentialMinor int64 `json:"essential_minor"`
	LifestyleMinor int64 `json:"lifestyle_minor"`
}

// ------------------------------------------------------------ hold queue

type HoldQueueData struct {
	Items []entity.PendingAction `json:"items"`
}

// ResolvedData reports the decision and, when confirming executed a
// parked draft, the transactions it wrote. A receipt writes one per
// line; TransactionID stays as the first of them so existing clients
// keep working.
type ResolvedData struct {
	Resolved       bool        `json:"resolved"`
	TransactionID  *uuid.UUID  `json:"transaction_id,omitempty"`
	TransactionIDs []uuid.UUID `json:"transaction_ids,omitempty"`
}

// ----------------------------------------------------------------- misc

type DeletedData struct {
	Deleted bool `json:"deleted"`
}

type BillActiveData struct {
	Active bool `json:"active"`
}

type SettingsData struct {
	IncomeHorizonDays      int        `json:"income_horizon_days"`
	PhotoDefaultSourceID   *uuid.UUID `json:"photo_default_source_id"`
	PhotoDefaultSourceName *string    `json:"photo_default_source_name"`
	ReminderHour           int        `json:"reminder_hour"`
}

// ------------------------------------------------------------ admin: llm

// LLMProviderData is the AI provider as the dashboard may see it. The
// API key is write-only: Source says where the configuration came from
// ("dashboard" once saved, "env" while the environment is
// authoritative), APIKeySet says whether one is installed and
// APIKeyLast4 is the only fragment that ever leaves the server.
// Enabled is the bot's own answer: no key, no AI.
type LLMProviderData struct {
	Source               string `json:"source"`
	BaseURL              string `json:"base_url"`
	Model                string `json:"model"`
	VisionModel          string `json:"vision_model"`
	TimeoutSeconds       int    `json:"timeout_seconds"`
	VisionTimeoutSeconds int    `json:"vision_timeout_seconds"`
	APIKeySet            bool   `json:"api_key_set"`
	APIKeyLast4          string `json:"api_key_last4"`
	Enabled              bool   `json:"enabled"`
}

// LLMModelsData is the provider's catalogue, sorted, so the dashboard
// can offer a picker instead of asking the admin to type a model id.
type LLMModelsData struct {
	Models []string `json:"models"`
}

// LLMTestData is one connection test: it answered, how fast, and as
// which model.
type LLMTestData struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Model     string `json:"model"`
}
