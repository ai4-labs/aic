package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rakyll/ai-container/internal/proto"
)

// fakeMachine echoes the image back as the export.
type fakeMachine struct{ closed bool }

func (f *fakeMachine) Run(_ context.Context, spec proto.Spec, image io.Reader, stdout, stderr, export io.Writer) (proto.Result, error) {
	b, err := io.ReadAll(image)
	if err != nil {
		return proto.Result{}, err
	}
	switch spec.Args[0] {
	case "fail":
		stderr.Write([]byte("boom\n"))
		return proto.Result{ExitCode: 2}, nil
	case "die":
		f.closed = true
		return proto.Result{}, errors.New("VM crashed")
	}
	stdout.Write([]byte("ran " + strings.Join(spec.Args, " ") + "\n"))
	export.Write(b)
	return proto.Result{}, nil
}

func (f *fakeMachine) Closed() bool { return f.closed }

func startFake(t *testing.T) (string, *fakeMachine, chan struct{}) {
	t.Helper()
	dir, err := os.MkdirTemp("", "aicvm") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeMachine{}
	done := make(chan struct{})
	go func() {
		serveUntil(context.Background(), ln, fm, Status{PID: 42, CPUs: 2, MemoryMiB: 512, Kernel: "test"})
		close(done)
	}()
	return dir, fm, done
}

func TestDaemon(t *testing.T) {
	dir, fm, done := startFake(t)
	if !Running(dir) {
		t.Fatal("daemon not running")
	}
	if _, err := listen(dir); err == nil {
		t.Error("a second daemon could listen")
	}
	if fi, err := os.Stat(SocketPath(dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, %v", fi.Mode(), err)
	}

	c, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	image := bytes.Repeat([]byte("layer"), 300_000) // spans several frames
	var stdout, stderr, export bytes.Buffer
	res, err := c.Run(context.Background(), proto.Spec{Args: []string{"echo", "hi"}}, bytes.NewReader(image), &stdout, &stderr, &export)
	if err != nil || res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if stdout.String() != "ran echo hi\n" || !bytes.Equal(export.Bytes(), image) {
		t.Errorf("stdout %q, export %d bytes", stdout.String(), export.Len())
	}

	res, err = c.Run(context.Background(), proto.Spec{Args: []string{"fail"}}, strings.NewReader("x"), &stdout, &stderr, io.Discard)
	if err != nil || res.ExitCode != 2 || stderr.String() != "boom\n" {
		t.Errorf("failing run = %+v, %v, stderr %q", res, err, stderr.String())
	}

	st, err := DaemonStatus(dir)
	if err != nil || st.PID != 42 || st.CPUs != 2 || st.Runs != 2 {
		t.Errorf("status = %+v, %v", st, err)
	}

	if err := StopDaemon(dir); err != nil {
		t.Fatal(err)
	}
	<-done
	if Running(dir) {
		t.Error("daemon still running after stop")
	}
	if _, err := NewClient(dir); !errors.Is(err, ErrNotRunning) {
		t.Errorf("NewClient after stop: %v", err)
	}
	if err := StopDaemon(dir); !errors.Is(err, ErrNotRunning) {
		t.Errorf("second stop: %v", err)
	}
	_ = fm
}

func TestDaemonStopsWhenVMDies(t *testing.T) {
	dir, _, done := startFake(t)
	c, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(context.Background(), proto.Spec{Args: []string{"die"}}, strings.NewReader("x"), io.Discard, io.Discard, io.Discard)
	if err != nil || !strings.Contains(res.Error, "VM crashed") {
		t.Errorf("run = %+v, %v", res, err)
	}
	<-done
}
