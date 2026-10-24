// Package proto defines how a RUN request travels to the process that
// executes it: the container spec, its result, and a framing format used
// to multiplex streams over a single byte channel (the virtio serial port
// between aic on macOS and its Linux VM).
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Spec describes a command to run in a container built from an image.
type Spec struct {
	Args    []string `json:"args"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// Network is "none" (default: an isolated network namespace with only
	// loopback) or "host" (share the network of the host or VM).
	Network  string `json:"network,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// Result reports how a run ended. Error is set if the container could not
// be set up or exported; otherwise ExitCode is the command's exit status.
type Result struct {
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// Frame types.
const (
	FrameReady  byte = 'P' // guest -> host: booted and serving
	FrameSpec   byte = 'S' // host -> guest: JSON Spec, starts a run
	FrameImage  byte = 'I' // host -> guest: flattened rootfs tar; empty = EOF
	FrameStdout byte = 'O' // guest -> host
	FrameStderr byte = 'E' // guest -> host
	FrameExport byte = 'X' // guest -> host: container rootfs tar; empty = EOF
	FrameResult byte = 'R' // guest -> host: JSON Result, ends a run
)

// MaxPayload bounds a frame's payload.
const MaxPayload = 1 << 20

// WriteFrame writes one frame. It is safe for concurrent use only through
// a Writer.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("frame payload of %d bytes exceeds %d", len(payload), MaxPayload)
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame. buf is reused if large enough.
func ReadFrame(r io.Reader, buf []byte) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("frame payload of %d bytes exceeds %d", n, MaxPayload)
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

// Writer serializes frames from several goroutines onto one channel.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (fw *Writer) Frame(typ byte, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return WriteFrame(fw.w, typ, payload)
}

// Stream returns an io.Writer that sends its writes as frames of typ,
// split to MaxPayload. Zero-length writes are dropped, since an empty
// frame marks the end of a stream; use Frame(typ, nil) for that.
func (fw *Writer) Stream(typ byte) io.Writer { return streamWriter{fw, typ} }

type streamWriter struct {
	fw  *Writer
	typ byte
}

func (s streamWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := p
		if len(c) > MaxPayload {
			c = c[:MaxPayload]
		}
		if err := s.fw.Frame(s.typ, c); err != nil {
			return n, err
		}
		n += len(c)
		p = p[len(c):]
	}
	return n, nil
}

// ErrUnexpectedFrame is returned for frames that make no sense in context.
var ErrUnexpectedFrame = errors.New("unexpected frame")
