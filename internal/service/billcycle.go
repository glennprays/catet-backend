// Bill recurrence and reservation arithmetic: plain, pure code with no
// I/O, because the engine owns every number (Principle 2) and the
// calendar, the reminder digest and Safe-to-Spend must agree on what
// "the next occurrence" means.
package service

import (
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
)

// occurrenceCap bounds every projection loop. A caller asking for a
// decade of a weekly bill gets 400 dates, not an unbounded walk.
const occurrenceCap = 400

// AnchorDay is the day-of-month a bill is pinned to.
func AnchorDay(d time.Time) int {
	return d.Day()
}

// NextOccurrence returns the due date after `from` for a cadence,
// re-anchored to anchorDay so a bill due the 31st survives February:
// Jan 31 → Feb 28 → Mar 31, never decaying to the 28th. ok is false for
// `once`, which has no next occurrence.
func NextOccurrence(cadence string, from time.Time, anchorDay int) (time.Time, bool) {
	switch cadence {
	case "once":
		return time.Time{}, false
	case "weekly":
		return from.AddDate(0, 0, 7), true
	case "monthly":
		return anchored(from.Year(), from.Month()+1, anchorDay, from.Location()), true
	case "annual":
		return anchored(from.Year()+1, from.Month(), anchorDay, from.Location()), true
	default:
		return time.Time{}, false
	}
}

// anchored builds year-month-day clamped to the month's length, so
// Feb 29 in a common year lands on Feb 28 and the anchor is kept for
// the month after.
func anchored(year int, month time.Month, day int, loc *time.Location) time.Time {
	// Normalise month overflow (December + 1) through time.Date on day 1.
	first := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	if last := daysInMonth(first.Year(), first.Month(), loc); day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, loc)
}

func daysInMonth(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

// Occurrences lists a bill's due dates inside [from, to), walking from
// its current next_due_date. A tenor (remaining_count) caps how many
// occurrences are left in total, so a 2-installment bill never projects
// a third. Always non-nil.
func Occurrences(b entity.Bill, from, to time.Time) []time.Time {
	out := make([]time.Time, 0, 8)
	if !to.After(from) {
		return out
	}
	left := occurrenceCap
	if b.RemainingCount != nil {
		if *b.RemainingCount <= 0 {
			return out
		}
		if *b.RemainingCount < left {
			left = *b.RemainingCount
		}
	}
	anchor := b.AnchorDay
	if anchor <= 0 {
		anchor = AnchorDay(b.NextDueDate)
	}
	d := b.NextDueDate
	for i := 0; i < left && i < occurrenceCap; i++ {
		if !d.Before(to) {
			break
		}
		if !d.Before(from) {
			out = append(out, d)
		}
		next, ok := NextOccurrence(b.Cadence, d, anchor)
		if !ok {
			break
		}
		d = next
	}
	return out
}

// ReservedBetween is the conservative reservation: band max for every
// occurrence of every active bill inside [from, to). includeOverdue
// pulls the window back to a bill's due date when it is already past,
// so an unpaid bill stays reserved instead of silently freeing money
// the user still owes (D7/D10).
func ReservedBetween(bills []entity.Bill, from, to time.Time, includeOverdue bool) int64 {
	var sum int64
	for i := range bills {
		b := bills[i]
		if !b.Active {
			continue
		}
		start := from
		if includeOverdue && b.NextDueDate.Before(start) {
			start = b.NextDueDate
		}
		sum += b.AmountMaxMinor * int64(len(Occurrences(b, start, to)))
	}
	return sum
}
