package auth

import (
	"sync"
	"time"
)

// Throttle is an in-memory sliding-window counter keyed by an arbitrary
// string (a phone, a client IP). It lives in Go rather than in front of
// the process because the Go API is internal-only: the Next.js BFF is
// the single client, and putting the limit here means it applies to the
// operator CLI and the bot as well.
//
// One process only. A second API instance would have its own window,
// which is acceptable for a self-hosted single-node deployment and is
// the reason this is a counter and not a promise.
type Throttle struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func NewThrottle(limit int, window time.Duration) *Throttle {
	return &Throttle{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
		now:    time.Now,
	}
}

// Allow records one attempt and reports whether it stays within the
// limit. An empty key is never counted: it would merge every anonymous
// caller into one bucket and lock them all out together.
func (t *Throttle) Allow(key string) bool {
	if key == "" {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	cutoff := now.Add(-t.window)

	kept := t.hits[key][:0]
	for _, at := range t.hits[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= t.limit {
		t.hits[key] = kept
		return false
	}
	t.hits[key] = append(kept, now)

	// Keys whose window has emptied are dropped so a stream of distinct
	// IPs cannot grow the map without bound.
	t.prune(cutoff)
	return true
}

// prune drops empty buckets. Called under the lock from Allow; the scan
// is bounded by the number of distinct keys seen inside one window,
// which for this deployment is tiny.
func (t *Throttle) prune(cutoff time.Time) {
	for key, times := range t.hits {
		if len(times) == 0 {
			delete(t.hits, key)
			continue
		}
		if times[len(times)-1].Before(cutoff) {
			delete(t.hits, key)
		}
	}
}
