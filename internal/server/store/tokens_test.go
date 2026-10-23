package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
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

	rows, err := s.db.Query(`SELECT * FROM tokens`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		for _, v := range vals {
			var str string
			switch x := v.(type) {
			case string:
				str = x
			case []byte:
				str = string(x)
			}
			if strings.Contains(str, plain) {
				t.Fatal("plaintext token stored in DB")
			}
		}
	}
	rows.Close()

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
