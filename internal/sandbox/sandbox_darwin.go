package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// sandboxExec applies a Seatbelt profile and then execs the command, so the
// sandboxed process replaces it (same pid). Always invoked by absolute path.
const sandboxExec = "/usr/bin/sandbox-exec"

// darwinBackend runs native Mach-O processes under a generated Seatbelt
// (SBPL) profile that starts from (deny default).
type darwinBackend struct{}

func newBackend() (Backend, error) { return darwinBackend{}, nil }

func (darwinBackend) Name() string { return "darwin-seatbelt" }

// baseProfile is the fixed part of every profile. Paths granted per run
// are appended as (param "...") references, so no host path is ever spliced
// into SBPL source.
const baseProfile = `(version 1)
(deny default)

; Process basics. Signals only within this sandbox.
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read)

; stat() on any path. dyld, shells and path resolution need it. It reveals
; whether a path exists, never its contents.
; TODO(security): narrow file-read-metadata to the paths actually needed.
(allow file-read-metadata)

; Read-only OS files needed to load and run system binaries.
(allow file-read*
  (literal "/")
  (subpath "/bin") (subpath "/sbin") (subpath "/usr")
  (subpath "/System") (subpath "/Library/Apple")
  (subpath "/private/var/db/dyld") (subpath "/private/var/db/timezone")
  (literal "/private/etc/localtime")
  (literal "/dev/null") (literal "/dev/zero")
  (literal "/dev/random") (literal "/dev/urandom")
  (literal "/dev/tty") (subpath "/dev/fd") (literal "/dev/dtracehelper"))
(allow file-write-data
  (literal "/dev/null") (literal "/dev/zero")
  (literal "/dev/tty") (literal "/dev/dtracehelper"))
(allow file-ioctl (literal "/dev/tty") (literal "/dev/dtracehelper"))

; No network rules: all network access is denied.
`

// profile returns the SBPL source and the -D parameters it references.
// reads and writes must be real (symlink-free) absolute paths, because
// Seatbelt matches against resolved vnode paths.
func profile(reads, writes []string) (string, []string) {
	var b strings.Builder
	b.WriteString(baseProfile)
	var params []string
	for i, p := range reads {
		name := fmt.Sprintf("R%d", i)
		fmt.Fprintf(&b, "(allow file-read* (subpath (param %q)))\n", name)
		params = append(params, name+"="+p)
	}
	for i, p := range writes {
		name := fmt.Sprintf("W%d", i)
		fmt.Fprintf(&b, "(allow file-read* file-write* (subpath (param %q)))\n", name)
		params = append(params, name+"="+p)
	}
	return b.String(), params
}

// unsupported rejects config features this backend cannot enforce yet, so
// that a run never silently gets weaker isolation than the operator asked
// for.
func unsupported(c *Spec) error {
	switch {
	case len(c.Config.Network.Egress) > 0:
		return fmt.Errorf("%w: network egress needs the egress proxy (milestone M5); remove network.egress from the config", ErrUnsupported)
	case c.Config.MaxMemory > 0:
		// TODO(security): enforce via a supervisor polling proc_pid_rusage.
		return fmt.Errorf("%w: maxMemory is not enforced on macOS yet; remove it from the config", ErrUnsupported)
	case c.Config.MaxProcs > 0:
		// TODO(security): RLIMIT_NPROC is per-user on macOS; needs a supervisor.
		return fmt.Errorf("%w: maxProcs is not enforced on macOS yet; remove it from the config", ErrUnsupported)
	}
	return nil
}

func (darwinBackend) Run(ctx context.Context, spec Spec) (int, error) {
	if err := spec.validate(); err != nil {
		return -1, err
	}
	if err := unsupported(&spec); err != nil {
		return -1, err
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return -1, fmt.Errorf("%w: %s not available: %v", ErrUnsupported, sandboxExec, err)
	}

	root, err := filepath.EvalSymlinks(spec.Root)
	if err != nil {
		return -1, err
	}
	reads := []string{root}
	var writes []string
	for _, m := range spec.Mounts {
		src, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			return -1, fmt.Errorf("sandbox: mount %s: %w", m.Source, err)
		}
		if m.Target != "" {
			if err := linkInto(root, m.Target, src); err != nil {
				return -1, err
			}
		}
		if m.Writable {
			writes = append(writes, src)
		} else {
			reads = append(reads, src)
		}
	}

	prof, params := profile(reads, writes)
	args := make([]string, 0, 2*len(params)+3+len(spec.Cmd))
	for _, p := range params {
		args = append(args, "-D", p)
	}
	args = append(args, "-p", prof, "--")
	args = append(args, spec.Cmd...)

	cmd := exec.Command(sandboxExec, args...)
	cmd.Dir = filepath.Join(root, spec.WorkDir)
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr

	// Non-interactive runs get their own process group so the whole tree
	// can be signalled and cleaned up. Interactive runs stay in the
	// terminal's foreground group so job control and Ctrl-C work.
	// TODO(security): reap descendants of interactive runs too (kqueue
	// EVFILT_PROC supervisor, see design doc).
	interactive := isTerminal(spec.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !interactive}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("sandbox: start: %w", err)
	}
	pid := cmd.Process.Pid
	kill := func(sig syscall.Signal) {
		if interactive {
			_ = syscall.Kill(pid, sig)
		} else {
			_ = syscall.Kill(-pid, sig)
		}
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				kill(syscall.SIGKILL)
				return
			case s := <-sigs:
				// In interactive mode the terminal already delivered
				// SIGINT to the child's (shared) process group.
				if !interactive || s != os.Interrupt {
					kill(s.(syscall.Signal))
				}
			}
		}
	}()

	err = cmd.Wait()
	close(done)
	if !interactive {
		// Kill anything the process left behind in its group.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return exitCode(err)
}

// linkInto makes root/target point at src unless root/target already
// resolves (for example through a parent mount's symlink).
func linkInto(root, target, src string) error {
	dst := filepath.Join(root, target)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(src, dst)
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return -1, err
}

// isTerminal reports whether r is an *os.File connected to a terminal.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
