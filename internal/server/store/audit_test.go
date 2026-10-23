package store

import (
	"context"
	"testing"
)

func TestAudit(t *testing.T) {
	s := openTest(t)
	if err := s.Audit(context.Background(), 0, "ban", "x"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d %v", n, err)
	}
}
