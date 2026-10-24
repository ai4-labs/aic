package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ai4-labs/aic"
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
				if errors.As(err, &ue) || errors.Is(err, aic.ErrInvalid) {
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
	var hostDir, wsPath string
	if o.copyBack && o.workspace == "" {
		return usagef("--copy-back needs a workspace (-w DIR)")
	}
	if o.workspace != "" {
		var err error
		if hostDir, wsPath, err = parseWorkspace(o.workspace); err != nil {
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
	var env []string
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
	ro := aic.RunOptions{
		Image:         imageName,
		Command:       command,
		Env:           env,
		User:          o.user,
		WorkDir:       o.workdir,
		Network:       o.network,
		Platform:      o.platform,
		Workspace:     hostDir,
		WorkspacePath: wsPath,
		CopyBack:      o.copyBack,
		Stdout:        stdout,
		Stderr:        stderr,
		TTY:           o.tty,
		Progress:      stderr,
	}
	if o.interactive {
		ro.Stdin = stdin
	}
	// Signing may re-execute aic, so do it before touching the terminal.
	if err := ensureVMAccess(stderr); err != nil {
		return err
	}
	if o.tty {
		sizeFd := outFd
		if sizeFd < 0 {
			sizeFd = inFd
		}
		if sizeFd >= 0 {
			if w, h, err := term.GetSize(sizeFd); err == nil {
				ro.Size = aic.TerminalSize{Rows: uint16(h), Cols: uint16(w)}
			}
			resize, stop := watchResize(sizeFd)
			defer stop()
			ro.Resize = resize
		}
		if o.interactive {
			ro.Progress = crlfWriter{stderr} // the terminal is in raw mode
			old, err := term.MakeRaw(inFd)
			if err != nil {
				return err
			}
			defer term.Restore(inFd, old)
		}
	}
	c, err := newClient(stderr)
	if err != nil {
		return err
	}
	code, err := c.Run(cmd.Context(), ro)
	switch {
	case errors.Is(err, aic.ErrCommandNotFound):
		return exitStatus{exitNotFound, err}
	case err != nil:
		return err
	case code != 0:
		return exitStatus{code: code}
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

// crlfWriter turns \n into \r\n, for writing to a terminal in raw mode.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}
