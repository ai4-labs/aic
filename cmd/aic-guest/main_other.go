//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "aic-guest only runs on Linux, inside aic's build VM")
	os.Exit(2)
}
