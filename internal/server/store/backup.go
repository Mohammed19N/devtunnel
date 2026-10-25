package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Backup writes a consistent copy of the database to dest with VACUUM INTO.
// It fails if dest already exists, so a backup never overwrites another.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}
