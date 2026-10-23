package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMigratesOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.db")
	for i := 0; i < 2; i++ {
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("open %d: schema_migrations rows = %d, want 1", i, n)
		}
		s.Close()
	}
}

func TestOpenWAL(t *testing.T) {
	s := openTest(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}
}

func TestOpenPathWithSpecialChars(t *testing.T) {
	p := filepath.Join(t.TempDir(), "we?ird#na%me.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("db not at requested path: %v", err)
	}
}
