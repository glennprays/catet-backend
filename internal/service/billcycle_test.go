package service

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
)

func jkt(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, jakartaLoc)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBillCycleNextOccurrence(t *testing.T) {
	cases := []struct {
		name    string
		cadence string
		from    string
		anchor  int
		want    string
		wantOK  bool
	}{
		{"once has no next", "once", "2026-01-31", 31, "", false},
		{"weekly adds seven days", "weekly", "2026-01-28", 28, "2026-02-04", true},
		{"monthly clamps to February", "monthly", "2026-01-31", 31, "2026-02-28", true},
		{"monthly recovers the anchor", "monthly", "2026-02-28", 31, "2026-03-31", true},
		{"monthly wraps the year", "monthly", "2026-12-15", 15, "2027-01-15", true},
		{"annual keeps the month", "annual", "2026-03-10", 10, "2027-03-10", true},
		{"annual clamps leap day", "annual", "2028-02-29", 29, "2029-02-28", true},
		{"unknown cadence has no next", "quarterly", "2026-01-01", 1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NextOccurrence(tc.cadence, jkt(tc.from), tc.anchor)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if want := jkt(tc.want); !got.Equal(want) {
				t.Fatalf("next = %s, want %s", got.Format("2006-01-02"), tc.want)
			}
		})
	}
}

func bill(cadence, due string, maxMinor int64, remaining *int) entity.Bill {
	d := jkt(due)
	return entity.Bill{
		Cadence:        cadence,
		NextDueDate:    d,
		AnchorDay:      AnchorDay(d),
		AmountMaxMinor: maxMinor,
		RemainingCount: remaining,
		Active:         true,
	}
}

func TestBillCycleOccurrences(t *testing.T) {
	cases := []struct {
		name string
		b    entity.Bill
		from string
		to   string
		want []string
	}{
		{
			name: "monthly anchor recovery across February",
			b:    bill("monthly", "2026-01-31", 0, nil),
			from: "2026-01-01", to: "2026-04-01",
			want: []string{"2026-01-31", "2026-02-28", "2026-03-31"},
		},
		{
			name: "weekly four times in thirty days",
			b:    bill("weekly", "2026-03-02", 0, nil),
			from: "2026-03-01", to: "2026-03-31",
			want: []string{"2026-03-02", "2026-03-09", "2026-03-16", "2026-03-23", "2026-03-30"},
		},
		{
			name: "once yields at most one",
			b:    bill("once", "2026-03-10", 0, nil),
			from: "2026-01-01", to: "2027-01-01",
			want: []string{"2026-03-10"},
		},
		{
			name: "tenor caps the projection",
			b:    bill("monthly", "2026-01-10", 0, new(2)),
			from: "2026-01-01", to: "2027-01-01",
			want: []string{"2026-01-10", "2026-02-10"},
		},
		{
			name: "exhausted tenor yields nothing",
			b:    bill("monthly", "2026-01-10", 0, new(0)),
			from: "2026-01-01", to: "2027-01-01",
			want: nil,
		},
		{
			name: "window excludes its end",
			b:    bill("monthly", "2026-01-10", 0, nil),
			from: "2026-01-01", to: "2026-01-10",
			want: nil,
		},
		{
			name: "window skips occurrences before from",
			b:    bill("monthly", "2026-01-10", 0, nil),
			from: "2026-02-01", to: "2026-03-01",
			want: []string{"2026-02-10"},
		},
		{
			name: "empty window",
			b:    bill("monthly", "2026-01-10", 0, nil),
			from: "2026-01-01", to: "2026-01-01",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Occurrences(tc.b, jkt(tc.from), jkt(tc.to))
			if got == nil {
				t.Fatal("Occurrences returned nil, want empty slice")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d occurrences %v, want %v", len(got), fmtDates(got), tc.want)
			}
			for i, w := range tc.want {
				if !got[i].Equal(jkt(w)) {
					t.Fatalf("occurrence %d = %s, want %s", i, got[i].Format("2006-01-02"), w)
				}
			}
		})
	}
}

func fmtDates(ds []time.Time) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Format("2006-01-02")
	}
	return out
}

func TestReservedBetween(t *testing.T) {
	today := jkt("2026-03-01")

	cases := []struct {
		name           string
		bills          []entity.Bill
		from, to       string
		includeOverdue bool
		want           int64
	}{
		{
			name:  "weekly counts every occurrence in the horizon",
			bills: []entity.Bill{bill("weekly", "2026-03-02", 50_000, nil)},
			from:  "2026-03-01", to: "2026-03-31",
			want: 5 * 50_000,
		},
		{
			name:  "due outside the horizon reserves nothing",
			bills: []entity.Bill{bill("monthly", "2026-03-20", 400_000, nil)},
			from:  "2026-03-01", to: "2026-03-08",
			want: 0,
		},
		{
			name:  "overdue monthly stays reserved",
			bills: []entity.Bill{bill("monthly", "2026-01-01", 100_000, nil)},
			from:  "2026-03-01", to: "2026-03-31",
			includeOverdue: true,
			want:           3 * 100_000, // Jan 1, Feb 1, Mar 1
		},
		{
			name:  "overdue ignored without the pull-back",
			bills: []entity.Bill{bill("monthly", "2026-01-01", 100_000, nil)},
			from:  "2026-03-02", to: "2026-03-03",
			includeOverdue: false,
			want:           0,
		},
		{
			name:  "tenor caps the reservation",
			bills: []entity.Bill{bill("monthly", "2026-01-01", 100_000, new(1))},
			from:  "2026-03-01", to: "2026-03-31",
			includeOverdue: true,
			want:           100_000,
		},
		{
			name: "inactive bills reserve nothing",
			bills: []entity.Bill{func() entity.Bill {
				b := bill("monthly", "2026-03-05", 100_000, nil)
				b.Active = false
				return b
			}()},
			from: "2026-03-01", to: "2026-03-31",
			want: 0,
		},
		{
			name:  "tomorrow-only window picks the single occurrence",
			bills: []entity.Bill{bill("monthly", "2026-03-02", 250_000, nil), bill("monthly", "2026-03-03", 999_000, nil)},
			from:  "2026-03-02", to: "2026-03-03",
			want: 250_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReservedBetween(tc.bills, jkt(tc.from), jkt(tc.to), tc.includeOverdue)
			if got != tc.want {
				t.Fatalf("reserved = %d, want %d (today %s)", got, tc.want, today.Format("2006-01-02"))
			}
		})
	}
}
