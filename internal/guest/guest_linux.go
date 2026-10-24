//go:build linux

// Package guest is the init process of aic's build VM on macOS.
//
// It mounts the basic filesystems, reads the DHCP-provided DNS servers,
// and then serves RUN requests from the host over the second virtio
// console port (/dev/hvc1), using the framing in package proto. Each
// request runs in a fresh container (package container).
package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/rakyll/ai-container/internal/container"
	"github.com/rakyll/ai-container/internal/proto"
)

// Main runs the guest. It does not return.
func Main() {
	if err := setup(); err != nil {
		log.Printf("aic-guest: setup: %v", err)
		poweroff()
	}
	ch, err := os.OpenFile("/dev/hvc1", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		log.Printf("aic-guest: %v", err)
		poweroff()
	}
	if err := makeRaw(int(ch.Fd())); err != nil {
		log.Printf("aic-guest: raw mode: %v", err)
		poweroff()
	}
	if err := serve(ch, ch); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("aic-guest: %v", err)
	}
	poweroff()
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
	// The kernel configured eth0 with DHCP (ip=dhcp) and recorded the
	// DNS servers in /proc/net/pnp.
	var resolv strings.Builder
	if f, err := os.Open("/proc/net/pnp"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "nameserver") {
				resolv.WriteString(sc.Text() + "\n")
			}
		}
		f.Close()
	}
	return os.WriteFile("/etc/resolv.conf", []byte(resolv.String()), 0o644)
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
	if err := fw.Frame(proto.FrameReady, nil); err != nil {
		return err
	}
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
		if err := runOne(r, fw, spec); err != nil {
			return err
		}
	}
}

func runOne(r io.Reader, fw *proto.Writer, spec proto.Spec) error {
	imgR, imgW := io.Pipe()
	done := make(chan proto.Result, 1)
	go func() {
		res, err := container.Run(context.Background(), spec, imgR,
			fw.Stream(proto.FrameStdout), fw.Stream(proto.FrameStderr), fw.Stream(proto.FrameExport))
		if err != nil {
			res = proto.Result{Error: err.Error()}
		}
		imgR.CloseWithError(errors.New("container exited"))
		done <- res
	}()

	// Relay the image until its end marker, even if the container has
	// already failed: the host doesn't read results until it is done.
	buf := make([]byte, 0, proto.MaxPayload)
	for {
		typ, p, err := proto.ReadFrame(r, buf)
		if err != nil {
			imgW.CloseWithError(err)
			return err
		}
		if typ != proto.FrameImage {
			return fmt.Errorf("%w %q, want image data", proto.ErrUnexpectedFrame, typ)
		}
		if len(p) == 0 {
			imgW.Close()
			break
		}
		_, _ = imgW.Write(p) // fails once the container stops reading
	}
	res := <-done
	if err := fw.Frame(proto.FrameExport, nil); err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return fw.Frame(proto.FrameResult, b)
}
