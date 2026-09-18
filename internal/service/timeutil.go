// Time helpers: every day boundary and calendar-date param is
// Asia/Jakarta; storage stays UTC (timestamptz).
package service

import (
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
)

var jakartaLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// time/tzdata is embedded in main; failure here means a broken
		// build, not a runtime condition.
		panic("load Asia/Jakarta timezone: " + err.Error())
	}
	return loc
}()

// JakartaNow returns the current instant in the Jakarta location.
func JakartaNow() time.Time {
	return time.Now().In(jakartaLoc)
}

// Jakarta is the location every calendar comparison runs in; the host's
// time.Local says nothing about an Indonesian ledger.
func Jakarta() *time.Location { return jakartaLoc }

// JakartaDate returns today's calendar date (YYYY-MM-DD).
func JakartaDate() string {
	return JakartaNow().Format("2006-01-02")
}

// JakartaToday returns today as a Jakarta-midnight date — the anchor
// every bill projection, reservation window and reminder slot is
// measured from.
func JakartaToday() time.Time {
	now := JakartaNow()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, jakartaLoc)
}

// parseJakartaDate parses a strict YYYY-MM-DD calendar date.
func parseJakartaDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, jakartaLoc)
	if err != nil {
		return time.Time{}, apperror.BadRequestf("invalid date %q, want YYYY-MM-DD", s)
	}
	return t, nil
}

// JakartaDayBounds maps a calendar date to the UTC instant range
// [start, end) covering that Jakarta day.
func JakartaDayBounds(dateStr string) (time.Time, time.Time, error) {
	start, err := parseJakartaDate(dateStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, start.AddDate(0, 0, 1), nil
}

// JakartaNoon maps a calendar date to midday Jakarta — the canonical
// timestamp for events the user only dated, never timed.
func JakartaNoon(dateStr string) (time.Time, error) {
	start, err := parseJakartaDate(dateStr)
	if err != nil {
		return time.Time{}, err
	}
	return start.Add(12 * time.Hour), nil
}

// JakartaMonthBounds maps "YYYY-MM" to the UTC instant range covering
// that Jakarta calendar month.
func JakartaMonthBounds(period string) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01", period, jakartaLoc)
	if err != nil {
		return time.Time{}, time.Time{}, apperror.BadRequestf("invalid period %q, want YYYY-MM", period)
	}
	return start, start.AddDate(0, 1, 0), nil
}

// JakartaMonthPeriod returns the YYYY-MM period containing t.
func JakartaMonthPeriod(t time.Time) string {
	return t.In(jakartaLoc).Format("2006-01")
}

// JakartaMonthStart maps "YYYY-MM" to the Jakarta first-of-month at
// 00:00 — the anchor every budget segment bound is compared against.
func JakartaMonthStart(period string) (time.Time, error) {
	start, _, err := JakartaMonthBounds(period)
	return start, err
}

// JakartaMonthOf re-anchors any instant (including a `date` read back
// from Postgres as UTC midnight) to the Jakarta first-of-month it names.
func JakartaMonthOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, jakartaLoc)
}

// MonthAfter and MonthBefore step a first-of-month anchor by one month.
func MonthAfter(t time.Time) time.Time  { return t.AddDate(0, 1, 0) }
func MonthBefore(t time.Time) time.Time { return t.AddDate(0, -1, 0) }

// currentMonthBounds returns [start, end) for the current Jakarta month.
func currentMonthBounds() (time.Time, time.Time) {
	now := JakartaNow()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, jakartaLoc)
	return start, start.AddDate(0, 1, 0)
}
