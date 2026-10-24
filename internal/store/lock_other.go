//go:build !unix

package store

// withLock is a no-op where flock is unavailable. aic only supports
// macOS and Linux; this keeps the package building elsewhere.
func (s *Store) withLock(fn func() error) error { return fn() }
