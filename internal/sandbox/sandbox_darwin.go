package sandbox

import "context"

// darwinBackend will run native Mach-O processes under a generated Seatbelt
// (SBPL) profile starting from (deny default), with a supervisor that tracks
// descendants via kqueue and enforces memory limits. Implemented in M2.
type darwinBackend struct{}

func newBackend() (Backend, error) { return darwinBackend{}, nil }

func (darwinBackend) Name() string { return "darwin-seatbelt" }

func (darwinBackend) Run(ctx context.Context, spec Spec) (int, error) {
	return -1, ErrNotImplemented
}
