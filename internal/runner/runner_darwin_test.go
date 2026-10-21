//go:build darwin

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rakyll/ai-container/internal/config"
)

const testYAML = `schemaVersion: 1
name: "t"
version: "1"
targets: [darwin-arm64, darwin-amd64]
include: ["src/**"]
entrypoint:
  cmd: [./src/start]
  env:
    GREETING: "hello"
`

// newProject writes a project whose entrypoint is the given shell script.
func newProject(t *testing.T, yaml, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aic.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "start"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not included: must not appear in the container.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, opts Options) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout, opts.Stderr = &out, &errb
	code, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (stderr: %s)", err, errb.String())
	}
	return code, out.String(), errb.String()
}

func TestRunEntrypointAndEnvironment(t *testing.T) {
	t.Setenv("AIC_TEST_HOST_SECRET", "leaked")
	dir := newProject(t, testYAML, `
echo "greeting=$GREETING"
echo "secret=$AIC_TEST_HOST_SECRET"
echo "path=$PATH"
case "$HOME" in "$AIC_ROOT"/*) echo home-inside ;; *) echo "home-outside=$HOME" ;; esac
ls "$AIC_ROOT/app"
exit 7
`)
	code, out, _ := run(t, Options{ProjectDir: dir})
	if code != 7 {
		t.Fatalf("exit code = %d, want 7; output:\n%s", code, out)
	}
	for _, want := range []string{"greeting=hello", "secret=\n", "path=" + systemPath, "home-inside", "src"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notes.txt") {
		t.Errorf("file outside include was staged:\n%s", out)
	}
}

func TestRunDeniesHostFiles(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	dir := newProject(t, testYAML, `
cat "$1" 2>/dev/null && echo READ-SECRET || echo secret-denied
ls "$2" >/dev/null 2>&1 && echo READ-HOME || echo home-denied
echo x > "$AIC_ROOT/app/new" 2>/dev/null && echo WROTE-APP || echo app-readonly
echo x > "$AIC_SCRATCH/new" && echo scratch-ok
echo x > /tmp/aic-runner-test-probe 2>/dev/null && echo WROTE-TMP || echo tmp-denied
`)
	_, out, _ := run(t, Options{ProjectDir: dir, Cmd: []string{"./src/start", secret, home}})
	for _, want := range []string{"secret-denied", "home-denied", "app-readonly", "scratch-ok", "tmp-denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunWorkspaceGrants(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "input.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := newProject(t, testYAML, `
cat "$AIC_WORKSPACE/input.txt" && echo
echo x > "$AIC_WORKSPACE/top.txt" 2>/dev/null && echo WROTE-TOP || echo top-readonly
echo result > "$AIC_WORKSPACE/out/result.txt" 2>/dev/null && echo wrote-out || echo out-denied
`)
	// Deny-all: workspace is read-only.
	_, out, _ := run(t, Options{ProjectDir: dir, Workspace: ws})
	if !strings.Contains(out, "data") || !strings.Contains(out, "top-readonly") || !strings.Contains(out, "out-denied") {
		t.Fatalf("deny-all workspace:\n%s", out)
	}
	// Granting $WORKSPACE/out allows writes there and nowhere else.
	cfg := config.Config{Filesystem: config.Filesystem{Writable: []string{"$WORKSPACE/out"}}}
	_, out, _ = run(t, Options{ProjectDir: dir, Workspace: ws, Config: cfg})
	if !strings.Contains(out, "top-readonly") || !strings.Contains(out, "wrote-out") {
		t.Fatalf("granted workspace:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "out", "result.txt")); err != nil || string(b) != "result\n" {
		t.Fatalf("result not written to host: %q, %v", b, err)
	}
}

func TestRunRejectsSymlinkEscapeInWritable(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	dir := newProject(t, testYAML, "echo should-not-run\n")
	cfg := config.Config{Filesystem: config.Filesystem{Writable: []string{"$WORKSPACE/out"}}}
	_, err := Run(context.Background(), Options{ProjectDir: dir, Workspace: ws, Config: cfg})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want symlink escape rejected", err)
	}
	// Also through a symlinked parent of a not-yet-existing directory.
	cfg.Filesystem.Writable = []string{"$WORKSPACE/out/new/deeper"}
	if _, err := Run(context.Background(), Options{ProjectDir: dir, Workspace: ws, Config: cfg}); err == nil {
		t.Fatal("symlinked parent escape was not rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory was created outside the workspace")
	}
}

func TestRunFailsClosed(t *testing.T) {
	dir := newProject(t, testYAML, "echo should-not-run\n")
	for name, cfg := range map[string]config.Config{
		"egress":    {Network: config.Network{Egress: []string{"example.com:443"}}},
		"maxMemory": {MaxMemory: 1 << 30},
		"maxProcs":  {MaxProcs: 10},
	} {
		if _, err := Run(context.Background(), Options{ProjectDir: dir, Config: cfg}); err == nil {
			t.Errorf("%s: Run succeeded, want unsupported error", name)
		}
	}
	tc := newProject(t, strings.Replace(testYAML, "include:", "toolchains:\n  python: {}\ninclude:", 1), "")
	if _, err := Run(context.Background(), Options{ProjectDir: tc}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("toolchains: err = %v, want ErrUnsupported", err)
	}
	ws := filepath.Join(t.TempDir(), "missing")
	if _, err := Run(context.Background(), Options{ProjectDir: dir, Workspace: ws}); err == nil {
		t.Error("missing workspace: Run succeeded")
	}
	grant := config.Config{Filesystem: config.Filesystem{Writable: []string{"$WORKSPACE/out"}}}
	if _, err := Run(context.Background(), Options{ProjectDir: dir, Config: grant}); err == nil {
		t.Error("$WORKSPACE grant without --workspace: Run succeeded")
	}
}

func TestRunRejectsSymlinkInInclude(t *testing.T) {
	dir := newProject(t, testYAML, "echo hi\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "src", "passwd")); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{ProjectDir: dir})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want symlink rejected", err)
	}
}
