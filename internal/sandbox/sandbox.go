// Package sandbox defines the interface between the portable runtime core
// and the platform-specific isolation backends.
//
// Backends live in this package behind build constraints:
// sandbox_linux.go (namespaces, cgroups v2, seccomp, Landlock) and
// sandbox_darwin.go (Seatbelt + supervisor). Other platforms get an
// unsupported stub.
package sandbox

import (
	"context"
	"errors"
	"io"

	"github.com/rakyll/ai-container/internal/config"
)

// ErrNotImplemented is returned by backends that are not built yet.
var ErrNotImplemented = errors.New("sandbox: not implemented")

// ErrUnsupported is returned on platforms without a backend.
var ErrUnsupported = errors.New("sandbox: unsupported platform")

// Mount exposes a host directory at a path inside the container.
type Mount struct {
	// Source is an absolute host path (typically a materialized component
	// in the local store).
	Source string
	// Target is a path relative to the container root.
	Target string
	// Writable mounts are only used for scratch/workspace dirs; component
	// mounts are always read-only.
	Writable bool
}

// Spec fully describes one container process to launch.
type Spec struct {
	// ID uniquely identifies the container instance.
	ID string
	// Root is the per-container host directory holding the assembled view
	// and scratch space.
	Root   string
	Mounts []Mount
	Cmd    []string
	Env    []string
	// WorkDir is relative to the container root.
	WorkDir string
	Config  config.Config

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Backend launches and supervises sandboxed processes.
type Backend interface {
	// Name returns a short identifier, e.g. "linux-ns" or "darwin-seatbelt".
	Name() string
	// Run starts spec.Cmd inside the sandbox, waits for it and all its
	// descendants to exit, and returns the process exit code. Cancelling
	// ctx kills the whole process tree.
	Run(ctx context.Context, spec Spec) (exitCode int, err error)
}

// New returns the backend for the host platform.
func New() (Backend, error) { return newBackend() }
