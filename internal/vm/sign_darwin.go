//go:build darwin

package vm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const entitlement = "com.apple.security.virtualization"

const entitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>` + entitlement + `</key>
	<true/>
</dict>
</plist>
`

// EnsureSigned makes sure the running aic may use Virtualization.framework.
// Binaries from `make` and releases are signed with the entitlement;
// one from `go install` is not, so aic signs itself (ad hoc, like `make`
// does) and re-executes. It returns only if the binary is signed.
func EnsureSigned(progress io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	var out bytes.Buffer
	check := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", exe)
	check.Stdout, check.Stderr = &out, &out
	if check.Run() == nil && bytes.Contains(out.Bytes(), []byte(entitlement)) {
		return nil
	}
	if os.Getenv("AIC_SELF_SIGNED") != "" {
		return fmt.Errorf("%s is still missing the %s entitlement after signing", exe, entitlement)
	}

	fmt.Fprintf(progress, "Signing %s with the %s entitlement (once)...\n", exe, entitlement)
	f, err := os.CreateTemp("", "aic-entitlements-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(entitlements); err != nil {
		f.Close()
		return err
	}
	f.Close()
	sign := exec.Command("codesign", "--force", "--sign", "-", "--entitlements", f.Name(), exe)
	if b, err := sign.CombinedOutput(); err != nil {
		return fmt.Errorf("signing %s for Virtualization.framework: %v: %s\nsign it yourself: codesign --force --sign - --entitlements aic.entitlements %s", exe, err, bytes.TrimSpace(b), exe)
	}
	// The running image keeps its old signature; start over with the
	// signed one.
	return syscall.Exec(exe, os.Args, append(os.Environ(), "AIC_SELF_SIGNED=1"))
}
