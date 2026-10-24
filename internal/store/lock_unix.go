//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
)

// withLock runs fn holding an exclusive lock on the store, so concurrent
// aic processes don't interleave writes to index.json.
func (s *Store) withLock(fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
