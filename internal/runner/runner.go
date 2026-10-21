// Package runner runs a project's entrypoint in the platform sandbox.
//
// Until content-addressed bundles exist (milestone M1), aic run works in
// development mode: it reads aic.yaml from a project directory, copies the
// files matched by include into a fresh container root, and hands the
// sandbox backend a view assembled from that root, the operator's config,
// and an optional workspace directory.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rakyll/ai-container/internal/config"
	"github.com/rakyll/ai-container/internal/manifest"
	"github.com/rakyll/ai-container/internal/project"
	"github.com/rakyll/ai-container/internal/sandbox"
)

// systemPath is the minimal PATH every container starts with.
const systemPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// Container layout, relative to the root.
const (
	appDir       = "app"
	scratchDir   = "scratch"
	workspaceDir = "workspace"
)

// ErrUnsupported marks projects or options this runner cannot run yet.
var ErrUnsupported = errors.New("not supported yet")

// Options configures one run.
type Options struct {
	ProjectDir string
	Config     config.Config
	// Workspace is an optional host directory exposed at
	// $AIC_ROOT/workspace (read-only unless the config grants writes).
	Workspace string
	// Cmd overrides the entrypoint command when non-empty.
	Cmd []string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Backend overrides the platform sandbox (tests).
	Backend sandbox.Backend
}

// Run executes the project's entrypoint (or opts.Cmd) in the sandbox and
// returns its exit code. The container root is deleted afterwards.
func Run(ctx context.Context, opts Options) (int, error) {
	p, err := project.Load(opts.ProjectDir)
	if err != nil {
		return -1, err
	}
	if len(p.Toolchains) > 0 {
		return -1, fmt.Errorf("%w: toolchains (%s) arrive in milestone M4; aic run currently supports projects without toolchains",
			ErrUnsupported, strings.Join(p.Toolchains, ", "))
	}
	if host := manifest.HostPlatform(); !p.Supports(host) {
		return -1, fmt.Errorf("project does not list %s in targets", host)
	}
	if err := opts.Config.Validate(); err != nil {
		return -1, err
	}

	backend := opts.Backend
	if backend == nil {
		if backend, err = sandbox.New(); err != nil {
			return -1, err
		}
	}

	tmp, err := os.MkdirTemp("", "aic-run-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(tmp)
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return -1, err
	}

	if err := stage(p, filepath.Join(root, appDir)); err != nil {
		return -1, err
	}
	scratch := filepath.Join(root, scratchDir)
	home, tmpdir := filepath.Join(scratch, "home"), filepath.Join(scratch, "tmp")
	for _, d := range []string{home, tmpdir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return -1, err
		}
	}

	mounts := []sandbox.Mount{{Source: scratch, Writable: true}}
	var workspace string
	if opts.Workspace != "" {
		workspace, err = realDir(opts.Workspace)
		if err != nil {
			return -1, fmt.Errorf("workspace: %w", err)
		}
		mounts = append(mounts, sandbox.Mount{Source: workspace, Target: workspaceDir})
	}
	writable, err := writableMounts(opts.Config, scratch, workspace)
	if err != nil {
		return -1, err
	}
	mounts = append(mounts, writable...)

	cmd := p.Entrypoint.Cmd
	if len(opts.Cmd) > 0 {
		cmd = opts.Cmd
	}
	workdir := p.Entrypoint.WorkDir
	if fi, err := os.Stat(filepath.Join(root, workdir)); err != nil || !fi.IsDir() {
		return -1, fmt.Errorf("entrypoint.workdir %q does not exist in the container (is it covered by include?)", workdir)
	}

	env := []string{
		"PATH=" + systemPath,
		"HOME=" + home,
		"TMPDIR=" + tmpdir,
		"AIC_ROOT=" + root,
		"AIC_SCRATCH=" + scratch,
	}
	if workspace != "" {
		env = append(env, "AIC_WORKSPACE="+filepath.Join(root, workspaceDir))
	}
	keys := make([]string, 0, len(p.Entrypoint.Env))
	for k := range p.Entrypoint.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+p.Entrypoint.Env[k])
	}

	return backend.Run(ctx, sandbox.Spec{
		Root:    root,
		Mounts:  mounts,
		Cmd:     cmd,
		Env:     env,
		WorkDir: workdir,
		Config:  opts.Config,
		Stdin:   opts.Stdin,
		Stdout:  opts.Stdout,
		Stderr:  opts.Stderr,
	})
}

// stage copies every regular file in the project matched by include into
// dst, preserving the executable bit. Symlinks and special files are
// rejected rather than followed, so nothing outside the project can be
// pulled into the container.
func stage(p *project.Project, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(p.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(p.Dir, path)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !p.Included(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("include: %s is not a regular file (symlinks and special files are not supported)", name)
		}
		mode := fs.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return copyFile(path, filepath.Join(dst, rel), mode)
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// realDir resolves dir to an absolute, symlink-free directory path.
func realDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return real, nil
}

// writableMounts turns config writable entries into writable mounts. Each
// path is created if needed and must resolve, after following symlinks, to
// a location inside its base ($SCRATCH or $WORKSPACE), so a symlink in the
// workspace cannot redirect a write grant elsewhere on the host.
func writableMounts(c config.Config, scratch, workspace string) ([]sandbox.Mount, error) {
	var mounts []sandbox.Mount
	for _, w := range c.Filesystem.Writable {
		v, rel, err := config.SplitWritable(w)
		if err != nil {
			return nil, err
		}
		base, target := scratch, filepath.Join(scratchDir, filepath.FromSlash(rel))
		if v == config.VarWorkspace {
			if workspace == "" {
				return nil, fmt.Errorf("config grants %s, but no --workspace was given", w)
			}
			base, target = workspace, filepath.Join(workspaceDir, filepath.FromSlash(rel))
		}
		real, err := ensureWithin(base, filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("config writable %s: %w", w, err)
		}
		mounts = append(mounts, sandbox.Mount{Source: real, Target: target, Writable: true})
	}
	return mounts, nil
}

// ensureWithin creates dir (and parents) and returns its real path,
// checking before and after creation that it stays inside base. The check
// before creation keeps MkdirAll from following a symlink out of base.
func ensureWithin(base, dir string) (string, error) {
	existing := dir
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("%s does not exist", dir)
		}
		existing = parent
	}
	if err := within(base, existing); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := within(base, dir); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

func within(base, p string) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	if real != base && !strings.HasPrefix(real, base+string(filepath.Separator)) {
		return fmt.Errorf("resolves to %s, outside %s", real, base)
	}
	return nil
}
