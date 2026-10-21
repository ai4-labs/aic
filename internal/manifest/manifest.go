// Package manifest defines the machine-readable app bundle manifest
// described in spec/format.md: code, data, and per-platform runtime/packages
// components.
//
// The manifest never carries a sandbox config; the operator supplies it at
// run time. Nothing in the manifest is specific to a language.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/rakyll/ai-container/internal/digest"
)

// SchemaVersion is the manifest schema version produced by this code.
const SchemaVersion = 1

// MediaType identifies an aic app bundle manifest.
const MediaType = "application/vnd.aic.manifest.v1+json"

// Kind is the type of a component.
type Kind string

// Component kinds. Shared kinds are platform-independent and are listed at
// the top level of the manifest; target kinds are per-platform.
const (
	KindCode     Kind = "code"
	KindData     Kind = "data"
	KindRuntime  Kind = "runtime"
	KindPackages Kind = "packages"
)

// Shared reports whether components of kind k are platform-independent.
func (k Kind) Shared() bool {
	switch k {
	case KindCode, KindData:
		return true
	}
	return false
}

func (k Kind) valid() bool {
	switch k {
	case KindCode, KindData, KindRuntime, KindPackages:
		return true
	}
	return false
}

// ReservedMounts are top-level container paths owned by the runtime. App
// components must not mount at or below them.
var ReservedMounts = []string{"workspace", "scratch"}

// Platform is "<os>-<arch>", e.g. "darwin-arm64" or "linux-amd64".
type Platform string

var platformRE = regexp.MustCompile(`^(darwin|linux)-(amd64|arm64)$`)

// Valid reports whether p is a supported platform string.
func (p Platform) Valid() bool { return platformRE.MatchString(string(p)) }

// HostPlatform returns the platform of the running process.
func HostPlatform() Platform {
	return Platform(runtime.GOOS + "-" + runtime.GOARCH)
}

// Component is a content-addressed Merkle tree with a role in the bundle.
type Component struct {
	Kind Kind `json:"kind"`
	// Name distinguishes multiple components of the same kind. Must be
	// unique per kind within its scope.
	Name string `json:"name"`
	// Tree is the digest of the component's root tree object.
	Tree digest.Digest `json:"tree"`
	// Mount is the path, relative to the container root, where the
	// component is exposed read-only.
	Mount string `json:"mount"`
	// Size is the total uncompressed size of all files, in bytes.
	Size int64 `json:"size"`
	// Path lists directories, relative to the component root, that the
	// runtime prepends to PATH. This is how a component exposes its
	// executables without the runtime knowing what language they are.
	Path []string `json:"path,omitempty"`
	// Env holds environment variables exported by the component. The
	// token ${AIC_MOUNT} expands to the component's absolute mount path
	// at run time (e.g. "PYTHONHOME": "${AIC_MOUNT}").
	Env map[string]string `json:"env,omitempty"`
}

// Target holds the components for one platform. A target is identified
// only by "<os>-<arch>".
type Target struct {
	Components []Component `json:"components"`
}

// Entrypoint describes the default process.
type Entrypoint struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
}

// Manifest is the root object of an app bundle.
type Manifest struct {
	SchemaVersion int                 `json:"schemaVersion"`
	MediaType     string              `json:"mediaType"`
	Name          string              `json:"name"`
	Version       string              `json:"version"`
	Lock          digest.Digest       `json:"lock"`
	Components    []Component         `json:"components"`
	Targets       map[Platform]Target `json:"targets"`
	Entrypoint    Entrypoint          `json:"entrypoint"`
	Annotations   map[string]string   `json:"annotations,omitempty"`
}

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	mountRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// ValidName reports whether s is a valid artifact or component name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Validate checks structural rules from spec/format.md.
func (m *Manifest) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("manifest: "+format, a...)) }

	if m.SchemaVersion != SchemaVersion {
		add("unsupported schemaVersion %d", m.SchemaVersion)
	}
	if m.MediaType != MediaType {
		add("mediaType = %q, want %q", m.MediaType, MediaType)
	}
	if !nameRE.MatchString(m.Name) {
		add("invalid name %q", m.Name)
	}
	if m.Version == "" {
		add("version is required")
	}
	if m.Lock.IsZero() {
		add("lock digest is required")
	}
	if len(m.Entrypoint.Cmd) == 0 {
		add("entrypoint.cmd is required")
	}
	for k, v := range m.Entrypoint.Env {
		if err := checkEnv(k, v); err != nil {
			add("entrypoint.env: %v", err)
		}
	}
	if len(m.Targets) == 0 {
		add("at least one target is required")
	}

	mounts := map[string]string{}
	envOwners := map[string]string{}
	checkComponents := func(scope string, cs []Component, wantShared bool, mounts, envOwners map[string]string) {
		seen := map[string]bool{}
		for _, c := range cs {
			id := string(c.Kind) + "/" + c.Name
			switch {
			case !c.Kind.valid():
				add("%s: unknown component kind %q", scope, c.Kind)
				continue
			case c.Kind.Shared() != wantShared:
				add("%s: component kind %q not allowed here", scope, c.Kind)
			}
			if !nameRE.MatchString(c.Name) {
				add("%s: invalid component name %q", scope, c.Name)
			}
			if seen[id] {
				add("%s: duplicate component %s", scope, id)
			}
			seen[id] = true
			if c.Tree.IsZero() {
				add("%s: component %s: tree digest is required", scope, id)
			}
			switch {
			case !mountRE.MatchString(c.Mount) || hasDotSegment(c.Mount):
				add("%s: component %s: invalid mount %q", scope, id, c.Mount)
			case isReserved(c.Mount):
				add("%s: component %s: mount %q is reserved for the runtime", scope, id, c.Mount)
			default:
				if other, ok := mounts[c.Mount]; ok {
					add("%s: component %s: mount %q already used by %s", scope, id, c.Mount, other)
				} else {
					mounts[c.Mount] = id
				}
			}
			if c.Size < 0 {
				add("%s: component %s: negative size", scope, id)
			}
			for _, p := range c.Path {
				if !mountRE.MatchString(p) || hasDotSegment(p) {
					add("%s: component %s: invalid path entry %q", scope, id, p)
				}
			}
			for _, k := range sortedKeys(c.Env) {
				if err := checkEnv(k, c.Env[k]); err != nil {
					add("%s: component %s: %v", scope, id, err)
					continue
				}
				if other, ok := envOwners[k]; ok {
					add("%s: component %s: env %s already set by %s", scope, id, k, other)
				} else {
					envOwners[k] = id
				}
			}
		}
	}
	checkComponents("components", m.Components, true, mounts, envOwners)

	// Each target sees shared mounts/env plus its own; check conflicts per target.
	platforms := make([]Platform, 0, len(m.Targets))
	for p := range m.Targets {
		platforms = append(platforms, p)
	}
	sort.Slice(platforms, func(i, j int) bool { return platforms[i] < platforms[j] })
	for _, p := range platforms {
		if !p.Valid() {
			add("invalid target platform %q", p)
			continue
		}
		checkComponents("targets."+string(p), m.Targets[p].Components, false, clone(mounts), clone(envOwners))
	}
	return errors.Join(errs...)
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are variables the runtime owns; bundles cannot set them.
var reservedEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true}

func checkEnv(k, v string) error { return CheckEnv(k, v) }

// CheckEnv validates an environment variable set by a bundle or project:
// the name must be a valid identifier and must not be reserved for the
// runtime (PATH, HOME, TMPDIR, AIC_*), and the value must not contain NUL.
func CheckEnv(k, v string) error {
	switch {
	case !envKeyRE.MatchString(k):
		return fmt.Errorf("invalid env name %q", k)
	case reservedEnv[k] || strings.HasPrefix(k, "AIC_"):
		return fmt.Errorf("env %s is reserved for the runtime", k)
	case strings.ContainsRune(v, 0):
		return fmt.Errorf("env %s contains NUL", k)
	}
	return nil
}

// ValidRelPath reports whether p is a relative, slash-separated path of
// [A-Za-z0-9._-] segments with no "." or ".." segments, i.e. a path that
// cannot escape the directory it is resolved against.
func ValidRelPath(p string) bool {
	return mountRE.MatchString(p) && !hasDotSegment(p)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isReserved(mount string) bool {
	top, _, _ := strings.Cut(mount, "/")
	for _, r := range ReservedMounts {
		if top == r {
			return true
		}
	}
	return false
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// Parse decodes and validates an app manifest. Unknown fields are rejected
// so that typos and future-incompatible manifests fail loudly.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := decodeStrict(b, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("manifest: decode: %w", err)
	}
	if dec.More() {
		return errors.New("manifest: trailing data after JSON object")
	}
	return nil
}
