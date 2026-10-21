//go:build !linux && !darwin

package sandbox

func newBackend() (Backend, error) { return nil, ErrUnsupported }
