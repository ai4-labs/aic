// Command aic builds container images from Dockerfiles and moves them
// to and from registries.
package main

import (
	"os"

	"github.com/ai4-labs/aic/internal/container"
)

func main() {
	// RUN containers re-execute this binary on Linux.
	container.Init()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
