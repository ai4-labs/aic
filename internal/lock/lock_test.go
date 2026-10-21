package lock

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func project(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readLockBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnsureCreatesMissingLock(t *testing.T) {
	dir := project(t, "name: a\n")
	res, err := Ensure(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Reason != "aic.lock missing" {
		t.Fatalf("Ensure = %+v, want Updated with reason 'aic.lock missing'", res)
	}
	l, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Inputs[ProjectFile]; !ok {
		t.Fatalf("lock inputs = %v, want %s", l.Inputs, ProjectFile)
	}
}

func TestEnsureNoOpWhenFresh(t *testing.T) {
	dir := project(t, "name: a\n")
	if _, err := Ensure(dir, false); err != nil {
		t.Fatal(err)
	}
	before := readLockBytes(t, dir)
	for _, locked := range []bool{false, true} {
		res, err := Ensure(dir, locked)
		if err != nil {
			t.Fatalf("locked=%v: %v", locked, err)
		}
		if res.Updated || res.Reason != "" {
			t.Fatalf("locked=%v: Ensure = %+v, want no update", locked, res)
		}
	}
	if !bytes.Equal(before, readLockBytes(t, dir)) {
		t.Fatal("fresh lock was rewritten")
	}
}

func TestEnsureRelocksWhenProjectChanges(t *testing.T) {
	dir := project(t, "name: a\n")
	if _, err := Ensure(dir, false); err != nil {
		t.Fatal(err)
	}
	before := readLockBytes(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte("name: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Ensure(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Reason != "aic.yaml changed" {
		t.Fatalf("Ensure = %+v, want Updated with reason 'aic.yaml changed'", res)
	}
	if bytes.Equal(before, readLockBytes(t, dir)) {
		t.Fatal("lock was not rewritten")
	}
}

func TestEnsureLockedRefusesToWrite(t *testing.T) {
	// Missing lock.
	dir := project(t, "name: a\n")
	_, err := Ensure(dir, true)
	if !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "aic.lock missing") {
		t.Fatalf("missing lock: err = %v, want ErrStale mentioning missing lock", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("locked mode created aic.lock")
	}

	// Stale lock.
	if _, err := Ensure(dir, false); err != nil {
		t.Fatal(err)
	}
	before := readLockBytes(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte("name: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Ensure(dir, true)
	if !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "aic.yaml changed") {
		t.Fatalf("stale lock: err = %v, want ErrStale mentioning aic.yaml", err)
	}
	if !bytes.Equal(before, readLockBytes(t, dir)) {
		t.Fatal("locked mode rewrote aic.lock")
	}
}

func TestEnsureNeverOverwritesUnreadableLock(t *testing.T) {
	dir := project(t, "name: a\n")
	conflict := []byte("<<<<<<< HEAD\n{}\n=======\n{}\n>>>>>>> branch\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), conflict, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, false); err == nil || !strings.Contains(err.Error(), "fix or delete it") {
		t.Fatalf("err = %v, want parse error telling the user to fix or delete", err)
	}
	if !bytes.Equal(conflict, readLockBytes(t, dir)) {
		t.Fatal("unreadable lock was overwritten")
	}
}

func TestEnsureNoProject(t *testing.T) {
	_, err := Ensure(t.TempDir(), false)
	if !errors.Is(err, ErrNoProject) {
		t.Fatalf("err = %v, want ErrNoProject", err)
	}
}

func TestMarshalDeterministic(t *testing.T) {
	dir := project(t, "name: a\n")
	a, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := a.Marshal()
	bb, _ := b.Marshal()
	if !bytes.Equal(ab, bb) || !bytes.HasSuffix(ab, []byte("\n")) {
		t.Fatalf("non-deterministic or missing trailing newline:\n%s\n%s", ab, bb)
	}
	if _, err := Parse(ab); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	const d = `"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`
	for name, in := range map[string]string{
		"unknown field":   `{"schemaVersion":1,"inputs":{"aic.yaml":` + d + `},"extra":1}`,
		"wrong schema":    `{"schemaVersion":2,"inputs":{"aic.yaml":` + d + `}}`,
		"no inputs":       `{"schemaVersion":1,"inputs":{}}`,
		"bad digest":      `{"schemaVersion":1,"inputs":{"aic.yaml":"sha256:nope"}}`,
		"escaping input":  `{"schemaVersion":1,"inputs":{"../secret":` + d + `}}`,
		"absolute input":  `{"schemaVersion":1,"inputs":{"/etc/passwd":` + d + `}}`,
		"unclean input":   `{"schemaVersion":1,"inputs":{"a/./b":` + d + `}}`,
		"trailing data":   `{"schemaVersion":1,"inputs":{"aic.yaml":` + d + `}} {}`,
		"not json at all": `nope`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse succeeded, want error", name)
		}
	}
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := project(t, "name: a\n")
	l, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".aic.lock-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
