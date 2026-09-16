package repository

import (
	"strconv"
	"time"
)

// jakartaLoc is the ledger's day boundary. pgx hands back a `date`
// column as UTC midnight; every consumer reasons in Jakarta calendar
// days, so dates are re-pinned on the way out and a due date compares
// equal to the one the service wrote.
var jakartaLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		panic("load Asia/Jakarta timezone: " + err.Error())
	}
	return loc
}()

// jakartaDay re-pins a scanned date to Jakarta midnight, keeping its
// calendar day.
func jakartaDay(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, jakartaLoc)
}

// jakartaDayPtr is jakartaDay for a nullable date column.
func jakartaDayPtr(d *time.Time) *time.Time {
	if d == nil {
		return nil
	}
	out := jakartaDay(*d)
	return &out
}

// itoa is a tiny shorthand for building numbered SQL placeholders.
func itoa(i int) string {
	return strconv.Itoa(i)
}

// joinComma joins SQL fragments with ", ".
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
