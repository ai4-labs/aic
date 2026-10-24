//go:build darwin

package vm

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Serve boots a VM and serves builds on its socket until ctx is done or
// `aic vm stop` is run. This is the body of the `aic vm start` daemon.
func Serve(ctx context.Context, o Options) error {
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return err
	}
	if Running(o.Dir) {
		return fmt.Errorf("the VM is already running")
	}
	m, err := Start(ctx, o)
	if err != nil {
		return err
	}
	defer m.Close()
	ln, err := listen(o.Dir)
	if err != nil {
		return err
	}
	defer os.Remove(SocketPath(o.Dir))
	serveUntil(ctx, ln, m, Status{
		PID:       os.Getpid(),
		CPUs:      m.CPUs(),
		MemoryMiB: m.MemoryMiB(),
		Kernel:    kernelVersion,
		Started:   time.Now(),
	})
	return nil
}
