// Package aic builds, runs, pulls and pushes OCI container images. It is
// the library behind the aic command: each command is a method on
// Client.
//
//	c, err := aic.New(aic.Options{Progress: os.Stderr})
//	img, err := c.Build(ctx, aic.BuildOptions{ContextDir: ".", Tags: []string{"app"}})
//	code, err := c.Run(ctx, aic.RunOptions{Image: "app", Command: []string{"make", "test"},
//		Stdout: os.Stdout, Stderr: os.Stderr})
//
// Programs that build images with RUN steps or run containers must call
// Init first thing in main: on Linux, containers are created by
// re-executing the program. On macOS, containers run in a Linux VM that
// requires the com.apple.security.virtualization entitlement; if the
// program lacks it, the first build or run signs the executable (ad hoc)
// and re-executes it.
package aic

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ai4-labs/aic/internal/auth"
	"github.com/ai4-labs/aic/internal/container"
	"github.com/ai4-labs/aic/internal/executor"
	"github.com/ai4-labs/aic/internal/store"
)

// Init must be called at the start of main by programs that run
// containers. In the re-executed container process it never returns.
func Init() { container.Init() }

// ErrInvalid is wrapped by errors caused by invalid arguments, such as a
// malformed image name, as opposed to failures while doing the work.
var ErrInvalid = errors.New("invalid argument")

type invalidError struct{ err error }

func (e invalidError) Error() string         { return e.err.Error() }
func (e invalidError) Unwrap() []error       { return []error{e.err, ErrInvalid} }
func invalidf(format string, a ...any) error { return invalidError{fmt.Errorf(format, a...)} }

// Options configures a Client. The zero value uses the same locations
// and environment variables as the aic command.
type Options struct {
	// StoreDir is the image store (default $AIC_STORE or ~/.aic/store).
	StoreDir string
	// AuthFile holds registry credentials saved by Login (default
	// $AIC_AUTH_FILE or ~/.aic/auth.json). Docker's and podman's
	// credentials are also used for pulling and pushing.
	AuthFile string
	// VMDir holds the macOS VM's kernel and initramfs (default
	// $AIC_VM_DIR, or "vm" next to the store).
	VMDir string
	// VMCPUs and VMMemoryMiB size the macOS VM (default $AIC_VM_CPUS
	// and $AIC_VM_MEMORY, else min(4, CPUs) and 4096 MiB).
	VMCPUs      uint
	VMMemoryMiB uint64
	// Progress receives progress messages: layers being pulled and
	// one-time VM setup. Nil discards them.
	Progress io.Writer
	// UserAgent is sent to registries (default "aic").
	UserAgent string
}

// Client performs aic's operations on one image store.
type Client struct {
	opts  Options
	store *store.Store
}

// New opens (creating if needed) the image store and returns a Client.
func New(o Options) (*Client, error) {
	var err error
	if o.StoreDir == "" {
		if o.StoreDir, err = store.DefaultDir(); err != nil {
			return nil, err
		}
	}
	if o.AuthFile == "" {
		if o.AuthFile, err = auth.DefaultPath(); err != nil {
			return nil, err
		}
	}
	if o.VMDir == "" {
		o.VMDir = os.Getenv("AIC_VM_DIR")
		if o.VMDir == "" {
			o.VMDir = filepath.Join(filepath.Dir(o.StoreDir), "vm")
		}
	}
	if o.VMCPUs == 0 {
		if n, err := strconv.ParseUint(os.Getenv("AIC_VM_CPUS"), 10, 32); err == nil {
			o.VMCPUs = uint(n)
		}
	}
	if o.VMMemoryMiB == 0 {
		if n, err := strconv.ParseUint(os.Getenv("AIC_VM_MEMORY"), 10, 64); err == nil {
			o.VMMemoryMiB = n
		}
	}
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	if o.UserAgent == "" {
		o.UserAgent = "aic"
	}
	st, err := store.Open(o.StoreDir)
	if err != nil {
		return nil, err
	}
	st.UserAgent = o.UserAgent
	st.Keychain = auth.Keychain(o.AuthFile)
	st.Progress = o.Progress
	return &Client{opts: o, store: st}, nil
}

// StoreDir, AuthFile and VMDir report the locations the Client uses.
func (c *Client) StoreDir() string { return c.opts.StoreDir }
func (c *Client) AuthFile() string { return c.opts.AuthFile }
func (c *Client) VMDir() string    { return c.opts.VMDir }

// newExecutor returns the native executor (rootless containers on Linux,
// a VM on macOS), reporting one-time setup to progress.
func (c *Client) newExecutor(progress io.Writer) (executor.Executor, error) {
	return executor.NewNative(executor.Options{
		Dir:       c.opts.VMDir,
		CPUs:      c.opts.VMCPUs,
		MemoryMiB: c.opts.VMMemoryMiB,
		Progress:  progress,
	})
}
