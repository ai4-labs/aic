//go:build darwin

package vm

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
)

// guestFS holds aic-guest binaries built by `make` (see the Makefile).
//
//go:embed guest
var guestFS embed.FS

const modulePath = "github.com/ai4-labs/aic"

// guestBinary returns the Linux guest that runs in the VM: the embedded
// one if aic was built with `make` (as releases are), else one built
// once from aic's own module source with the Go toolchain, which is how
// `go install` users get it.
func guestBinary(ctx context.Context, dir string, progress io.Writer) ([]byte, error) {
	if b, err := guestFS.ReadFile("guest/aic-guest-linux-" + runtime.GOARCH); err == nil && len(b) > 0 {
		return b, nil
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path != modulePath || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return nil, errors.New("this aic binary was built without the build VM's guest; build it with `make`, or install it with `go install " + modulePath + "/cmd/aic@latest`")
	}
	version := info.Main.Version
	cached := filepath.Join(dir, "guest", "aic-guest-"+strings.ReplaceAll(version, "/", "_")+"-linux-"+runtime.GOARCH)
	if b, err := os.ReadFile(cached); err == nil && len(b) > 0 {
		return b, nil
	}
	fmt.Fprintf(progress, "Building the VM guest for %s (once)...\n", version)
	b, err := buildGuest(ctx, version)
	if err != nil {
		return nil, fmt.Errorf("building the VM guest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		return nil, err
	}
	tmp := cached + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp, cached)
}

// buildGuest cross-compiles cmd/aic-guest from the module source of the
// given version, which `go install` left in the module cache.
func buildGuest(ctx context.Context, version string) ([]byte, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, serr := os.Stat(goBin); serr != nil {
			return nil, errors.New("the Go toolchain (go) is not in PATH")
		}
	}
	tmp, err := os.MkdirTemp("", "aic-guest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")

	// Locate the source (downloading it if the cache was cleaned).
	dl := exec.CommandContext(ctx, goBin, "mod", "download", "-json", modulePath+"@"+version)
	dl.Dir, dl.Env = tmp, env
	var stderr bytes.Buffer
	dl.Stderr = &stderr
	out, err := dl.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod download: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var mod struct{ Dir string }
	if err := json.Unmarshal(out, &mod); err != nil || mod.Dir == "" {
		return nil, fmt.Errorf("go mod download: no source directory for %s@%s", modulePath, version)
	}

	bin := filepath.Join(tmp, "aic-guest")
	build := exec.CommandContext(ctx, goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "./cmd/aic-guest")
	build.Dir = mod.Dir
	build.Env = append(env, "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	stderr.Reset()
	build.Stderr = &stderr
	if err := build.Run(); err != nil {
		return nil, fmt.Errorf("go build: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(bin)
}
