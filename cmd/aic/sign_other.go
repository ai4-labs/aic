//go:build !darwin

package main

import "io"

func ensureVMAccess(io.Writer) error { return nil }
