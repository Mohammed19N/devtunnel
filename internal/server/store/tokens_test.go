package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
)

func TestTokenLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	other := mkUser(t, s, 2, "b")

	plain, tok, err := s.CreateToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^pw_[A-Za-z0-9_-]{43}$`).MatchString(plain) {
		t.Fatalf("plain = %q", plain)
	}
	if tok.Prefix != plain[:10] || tok.UserID != u.ID {
		t.Fatalf("tok = %+v", tok)
	}
	got, err := s.TokenByPlain(ctx, plain)
	if err != nil || got.ID != tok.ID || got.UserID != u.ID {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	list, _ := s.ListTokens(ctx, u.ID)
	if len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("list = %+v", list)
	}

	assertNoPlaintext(t, s, plain)

	if err := s.RevokeToken(ctx, other.ID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke other: %v", err)
	}
	if err := s.RevokeToken(ctx, u.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenByPlain(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revoke: %v", err)
	}
	if _, err := s.TokenByPlain(ctx, "pw_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestAdminTokens(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_, tok, err := s.CreateToken(ctx, 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTokens(ctx, 0)
	if err != nil || len(list) != 1 || list[0].ID != tok.ID || list[0].UserID != 0 {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := s.RevokeToken(ctx, 0, tok.ID); err != nil {
		t.Fatal(err)
	}
}

// assertNoPlaintext scans every raw column of every tokens row, as bytes.
func assertNoPlaintext(t *testing.T, s *Store, plain string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM tokens`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	hash := hashOf(plain)
	var seen, hashSeen bool
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		seen = true
		for _, v := range raw {
			if bytes.Contains(v, []byte(plain)) {
				t.Fatal("plaintext token stored in DB")
			}
			if bytes.Equal(v, hash) {
				hashSeen = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen || !hashSeen {
		t.Fatalf("scan not meaningful: rows=%v hash=%v", seen, hashSeen)
	}
}
