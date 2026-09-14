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

// JakartaDate returns today's calendar date (YYYY-MM-DD).
func JakartaDate() string {
	return JakartaNow().Format("2006-01-02")
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

// currentMonthBounds returns [start, end) for the current Jakarta month.
func currentMonthBounds() (time.Time, time.Time) {
	now := JakartaNow()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, jakartaLoc)
	return start, start.AddDate(0, 1, 0)
}
