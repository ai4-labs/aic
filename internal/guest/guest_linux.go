//go:build linux

// Package guest is the init process of aic's build VM on macOS.
//
// It mounts the basic filesystems, reads the DHCP-provided DNS servers,
// and then serves RUN requests from the host over the second virtio
// console port (/dev/hvc1), using the framing in package proto. Each
// request runs in a fresh container (package container).
package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ai4-labs/aic/internal/container"
	"github.com/ai4-labs/aic/internal/proto"
)

// Main runs the guest. It does not return.
func Main() {
	if err := setup(); err != nil {
		log.Printf("aic-guest: setup: %v", err)
		poweroff()
	}
	startNetwork()
	ch, err := os.OpenFile("/dev/hvc1", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		log.Printf("aic-guest: %v", err)
		poweroff()
	}
	if err := makeRaw(int(ch.Fd())); err != nil {
		log.Printf("aic-guest: raw mode: %v", err)
		poweroff()
	}
	// Requests come over vsock if the modules load: it is much faster
	// than the console port, which is the fallback. Either way, the
	// ready frame on the console port says which.
	var r io.Reader
	var w io.Writer
	fw := proto.NewWriter(ch)
	if conn, err := acceptVsock(fw); err == nil {
		r, w = conn, conn
	} else {
		log.Printf("aic-guest: vsock unavailable, using the console port: %v", err)
		if err := fw.Frame(proto.FrameReady, nil); err != nil {
			log.Printf("aic-guest: %v", err)
			poweroff()
		}
		r, w = ch, ch
	}
	if err := serve(r, w); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("aic-guest: %v", err)
	}
	poweroff()
}

// VsockPort is where the guest accepts the host's connection.
const vsockPort = 1024

// acceptVsock loads the vsock modules, listens, announces the port on
// the console port and accepts the host's connection.
func acceptVsock(console *proto.Writer) (*os.File, error) {
	order, err := os.ReadFile("/lib/modules/order")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Fields(string(order)) {
		if err := loadModule("/lib/modules/" + name); err != nil {
			return nil, fmt.Errorf("loading %s: %w", name, err)
		}
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: vsockPort}); err != nil {
		return nil, err
	}
	if err := unix.Listen(fd, 1); err != nil {
		return nil, err
	}
	if err := console.Frame(proto.FrameReady, []byte(fmt.Sprintf("vsock:%d", vsockPort))); err != nil {
		return nil, err
	}
	// From here on the host expects vsock: failing is fatal.
	nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
	if err != nil {
		log.Printf("aic-guest: accepting the host's connection: %v", err)
		poweroff()
	}
	return os.NewFile(uintptr(nfd), "vsock"), nil
}

func loadModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return nil
}

func poweroff() {
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}

func setup() error {
	for _, m := range []struct{ src, dst, fstype, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", ""},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
	} {
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.src, m.dst, m.fstype, 0, m.data); err != nil && !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	if console, err := os.OpenFile("/dev/hvc0", os.O_WRONLY, 0); err == nil {
		log.SetOutput(console)
	}
	if err := unix.Sethostname([]byte("aic-vm")); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return err
	}
	// startNetwork writes it once DHCP has answered.
	return os.WriteFile("/etc/resolv.conf", nil, 0o644)
}

// makeRaw puts the serial port in raw mode so bytes pass through as-is.
func makeRaw(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

// serve handles RUN requests until the channel closes.
func serve(r io.Reader, w io.Writer) error {
	fw := proto.NewWriter(w)
	var buf []byte
	for {
		typ, payload, err := proto.ReadFrame(r, buf)
		if err != nil {
			return err
		}
		buf = payload[:0]
		if typ != proto.FrameSpec {
			return fmt.Errorf("%w %q, want spec", proto.ErrUnexpectedFrame, typ)
		}
		var spec proto.Spec
		if err := json.Unmarshal(payload, &spec); err != nil {
			return err
		}
		if err := proto.ServeRun(r, fw, spec, func(ctx context.Context, image io.Reader, rio proto.IO) proto.Result {
			if spec.Network == "host" {
				if err := waitNetwork(10 * time.Second); err != nil {
					fmt.Fprintf(rio.Stderr, "aic: the VM has no network: %v\n", err)
				}
			}
			res, err := container.Run(ctx, spec, image, rio)
			if err != nil {
				res = proto.Result{Error: err.Error()}
			}
			return res
		}); err != nil {
			return err
		}
	}
}
