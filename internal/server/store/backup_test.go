package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mkUser(t, s, 1, "alice")
	mkUser(t, s, 2, "bob")
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	b, err := OpenExisting(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, l := range []string{"alice", "bob"} {
		if _, err := b.UserByLogin(ctx, l); err != nil {
			t.Errorf("backup lacks %s: %v", l, err)
		}
	}
}

func TestBackupRefusesExisting(t *testing.T) {
	s := openTest(t)
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(context.Background(), dest); err == nil {
		t.Fatal("Backup over an existing file succeeded")
	}
	if got, _ := os.ReadFile(dest); string(got) != "precious" {
		t.Fatalf("existing file changed: %q", got)
	}
}

func TestVerifiedDomainOwnerAndWaitlistCount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "alice")
	if _, err := s.AddDomain(ctx, u.ID, "app.example.com", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifiedDomainOwner(ctx, "app.example.com"); err != ErrNotFound {
		t.Fatalf("unverified owner err = %v, want ErrNotFound", err)
	}
	if err := s.VerifyDomain(ctx, u.ID, "app.example.com"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.VerifiedDomainOwner(ctx, "APP.example.com."); err != nil || id != u.ID {
		t.Fatalf("owner = %d, %v", id, err)
	}
	if err := s.SetWaitlisted(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountWaitlisted(ctx); err != nil || n != 1 {
		t.Fatalf("waitlisted = %d, %v", n, err)
	}
}
