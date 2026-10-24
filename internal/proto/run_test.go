package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// serve runs ServeRun for each spec frame on conn, like the VM guest.
func serve(t *testing.T, conn net.Conn, run func(context.Context, Spec, io.Reader, IO) Result) {
	fw := NewWriter(conn)
	for {
		typ, p, err := ReadFrame(conn, nil)
		if err != nil {
			return
		}
		if typ != FrameSpec {
			t.Errorf("frame %q, want spec", typ)
			return
		}
		var spec Spec
		if err := json.Unmarshal(p, &spec); err != nil {
			t.Error(err)
			return
		}
		if err := ServeRun(conn, fw, spec, func(ctx context.Context, image io.Reader, rio IO) Result {
			return run(ctx, spec, image, rio)
		}); err != nil {
			return
		}
	}
}

// A command that exits while the host's stdin is still open ends the run,
// and the next run on the channel isn't disturbed.
func TestRunEndsWithStdinOpen(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	go serve(t, guest, func(_ context.Context, spec Spec, image io.Reader, rio IO) Result {
		io.Copy(io.Discard, image)
		if spec.Args[0] == "read" {
			b := make([]byte, 3)
			io.ReadFull(rio.Stdin, b)
			rio.Stdout.Write(b)
		}
		io.WriteString(rio.Stdout, "done\n")
		return Result{ExitCode: 3}
	})
	fw := NewWriter(host)
	for i, args := range [][]string{{"read"}, {"exit"}, {"read"}} {
		stdinR, stdinW := io.Pipe() // like a terminal: never EOF
		defer stdinW.Close()
		go stdinW.Write([]byte("abc"))
		var stdout bytes.Buffer
		done := make(chan error, 1)
		var res Result
		go func() {
			var err error
			res, err = HostRun(context.Background(), host, fw, Spec{Args: args, Stdin: true, TTY: true}, strings.NewReader("image"),
				IO{Stdin: stdinR, Stdout: &stdout, Stderr: io.Discard, Export: io.Discard})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil || res.ExitCode != 3 || !strings.HasSuffix(stdout.String(), "done\n") {
				t.Fatalf("run %d = %+v, %v, stdout %q", i, res, err, stdout.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d did not end", i)
		}
	}
}

// Cancelling the host's context kills the run, which still reports its
// result, and the channel stays usable.
func TestRunCancel(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	go serve(t, guest, func(ctx context.Context, spec Spec, image io.Reader, rio IO) Result {
		io.Copy(io.Discard, image)
		if spec.Args[0] == "block" {
			<-ctx.Done() // like a container that never exits
			return Result{Error: "killed"}
		}
		return Result{}
	})
	fw := NewWriter(host)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	res, err := HostRun(ctx, host, fw, Spec{Args: []string{"block"}, Stdin: true}, strings.NewReader("image"),
		IO{Stdin: stdinR, Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard})
	if err != nil || res.Error != "killed" {
		t.Fatalf("cancelled run = %+v, %v", res, err)
	}
	res, err = HostRun(context.Background(), host, fw, Spec{Args: []string{"ok"}}, strings.NewReader("image"),
		IO{Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard})
	if err != nil || res.Error != "" {
		t.Fatalf("next run = %+v, %v", res, err)
	}
}

// When the host goes away, the run is cancelled.
func TestRunHangup(t *testing.T) {
	host, guest := net.Pipe()
	cancelled := make(chan struct{})
	go serve(t, guest, func(ctx context.Context, spec Spec, image io.Reader, rio IO) Result {
		io.Copy(io.Discard, image)
		<-ctx.Done()
		close(cancelled)
		return Result{}
	})
	go HostRun(context.Background(), host, NewWriter(host), Spec{Args: []string{"x"}}, strings.NewReader("image"),
		IO{Stdout: io.Discard, Stderr: io.Discard, Export: io.Discard})
	time.Sleep(100 * time.Millisecond)
	host.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("run not cancelled after hangup")
	}
}
