package executor

import "io"

// Options configures the native executor.
type Options struct {
	// Dir holds VM files (kernel, initramfs, console log) on macOS.
	Dir string
	// CPUs and MemoryMiB size the VM on macOS. Zero picks a default.
	CPUs      uint
	MemoryMiB uint64
	// Progress receives one-time setup messages.
	Progress io.Writer
}
