//go:build !linux && !darwin

package executor

// NewNative reports that RUN is unsupported here.
func NewNative(Options) (Executor, error) { return nil, ErrUnavailable }
