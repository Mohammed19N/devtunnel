package store

import (
	"bytes"
	"context"
	"testing"
)

func TestSettingStable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	calls := 0
	gen := func() []byte { calls++; return []byte("secret") }
	a, err := s.Setting(ctx, "k", gen)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Setting(ctx, "k", gen)
	if err != nil || !bytes.Equal(a, b) || calls != 1 {
		t.Fatalf("a=%q b=%q calls=%d err=%v", a, b, calls, err)
	}
}

func TestAudit(t *testing.T) {
	s := openTest(t)
	if err := s.Audit(context.Background(), 0, "ban", "x"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d", n)
	}
}
