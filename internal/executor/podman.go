package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Podman runs RUN steps with podman. On macOS this uses the podman
// machine VM; on Linux it uses the local podman installation.
//
// For each RUN it loads the current image into podman, creates and starts
// a container, exports the container's filesystem and removes both again.
type Podman struct {
	bin string
}

// NewPodman finds podman on PATH.
func NewPodman() (*Podman, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("%w: podman not found on PATH (install podman; on macOS also run `podman machine init && podman machine start`)", ErrUnavailable)
	}
	return &Podman{bin: bin}, nil
}

func (p *Podman) Name() string { return "podman" }

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// podman runs a podman subcommand and returns its stdout. stderr is
// included in the error.
func (p *Podman) podman(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("podman %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}

func (p *Podman) Run(ctx context.Context, spec Spec, rootfs io.Writer) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	tag, err := name.NewTag("localhost/aic-build-" + id + ":run")
	if err != nil {
		return err
	}
	cleanup := context.WithoutCancel(ctx)

	// Load the image. The archive is streamed, never written to disk.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarball.Write(tag, spec.Image, pw)) }()
	_, err = p.podman(ctx, pr, "load", "--quiet")
	pr.CloseWithError(errors.New("podman load exited"))
	if err != nil {
		return err
	}
	defer p.podman(cleanup, nil, "rmi", "--force", tag.String())

	entrypoint, err := json.Marshal(spec.Args)
	if err != nil {
		return err
	}
	workdir := spec.WorkDir
	if workdir == "" {
		workdir = "/"
	}
	network := spec.Network
	if network == "" || network == "default" {
		network = "" // podman's default network
	}
	ctr := "aic-build-" + id
	args := []string{
		"create", "--pull=never", "--name", ctr,
		"--entrypoint", string(entrypoint),
		"--workdir", workdir,
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	for _, kv := range spec.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, tag.String())
	if _, err := p.podman(ctx, nil, args...); err != nil {
		return err
	}
	defer p.podman(cleanup, nil, "rm", "--force", "--volumes", ctr)

	start := exec.CommandContext(ctx, p.bin, "start", "--attach", ctr)
	start.Stdout, start.Stderr = spec.Stdout, spec.Stderr
	if err := start.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			return &ExitError{Code: ee.ExitCode()}
		}
		return fmt.Errorf("podman start: %w", err)
	}

	export := exec.CommandContext(ctx, p.bin, "export", ctr)
	export.Stdout = rootfs
	var stderr bytes.Buffer
	export.Stderr = &stderr
	if err := export.Run(); err != nil {
		return fmt.Errorf("podman export: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
