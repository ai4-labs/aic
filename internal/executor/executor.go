// Package executor runs RUN instructions.
//
// An Executor receives the image built so far, runs a command in a
// container created from it, and returns the container's root filesystem
// as a tar stream. The builder diffs that filesystem against the image to
// produce the new layer, so executors never need to know about layers.
//
// The native executor needs no container runtime: on Linux it uses
// rootless namespaces (package container); on macOS it runs the same code
// inside a lightweight Linux VM (package vm).
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/rakyll/ai-container/internal/proto"
)

// Spec describes one RUN.
type Spec struct {
	Image   v1.Image
	Args    []string // full argv, already wrapped in the shell for shell form
	Env     []string // KEY=VALUE
	WorkDir string
	User    string
	// Network is "none" (default; isolated, loopback only) or "host".
	Network        string
	Stdout, Stderr io.Writer
}

// Executor runs commands in containers.
type Executor interface {
	// Run executes spec and writes the container's root filesystem, as
	// an uncompressed tar stream, to rootfs.
	Run(ctx context.Context, spec Spec, rootfs io.Writer) error
	// Close releases resources such as a VM.
	Close() error
}

// ExitError reports that the command ran and exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ErrUnavailable is returned when RUN cannot be executed on this host.
var ErrUnavailable = errors.New("RUN is not supported on this platform")

// runFunc is the shape of container.Run and vm.Machine.Run.
type runFunc func(ctx context.Context, spec proto.Spec, image io.Reader, stdout, stderr, export io.Writer) (proto.Result, error)

// run adapts a runFunc to Executor.Run.
func run(ctx context.Context, fn runFunc, spec Spec, rootfs io.Writer) error {
	network := spec.Network
	if network != "host" {
		network = "none"
	}
	ps := proto.Spec{
		Args:    spec.Args,
		Env:     spec.Env,
		WorkDir: spec.WorkDir,
		User:    spec.User,
		Network: network,
	}
	stdout, stderr := spec.Stdout, spec.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// mutate.Extract flattens the layers and applies whiteouts.
	image := mutate.Extract(spec.Image)
	defer image.Close()
	res, err := fn(ctx, ps, image, stdout, stderr, rootfs)
	if err != nil {
		return err
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	if res.ExitCode != 0 {
		return &ExitError{Code: res.ExitCode}
	}
	return nil
}
