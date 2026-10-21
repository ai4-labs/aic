package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != exitUsage {
		t.Fatalf("no args: code = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"help"}, &out, &errb); code != exitOK {
		t.Fatalf("help: code = %d", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != exitUsage {
		t.Fatalf("bogus: code = %d", code)
	}
}

func TestNotImplemented(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"inspect"}, &out, &errb); code != exitNotImplemented {
		t.Fatalf("code = %d, want %d", code, exitNotImplemented)
	}
	if !strings.Contains(errb.String(), "M1") {
		t.Fatalf("stderr = %q, want milestone", errb.String())
	}
}

func initProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	var out, errb bytes.Buffer
	if code := run([]string{"init", dir}, &out, &errb); code != exitOK {
		t.Fatalf("init: code = %d, stderr = %s", code, errb.String())
	}
	return dir
}

func TestBuildAutoLocks(t *testing.T) {
	dir := initProject(t)
	lockPath := filepath.Join(dir, "aic.lock")

	// First build: no lock yet, so it is created.
	var out, errb bytes.Buffer
	if code := run([]string{"build", dir}, &out, &errb); code != exitNotImplemented {
		t.Fatalf("build #1: code = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "updated aic.lock (aic.lock missing)") {
		t.Fatalf("build #1: stderr = %q", errb.String())
	}
	first, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second build: lock is fresh, nothing is rewritten.
	errb.Reset()
	run([]string{"build", dir}, &out, &errb)
	if strings.Contains(errb.String(), "updated aic.lock") {
		t.Fatalf("build #2 relocked a fresh lock: %q", errb.String())
	}

	// Edit aic.yaml: --locked refuses, plain build relocks.
	f, err := os.OpenFile(filepath.Join(dir, "aic.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("# edited\n")
	f.Close()

	errb.Reset()
	if code := run([]string{"build", "--locked", dir}, &out, &errb); code != exitError {
		t.Fatalf("build --locked on stale lock: code = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "aic.yaml changed") || !strings.Contains(errb.String(), "without --locked") {
		t.Fatalf("build --locked: stderr = %q", errb.String())
	}
	if b, _ := os.ReadFile(lockPath); !bytes.Equal(b, first) {
		t.Fatal("build --locked modified aic.lock")
	}

	errb.Reset()
	run([]string{"build", dir}, &out, &errb)
	if !strings.Contains(errb.String(), "updated aic.lock (aic.yaml changed)") {
		t.Fatalf("build #3: stderr = %q", errb.String())
	}

	// Deleting aic.lock is how you force a full re-resolve.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	run([]string{"build", dir}, &out, &errb)
	if !strings.Contains(errb.String(), "updated aic.lock (aic.lock missing)") {
		t.Fatalf("build after deleting lock: stderr = %q", errb.String())
	}
}

func TestBuildNoProject(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"build", t.TempDir()}, &out, &errb); code != exitError {
		t.Fatalf("code = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "no aic.yaml") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if code := run([]string{"build", "a", "b"}, &out, &errb); code != exitUsage {
		t.Fatalf("extra arg: code = %d, want %d", code, exitUsage)
	}
}

func TestRunUsageAndSetupErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"run", "--"}, &out, &errb); code != exitUsage {
		t.Errorf("no command after --: code = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"run", "a", "b"}, &out, &errb); code != exitUsage {
		t.Errorf("two dirs: code = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"run", t.TempDir()}, &out, &errb); code != exitRunFailed {
		t.Errorf("no aic.yaml: code = %d, want %d", code, exitRunFailed)
	}
	dir := initProject(t)
	bad := filepath.Join(t.TempDir(), "config.json")
	for _, content := range []string{`{"gpu": true}`, `not json`, `{"filesystem":{"writable":["/etc"]}}`} {
		if err := os.WriteFile(bad, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		errb.Reset()
		if code := run([]string{"run", "--config", bad, dir}, &out, &errb); code != exitRunFailed {
			t.Errorf("config %s: code = %d, want %d (stderr %q)", content, code, exitRunFailed, errb.String())
		}
	}
}

func TestRunExitCodeAndOverride(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("aic run needs the macOS sandbox for now")
	}
	dir := initProject(t)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "start"), []byte("#!/bin/sh\necho from-entrypoint\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"run", dir}, &out, &errb); code != 4 || !strings.Contains(out.String(), "from-entrypoint") {
		t.Fatalf("entrypoint: code = %d, stdout = %q, stderr = %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"run", dir, "--", "/bin/echo", "overridden"}, &out, &errb); code != 0 || out.String() != "overridden\n" {
		t.Fatalf("override: code = %d, stdout = %q", code, out.String())
	}
}

func TestLockCommandRemoved(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"lock"}, &out, &errb); code != exitUsage {
		t.Fatalf("code = %d, want %d (unknown command)", code, exitUsage)
	}
}

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "platform:") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Project")
	var out, errb bytes.Buffer
	if code := run([]string{"init", dir}, &out, &errb); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "aic.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `name: "my-project"`) {
		t.Fatalf("aic.yaml missing sanitized name:\n%s", b)
	}
	if regexp.MustCompile(`(?m)^config:`).Match(b) {
		t.Fatalf("aic.yaml must not declare a config:\n%s", b)
	}
	if strings.Contains(string(b), "\t") {
		t.Fatalf("aic.yaml must not contain tabs (invalid YAML indentation):\n%s", b)
	}
	// Second init must not overwrite.
	errb.Reset()
	if code := run([]string{"init", dir}, &out, &errb); code != exitError {
		t.Fatalf("second init code = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestInitInvalidName(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"init", "-name", "Bad Name!", t.TempDir()}, &out, &errb)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"My Project": "my-project",
		"--x":        "x",
		"!!!":        "app",
		"ok.name_1":  "ok.name_1",
	}
	for in, want := range tests {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
