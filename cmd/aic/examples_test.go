//go:build darwin

package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examplesDir is the repository's examples folder, relative to this package.
const examplesDir = "../../examples"

// copyDir copies a workspace folder into a temp dir so running examples
// never writes into the repository.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func runExample(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"run"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestExampleHello(t *testing.T) {
	code, out, errOut := runExample(t, filepath.Join(examplesDir, "hello"))
	if code != 0 || !strings.Contains(out, "Hello from aic!") || !strings.Contains(out, "PATH:        /usr/bin:/bin") {
		t.Fatalf("code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

func TestExampleCSVReport(t *testing.T) {
	dir := filepath.Join(examplesDir, "csv-report")
	ws := copyDir(t, filepath.Join(dir, "data"))

	code, out, errOut := runExample(t, "--workspace", ws, dir)
	if code != 1 || !strings.Contains(out, "west       3 orders     510.75") || !strings.Contains(errOut, "cannot write") {
		t.Fatalf("deny-all: code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	code, out, errOut = runExample(t, "--config", filepath.Join(dir, "config.json"), "--workspace", ws, dir)
	if code != 0 || !strings.Contains(out, "wrote out/summary.txt") {
		t.Fatalf("granted: code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	b, err := os.ReadFile(filepath.Join(ws, "out", "summary.txt"))
	if err != nil || !strings.Contains(string(b), "east       2 orders     309.00") {
		t.Fatalf("summary.txt = %q, %v", b, err)
	}
}

func TestExampleSandbox(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-must-not-leak")
	dir := filepath.Join(examplesDir, "sandbox")
	ws := copyDir(t, filepath.Join(dir, "job"))
	before, _ := os.ReadFile(filepath.Join(ws, "solution.sh"))

	code, out, errOut := runExample(t, "--workspace", ws, dir)
	if code != 0 || !strings.Contains(out, "2 3 5 7 11 13") {
		t.Fatalf("code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if n := strings.Count(out, "ALLOWED"); n != 1 || !strings.Contains(out, "ALLOWED  write to its scratch folder") {
		t.Fatalf("want exactly one ALLOWED (scratch):\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(ws, "solution.sh")); !bytes.Equal(before, after) {
		t.Fatal("job modified its own solution.sh")
	}
}
