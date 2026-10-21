package main

import (
	"context"
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

	"github.com/rakyll/ai-container/internal/config"
	"github.com/rakyll/ai-container/internal/lock"
	"github.com/rakyll/ai-container/internal/manifest"
	"github.com/rakyll/ai-container/internal/runner"
	"github.com/rakyll/ai-container/internal/sandbox"
)

// Exit codes.
const (
	exitOK             = 0
	exitError          = 1
	exitUsage          = 2
	exitNotImplemented = 3
	// exitRunFailed means aic run could not start the app. Otherwise
	// aic run exits with the app's own exit code (Docker uses 125 too).
	exitRunFailed = 125
)

type command struct {
	name  string
	usage string
	// milestone is shown in help for commands that are not fully
	// implemented yet. If run is nil the command only reports it.
	milestone string
	run       func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{name: "init", usage: "init [-name NAME] [dir]   scaffold an aic.yaml", run: runInit},
		{name: "build", usage: "build [--locked] [dir]    lock if needed, then produce a bundle\n" +
			"                                --locked: fail instead of updating a missing or stale aic.lock (CI)", milestone: "M1", run: runBuild},
		{name: "run", usage: "run [flags] [dir] [-- cmd...]\n" +
			"                                run a project in a sandbox (deny-all if no --config); macOS only for now\n" +
			"                                flags: --config FILE, --workspace DIR", milestone: "M3 for Linux", run: runRun},
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

// projectDir parses an optional single [dir] argument.
func projectDir(fl *flag.FlagSet) (string, bool) {
	switch fl.NArg() {
	case 0:
		return ".", true
	case 1:
		return fl.Arg(0), true
	}
	return "", false
}

func runBuild(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("build", flag.ContinueOnError)
	fl.SetOutput(stderr)
	locked := fl.Bool("locked", false, "fail instead of updating a missing or stale aic.lock")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	dir, ok := projectDir(fl)
	if !ok {
		fmt.Fprintln(stderr, "usage: aic build [--locked] [dir]")
		return exitUsage
	}
	res, err := lock.Ensure(dir, *locked)
	if err != nil {
		fmt.Fprintf(stderr, "aic build: %v\n", err)
		return exitError
	}
	if res.Updated {
		fmt.Fprintf(stderr, "aic build: updated %s (%s)\n", lock.FileName, res.Reason)
	}
	fmt.Fprintln(stderr, "aic build: assembling the bundle is not implemented yet (planned for milestone M1)")
	return exitNotImplemented
}

// maxConfigSize bounds how much of a --config file is read.
const maxConfigSize = 1 << 20

func runRun(args []string, stdout, stderr io.Writer) int {
	// Split "-- cmd..." off first: the flag package would otherwise
	// consume the "--" and make the override look like a directory.
	var override []string
	for i, a := range args {
		if a == "--" {
			args, override = args[:i], args[i+1:]
			if len(override) == 0 {
				fmt.Fprintln(stderr, "aic run: no command after --")
				return exitUsage
			}
			break
		}
	}
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "", "operator config file (JSON); default: deny all")
	workspace := fl.String("workspace", "", "host directory to expose at $AIC_ROOT/workspace")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	dir, ok := projectDir(fl)
	if !ok {
		fmt.Fprintln(stderr, "usage: aic run [--config FILE] [--workspace DIR] [dir] [-- cmd...]")
		return exitUsage
	}

	cfg := config.DenyAll()
	if *configPath != "" {
		var err error
		if cfg, err = readConfig(*configPath); err != nil {
			fmt.Fprintf(stderr, "aic run: %v\n", err)
			return exitRunFailed
		}
	}

	code, err := runner.Run(context.Background(), runner.Options{
		ProjectDir: dir,
		Config:     cfg,
		Workspace:  *workspace,
		Cmd:        override,
		Stdin:      os.Stdin,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	switch {
	case errors.Is(err, sandbox.ErrNotImplemented):
		fmt.Fprintf(stderr, "aic run: the %s sandbox is not implemented yet (milestone M3)\n", manifest.HostPlatform())
		return exitNotImplemented
	case err != nil:
		fmt.Fprintf(stderr, "aic run: %v\n", err)
		return exitRunFailed
	}
	return code
}

func readConfig(path string) (config.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return config.Config{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return config.Config{}, err
	}
	if len(b) > maxConfigSize {
		return config.Config{}, fmt.Errorf("%s: larger than %d bytes", path, maxConfigSize)
	}
	c, err := config.Parse(b)
	if err != nil {
		return config.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
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
const projectFile = lock.ProjectFile

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
