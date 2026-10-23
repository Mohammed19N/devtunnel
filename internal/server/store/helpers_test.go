package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkUser(t *testing.T, s *Store, ghID int64, login string) User {
	t.Helper()
	u, _, err := s.UpsertGitHubUser(context.Background(), User{GitHubID: ghID, Login: login})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
