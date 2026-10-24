//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/rakyll/ai-container/internal/proto"
)

// watchResize reports the size of the terminal fd whenever it changes.
func watchResize(fd int) (<-chan proto.Size, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	sizes := make(chan proto.Size, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				w, h, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				select {
				case <-sizes: // keep only the latest
				default:
				}
				sizes <- proto.Size{Rows: uint16(h), Cols: uint16(w)}
			}
		}
	}()
	return sizes, func() {
		signal.Stop(sig)
		close(done)
	}
}
