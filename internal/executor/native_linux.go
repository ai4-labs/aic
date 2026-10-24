//go:build linux

package executor

import (
	"context"
	"io"

	"github.com/ai4-labs/aic/internal/container"
)

// Native runs RUN in rootless Linux namespaces on this host.
type Native struct{}

// NewNative returns the native executor.
func NewNative(Options) (Executor, error) { return Native{}, nil }

func (Native) Run(ctx context.Context, spec Spec, rootfs io.Writer) error {
	return run(ctx, container.Run, spec, rootfs)
}

func (Native) Close() error { return nil }
