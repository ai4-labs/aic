//go:build linux

// Package container runs a command in a throwaway Linux container built
// from a flattened image, without any external runtime.
//
// Run re-executes the current binary in new mount, PID, UTS, IPC and
// (unless the network is "host") network namespaces. Unprivileged callers
// also get a user namespace: the caller maps to root, and if newuidmap,
// newgidmap and /etc/subuid entries are available, the rest of the
// subordinate ID range is mapped too. The child mounts a tmpfs as the
// container root, pivots into it, extracts the image, runs the command,
// and streams the resulting filesystem back as a tar. Because the root is
// a tmpfs inside the child's mount namespace, it vanishes when the child
// exits; nothing needs cleaning up on the host.
package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/rakyll/ai-container/internal/proto"
)

const childEnv = "_AIC_CONTAINER_INIT"

// childConfig is sent from Run to the child over fd 3.
type childConfig struct {
	Spec       proto.Spec `json:"spec"`
	Rootfs     string     `json:"rootfs"`     // empty host directory used as the tmpfs mount point
	ResolvConf string     `json:"resolvConf"` // content for /etc/resolv.conf
	// SingleID is set when only the caller's own UID/GID are mapped into
	// the user namespace, so other owners can't be represented on disk.
	SingleID bool `json:"singleID"`
	// Reexec is set when the ID maps were written after the child
	// started (newuidmap). The child exec'd while still unmapped, which
	// cost it its capabilities, so it must exec itself once more.
	Reexec bool `json:"reexec"`
}

const configEnv = "_AIC_CONTAINER_CONFIG"

// Child file descriptors.
const (
	fdConfig = 3
	fdImage  = 4
	fdExport = 5
	fdStatus = 6
)

// Init runs the container child if this process is one. Call it first
// thing in main; it returns immediately otherwise.
func Init() {
	if os.Getenv(childEnv) != "1" {
		return
	}
	status := os.NewFile(fdStatus, "status")
	res := child()
	_ = json.NewEncoder(status).Encode(res)
	os.Exit(0)
}

// Run runs spec in a container whose root filesystem is the tar stream
// image (already flattened: no whiteouts). The command's output goes to
// stdout and stderr; afterwards the container's root filesystem is
// written to export as a tar stream, unless the command failed.
func Run(ctx context.Context, spec proto.Spec, image io.Reader, stdout, stderr, export io.Writer) (proto.Result, error) {
	dir, err := os.MkdirTemp("", "aic-rootfs-")
	if err != nil {
		return proto.Result{}, err
	}
	defer os.Remove(dir)

	cfg := childConfig{Spec: spec, Rootfs: dir, ResolvConf: resolvConf(spec.Network)}

	var parentEnds, childEnds []*os.File
	pipe := func(childReads bool) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		if childReads {
			childEnds, parentEnds = append(childEnds, r), append(parentEnds, w)
			return w, nil
		}
		childEnds, parentEnds = append(childEnds, w), append(parentEnds, r)
		return r, nil
	}
	defer func() {
		for _, f := range append(parentEnds, childEnds...) {
			f.Close()
		}
	}()
	cfgW, err := pipe(true)
	if err != nil {
		return proto.Result{}, err
	}
	imgW, err := pipe(true)
	if err != nil {
		return proto.Result{}, err
	}
	expR, err := pipe(false)
	if err != nil {
		return proto.Result{}, err
	}
	statusR, err := pipe(false)
	if err != nil {
		return proto.Result{}, err
	}

	self, err := os.Executable()
	if err != nil {
		return proto.Result{}, err
	}
	cmd := exec.CommandContext(ctx, self)
	cmd.Args = []string{"aic-container-init"}
	cmd.Env = []string{childEnv + "=1"}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = childEnds
	attr := &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		Pdeathsig:  syscall.SIGKILL,
	}
	if spec.Network != "host" {
		attr.Cloneflags |= syscall.CLONE_NEWNET
	}
	var sub *subIDs
	if os.Geteuid() != 0 {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		if sub = findSubIDs(); sub == nil {
			cfg.SingleID = true
			singleIDHint.Do(func() {
				fmt.Fprintln(stderr, "aic: only your own user is mapped into containers (no newuidmap/newgidmap or /etc/subuid entry); files can't be chowned to other users")
			})
			attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
			attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
			attr.GidMappingsEnableSetgroups = false
		}
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return proto.Result{}, fmt.Errorf("starting container: %w (are unprivileged user namespaces enabled?)", err)
	}
	for _, f := range childEnds {
		f.Close()
	}
	childEnds = nil

	if sub != nil {
		cfg.Reexec = true
		if err := sub.apply(cmd.Process.Pid); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return proto.Result{}, err
		}
	}
	// The child blocks reading its config until the ID mappings are set.
	if err := json.NewEncoder(cfgW).Encode(cfg); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return proto.Result{}, err
	}
	cfgW.Close()

	go func() {
		// Errors here mean the child stopped reading (it failed or is
		// gone); its status says why.
		io.Copy(imgW, image)
		imgW.Close()
	}()
	expDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(export, expR)
		expDone <- err
	}()
	statusDone := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(statusR)
		statusDone <- b
	}()

	waitErr := cmd.Wait()
	expErr := <-expDone
	status := <-statusDone
	if ctx.Err() != nil {
		return proto.Result{}, ctx.Err()
	}
	var res proto.Result
	if err := json.Unmarshal(status, &res); err != nil {
		return proto.Result{}, fmt.Errorf("container init failed: %v", waitErr)
	}
	if res.Error == "" && expErr != nil {
		return proto.Result{}, fmt.Errorf("reading container filesystem: %w", expErr)
	}
	return res, nil
}

// resolvConf returns the DNS configuration for the container.
func resolvConf(network string) string {
	if network != "host" {
		return "" // no network, no DNS
	}
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	return string(b)
}

// subIDs describes the subordinate ID ranges of the current user.
type subIDs struct {
	uid, gid             int
	uidStart, uidCount   int
	gidStart, gidCount   int
	newuidmap, newgidmap string
}

// findSubIDs returns the user's subordinate ID ranges if newuidmap and
// newgidmap are installed and /etc/subuid and /etc/subgid have entries.
func findSubIDs() *subIDs {
	nu, err1 := exec.LookPath("newuidmap")
	ng, err2 := exec.LookPath("newgidmap")
	if err1 != nil || err2 != nil {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return nil
	}
	us, uc, ok1 := subRange("/etc/subuid", u.Username, u.Uid)
	gs, gc, ok2 := subRange("/etc/subgid", u.Username, u.Uid)
	if !ok1 || !ok2 {
		return nil
	}
	return &subIDs{uid: os.Geteuid(), gid: os.Getegid(), uidStart: us, uidCount: uc, gidStart: gs, gidCount: gc, newuidmap: nu, newgidmap: ng}
}

func subRange(file, name, uid string) (start, count int, ok bool) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(parts) != 3 || (parts[0] != name && parts[0] != uid) {
			continue
		}
		s, err1 := strconv.Atoi(parts[1])
		c, err2 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil && c > 0 {
			return s, c, true
		}
	}
	return 0, 0, false
}

func (s *subIDs) apply(pid int) error {
	p := strconv.Itoa(pid)
	for _, c := range []struct {
		bin             string
		id, start, size int
	}{{s.newuidmap, s.uid, s.uidStart, s.uidCount}, {s.newgidmap, s.gid, s.gidStart, s.gidCount}} {
		out, err := exec.Command(c.bin, p, "0", strconv.Itoa(c.id), "1", "1", strconv.Itoa(c.start), strconv.Itoa(c.size)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", c.bin, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

var errNotMapped = errors.New("not mapped in the user namespace")

var singleIDHint sync.Once
