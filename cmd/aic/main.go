// Command aic builds container images from Dockerfiles and moves them
// to and from registries.
package main

import (
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
