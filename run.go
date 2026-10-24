package aic

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/ai4-labs/aic/internal/builder"
	"github.com/ai4-labs/aic/internal/executor"
	"github.com/ai4-labs/aic/internal/proto"
	"github.com/ai4-labs/aic/internal/store"
	"github.com/ai4-labs/aic/internal/workspace"
)

// ErrCommandNotFound is wrapped by Run's error when the command does not
// exist in the image.
var ErrCommandNotFound = errors.New("command not found")

// TerminalSize is a terminal's size in characters.
type TerminalSize = proto.Size

// RunOptions configures Run.
type RunOptions struct {
	// Image is a local image name, digest or ID; images not in the store
	// are pulled.
	Image string
	// Command replaces the image's CMD (the ENTRYPOINT is kept).
	Command []string
	// Env adds KEY=VALUE variables to the image's.
	Env []string
	// User and WorkDir override the image's.
	User, WorkDir string
	// Network is "none" (the default: loopback only) or "host" (the
	// host's network; on macOS, the VM's).
	Network string
	// Platform is the image platform to pull (default linux/<host arch>).
	Platform string

	// Workspace is a host directory copied into the container at
	// WorkspacePath (default /workspace), which becomes the working
	// directory unless WorkDir is set.
	Workspace, WorkspacePath string
	// CopyBack copies the container's changes to the workspace (new,
	// modified and deleted files) back to Workspace when the command
	// exits, whatever its exit status. Files changed on the host in
	// the meantime are kept unless the container changed them too.
	CopyBack bool

	// Stdin, if set, is connected to the command's standard input.
	Stdin io.Reader
	// Stdout and Stderr receive the command's output; nil discards.
	Stdout, Stderr io.Writer
	// TTY runs the command on a terminal of size Size; Resize delivers
	// later sizes. The terminal's output goes to Stdout.
	TTY    bool
	Size   TerminalSize
	Resize <-chan TerminalSize
	// Progress receives messages about pulling the image and one-time
	// VM setup (default: the Client's Progress).
	Progress io.Writer
}

// Run runs a command in a new container and returns its exit status. The
// error is set if the command could not be run; it wraps
// ErrCommandNotFound if the command does not exist.
func (c *Client) Run(ctx context.Context, o RunOptions) (int, error) {
	if o.Network == "" {
		o.Network = "none"
	}
	if o.Network != "none" && o.Network != "host" {
		return 0, invalidf("invalid network %q: want none or host", o.Network)
	}
	plat, err := ParsePlatform(o.Platform)
	if err != nil {
		return 0, err
	}
	if o.CopyBack && o.Workspace == "" {
		return 0, invalidf("copying back needs a workspace")
	}
	var hostDir, wsPath string
	if o.Workspace != "" {
		if hostDir, wsPath, err = checkWorkspace(o.Workspace, o.WorkspacePath); err != nil {
			return 0, err
		}
	}
	progress := o.Progress
	if progress == nil {
		progress = c.opts.Progress
	}

	img, err := c.image(ctx, o.Image, plat, progress)
	if err != nil {
		return 0, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return 0, err
	}
	cfg := cf.Config
	args := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	if len(o.Command) > 0 {
		args = append(append([]string{}, cfg.Entrypoint...), o.Command...)
	}
	if len(args) == 0 {
		return 0, invalidf("%s has no default command; give one after the image name", o.Image)
	}
	env := append(append([]string{}, cfg.Env...), o.Env...)
	if o.TTY {
		env = terminalEnv(env)
	}
	// The store keeps the image's filesystem unpacked for the next run.
	flat, err := c.store.Flat(img)
	if err != nil {
		return 0, err
	}
	defer flat.Close()
	spec := executor.Spec{
		Image:   img,
		Flat:    flat,
		Args:    args,
		Env:     env,
		WorkDir: cfg.WorkingDir,
		User:    cfg.User,
		Network: o.Network,
		Stdin:   o.Stdin,
		Stdout:  orDiscard(o.Stdout),
		Stderr:  orDiscard(o.Stderr),
		TTY:     o.TTY,
		Size:    o.Size,
		Resize:  o.Resize,
	}
	if o.User != "" {
		spec.User = o.User
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
	if o.WorkDir != "" {
		spec.WorkDir = o.WorkDir
	}

	// The executor writes the container's copy of the workspace here.
	var export io.Writer // nil: nothing comes back
	var exportFile *os.File
	if o.CopyBack {
		if exportFile, err = os.CreateTemp("", "aic-workspace-*.tar"); err != nil {
			return 0, err
		}
		defer os.Remove(exportFile.Name())
		defer exportFile.Close()
		export = exportFile
	}

	ex, err := c.newExecutor(progress)
	if err != nil {
		return 0, err
	}
	defer ex.Close()
	runErr := ex.Run(ctx, spec, export)
	var exitErr *executor.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		if strings.Contains(runErr.Error(), "executable file not found") {
			return 0, fmt.Errorf("%w: %w", ErrCommandNotFound, runErr)
		}
		return 0, runErr
	}
	if exportFile != nil {
		if _, err := exportFile.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if _, err := workspace.Apply(hostDir, exportFile, snap); err != nil {
			return 0, fmt.Errorf("copying the workspace back: %w", err)
		}
	}
	if exitErr != nil {
		return exitErr.Code, nil
	}
	return 0, nil
}

// checkWorkspace validates the host directory and container path.
func checkWorkspace(dir, ctrPath string) (string, string, error) {
	if ctrPath == "" {
		ctrPath = "/workspace"
	}
	if !path.IsAbs(ctrPath) || path.Clean(ctrPath) == "/" {
		return "", "", invalidf("invalid workspace path %q: want an absolute path other than /", ctrPath)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", "", invalidError{err}
	}
	if !fi.IsDir() {
		return "", "", invalidf("workspace %s is not a directory", dir)
	}
	return dir, path.Clean(ctrPath), nil
}

// image finds ref in the store, pulling it if it isn't there.
func (c *Client) image(ctx context.Context, ref string, plat v1.Platform, progress io.Writer) (v1.Image, error) {
	img, err := c.store.Image(ref)
	if err == nil {
		return img, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	r, perr := name.ParseReference(ref)
	if perr != nil {
		return nil, invalidError{perr}
	}
	fmt.Fprintf(progress, "Pulling %s (%s)...\n", builder.DisplayName(r), PlatformString(plat))
	return c.pullWith(ctx, r, plat, progress)
}

// terminalEnv adds the host's terminal type to env, unless the image or
// the caller set it: without TERM, programs assume a dumb terminal and
// don't use colors.
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
