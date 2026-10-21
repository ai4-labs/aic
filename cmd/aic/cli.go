package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/rakyll/ai-container/internal/manifest"
	"github.com/rakyll/ai-container/internal/sandbox"
)

// Exit codes.
const (
	exitOK             = 0
	exitError          = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

type command struct {
	name      string
	usage     string
	milestone string // non-empty if not implemented yet
	run       func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{name: "init", usage: "init [-name NAME] [dir]   scaffold an aic.yaml", run: runInit},
		{name: "lock", usage: "lock                      resolve aic.yaml into aic.lock via toolchain providers", milestone: "M4"},
		{name: "build", usage: "build                     materialize components and produce a bundle", milestone: "M1"},
		{name: "run", usage: "run [flags] <app> [-- cmd...]\n                                run an app in a sandbox (deny-all if no config)\n" +
			"                                flags: --config FILE, --workspace DIR", milestone: "M2/M3"},
		{name: "shell", usage: "shell [flags] <app>       interactive shell inside the sandbox (same flags as run)", milestone: "M2/M3"},
		{name: "inspect", usage: "inspect <ref>             show an app manifest", milestone: "M1"},
		{name: "push", usage: "push <ref>                upload an app", milestone: "M6"},
		{name: "pull", usage: "pull <ref>                download an app", milestone: "M6"},
		{name: "gc", usage: "gc                        prune unreferenced store objects", milestone: "M1"},
		{name: "version", usage: "version                   print version and platform info", run: runVersion},
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		if c.run == nil {
			fmt.Fprintf(stderr, "aic %s: not implemented yet (planned for milestone %s)\n", c.name, c.milestone)
			return exitNotImplemented
		}
		return c.run(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "aic: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "aic — AI container format and runtime")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	for _, c := range commands() {
		suffix := ""
		if c.milestone != "" {
			suffix = "  (" + c.milestone + ")"
		}
		fmt.Fprintf(w, "  aic %s%s\n", c.usage, suffix)
	}
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: aic version")
		return exitUsage
	}
	backend := "none"
	if b, err := sandbox.New(); err == nil {
		backend = b.Name()
	}
	fmt.Fprintf(stdout, "aic %s\nplatform: %s\nsandbox: %s\nmanifest schema: v%d\n",
		version(), manifest.HostPlatform(), backend, manifest.SchemaVersion)
	return exitOK
}

var projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func runInit(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	name := fl.String("name", "", "project name (default: directory name)")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	if fl.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: aic init [-name NAME] [dir]")
		return exitUsage
	}
	dir := "."
	if fl.NArg() == 1 {
		dir = fl.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "aic init: %v\n", err)
		return exitError
	}
	if *name == "" {
		*name = sanitizeName(filepath.Base(abs))
	}
	if !projectNameRE.MatchString(*name) {
		fmt.Fprintf(stderr, "aic init: invalid project name %q (want lowercase [a-z0-9._-], max 128 chars)\n", *name)
		return exitUsage
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		fmt.Fprintf(stderr, "aic init: %v\n", err)
		return exitError
	}
	path := filepath.Join(abs, projectFile)
	// O_EXCL: never clobber an existing manifest.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		fmt.Fprintf(stderr, "aic init: %s already exists\n", path)
		return exitError
	}
	if err != nil {
		fmt.Fprintf(stderr, "aic init: %v\n", err)
		return exitError
	}
	_, werr := fmt.Fprintf(f, yamlTemplate, *name)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintf(stderr, "aic init: writing %s: %v\n", path, werr)
		return exitError
	}
	fmt.Fprintf(stdout, "created %s\n", path)
	return exitOK
}

// sanitizeName turns a directory name into a valid project name.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 128 {
		out = out[:128]
	}
	if out == "" {
		return "app"
	}
	return out
}

// projectFile is the human-authored project file. Only this exact name is
// recognized (not aic.yml) so there is never ambiguity about which file wins.
const projectFile = "aic.yaml"

// yamlTemplate is the scaffold written by `aic init`. It is deliberately
// language-neutral: languages come from toolchain providers, and the
// entrypoint is just a command. Version-like values are quoted on purpose:
// unquoted 3.10 is the float 3.1 in YAML.
const yamlTemplate = `# aic project file. See spec/format.md.
schemaVersion: 1
name: %q
version: "0.1.0"

# Toolchains supply language runtimes and dependencies. Each key is a
# provider; its options are defined by that provider. Uncomment what you
# need, or add your own provider (aic-toolchain-<name> on PATH).
toolchains: {}
#  python:
#    version: "3.12"
#    requirements: requirements.txt
#  node:
#    version: "22"
#    lockfile: package-lock.json
#  prebuilt:                       # any language: ship compiled binaries
#    darwin-arm64: dist/darwin-arm64
#    linux-amd64: dist/linux-amd64

# Platforms to build for (<os>-<arch>).
targets:
  - darwin-arm64
  - linux-amd64

# Project files to ship (globs relative to this file).
include:
  - "src/**"

# Any command. Toolchain executables are on PATH.
entrypoint:
  cmd: [./src/start]

# The sandbox config is intentionally not declared here. Whoever runs the
# app supplies it:
#   aic run --config config.json <app>
`
