package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionExpiry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")

	id, sess, err := s.CreateSession(ctx, u.ID, time.Hour)
	if err != nil || len(id) != 43 || sess.CSRF == "" {
		t.Fatalf("create: %q %+v %v", id, sess, err)
	}
	got, err := s.SessionByID(ctx, id)
	if err != nil || got.UserID != u.ID || got.CSRF != sess.CSRF {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT id_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == id {
		t.Fatal("session id stored in plaintext")
	}
	if err := s.DeleteSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}

	id2, _, err := s.CreateSession(ctx, u.ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByID(ctx, id2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
}
