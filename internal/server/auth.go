package server

import (
	"context"
	"crypto/subtle"
	"errors"
)

// ErrInvalidToken means the token is unknown or revoked. Any other error from
// an Authenticator is treated as transient.
var ErrInvalidToken = errors.New("invalid token")

// Account is an authenticated tunnel owner.
type Account struct {
	ID    string // decimal user id, or "static" / "anonymous"
	Login string
	Plan  Plan
}

// Authenticator validates a client token and returns the owning account.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Account, error)
}

// StaticTokens is an Authenticator over a fixed token list.
// An empty list allows every token (development only).
type StaticTokens []string

// Authenticate implements Authenticator.
func (s StaticTokens) Authenticate(_ context.Context, token string) (Account, error) {
	if len(s) == 0 {
		return Account{ID: "anonymous", Plan: PlanUnlimited}, nil
	}
	ok := false
	for _, t := range s {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	if !ok {
		return Account{}, ErrInvalidToken
	}
	return Account{ID: "static", Plan: PlanUnlimited}, nil
}
