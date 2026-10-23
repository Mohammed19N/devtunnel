package server

import (
	"context"
	"errors"
	"testing"
)

type fakeAuth struct {
	acc   Account
	err   error
	calls int
}

func (f *fakeAuth) Authenticate(context.Context, string) (Account, error) {
	f.calls++
	return f.acc, f.err
}

func TestChain(t *testing.T) {
	boom := errors.New("db down")
	ok := Account{ID: "b"}
	for _, c := range []struct {
		name   string
		a, b   *fakeAuth
		want   error
		wantID string
		bCalls int
	}{
		{"second succeeds", &fakeAuth{err: ErrInvalidToken}, &fakeAuth{acc: ok}, nil, "b", 1},
		{"first error stops", &fakeAuth{err: boom}, &fakeAuth{acc: ok}, boom, "", 0},
		{"all invalid", &fakeAuth{err: ErrInvalidToken}, &fakeAuth{err: ErrInvalidToken}, ErrInvalidToken, "", 1},
	} {
		acc, err := Chain{c.a, c.b}.Authenticate(context.Background(), "t")
		if !errors.Is(err, c.want) || acc.ID != c.wantID || c.b.calls != c.bCalls {
			t.Errorf("%s: acc=%q err=%v bCalls=%d", c.name, acc.ID, err, c.b.calls)
		}
	}
	if _, err := (Chain{}).Authenticate(context.Background(), "t"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("empty chain err = %v", err)
	}
}
