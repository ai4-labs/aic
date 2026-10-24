package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ai4-labs/aic/internal/builder"
	"github.com/ai4-labs/aic/internal/executor"
	"github.com/ai4-labs/aic/internal/proto"
	"github.com/ai4-labs/aic/internal/store"
	"github.com/ai4-labs/aic/internal/workspace"
)

// Exit codes of aic run when the command itself didn't run.
const (
	exitRunFailed = 125 // aic or the container could not start the command
	exitNotFound  = 127 // the command was not found
)

// exitStatus ends aic with a code; err, if set, is printed first.
type exitStatus struct {
	code int
	err  error
}

func (e exitStatus) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e exitStatus) Unwrap() error { return e.err }

type runOptions struct {
	interactive bool
	tty         bool
	env         []string
	user        string
	workdir     string
	workspace   string
	copyBack    bool
	network     string
	platform    string
}

func newRunCmd() *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run [flags] IMAGE [COMMAND [ARG...]]",
		Short: "Run a command in a new container",
		Long: `Run a command in a new container.

The container has no network unless --network=host is given, and no
access to host files. With -w, a host directory is copied into the
container before the command runs; the container works on its copy.

With --copy-back, the container's changes to the workspace (new, modified
and deleted files) are copied back to the host directory when it exits,
whatever its exit status. Files edited on the host in the meantime are
kept unless the container changed them too.

aic exits with the command's exit status, 125 if the command could not be
run, or 127 if it was not found.`,
		Example: `  aic run alpine:3.20 echo hello
  aic run -it python:3.12 python
  aic run -w . python:3.12 python -m pytest
  aic run -w . --copy-back python:3.12 ruff format .
  aic run -w .:/src --network=host golang:1.26 go build ./...`,
		Args: args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			if err := runRun(cmd, a[0], a[1:], o); err != nil {
				var es exitStatus
				if errors.As(err, &es) {
					return es
				}
				var ue usageError
				if errors.As(err, &ue) {
					return err
				}
				return exitStatus{exitRunFailed, err}
			}
			return nil
		},
	}
	// Flags after IMAGE belong to the command.
	cmd.Flags().SetInterspersed(false)
	f := cmd.Flags()
	f.BoolVarP(&o.interactive, "interactive", "i", false, "connect standard input to the command")
	f.BoolVarP(&o.tty, "tty", "t", false, "run the command on a terminal")
	f.StringArrayVarP(&o.env, "env", "e", nil, "set an environment variable `KEY[=VALUE]` (KEY alone takes the host's value); repeatable")
	f.StringVarP(&o.user, "user", "u", "", "run as `USER[:GROUP]` (default: the image's user)")
	f.StringVar(&o.workdir, "workdir", "", "working `DIR` in the container (default: the workspace, else the image's)")
	f.StringVarP(&o.workspace, "workspace", "w", "", "copy host `DIR[:PATH]` into the container at PATH (default /workspace)")
	f.BoolVar(&o.copyBack, "copy-back", false, "copy the container's changes to the workspace back to DIR when it exits")
	f.StringVar(&o.network, "network", "none", "network `MODE`: none (isolated) or host")
	f.StringVar(&o.platform, "platform", "", "image platform `OS/ARCH[/VARIANT]` (default linux/<host arch>)")
	return cmd
}

func runRun(cmd *cobra.Command, imageName string, command []string, o runOptions) error {
	if o.network != "none" && o.network != "host" {
		return usagef("invalid --network %q: want none or host", o.network)
	}
	plat, err := parsePlatform(o.platform)
	if err != nil {
		return err
	}
	var hostDir, wsPath string
	if o.copyBack && o.workspace == "" {
		return usagef("--copy-back needs a workspace (-w DIR)")
	}
	if o.workspace != "" {
		hostDir, wsPath, err = parseWorkspace(o.workspace)
		if err != nil {
			return err
		}
	}
	stdin, stdout, stderr := cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	var inFd, outFd int = -1, -1
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		inFd = int(f.Fd())
	}
	if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		outFd = int(f.Fd())
	}
	if o.tty && o.interactive && inFd < 0 {
		return usagef("-t needs a terminal on standard input; drop -t to pipe input")
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	img, err := runImage(cmd, st, imageName, plat)
	if err != nil {
		return err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	cfg := cf.Config
	args := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	if len(command) > 0 {
		args = append(append([]string{}, cfg.Entrypoint...), command...)
	}
	if len(args) == 0 {
		return usagef("%s has no default command; give one after the image name", imageName)
	}
	env := append([]string{}, cfg.Env...)
	for _, e := range o.env {
		if !strings.Contains(e, "=") {
			v, ok := os.LookupEnv(e)
			if !ok {
				continue
			}
			e += "=" + v
		}
		env = append(env, e)
	}
	if o.tty {
		env = terminalEnv(env)
	}
	// The store keeps the image's filesystem unpacked for the next run.
	flat, err := st.Flat(img)
	if err != nil {
		return err
	}
	defer flat.Close()
	spec := executor.Spec{
		Image:   img,
		Flat:    flat,
		Args:    args,
		Env:     env,
		WorkDir: cfg.WorkingDir,
		User:    cfg.User,
		Network: o.network,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	if o.user != "" {
		spec.User = o.user
	}
	var snap workspace.Snapshot
	if hostDir != "" {
		spec.WorkDir = wsPath
		spec.Workspace = &executor.Workspace{
			Path: wsPath,
			Write: func(tw *tar.Writer, prefix string) (err error) {
				snap, err = workspace.Pack(tw, hostDir, prefix)
				return err
			},
		}
	}
	if o.workdir != "" {
		spec.WorkDir = o.workdir
	}
	if o.interactive {
		spec.Stdin = stdin
	}
	// Signing may re-execute aic, so do it before touching the terminal.
	if err := ensureVMAccess(stderr); err != nil {
		return err
	}
	if o.tty {
		spec.TTY = true
		sizeFd := outFd
		if sizeFd < 0 {
			sizeFd = inFd
		}
		if sizeFd >= 0 {
			if w, h, err := term.GetSize(sizeFd); err == nil {
				spec.Size = proto.Size{Rows: uint16(h), Cols: uint16(w)}
			}
			resize, stop := watchResize(sizeFd)
			defer stop()
			spec.Resize = resize
		}
		if o.interactive {
			old, err := term.MakeRaw(inFd)
			if err != nil {
				return err
			}
			defer term.Restore(inFd, old)
		}
	}

	// The executor writes the container's copy of the workspace here.
	var export io.Writer // nil: nothing comes back
	var exportFile *os.File
	if o.copyBack {
		exportFile, err = os.CreateTemp("", "aic-workspace-*.tar")
		if err != nil {
			return err
		}
		defer os.Remove(exportFile.Name())
		defer exportFile.Close()
		export = exportFile
	}

	progress := stderr
	if o.tty && o.interactive {
		progress = crlfWriter{stderr} // the terminal is in raw mode
	}
	opts, err := executorOptions(progress)
	if err != nil {
		return err
	}
	ex, err := executor.NewNative(opts)
	if err != nil {
		return err
	}
	defer ex.Close()
	runErr := ex.Run(cmd.Context(), spec, export)
	var exitErr *executor.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		if strings.Contains(runErr.Error(), "executable file not found") {
			return exitStatus{exitNotFound, runErr}
		}
		return runErr
	}
	if exportFile != nil {
		if _, err := exportFile.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := workspace.Apply(hostDir, exportFile, snap); err != nil {
			return fmt.Errorf("copying the workspace back: %w", err)
		}
	}
	if exitErr != nil {
		return exitStatus{code: exitErr.Code}
	}
	return nil
}

// parseWorkspace splits DIR[:PATH].
func parseWorkspace(s string) (hostDir, ctrPath string, err error) {
	hostDir, ctrPath = s, "/workspace"
	if i := strings.LastIndex(s, ":"); i > 0 && strings.HasPrefix(s[i+1:], "/") {
		hostDir, ctrPath = s[:i], s[i+1:]
	}
	if !path.IsAbs(ctrPath) || path.Clean(ctrPath) == "/" {
		return "", "", usagef("invalid workspace path %q: want an absolute path other than /", ctrPath)
	}
	hostDir, err = filepath.Abs(hostDir)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Stat(hostDir)
	if err != nil {
		return "", "", usageError{err}
	}
	if !fi.IsDir() {
		return "", "", usagef("workspace %s is not a directory", hostDir)
	}
	return hostDir, path.Clean(ctrPath), nil
}

// runImage finds ref in the store, pulling it if it isn't there.
func runImage(cmd *cobra.Command, st *store.Store, ref string, plat v1.Platform) (v1.Image, error) {
	img, err := st.Image(ref)
	if err == nil {
		return img, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	r, perr := name.ParseReference(ref)
	if perr != nil {
		return nil, usageError{perr}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Pulling %s (%s)...\n", builder.DisplayName(r), platformString(plat))
	return st.Pull(cmd.Context(), r, plat)
}

// crlfWriter turns \n into \r\n, for writing to a terminal in raw mode.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// terminalEnv adds the host's terminal type to env, unless the image or
// -e set it: without TERM, programs assume a dumb terminal and don't use
// colors.
func terminalEnv(env []string) []string {
	has := func(k string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, k+"=") {
				return true
			}
		}
		return false
	}
	if !has("TERM") {
		t := os.Getenv("TERM")
		if t == "" || t == "dumb" {
			t = "xterm-256color"
		}
		env = append(env, "TERM="+t)
	}
	if v, ok := os.LookupEnv("COLORTERM"); ok && !has("COLORTERM") {
		env = append(env, "COLORTERM="+v)
	}
	return env
}
