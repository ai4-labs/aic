// Package sandbox defines the interface between the portable runtime core
// and the platform-specific isolation backends.
//
// Backends live in this package behind build constraints:
// sandbox_linux.go (namespaces, cgroups v2, seccomp, Landlock) and
// sandbox_darwin.go (Seatbelt). Other platforms get an unsupported stub.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/rakyll/ai-container/internal/config"
)

// ErrNotImplemented is returned by backends that are not built yet.
var ErrNotImplemented = errors.New("sandbox: not implemented")

// ErrUnsupported is returned on platforms without a backend, or when a
// Spec asks for something the backend cannot enforce. Backends fail closed:
// they never run a process with weaker isolation than requested.
var ErrUnsupported = errors.New("sandbox: unsupported")

// Mount exposes a host directory inside the container.
type Mount struct {
	// Source is an absolute host path.
	Source string
	// Target is a path relative to the container root where Source
	// appears. Backends without mount namespaces (macOS) create a symlink
	// there unless the path already resolves (e.g. through a parent
	// mount). Empty means "grant access only".
	Target string
	// Writable grants write access; otherwise access is read-only.
	Writable bool
}

// Spec fully describes one container process to launch. Everything not
// granted by Root and Mounts is denied, including all network access.
type Spec struct {
	// Root is the absolute per-container host directory holding the
	// assembled view. The process can read everything under it but write
	// only where a writable Mount grants it.
	Root   string
	Mounts []Mount
	Cmd    []string
	// Env is the complete environment; the host environment is not
	// inherited.
	Env []string
	// WorkDir is relative to Root.
	WorkDir string
	// Config is the operator's config. Network egress and resource
	// limits are read from it; filesystem grants must already be
	// expressed as Mounts.
	Config config.Config

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (s *Spec) validate() error {
	if !filepath.IsAbs(s.Root) {
		return fmt.Errorf("sandbox: root %q must be absolute", s.Root)
	}
	if len(s.Cmd) == 0 || s.Cmd[0] == "" {
		return errors.New("sandbox: empty command")
	}
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("sandbox: mount source %q must be absolute", m.Source)
		}
		if m.Target != "" && (filepath.IsAbs(m.Target) || !filepath.IsLocal(m.Target)) {
			return fmt.Errorf("sandbox: mount target %q must be a local relative path", m.Target)
		}
	}
	if s.WorkDir != "" && !filepath.IsLocal(s.WorkDir) {
		return fmt.Errorf("sandbox: workdir %q must be a local relative path", s.WorkDir)
	}
	return nil
}

// Backend launches and supervises sandboxed processes.
type Backend interface {
	// Name returns a short identifier, e.g. "linux-ns" or "darwin-seatbelt".
	Name() string
	// Run starts spec.Cmd inside the sandbox, waits for it to exit, and
	// returns its exit code (128+N if killed by signal N). Cancelling ctx
	// kills the process.
	Run(ctx context.Context, spec Spec) (exitCode int, err error)
}

// New returns the backend for the host platform.
func New() (Backend, error) { return newBackend() }
