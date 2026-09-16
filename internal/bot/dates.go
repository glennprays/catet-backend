package bot

import "time"

// Localised calendar labels. These are date *formatting*, not copy: the
// templates own every sentence, but a date still has to render in the
// reader's language.
var (
	idMonths   = [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}
	idWeekdays = [...]string{"Min", "Sen", "Sel", "Rab", "Kam", "Jum", "Sab"}
)

// formatDay renders "12 Sep" in the requested language.
func formatDay(t time.Time, lang string) string {
	if lang == "en" {
		return t.Format("2 Jan")
	}
	return t.Format("2") + " " + idMonths[int(t.Month())-1]
}

// formatWeekday renders "Sen 12 Sep" — the anomaly-day label in the
// weekly ritual.
func formatWeekday(t time.Time, lang string) string {
	if lang == "en" {
		return t.Format("Mon 2 Jan")
	}
	return idWeekdays[int(t.Weekday())] + " " + formatDay(t, lang)
}
