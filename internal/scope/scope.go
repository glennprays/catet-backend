// Package scope carries the authenticated user through the context.
//
// Every user-owned table has a user_id column and every repository
// method that touches one reads its tenant from here — no method takes
// a userID parameter, so there is exactly one mechanism and exactly one
// way to forget it. Forgetting fails closed: UserID returns ErrNoUser
// and the query is never built.
//
// The scope is set in three places: the HTTP middleware (auth.RequireAuth),
// the bot harness once it has resolved the sender to a user, and the
// operator CLI.
package scope

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNoUser means the context carries no user: a programming error at
// every call site, surfaced instead of quietly reading another tenant's
// rows.
var ErrNoUser = errors.New("no user in context")

// userIDKey is unexported so no other package can plant a user.
type userIDKey struct{}

// WithUserID returns a context scoped to one user.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey{}, id)
}

// UserID reads the scoped user. Repositories call it first and return
// the error unchanged; the HTTP layer maps it to 500, because a request
// that reached a repository without a scope is a bug in the wiring, not
// a client mistake.
func UserID(ctx context.Context) (uuid.UUID, error) {
	id, ok := ctx.Value(userIDKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, ErrNoUser
	}
	return id, nil
}

// MustUserID is the test-only shorthand; it panics when the context has
// no user.
func MustUserID(ctx context.Context) uuid.UUID {
	id, err := UserID(ctx)
	if err != nil {
		panic(err)
	}
	return id
}
