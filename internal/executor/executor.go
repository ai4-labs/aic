// Package executor runs RUN instructions.
//
// An Executor receives the image built so far, runs a command in a
// container created from it, and returns the container's root filesystem
// as a tar stream. The builder diffs that filesystem against the image to
// produce the new layer, so executors never need to know about layers.
//
// The native executor needs no container runtime: on Linux it uses
// rootless namespaces (package container); on macOS it runs the same code
// inside a lightweight Linux VM (package vm).
package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/rakyll/ai-container/internal/proto"
	"github.com/rakyll/ai-container/internal/rootfs"
)

// Spec describes one RUN.
type Spec struct {
	Image   v1.Image
	Args    []string // full argv, already wrapped in the shell for shell form
	Env     []string // KEY=VALUE
	WorkDir string
	User    string
	// Network is "host" (share the host's network; on macOS, the VM's)
	// or "none" (isolated, loopback only). Empty means "host".
	Network        string
	Hostname       string
	Stdout, Stderr io.Writer

	// Stdin, if set, is connected to the command's standard input.
	Stdin io.Reader
	// TTY runs the command on a terminal of size Size; Resize delivers
	// later sizes.
	TTY    bool
	Size   proto.Size
	Resize <-chan proto.Size

	// Workspace, if set, is copied into the container before the
	// command runs. Afterwards only the workspace, rather than the root
	// filesystem, is written to Run's rootfs writer, with names
	// relative to it; this happens whatever the command's exit status.
	Workspace *Workspace
}

// Workspace is a directory copied into a container.
type Workspace struct {
	Path string // absolute path in the container
	// Write writes the directory's contents to tw, with names prefixed
	// by prefix ("workspace/", for Path "/workspace").
	Write func(tw *tar.Writer, prefix string) error
}

// Executor runs commands in containers.
type Executor interface {
	// Run executes spec and writes the container's root filesystem, as
	// an uncompressed tar stream, to rootfs.
	Run(ctx context.Context, spec Spec, rootfs io.Writer) error
	// Close releases resources such as a VM.
	Close() error
}

// ExitError reports that the command ran and exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ErrUnavailable is returned when RUN cannot be executed on this host.
var ErrUnavailable = errors.New("RUN is not supported on this platform")

// runFunc is the shape of container.Run and vm.Machine.Run.
type runFunc func(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error)

// run adapts a runFunc to Executor.Run.
func run(ctx context.Context, fn runFunc, spec Spec, rootfs io.Writer) error {
	network := "host"
	if spec.Network == "none" {
		network = "none"
	}
	ps := proto.Spec{
		Args:     spec.Args,
		Env:      spec.Env,
		WorkDir:  spec.WorkDir,
		User:     spec.User,
		Network:  network,
		Hostname: spec.Hostname,
		Stdin:    spec.Stdin != nil,
		TTY:      spec.TTY,
		Size:     spec.Size,
	}
	rio := proto.IO{
		Stdin:  spec.Stdin,
		Resize: spec.Resize,
		Stdout: spec.Stdout,
		Stderr: spec.Stderr,
		Export: rootfs,
	}
	if rio.Stdout == nil {
		rio.Stdout = io.Discard
	}
	if rio.Stderr == nil {
		rio.Stderr = io.Discard
	}
	// mutate.Extract flattens the layers and applies whiteouts.
	var image io.ReadCloser = mutate.Extract(spec.Image)
	defer image.Close()
	if ws := spec.Workspace; ws != nil {
		dir, err := workspaceDir(ws.Path)
		if err != nil {
			return err
		}
		ps.Export, ps.Chown = "/"+dir, "/"+dir
		wr := withWorkspace(image, dir, ws.Write)
		res, err := fn(ctx, ps, wr, rio)
		if werr := wr.Close(); werr != nil {
			return werr // the cause of whatever the container reported
		}
		return result(res, err)
	}
	return result(fn(ctx, ps, image, rio))
}

func result(res proto.Result, err error) error {
	if err != nil {
		return err
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	if res.ExitCode != 0 {
		return &ExitError{Code: res.ExitCode}
	}
	return nil
}

func workspaceDir(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("workspace path %q is not absolute", p)
	}
	dir, err := rootfs.CleanName(p)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", errors.New("the workspace can't be the root directory")
	}
	return dir, nil
}

// workspaceReader is the image tar with a workspace added.
type workspaceReader struct {
	*io.PipeReader
	done chan struct{}
	err  error // from copying the workspace
}

// Close stops the copy and waits for it, returning its error if
// copying the workspace failed.
func (w *workspaceReader) Close() error {
	w.PipeReader.Close()
	<-w.done
	return w.err
}

// withWorkspace returns the image tar with the image's entries under dir
// replaced by the workspace.
func withWorkspace(image io.Reader, dir string, write func(*tar.Writer, string) error) *workspaceReader {
	pr, pw := io.Pipe()
	wr := &workspaceReader{PipeReader: pr, done: make(chan struct{})}
	go func() {
		defer close(wr.done)
		pw.CloseWithError(func() error {
			tr := tar.NewReader(image)
			tw := tar.NewWriter(pw)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				name, err := rootfs.CleanName(hdr.Name)
				if err != nil {
					return err
				}
				if name == dir || strings.HasPrefix(name, dir+"/") {
					continue
				}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if _, err := io.Copy(tw, tr); err != nil {
					return err
				}
			}
			if err := write(tw, dir+"/"); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					wr.err = fmt.Errorf("copying the workspace: %w", err)
				}
				return err
			}
			return tw.Close()
		}())
	}()
	return wr
}
