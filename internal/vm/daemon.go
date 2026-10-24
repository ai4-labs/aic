package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rakyll/ai-container/internal/proto"
)

// The VM daemon keeps a build VM running between builds (`aic vm start`).
// Builds find it through a Unix socket in the VM directory and send their
// RUN requests to it instead of booting a VM of their own.

// SocketPath returns the daemon's socket in dir.
func SocketPath(dir string) string { return filepath.Join(dir, "aic.sock") }

// Status describes a running VM daemon.
type Status struct {
	PID       int       `json:"pid"`
	CPUs      uint      `json:"cpus"`
	MemoryMiB uint64    `json:"memoryMiB"`
	Kernel    string    `json:"kernel"`
	Started   time.Time `json:"started"`
	Runs      int64     `json:"runs"`
}

// runner is what the daemon serves; *Machine in practice.
type runner interface {
	Run(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error)
	Closed() bool
}

type server struct {
	r      runner
	status Status
	runs   atomic.Int64
	stop   func()
}

// serve accepts connections until ln is closed.
func (s *server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	typ, p, err := proto.ReadFrame(conn, nil)
	if err != nil {
		return
	}
	fw := proto.NewWriter(conn)
	switch typ {
	case proto.FrameStatus:
		st := s.status
		st.Runs = s.runs.Load()
		b, _ := json.Marshal(st)
		_ = fw.Frame(proto.FrameStatus, b)
	case proto.FrameStop:
		_ = fw.Frame(proto.FrameStop, nil)
		s.stop()
	case proto.FrameSpec:
		var spec proto.Spec
		if err := json.Unmarshal(p, &spec); err != nil {
			return
		}
		s.runs.Add(1)
		// A client that goes away mid-run must not take the shared VM
		// down: the run completes and its output is discarded.
		_ = proto.ServeRun(conn, fw, spec, func(image io.Reader, rio proto.IO) proto.Result {
			res, err := s.r.Run(context.Background(), spec, image, rio)
			if err != nil {
				res = proto.Result{Error: err.Error()}
			}
			return res
		})
		if s.r.Closed() {
			s.stop() // the VM died; let the next build start afresh
		}
	}
}

// listen creates the daemon socket, accessible only to the current user.
func listen(dir string) (net.Listener, error) {
	sock := SocketPath(dir)
	if Running(dir) {
		return nil, errors.New("the VM is already running")
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveUntil runs the daemon on ln until ctx is done or a stop request.
func serveUntil(ctx context.Context, ln net.Listener, r runner, status Status) {
	stopped := make(chan struct{})
	s := &server{r: r, status: status, stop: sync.OnceFunc(func() { close(stopped) })}
	go s.serve(ln)
	select {
	case <-ctx.Done():
	case <-stopped:
	}
	ln.Close()
}

func dial(dir string) (net.Conn, error) {
	return net.DialTimeout("unix", SocketPath(dir), time.Second)
}

// Running reports whether a VM daemon is serving in dir.
func Running(dir string) bool {
	_, err := DaemonStatus(dir)
	return err == nil
}

// ErrNotRunning is returned when no VM daemon is running.
var ErrNotRunning = errors.New("the VM is not running")

// DaemonStatus asks the daemon in dir for its status.
func DaemonStatus(dir string) (Status, error) {
	conn, err := dial(dir)
	if err != nil {
		return Status{}, ErrNotRunning
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proto.WriteFrame(conn, proto.FrameStatus, nil); err != nil {
		return Status{}, err
	}
	typ, p, err := proto.ReadFrame(conn, nil)
	if err != nil {
		return Status{}, err
	}
	if typ != proto.FrameStatus {
		return Status{}, fmt.Errorf("%w %q", proto.ErrUnexpectedFrame, typ)
	}
	var st Status
	return st, json.Unmarshal(p, &st)
}

// StopDaemon asks the daemon in dir to stop, and waits until it has.
func StopDaemon(dir string) error {
	conn, err := dial(dir)
	if err != nil {
		return ErrNotRunning
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	err = proto.WriteFrame(conn, proto.FrameStop, nil)
	if err == nil {
		_, _, err = proto.ReadFrame(conn, nil)
	}
	conn.Close()
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if c, err := dial(dir); err != nil {
			return nil
		} else {
			c.Close()
		}
	}
	return errors.New("timed out waiting for the VM to stop")
}

// Client runs requests on a VM daemon.
type Client struct{ dir string }

// NewClient returns a client for the daemon in dir, if one is running.
func NewClient(dir string) (*Client, error) {
	if !Running(dir) {
		return nil, ErrNotRunning
	}
	return &Client{dir: dir}, nil
}

// Run is like Machine.Run, through the daemon.
func (c *Client) Run(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error) {
	conn, err := dial(c.dir)
	if err != nil {
		return proto.Result{}, fmt.Errorf("connecting to the VM: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	res, err := proto.HostRun(conn, proto.NewWriter(conn), spec, image, rio)
	if err != nil && ctx.Err() != nil {
		return proto.Result{}, ctx.Err()
	}
	var ce *proto.ChannelError
	if errors.As(err, &ce) {
		return proto.Result{}, fmt.Errorf("lost the connection to the VM (see `aic vm logs`): %w", ce.Err)
	}
	return res, err
}
