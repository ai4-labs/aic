package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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
	if code := run([]string{"build"}, &out, &errb); code != exitNotImplemented {
		t.Fatalf("code = %d, want %d", code, exitNotImplemented)
	}
	if !strings.Contains(errb.String(), "M1") {
		t.Fatalf("stderr = %q, want milestone", errb.String())
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
