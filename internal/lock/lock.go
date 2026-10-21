// Package lock manages aic.lock, the resolved and pinned form of a
// project's aic.yaml.
//
// Resolution (deciding exact versions) and materialization (fetching and
// assembling bytes) are separate steps so that builds are reproducible: a
// build uses only what the lock names. To keep that split out of the way,
// Ensure re-locks automatically when the lock is missing or was computed
// from different inputs, unless the caller asks for --locked behavior.
//
// A lock records the digest of every input it was computed from. Today the
// only input is aic.yaml; toolchain providers will add their own input
// files (e.g. requirements.txt) and resolved entries in milestone M4.
package lock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rakyll/ai-container/internal/digest"
)

const (
	// FileName is the lock file name, next to ProjectFile.
	FileName = "aic.lock"
	// ProjectFile is the human-authored project file.
	ProjectFile = "aic.yaml"
	// SchemaVersion is the lock schema version produced by this code.
	SchemaVersion = 1
)

var (
	// ErrNoProject is returned when the directory has no aic.yaml.
	ErrNoProject = errors.New("no " + ProjectFile + " found")
	// ErrStale is returned by Ensure in locked mode when the lock is
	// missing or out of date.
	ErrStale = errors.New(FileName + " is out of date")
)

// Lock is the decoded aic.lock file.
type Lock struct {
	SchemaVersion int `json:"schemaVersion"`
	// Inputs maps each input file (slash-separated, relative to the
	// project directory) to the digest of its contents when the lock was
	// computed.
	Inputs map[string]digest.Digest `json:"inputs"`
}

// inputFiles lists the files a lock is computed from.
func inputFiles() []string { return []string{ProjectFile} }

// Generate computes a fresh lock for the project in dir.
func Generate(dir string) (*Lock, error) {
	in, err := hashInputs(dir, inputFiles())
	if err != nil {
		return nil, err
	}
	return &Lock{SchemaVersion: SchemaVersion, Inputs: in}, nil
}

func hashInputs(dir string, names []string) (map[string]digest.Digest, error) {
	out := make(map[string]digest.Digest, len(names))
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) && name == ProjectFile {
			return nil, fmt.Errorf("%w in %s", ErrNoProject, dir)
		}
		if err != nil {
			return nil, err
		}
		d, _, err := digest.FromReader(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("hashing %s: %w", name, err)
		}
		out[name] = d
	}
	return out, nil
}

// Read loads and validates dir/aic.lock. If the file does not exist, the
// returned error satisfies errors.Is(err, fs.ErrNotExist).
func Read(dir string) (*Lock, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates lock file contents. Unknown fields are
// rejected so that a lock from a newer or different tool fails loudly.
func Parse(b []byte) (*Lock, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var l Lock
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%s: invalid: %w", FileName, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: invalid: trailing data after JSON object", FileName)
	}
	if l.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schemaVersion %d", FileName, l.SchemaVersion)
	}
	if len(l.Inputs) == 0 {
		return nil, fmt.Errorf("%s: no inputs recorded", FileName)
	}
	for name := range l.Inputs {
		if !validInputName(name) {
			return nil, fmt.Errorf("%s: invalid input name %q", FileName, name)
		}
	}
	return &l, nil
}

// validInputName accepts clean, relative, slash-separated paths that stay
// inside the project directory.
func validInputName(name string) bool {
	return name != "" && name != "." && !strings.HasPrefix(name, "/") &&
		!strings.Contains(name, `\`) && path.Clean(name) == name &&
		name != ".." && !strings.HasPrefix(name, "../")
}

// Marshal returns the canonical encoding of l: indented JSON with sorted
// keys and a trailing newline, so that unchanged locks produce identical
// bytes and diffs stay minimal.
func (l *Lock) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Write atomically replaces dir/aic.lock with l.
func (l *Lock) Write(dir string) error {
	b, err := l.Marshal()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aic.lock-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}

// Staleness explains why a lock no longer matches the project. The zero
// value means the lock is up to date.
func Staleness(dir string, l *Lock) (string, error) {
	cur, err := hashInputs(dir, inputFiles())
	if err != nil {
		return "", err
	}
	var changed []string
	for name, d := range cur {
		if old, ok := l.Inputs[name]; !ok || old != d {
			changed = append(changed, name)
		}
	}
	for name := range l.Inputs {
		if _, ok := cur[name]; !ok {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	sort.Strings(changed)
	return strings.Join(changed, ", ") + " changed", nil
}

// Result reports what Ensure did.
type Result struct {
	Lock *Lock
	// Updated is true if aic.lock was (re)written.
	Updated bool
	// Reason explains why it was written, e.g. "aic.lock missing" or
	// "aic.yaml changed". Empty when Updated is false.
	Reason string
}

// Ensure returns an up-to-date lock for the project in dir.
//
// If aic.lock is missing or stale, Ensure re-locks and writes it, unless
// locked is true, in which case it returns an error wrapping ErrStale and
// leaves the file untouched (for CI). A lock that exists but cannot be
// parsed is never overwritten: it may hold unresolved merge conflicts or
// other content the user wants to keep.
func Ensure(dir string, locked bool) (Result, error) {
	if _, err := os.Stat(filepath.Join(dir, ProjectFile)); errors.Is(err, fs.ErrNotExist) {
		return Result{}, fmt.Errorf("%w in %s", ErrNoProject, dir)
	}
	l, err := Read(dir)
	var reason string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		reason = FileName + " missing"
	case err != nil:
		return Result{}, fmt.Errorf("%w; fix or delete it, then retry", err)
	default:
		reason, err = Staleness(dir, l)
		if err != nil {
			return Result{}, err
		}
		if reason == "" {
			return Result{Lock: l}, nil
		}
	}
	if locked {
		return Result{}, fmt.Errorf("%w (%s); run `aic build` without --locked to update it", ErrStale, reason)
	}
	fresh, err := Generate(dir)
	if err != nil {
		return Result{}, err
	}
	if err := fresh.Write(dir); err != nil {
		return Result{}, fmt.Errorf("writing %s: %w", FileName, err)
	}
	return Result{Lock: fresh, Updated: true, Reason: reason}, nil
}
