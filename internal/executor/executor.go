// Package executor runs RUN instructions.
//
// An Executor receives the image built so far, runs a command in a
// container created from it, and returns the container's root filesystem
// as a tar stream. The builder diffs that filesystem against the image to
// produce the new layer, so executors never need to know about layers.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Spec describes one RUN.
type Spec struct {
	Image   v1.Image
	Args    []string // full argv, already wrapped in the shell for shell form
	Env     []string // KEY=VALUE
	WorkDir string
	User    string
	// Network is "default", "none" or "host".
	Network        string
	Stdout, Stderr io.Writer
}

// Executor runs commands in containers.
type Executor interface {
	// Run executes spec and writes the container's root filesystem, as
	// an uncompressed tar stream, to rootfs.
	Run(ctx context.Context, spec Spec, rootfs io.Writer) error
	// Name identifies the executor in messages.
	Name() string
}

// ExitError reports that the command ran and exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ErrUnavailable is returned by New when no executor can run on this host.
var ErrUnavailable = errors.New("no container runtime available to execute RUN")
