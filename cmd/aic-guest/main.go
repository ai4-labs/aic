//go:build linux

// Command aic-guest is the init process of aic's Linux build VM on macOS.
// It is embedded into the macOS aic binary; see the Makefile.
package main

import (
	"fmt"
	"os"

	"github.com/ai4-labs/aic/internal/container"
	"github.com/ai4-labs/aic/internal/guest"
)

func main() {
	container.Init() // container children re-execute this binary
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "aic-guest runs as init inside aic's build VM")
		os.Exit(2)
	}
	guest.Main()
}
