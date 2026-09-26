package llm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The retry is only real if the second attempt still has time to run:
// the vision call shares its deadline with the photo pipeline, so a
// first attempt that takes the whole configured timeout leaves nothing.
func TestAttemptTimeoutSplitsForARetry(t *testing.T) {
	tests := []struct {
		name         string
		remaining    time.Duration
		attemptsLeft int
		configured   time.Duration
		want         time.Duration
	}{
		{
			name:         "vision retry fits the work budget",
			remaining:    130 * time.Second,
			attemptsLeft: 2,
			configured:   90 * time.Second,
			want:         65 * time.Second,
		},
		{
			name:         "text call keeps its configured timeout",
			remaining:    45 * time.Second,
			attemptsLeft: 2,
			configured:   20 * time.Second,
			want:         20 * time.Second,
		},
		{
			name:         "below the split floor takes one full attempt",
			remaining:    200 * time.Millisecond,
			attemptsLeft: 2,
			configured:   100 * time.Millisecond,
			want:         100 * time.Millisecond,
		},
		{
			name:         "a trivially small share is not split",
			remaining:    time.Second,
			attemptsLeft: 2,
			configured:   90 * time.Second,
			want:         time.Second,
		},
		{
			name: "a budget that fits no full timeout is not split",
			// The auxiliary enrichment budget hands out five seconds
			// against a ten-second configured timeout: one bounded
			// attempt, not two fragments.
			remaining:    5 * time.Second,
			attemptsLeft: 2,
			configured:   10 * time.Second,
			want:         5 * time.Second,
		},
		{
			name:         "an expired budget has no attempt left",
			remaining:    0,
			attemptsLeft: 2,
			configured:   90 * time.Second,
			want:         0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			budget, cancel := context.WithTimeout(context.Background(), tc.remaining)
			defer cancel()

			got := attemptTimeout(budget, tc.attemptsLeft, tc.configured)

			assert.InDelta(t, float64(tc.want), float64(got), float64(5*time.Millisecond))
			if tc.want == 0 {
				assert.Zero(t, got, "an expired budget must stop the loop")
			}
		})
	}
}
