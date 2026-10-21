// Package project loads and validates aic.yaml, the human-authored project
// file (spec/format.md §8).
//
// aic.yaml is parsed as a strict subset of YAML 1.2: a single document, no
// anchors/aliases/merge keys, no explicit tags, no duplicate or unknown
// keys, and string fields must be strings in the source (so that 3.10 is
// an error instead of silently becoming 3.1).
package project

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/rakyll/ai-container/internal/manifest"
)

// FileName is the project file name.
const FileName = "aic.yaml"

// SchemaVersion is the aic.yaml schema version understood by this code.
const SchemaVersion = 1

// DefaultWorkDir is the entrypoint working directory, relative to the
// container root, when aic.yaml does not set one. Included files live there.
const DefaultWorkDir = "app"

// maxFileSize bounds how much of aic.yaml is read.
const maxFileSize = 1 << 20

// Project is a validated aic.yaml.
type Project struct {
	// Dir is the absolute directory containing aic.yaml.
	Dir           string
	SchemaVersion int
	Name          string
	Version       string
	// Toolchains lists the toolchain provider names, sorted. Provider
	// options are not interpreted here.
	Toolchains []string
	Targets    []manifest.Platform
	// Include holds slash-separated glob patterns relative to Dir.
	Include    []string
	Entrypoint Entrypoint
}

// Entrypoint is the default process.
type Entrypoint struct {
	Cmd     []string
	Env     map[string]string
	WorkDir string // relative to the container root; never empty after Load
}

// Supports reports whether p lists platform as a target.
func (p *Project) Supports(platform manifest.Platform) bool {
	for _, t := range p.Targets {
		if t == platform {
			return true
		}
	}
	return false
}

// Load reads and validates dir/aic.yaml.
func Load(dir string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(abs, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no %s found in %s", FileName, abs)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", FileName, maxFileSize)
	}
	p, err := Parse(b)
	if err != nil {
		return nil, err
	}
	p.Dir = abs
	return p, nil
}

// Parse decodes and validates aic.yaml contents.
func Parse(b []byte) (*Project, error) {
	if err := checkStructure(b); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var raw rawProject
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	p, err := raw.validate()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return p, nil
}

// checkStructure enforces the parts of the strict subset that the typed
// decoder does not: exactly one document, no anchors, aliases, merge keys,
// explicit tags, or duplicate keys.
func checkStructure(b []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("file is empty")
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple YAML documents are not allowed")
	}
	return walk(&doc)
}

func walk(n *yaml.Node) error {
	if n.Anchor != "" {
		return fmt.Errorf("line %d: anchors (&%s) are not allowed", n.Line, n.Anchor)
	}
	if n.Kind == yaml.AliasNode {
		return fmt.Errorf("line %d: aliases are not allowed", n.Line)
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return fmt.Errorf("line %d: explicit tags (%s) are not allowed", n.Line, n.Tag)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: mapping keys must be plain strings", k.Line)
			}
			if k.Value == "<<" && k.ShortTag() == "!!merge" {
				return fmt.Errorf("line %d: merge keys (<<) are not allowed", k.Line)
			}
			if seen[k.Value] {
				return fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := walk(c); err != nil {
			return err
		}
	}
	return nil
}

// str is a string that must be a YAML string in the source.
type str string

func (s *str) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: expected a string, got %s (quote the value)", n.Line, describe(n))
	}
	*s = str(n.Value)
	return nil
}

func describe(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	}
	return fmt.Sprintf("%s %q", strings.TrimPrefix(n.ShortTag(), "!!"), n.Value)
}

type rawProject struct {
	SchemaVersion int                  `yaml:"schemaVersion"`
	Name          str                  `yaml:"name"`
	Version       str                  `yaml:"version"`
	Toolchains    map[string]yaml.Node `yaml:"toolchains"`
	Targets       []str                `yaml:"targets"`
	Include       []str                `yaml:"include"`
	Entrypoint    struct {
		Cmd     []str          `yaml:"cmd"`
		Env     map[string]str `yaml:"env"`
		WorkDir str            `yaml:"workdir"`
	} `yaml:"entrypoint"`
}

func (r *rawProject) validate() (*Project, error) {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if r.SchemaVersion != SchemaVersion {
		add("schemaVersion must be %d, got %d", SchemaVersion, r.SchemaVersion)
	}
	if !manifest.ValidName(string(r.Name)) {
		add("invalid name %q (want lowercase [a-z0-9._-], max 128 chars)", r.Name)
	}
	if r.Version == "" {
		add("version is required")
	}

	p := &Project{
		SchemaVersion: r.SchemaVersion,
		Name:          string(r.Name),
		Version:       string(r.Version),
		Entrypoint:    Entrypoint{Env: map[string]string{}, WorkDir: string(r.Entrypoint.WorkDir)},
	}

	for name := range r.Toolchains {
		if !manifest.ValidName(name) {
			add("invalid toolchain name %q", name)
		}
		p.Toolchains = append(p.Toolchains, name)
	}
	sort.Strings(p.Toolchains)

	if len(r.Targets) == 0 {
		add("targets: at least one platform is required")
	}
	seen := map[manifest.Platform]bool{}
	for _, t := range r.Targets {
		pl := manifest.Platform(t)
		switch {
		case !pl.Valid():
			add("targets: unsupported platform %q (want darwin|linux-amd64|arm64)", t)
		case seen[pl]:
			add("targets: duplicate platform %q", t)
		}
		seen[pl] = true
		p.Targets = append(p.Targets, pl)
	}

	for _, pat := range r.Include {
		if err := ValidatePattern(string(pat)); err != nil {
			add("include: %v", err)
		}
		p.Include = append(p.Include, string(pat))
	}

	if len(r.Entrypoint.Cmd) == 0 || r.Entrypoint.Cmd[0] == "" {
		add("entrypoint.cmd is required")
	}
	for _, a := range r.Entrypoint.Cmd {
		if strings.ContainsRune(string(a), 0) {
			add("entrypoint.cmd: argument contains NUL")
		}
		p.Entrypoint.Cmd = append(p.Entrypoint.Cmd, string(a))
	}
	for k, v := range r.Entrypoint.Env {
		if err := manifest.CheckEnv(k, string(v)); err != nil {
			add("entrypoint.env: %v", err)
		}
		p.Entrypoint.Env[k] = string(v)
	}
	if p.Entrypoint.WorkDir == "" {
		p.Entrypoint.WorkDir = DefaultWorkDir
	} else if !manifest.ValidRelPath(p.Entrypoint.WorkDir) {
		add("entrypoint.workdir %q must be a relative path inside the container", p.Entrypoint.WorkDir)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// ValidatePattern checks an include glob: relative, slash-separated, no
// empty, "." or ".." segments, and each segment a valid path.Match pattern
// or "**".
func ValidatePattern(pat string) error {
	if pat == "" || strings.HasPrefix(pat, "/") || strings.ContainsAny(pat, "\\\x00") {
		return fmt.Errorf("pattern %q must be a relative, slash-separated path", pat)
	}
	for _, seg := range strings.Split(pat, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return fmt.Errorf("pattern %q: invalid segment %q", pat, seg)
		case seg == "**":
		default:
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("pattern %q: %v", pat, err)
			}
		}
	}
	return nil
}

// Match reports whether the slash-separated relative path name matches the
// include pattern. "**" matches zero or more whole path segments; other
// segments use path.Match syntax.
func Match(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(p, s []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for i := 0; i <= len(s); i++ {
				if matchSegs(p[1:], s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], s[0]); !ok {
			return false
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// Included reports whether name (slash-separated, relative to Dir) is
// matched by any include pattern.
func (p *Project) Included(name string) bool {
	for _, pat := range p.Include {
		if Match(pat, name) {
			return true
		}
	}
	return false
}
