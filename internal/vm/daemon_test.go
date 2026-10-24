package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ai4-labs/aic/internal/proto"
)

// fakeMachine echoes the image back as the export.
type fakeMachine struct{ closed bool }

func (f *fakeMachine) Run(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error) {
	b, err := io.ReadAll(image)
	if err != nil {
		return proto.Result{}, err
	}
	switch spec.Args[0] {
	case "fail":
		rio.Stderr.Write([]byte("boom\n"))
		return proto.Result{ExitCode: 2}, nil
	case "die":
		f.closed = true
		return proto.Result{}, errors.New("VM crashed")
	case "block":
		<-ctx.Done() // an interactive session nobody is attached to
		return proto.Result{}, ctx.Err()
	case "cat":
		// Report the first resize, then echo stdin.
		size := <-rio.Resize
		fmt.Fprintf(rio.Stdout, "%dx%d\n", size.Cols, size.Rows)
		io.Copy(rio.Stdout, rio.Stdin)
		return proto.Result{}, nil
	}
	rio.Stdout.Write([]byte("ran " + strings.Join(spec.Args, " ") + "\n"))
	rio.Export.Write(b)
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
	res, err := c.Run(context.Background(), proto.Spec{Args: []string{"echo", "hi"}}, bytes.NewReader(image),
		proto.IO{Stdout: &stdout, Stderr: &stderr, Export: &export})
	if err != nil || res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if stdout.String() != "ran echo hi\n" || !bytes.Equal(export.Bytes(), image) {
		t.Errorf("stdout %q, export %d bytes", stdout.String(), export.Len())
	}

	res, err = c.Run(context.Background(), proto.Spec{Args: []string{"fail"}}, strings.NewReader("x"),
		proto.IO{Stdout: &stdout, Stderr: &stderr, Export: io.Discard})
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

func TestDaemonStdin(t *testing.T) {
	dir, _, _ := startFake(t)
	c, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	resize := make(chan proto.Size, 1)
	resize <- proto.Size{Rows: 24, Cols: 80}
	input := strings.Repeat("hello\n", 50_000)
	for i := 0; i < 2; i++ { // a second run must not see the first one's frames
		var stdout bytes.Buffer
		res, err := c.Run(context.Background(), proto.Spec{Args: []string{"cat"}, Stdin: true, TTY: true},
			strings.NewReader("img"), proto.IO{Stdin: strings.NewReader(input), Resize: resize,
				Stdout: &stdout, Stderr: io.Discard, Export: io.Discard})
		if err != nil || res.ExitCode != 0 || res.Error != "" {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		if want := "80x24\n" + input; stdout.String() != want {
			t.Errorf("stdout = %d bytes, want %d", stdout.Len(), len(want))
		}
		resize <- proto.Size{Rows: 24, Cols: 80}
	}
}

func TestDaemonStopsWhenVMDies(t *testing.T) {
	dir, _, done := startFake(t)
	c, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(context.Background(), proto.Spec{Args: []string{"die"}}, strings.NewReader("x"),
		proto.IO{Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard})
	if err != nil || !strings.Contains(res.Error, "VM crashed") {
		t.Errorf("run = %+v, %v", res, err)
	}
	<-done
}

// A client that goes away (Ctrl-C, closed terminal) must not leave its
// container holding the VM.
func TestDaemonCancelsAbandonedRun(t *testing.T) {
	dir, _, _ := startFake(t)
	c, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	if _, err := c.Run(ctx, proto.Spec{Args: []string{"block"}, Stdin: true, TTY: true}, strings.NewReader("x"),
		proto.IO{Stdin: stdinR, Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard}); err == nil {
		t.Fatal("cancelled run succeeded")
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), proto.Spec{Args: []string{"echo"}}, strings.NewReader("x"),
			proto.IO{Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next run is stuck behind the abandoned one")
	}
}
