package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The window slides: an attacker who waits it out is allowed to try
// again, and a user who mistyped their password five minutes ago is not
// still being punished for it an hour later.
func TestThrottle_AllowsLimitThenDeniesUntilWindowPasses(t *testing.T) {
	now := time.Now()
	th := NewThrottle(3, 15*time.Minute)
	th.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		assert.True(t, th.Allow("+628123456789"), "attempt %d is within the limit", i)
	}
	assert.False(t, th.Allow("+628123456789"), "the 4th attempt inside the window is over the limit")

	// Still inside the window: the denial holds.
	now = now.Add(14 * time.Minute)
	assert.False(t, th.Allow("+628123456789"))

	// The first three hits have aged out.
	now = now.Add(2 * time.Minute)
	assert.True(t, th.Allow("+628123456789"))
}

// Buckets are per key: one number under attack must not lock everyone
// else out.
func TestThrottle_KeysAreIndependent(t *testing.T) {
	now := time.Now()
	th := NewThrottle(2, time.Minute)
	th.now = func() time.Time { return now }

	assert.True(t, th.Allow("a"))
	assert.True(t, th.Allow("a"))
	assert.False(t, th.Allow("a"))
	assert.True(t, th.Allow("b"), "b has its own window")
}

// An empty key means "we do not know who this is" (no client IP, for
// instance). Counting it would merge every anonymous caller into one
// bucket and lock them all out together.
func TestThrottle_EmptyKeyIsNeverCounted(t *testing.T) {
	now := time.Now()
	th := NewThrottle(1, time.Minute)
	th.now = func() time.Time { return now }

	for range 10 {
		assert.True(t, th.Allow(""))
	}
	assert.Empty(t, th.hits, "an unknown caller must not occupy a bucket at all")
}
