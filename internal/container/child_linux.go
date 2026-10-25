//go:build linux

package container

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ai4-labs/aic/internal/proto"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// child runs inside the new namespaces, as PID 1.
func child() proto.Result {
	fail := func(stage string, err error) proto.Result {
		return proto.Result{Error: fmt.Sprintf("%s: %v", stage, err)}
	}
	var cfg childConfig
	if env := os.Getenv(configEnv); env != "" {
		if err := json.Unmarshal([]byte(env), &cfg); err != nil {
			return fail("reading config", err)
		}
	} else {
		if err := json.NewDecoder(os.NewFile(fdConfig, "config")).Decode(&cfg); err != nil {
			return fail("reading config", err)
		}
		if cfg.Reexec {
			// Now that we are mapped to root, exec again to get the
			// capabilities of root in the user namespace. The image,
			// export and status descriptors are inherited.
			b, err := json.Marshal(cfg)
			if err != nil {
				return fail("re-executing", err)
			}
			self, err := os.Readlink("/proc/self/exe")
			if err != nil {
				return fail("re-executing", err)
			}
			err = unix.Exec(self, os.Args, []string{childEnv + "=1", configEnv + "=" + string(b)})
			return fail("re-executing", err)
		}
	}
	image := os.NewFile(fdImage, "image")
	export := os.NewFile(fdExport, "export")
	defer export.Close()

	if err := setupRoot(cfg); err != nil {
		return fail("setting up the root filesystem", err)
	}
	st, err := extract(image)
	image.Close()
	if err != nil {
		return fail("extracting the image", err)
	}
	if err := setupRuntime(cfg); err != nil {
		return fail("setting up the container", err)
	}
	code, err := runCommand(cfg)
	if err != nil {
		return fail("running the command", err)
	}
	if cfg.Spec.Export != "" {
		// A directory is sent back whatever the command's outcome. If it
		// is gone, nothing is sent rather than an empty directory, which
		// would read as every file having been deleted.
		if fi, err := os.Lstat(cfg.Spec.Export); err != nil || !fi.IsDir() {
			return fail("exporting "+cfg.Spec.Export, errors.New("the directory was removed or replaced"))
		}
		if err := exportTree(export, st, cfg.Spec.Export); err != nil {
			return fail("exporting "+cfg.Spec.Export, err)
		}
		return proto.Result{ExitCode: code}
	}
	if code != 0 || cfg.Spec.NoExport {
		return proto.Result{ExitCode: code}
	}
	if err := exportTree(export, st, "/"); err != nil {
		return fail("exporting the filesystem", err)
	}
	return proto.Result{}
}

func mount(src, dst, fstype string, flags uintptr, data string) error {
	if err := unix.Mount(src, dst, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", fstype+src, dst, err)
	}
	return nil
}

// setupRoot mounts a tmpfs as the new root, populates /dev, and pivots
// into it. Afterwards the host filesystem is no longer reachable.
func setupRoot(cfg childConfig) error {
	root := cfg.Rootfs
	if err := mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	opts := "mode=0755"
	if cfg.RootfsSize != "" {
		opts += ",size=" + cfg.RootfsSize
	}
	if err := mount("tmpfs", root, "tmpfs", 0, opts); err != nil {
		return err
	}
	dev := filepath.Join(root, "dev")
	for _, d := range []string{dev, filepath.Join(root, "proc"), filepath.Join(root, "sys")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	if err := mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=0755,size=65536k"); err != nil {
		return err
	}
	// Device nodes can't be created in a user namespace; bind the host's.
	for _, n := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		p := filepath.Join(dev, n)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		f.Close()
		if err := mount("/dev/"+n, p, "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	for _, d := range []string{"pts", "shm"} {
		if err := os.Mkdir(filepath.Join(dev, d), 0o755); err != nil {
			return err
		}
	}
	if err := mount("devpts", filepath.Join(dev, "pts"), "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return err
	}
	if err := mount("shm", filepath.Join(dev, "shm"), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=65536k"); err != nil {
		return err
	}
	for name, target := range map[string]string{
		"ptmx": "pts/ptmx", "fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
			return err
		}
	}

	// Mount /proc and /sys while the host's are still visible: in a user
	// namespace the kernel only allows a new procfs or sysfs if one is
	// already fully visible in the mount namespace.
	if err := mountProc(filepath.Join(root, "proc")); err != nil {
		return err
	}
	if cfg.Spec.Network != "host" {
		// sysfs can only be mounted by the owner of the network
		// namespace. It is optional for builds, so failure is not fatal.
		_ = unix.Mount("sysfs", filepath.Join(root, "sys"), "sysfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	}

	old := filepath.Join(root, ".aic-old-root")
	if err := os.Mkdir(old, 0o700); err != nil {
		return err
	}
	err := unix.PivotRoot(root, old)
	if err == nil {
		if err := unix.Chdir("/"); err != nil {
			return err
		}
		if err := unix.Unmount("/.aic-old-root", unix.MNT_DETACH); err != nil {
			return fmt.Errorf("unmounting the old root: %w", err)
		}
		return os.Remove("/.aic-old-root")
	}
	if !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// pivot_root is not possible when the current root is the initramfs
	// (as in aic's VM). Move the new root over / and chroot instead.
	if err := os.Remove(old); err != nil {
		return err
	}
	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := mount(root, "/", "", unix.MS_MOVE, ""); err != nil {
		return err
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return unix.Chdir("/")
}

// setupRuntime mounts /proc (and /sys), configures the network and writes
// the files runtimes manage in /etc. Those paths are never committed.
func setupRuntime(cfg childConfig) error {
	if cfg.Spec.Network != "host" {
		if err := loopbackUp(); err != nil {
			return fmt.Errorf("bringing up loopback: %w", err)
		}
	}
	hostname := cfg.Spec.Hostname
	if hostname == "" {
		hostname = "aic"
	}
	if err := unix.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if fi, err := os.Stat("/etc"); err != nil || !fi.IsDir() {
		return nil // e.g. FROM scratch: nothing to configure
	}
	files := map[string]string{
		"/etc/hosts":       "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + hostname + "\n",
		"/etc/hostname":    hostname + "\n",
		"/etc/resolv.conf": cfg.ResolvConf,
	}
	for p, content := range files {
		// Replace rather than write through: the image may have a symlink here.
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// mountProc mounts a procfs for the container's PID namespace at dst. Some
// hosts still refuse that in a user namespace (e.g. when parts of the
// host's /proc are covered); then the host's /proc is bound instead, which
// shows host processes but works for builds.
func mountProc(dst string) error {
	err := unix.Mount("proc", dst, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	if err == nil || !errors.Is(err, unix.EPERM) {
		if err != nil {
			return fmt.Errorf("mount proc on %s: %w", dst, err)
		}
		return nil
	}
	if berr := unix.Mount("/proc", dst, "", unix.MS_BIND|unix.MS_REC, ""); berr != nil {
		return fmt.Errorf("mount proc on %s: %v (binding the host's /proc: %v)", dst, err, berr)
	}
	return nil
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// runCommand runs the command as the configured user and returns its
// exit code.
func runCommand(cfg childConfig) (int, error) {
	spec := cfg.Spec
	if len(spec.Args) == 0 {
		return 0, errors.New("no command")
	}
	u, err := lookupUser(spec.User)
	if err != nil {
		return 0, err
	}
	env := append([]string(nil), spec.Env...)
	path := defaultPath
	hasHome := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
		hasHome = hasHome || strings.HasPrefix(kv, "HOME=")
	}
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH="+path)
	}
	if !hasHome {
		env = append(env, "HOME="+u.home)
	}
	env = append(env, "HOSTNAME="+mustHostname())

	workdir := spec.WorkDir
	if workdir == "" {
		workdir = "/"
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return 0, err
	}
	bin, err := lookPath(spec.Args[0], path)
	if err != nil {
		return 0, err
	}
	if spec.Chown != "" && (u.uid != 0 || u.gid != 0) {
		// Best effort: only root is mapped without subordinate IDs.
		_ = filepath.WalkDir(spec.Chown, func(p string, _ fs.DirEntry, err error) error {
			if err == nil {
				_ = unix.Lchown(p, u.uid, u.gid)
			}
			return nil
		})
	}
	var stdin *os.File = os.Stdin
	if !spec.Stdin {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			return 0, err
		}
		defer devnull.Close()
		stdin = devnull
	}
	cmd := &exec.Cmd{
		Path:   bin,
		Args:   spec.Args,
		Env:    env,
		Dir:    workdir,
		Stdin:  stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid), Groups: u.groups, NoSetGroups: cfg.SingleID},
		},
	}
	if spec.TTY {
		err = runOnTerminal(cmd, spec, u)
	} else {
		err = cmd.Run()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil && cfg.SingleID && (u.uid != 0 || u.gid != 0) && errors.Is(err, unix.EINVAL) {
		return 0, fmt.Errorf("user %q is %w: only root is available without newuidmap/newgidmap and an /etc/subuid entry", spec.User, errNotMapped)
	}
	return 0, err
}

func hasEnv(env []string, k string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			return true
		}
	}
	return false
}

func mustHostname() string {
	h, _ := os.Hostname()
	return h
}

// lookPath finds an executable in the container's PATH.
func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: executable file not found in $PATH", name)
}

type userInfo struct {
	uid, gid int
	groups   []uint32
	home     string
}

// lookupUser resolves user[:group] against the container's /etc/passwd
// and /etc/group, like Docker: a numeric user need not exist.
func lookupUser(spec string) (userInfo, error) {
	u := userInfo{home: "/"}
	if spec == "" {
		spec = "0"
	}
	name, group, hasGroup := strings.Cut(spec, ":")
	passwd := readDB("/etc/passwd")
	found := false
	for _, f := range passwd {
		if len(f) >= 6 && (f[0] == name || f[2] == name) {
			uid, err1 := strconv.Atoi(f[2])
			gid, err2 := strconv.Atoi(f[3])
			if err1 != nil || err2 != nil {
				continue
			}
			u.uid, u.gid, u.home, found = uid, gid, f[5], true
			name = f[0]
			break
		}
	}
	if !found {
		uid, err := strconv.Atoi(name)
		if err != nil {
			return u, fmt.Errorf("unable to find user %s: no matching entries in passwd file", name)
		}
		u.uid, u.gid = uid, 0
	}
	groups := readDB("/etc/group")
	if hasGroup {
		gid, err := strconv.Atoi(group)
		if err != nil {
			gid = -1
			for _, f := range groups {
				if len(f) >= 3 && f[0] == group {
					gid, _ = strconv.Atoi(f[2])
				}
			}
			if gid < 0 {
				return u, fmt.Errorf("unable to find group %s: no matching entries in group file", group)
			}
		}
		u.gid = gid
	} else if found {
		for _, f := range groups {
			if len(f) < 4 {
				continue
			}
			for _, m := range strings.Split(f[3], ",") {
				if m == name {
					if gid, err := strconv.Atoi(f[2]); err == nil && gid != u.gid {
						u.groups = append(u.groups, uint32(gid))
					}
				}
			}
		}
	}
	return u, nil
}

func readDB(p string) [][]string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out
}

// runOnTerminal runs cmd on a new pseudo-terminal inside the container,
// relaying it to init's standard streams and applying size changes that
// arrive on fdResize.
func runOnTerminal(cmd *exec.Cmd, spec proto.Spec, u userInfo) error {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening a terminal: %w", err)
	}
	defer master.Close()
	mfd := int(master.Fd())
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		return err
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		return err
	}
	setSize := func(s proto.Size) {
		if s.Rows > 0 && s.Cols > 0 {
			_ = unix.IoctlSetWinsize(mfd, unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
		}
	}
	setSize(spec.Size)
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	_ = unix.Fchown(int(slave.Fd()), u.uid, u.gid)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0
	err = cmd.Start()
	slave.Close()
	if err != nil {
		return err
	}
	if spec.Stdin {
		go io.Copy(master, os.Stdin)
	}
	go func() {
		dec := json.NewDecoder(os.NewFile(fdResize, "resize"))
		for {
			var s proto.Size
			if dec.Decode(&s) != nil {
				return
			}
			setSize(s)
		}
	}()
	outDone := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, master) // ends with EIO once the terminal is closed
		close(outDone)
	}()
	err = cmd.Wait()
	select {
	case <-outDone:
	case <-time.After(2 * time.Second): // background processes keep it open
	}
	return err
}
