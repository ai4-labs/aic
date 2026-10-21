package sandbox

import "context"

// linuxBackend will isolate processes with rootless user/mount/pid/net/ipc/
// uts namespaces, pivot_root into an assembled tmpfs root, seccomp-bpf,
// Landlock, and cgroups v2 limits. Implemented in milestone M3.
type linuxBackend struct{}

func newBackend() (Backend, error) { return linuxBackend{}, nil }

func (linuxBackend) Name() string { return "linux-ns" }

func (linuxBackend) Run(ctx context.Context, spec Spec) (int, error) {
	return -1, ErrNotImplemented
}
