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
	ID      string // decimal user id, or "static" / "anonymous"
	Login   string
	Plan    Plan
	TokenID int64 // the database token used to sign in; 0 for static tokens
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

// Chain tries each Authenticator in order and returns the first success. It
// moves on only for ErrInvalidToken; any other error is returned at once.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, token string) (Account, error) {
	for _, a := range c {
		acc, err := a.Authenticate(ctx, token)
		if !errors.Is(err, ErrInvalidToken) {
			return acc, err
		}
	}
	return Account{}, ErrInvalidToken
}
