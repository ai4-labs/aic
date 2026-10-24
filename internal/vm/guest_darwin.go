//go:build darwin

package vm

import (
	"embed"
	"fmt"
	"runtime"
)

// guestFS holds aic-guest binaries built by `make` (see the Makefile).
//
//go:embed guest
var guestFS embed.FS

func guestBinary() ([]byte, error) {
	b, err := guestFS.ReadFile("guest/aic-guest-linux-" + runtime.GOARCH)
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("this aic binary was built without the build VM's guest; build it with `make`")
	}
	return b, nil
}
