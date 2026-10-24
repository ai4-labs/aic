package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIUsage(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	for _, tc := range []struct {
		args []string
		code int
		want string // substring of stdout+stderr
	}{
		{[]string{"--help"}, exitOK, "build"},
		{[]string{"build", "--help"}, exitOK, "--build-arg"},
		{[]string{"version"}, exitOK, "image store:"},
		{[]string{"--version"}, exitOK, "aic version"},
		{[]string{"bogus"}, exitUsage, "unknown command"},
		{[]string{"build", "--nope"}, exitUsage, "unknown flag"},
		{[]string{"build", "a", "b"}, exitUsage, "accepts at most 1 arg"},
		{[]string{"build", "/does/not/exist"}, exitUsage, "not a directory"},
		{[]string{"pull"}, exitUsage, "accepts 1 arg"},
		{[]string{"push", "hello"}, exitUsage, "has no registry"},
		{[]string{"push", "registry.example.com/x:1"}, exitError, "image not known"},
		{[]string{"build", "--platform", "nonsense!", "."}, exitUsage, "platform"},
	} {
		code, stdout, stderr := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(stdout+stderr, tc.want) {
			t.Errorf("aic %q = %d\nstdout: %s\nstderr: %s\nwant code %d containing %q",
				tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
}

func TestCLIBuild(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nARG V\nLABEL v=$V\nCOPY f /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Flags after the positional argument, repeated -t.
	code, stdout, stderr := runCLI(t, "build", dir, "-t", "a", "--tag=b:2", "--build-arg", "V=1", "-q")
	if code != exitOK || !strings.HasPrefix(stdout, "sha256:") {
		t.Fatalf("build = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	digest := strings.TrimSpace(stdout)
	for _, ref := range []string{"a", "localhost/b:2", digest[:19]} {
		if code, _, stderr := runCLI(t, "push", ref, "127.0.0.1:1/x:1", "-q"); code != exitError || strings.Contains(stderr, "image not known") {
			t.Errorf("push %s: code %d, stderr %s (want a network error, not a lookup failure)", ref, code, stderr)
		}
	}
}
