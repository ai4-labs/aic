//go:build darwin

package executor

import (
	"context"
	"io"
	"sync"

	"github.com/ai4-labs/aic/internal/vm"
)

// Native runs RUN in a Linux VM booted for this build or run, and stopped
// when it is closed.
type Native struct {
	opts Options
	once sync.Once
	run  runFunc
	m    *vm.Machine // a VM owned by this build
	err  error
}

// NewNative returns the native executor.
func NewNative(o Options) (Executor, error) { return &Native{opts: o}, nil }

func (n *Native) Run(ctx context.Context, spec Spec, rootfs io.Writer) error {
	n.once.Do(func() {
		progress := n.opts.Progress
		if progress == nil {
			progress = io.Discard
		}
		n.m, n.err = vm.Start(ctx, vm.Options{Dir: n.opts.Dir, Progress: progress, CPUs: n.opts.CPUs, MemoryMiB: n.opts.MemoryMiB})
		if n.err == nil {
			n.run = n.m.Run
		}
	})
	if n.err != nil {
		return n.err
	}
	return run(ctx, n.run, spec, rootfs)
}

func (n *Native) Close() error {
	if n.m != nil {
		return n.m.Close()
	}
	return nil
}
