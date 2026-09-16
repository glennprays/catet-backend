package bot

import (
	"context"
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
)

// batal reverses the last batch this chat wrote (or one numbered line of
// it). Nothing is deleted: a reversal is a linked, visible entry
// (ADR-0001), which is why the bot can offer it without a confirmation.
// It returns the reversal rows it wrote, so the message can be stamped
// as having changed the ledger.
func (h *Harness) batal(ctx context.Context, state *repository.ChatState, index int, lang string) (string, []uuid.UUID, error) {
	if state.LastBatchMessageID == nil {
		return tmpl.Render(lang, "batal_nothing", nil), nil, nil
	}
	batch, err := h.inbound.Get(ctx, *state.LastBatchMessageID)
	if err != nil {
		return "", nil, fmt.Errorf("load batch: %w", err)
	}
	if batch == nil || len(batch.TransactionIDs) == 0 {
		return tmpl.Render(lang, "batal_nothing", nil), nil, nil
	}
	if index < 0 || index > len(batch.TransactionIDs) {
		return tmpl.Render(lang, "batal_bad_index", map[string]any{
			"N": index, "Count": len(batch.TransactionIDs),
		}), nil, nil
	}

	targets := batch.TransactionIDs
	if index > 0 {
		targets = []uuid.UUID{batch.TransactionIDs[index-1]}
	}

	type reversedItem struct {
		N        int
		Amount   string
		Merchant string
	}
	items := make([]reversedItem, 0, len(targets))
	reversalIDs := make([]uuid.UUID, 0, len(targets))
	remaining := make([]uuid.UUID, 0, len(batch.TransactionIDs))
	reversedSet := map[uuid.UUID]bool{}

	for _, id := range targets {
		original, err := h.txRepo.Get(ctx, id)
		if err != nil {
			return "", nil, fmt.Errorf("load transaction: %w", err)
		}
		if original == nil {
			continue
		}
		note := "batal via WhatsApp"
		reversal, err := h.txs.Reverse(ctx, id, &note, botOrigin)
		if err != nil {
			if errIsConflict(err) {
				// Already reversed or voided — nothing left to undo.
				continue
			}
			return "", nil, fmt.Errorf("reverse transaction: %w", err)
		}
		reversedSet[id] = true
		if reversal != nil {
			reversalIDs = append(reversalIDs, reversal.ID)
		}
		merchant := ""
		if original.Merchant != nil {
			merchant = *original.Merchant
		}
		items = append(items, reversedItem{
			N: len(items) + 1, Amount: money.FormatIDR(original.AmountMinor), Merchant: merchant,
		})
	}

	if len(items) == 0 {
		return tmpl.Render(lang, "batal_nothing", nil), nil, nil
	}

	for _, id := range batch.TransactionIDs {
		if !reversedSet[id] {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		// The whole batch is gone; there is nothing left to address.
		state.LastBatchMessageID = nil
	}
	if err := h.saveState(ctx, state); err != nil {
		return "", nil, err
	}

	return tmpl.Render(lang, "batal_done", map[string]any{"Items": items}), reversalIDs, nil
}

// rekapHari answers the pull-only daily ritual. The bot never sends it
// unprompted (Principle 4: no nagging).
func (h *Harness) rekapHari(ctx context.Context, lang string) (string, error) {
	today := service.JakartaDate()
	start, end, err := service.JakartaDayBounds(today)
	if err != nil {
		return "", fmt.Errorf("resolve today: %w", err)
	}
	_, expense, err := h.analytics.DailyFlows(ctx, start, end)
	if err != nil {
		return "", fmt.Errorf("daily flows: %w", err)
	}
	overview, err := h.over.Build(ctx)
	if err != nil {
		return "", fmt.Errorf("overview: %w", err)
	}
	monthStart, _, err := service.JakartaMonthBounds(service.JakartaMonthPeriod(service.JakartaNow()))
	if err != nil {
		return "", fmt.Errorf("resolve month: %w", err)
	}
	uncategorized, err := h.analytics.UncategorizedCount(ctx, monthStart)
	if err != nil {
		return "", fmt.Errorf("count uncategorized: %w", err)
	}

	// Due-tomorrow names and the overdue count ride along: the daily
	// ritual is where a user asks "what is coming?", and answering it
	// here keeps the bot pull-only for everything but the opted-in
	// digest (ADR-0006).
	bills, err := h.billRepo.ListActive(ctx)
	if err != nil {
		return "", fmt.Errorf("list bills: %w", err)
	}
	todayDate := service.JakartaToday()
	tomorrow := todayDate.AddDate(0, 0, 1)
	dueTomorrow := make([]string, 0, 2)
	overdue := 0
	for _, b := range bills {
		if len(service.Occurrences(b, tomorrow, tomorrow.AddDate(0, 0, 1))) > 0 {
			dueTomorrow = append(dueTomorrow, b.Name)
		}
		if b.NextDueDate.Before(todayDate) {
			overdue++
		}
	}

	spent := expense[today]
	verdict := h.verdict(ctx, lang, map[string]string{
		"period":        "today",
		"spent":         money.FormatIDR(spent),
		"safe_to_spend": money.FormatIDR(overview.StsMinor),
		"uncategorized": fmt.Sprintf("%d", uncategorized),
	})
	return tmpl.Render(lang, "rekap_hari", map[string]any{
		"Spent":            money.FormatIDR(spent),
		"Sts":              money.FormatIDR(overview.StsMinor),
		"ReservedTomorrow": money.FormatIDR(overview.ReservedTomorrowMinor),
		"Uncategorized":    uncategorized,
		"DueTomorrow":      dueTomorrow,
		"Overdue":          overdue,
		"Verdict":          verdict,
	}), nil
}

// rekapMinggu answers the weekly ritual: this Monday-start week against
// the previous one, the unusual days, and where the money went.
func (h *Harness) rekapMinggu(ctx context.Context, lang string) (string, error) {
	now := service.JakartaNow()
	weekStart := mondayOf(now)
	weekEnd := weekStart.AddDate(0, 0, 7)
	prevStart := weekStart.AddDate(0, 0, -7)

	_, thisWeek, err := h.analytics.DailyFlows(ctx, weekStart, weekEnd)
	if err != nil {
		return "", fmt.Errorf("daily flows: %w", err)
	}
	_, lastWeek, err := h.analytics.DailyFlows(ctx, prevStart, weekStart)
	if err != nil {
		return "", fmt.Errorf("daily flows (previous): %w", err)
	}
	var thisSum, lastSum int64
	for _, v := range thisWeek {
		thisSum += v
	}
	for _, v := range lastWeek {
		lastSum += v
	}

	heatmap, err := h.ana.Heatmap(ctx, weekStart, weekEnd)
	if err != nil {
		return "", err
	}
	anomalies := []string{}
	for _, day := range heatmap.Days {
		if !day.Anomaly {
			continue
		}
		parsedDay, err := time.ParseInLocation("2006-01-02", day.Date, now.Location())
		if err != nil {
			continue
		}
		anomalies = append(anomalies, formatWeekday(parsedDay, lang))
	}

	top, err := h.ana.TopMerchants(ctx, weekStart, weekEnd, 3)
	if err != nil {
		return "", err
	}
	type topItem struct{ Name, Amount string }
	items := make([]topItem, 0, len(top.Merchants))
	for _, m := range top.Merchants {
		items = append(items, topItem{Name: m.Name, Amount: money.FormatIDR(m.AmountMinor)})
	}

	monthStart, _, err := service.JakartaMonthBounds(service.JakartaMonthPeriod(now))
	if err != nil {
		return "", fmt.Errorf("resolve month: %w", err)
	}
	uncategorized, err := h.analytics.UncategorizedCount(ctx, monthStart)
	if err != nil {
		return "", fmt.Errorf("count uncategorized: %w", err)
	}

	verdict := h.verdict(ctx, lang, map[string]string{
		"period":        "this week",
		"spent":         money.FormatIDR(thisSum),
		"previous_week": money.FormatIDR(lastSum),
		"unusual_days":  fmt.Sprintf("%d", len(anomalies)),
	})
	return tmpl.Render(lang, "rekap_minggu", map[string]any{
		"Spent":         money.FormatIDR(thisSum),
		"Delta":         money.FormatSigned(thisSum - lastSum),
		"AnomalyDays":   anomalies,
		"Uncategorized": uncategorized,
		"Top":           items,
		"Verdict":       verdict,
	}), nil
}

// verdict is the optional one-line commentary. It is off by default and
// numbers are stripped by the client — the templates own every figure.
func (h *Harness) verdict(ctx context.Context, lang string, facts map[string]string) string {
	if !h.cfg.LlmVerdictEnabled || h.llm == nil {
		return ""
	}
	out, err := h.llm.Verdict(ctx, lang, facts)
	if err != nil {
		h.log.Warn(traceOf(ctx), "verdict failed", map[string]any{"error": err.Error()})
		return ""
	}
	return out
}

// mondayOf returns 00:00 of the Monday starting t's week (Jakarta).
func mondayOf(t time.Time) time.Time {
	offset := (int(t.Weekday()) + 6) % 7
	day := t.AddDate(0, 0, -offset)
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, t.Location())
}
