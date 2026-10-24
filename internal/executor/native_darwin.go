//go:build darwin

package executor

import (
	"context"
	"io"
	"sync"

	"github.com/rakyll/ai-container/internal/vm"
)

// Native runs RUN in aic's Linux VM, booted on first use.
type Native struct {
	opts Options
	once sync.Once
	m    *vm.Machine
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
	})
	if n.err != nil {
		return n.err
	}
	return run(ctx, n.m.Run, spec, rootfs)
}

func (n *Native) Close() error {
	if n.m != nil {
		return n.m.Close()
	}
	return nil
}
