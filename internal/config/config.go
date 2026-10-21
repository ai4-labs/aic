// Package config defines the portable sandbox config for a container.
//
// A config is never part of an app bundle: it is supplied
// by the operator at run time (aic run --config FILE), so an untrusted bundle
// cannot grant itself permissions. With no config, the runtime uses
// DenyAll.
//
// A Config expresses intent ("may reach api.openai.com:443", "may write to
// $WORKSPACE/out"). Each sandbox backend compiles it into its own mechanism:
// namespaces/seccomp/Landlock/cgroups on Linux, an SBPL profile plus a
// supervisor on macOS. Anything not granted is denied.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Config is the portable, deny-by-default permission set for a container.
type Config struct {
	Network    Network    `json:"network"`
	Filesystem Filesystem `json:"filesystem"`
	// MaxMemory is the memory limit in bytes; 0 means backend default.
	MaxMemory int64 `json:"maxMemory,omitempty"`
	// MaxProcs limits the number of processes/threads; 0 means backend default.
	MaxProcs int `json:"maxProcs,omitempty"`
}

// Network describes allowed network access. An empty Egress list means no
// network access at all.
type Network struct {
	// Egress is a list of "host:port" destinations reachable through the
	// runtime's egress proxy. Host may be a DNS name or an IP literal.
	Egress []string `json:"egress,omitempty"`
}

// Filesystem describes paths the container may write. Paths may begin with
// the variables $WORKSPACE or $SCRATCH, which the runtime expands.
type Filesystem struct {
	Writable []string `json:"writable,omitempty"`
}

// Validate checks the config for malformed entries.
func (p Config) Validate() error {
	var errs []error
	for _, e := range p.Network.Egress {
		if err := validateEgress(e); err != nil {
			errs = append(errs, err)
		}
	}
	for _, w := range p.Filesystem.Writable {
		if err := validateWritable(w); err != nil {
			errs = append(errs, err)
		}
	}
	if p.MaxMemory < 0 {
		errs = append(errs, fmt.Errorf("config: maxMemory must be >= 0, got %d", p.MaxMemory))
	}
	if p.MaxProcs < 0 {
		errs = append(errs, fmt.Errorf("config: maxProcs must be >= 0, got %d", p.MaxProcs))
	}
	return errors.Join(errs...)
}

// DenyAll is the config used when the operator supplies none: no network
// and no writable paths beyond the runtime's own scratch dir.
func DenyAll() Config { return Config{} }

// Parse decodes and validates an operator-supplied config file (JSON).
// Unknown fields are rejected so a typo cannot silently drop a restriction
// or be mistaken for a grant.
func Parse(b []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Config
	if err := dec.Decode(&p); err != nil {
		return Config{}, fmt.Errorf("config: decode: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("config: trailing data after JSON object")
	}
	if err := p.Validate(); err != nil {
		return Config{}, err
	}
	return p, nil
}

func validateEgress(e string) error {
	host, port, err := net.SplitHostPort(e)
	if err != nil {
		return fmt.Errorf("config: egress %q: want host:port: %v", e, err)
	}
	if host == "" || strings.ContainsAny(host, "*/ ") {
		return fmt.Errorf("config: egress %q: invalid host (wildcards are not supported)", e)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("config: egress %q: invalid port", e)
	}
	return nil
}

func validateWritable(w string) error {
	if !strings.HasPrefix(w, "$WORKSPACE") && !strings.HasPrefix(w, "$SCRATCH") {
		return fmt.Errorf("config: writable %q: must start with $WORKSPACE or $SCRATCH", w)
	}
	for _, seg := range strings.Split(w, "/") {
		if seg == ".." {
			return fmt.Errorf("config: writable %q: must not contain '..'", w)
		}
	}
	return nil
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	// Longest suffixes first so "GiB" is not matched as "B".
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"B", 1},
}

// ParseSize parses a human-readable byte size such as "16GiB", "512MB", or
// "1024" (bytes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	num := s
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			num = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("config: invalid size %q", s)
	}
	if n > 0 && mult > (1<<63-1)/n {
		return 0, fmt.Errorf("config: size %q overflows", s)
	}
	return n * mult, nil
}
