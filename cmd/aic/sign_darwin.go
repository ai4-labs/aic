package main

import (
	"io"

	"github.com/ai4-labs/aic/internal/vm"
)

// ensureVMAccess signs a `go install`ed aic for Virtualization.framework
// before anything else happens (it may re-execute aic).
func ensureVMAccess(progress io.Writer) error { return vm.EnsureSigned(progress) }
