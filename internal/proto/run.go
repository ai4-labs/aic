package proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ChannelError is a failure of the channel itself (as opposed to a
// failed run), after which the channel is unusable.
type ChannelError struct{ Err error }

func (e *ChannelError) Error() string { return e.Err.Error() }
func (e *ChannelError) Unwrap() error { return e.Err }

// sender writes frames until it is closed, after which writes are
// dropped. Pumps for stdin and resizes may outlive the run; closing the
// sender keeps them from writing into the next one.
type sender struct {
	mu     sync.Mutex
	fw     *Writer
	closed bool
}

var errSenderClosed = errors.New("run finished")

func (s *sender) frame(typ byte, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSenderClosed
	}
	return s.fw.Frame(typ, p)
}

func (s *sender) stream(typ byte) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		for n := 0; n < len(p); {
			c := p[n:]
			if len(c) > MaxPayload {
				c = c[:MaxPayload]
			}
			if err := s.frame(typ, c); err != nil {
				return n, err
			}
			n += len(c)
		}
		return len(p), nil
	})
}

// finish sends a last frame and closes the sender.
func (s *sender) finish(typ byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.fw.Frame(typ, nil)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// HostRun performs one run over a frame channel: it sends the spec, the
// image, then stdin and terminal resizes while dispatching output frames,
// until the result arrives; then it ends the run with FrameDone. It is
// the host side of the protocol, used against the guest's serial port.
// Cancelling ctx asks the other side to kill the container; HostRun still
// waits for the result.
func HostRun(ctx context.Context, r io.Reader, fw *Writer, spec Spec, image io.Reader, rio IO) (Result, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return Result{}, err
	}
	s := &sender{fw: fw}
	if err := s.frame(FrameSpec, b); err != nil {
		return Result{}, &ChannelError{err}
	}
	imageSent := make(chan error, 1)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			s.frame(FrameCancel, nil)
		case <-finished:
		}
	}()
	go func() {
		buf := make([]byte, 256<<10)
		_, err := io.CopyBuffer(s.stream(FrameImage), image, buf)
		// Always end the stream so the other side finishes the request.
		if ferr := s.frame(FrameImage, nil); err == nil {
			err = ferr
		}
		imageSent <- err
		if err != nil {
			return
		}
		if spec.Stdin && rio.Stdin != nil {
			go func() {
				if _, err := io.Copy(s.stream(FrameStdin), rio.Stdin); err == nil {
					s.frame(FrameStdin, nil) // EOF
				}
			}()
		}
		if spec.TTY && rio.Resize != nil {
			go func() {
				for {
					select {
					case <-finished:
						return
					case size := <-rio.Resize:
						b, _ := json.Marshal(size)
						if s.frame(FrameResize, b) != nil {
							return
						}
					}
				}
			}()
		}
	}()

	var buf []byte
	var werr error // first failure writing output; reported after the result
	for {
		typ, p, err := ReadFrame(r, buf)
		if err != nil {
			return Result{}, &ChannelError{err}
		}
		buf = p[:0]
		var w io.Writer
		switch typ {
		case FrameStdout:
			w = rio.Stdout
		case FrameStderr:
			w = rio.Stderr
		case FrameExport:
			if len(p) > 0 {
				w = rio.Export
			}
		case FrameResult:
			var res Result
			if err := json.Unmarshal(p, &res); err != nil {
				return Result{}, err
			}
			sendErr := <-imageSent
			if err := s.finish(FrameDone); err != nil {
				return Result{}, &ChannelError{err}
			}
			if sendErr != nil {
				return Result{}, fmt.Errorf("sending the image: %w", sendErr)
			}
			if werr != nil {
				return Result{}, werr
			}
			return res, nil
		default:
			return Result{}, &ChannelError{fmt.Errorf("%w %q", ErrUnexpectedFrame, typ)}
		}
		// After a write error, keep draining so the channel stays in sync.
		if w != nil && werr == nil {
			_, werr = w.Write(p)
		}
	}
}

// ServeRun is the other side of HostRun: it reads a run's image, stdin
// and resizes from r (after the spec frame) and calls run. Output goes
// to fw. The context given to run is cancelled when the host cancels the
// run or the channel fails. ServeRun returns once the host has sent
// FrameDone, or the channel has failed and run has returned.
func ServeRun(r io.Reader, fw *Writer, spec Spec,
	run func(ctx context.Context, image io.Reader, rio IO) Result) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	imgR, imgW := io.Pipe()
	var stdinR *io.PipeReader
	var stdin chan []byte // queued so a slow reader doesn't block this loop
	if spec.Stdin {
		var stdinW *io.PipeWriter
		stdinR, stdinW = io.Pipe()
		stdin = make(chan []byte, 256)
		go func() {
			for p := range stdin {
				if p == nil {
					break // EOF
				}
				if _, err := stdinW.Write(p); err != nil {
					break
				}
			}
			stdinW.Close()
			for range stdin { // drop the rest
			}
		}()
		defer close(stdin)
	}
	resize := make(chan Size, 8)
	rio := IO{
		Resize: resize,
		Stdout: fw.Stream(FrameStdout),
		Stderr: fw.Stream(FrameStderr),
		Export: fw.Stream(FrameExport),
	}
	if stdinR != nil {
		rio.Stdin = stdinR
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		res := run(ctx, imgR, rio)
		imgR.CloseWithError(errors.New("run finished"))
		if stdinR != nil {
			stdinR.CloseWithError(errors.New("run finished"))
		}
		if fw.Frame(FrameExport, nil) == nil {
			b, _ := json.Marshal(res)
			fw.Frame(FrameResult, b)
		}
	}()
	defer func() { <-done }()

	// Read the image until its end marker, then stdin and resizes until
	// the host says it is done. Data for a run that already ended is
	// dropped.
	var buf []byte
	inImage, stdinEOF := true, false
	for {
		typ, p, err := ReadFrame(r, buf)
		if err != nil {
			cancel()
			imgW.CloseWithError(err)
			return err
		}
		buf = p[:0]
		switch {
		case typ == FrameCancel:
			cancel()
		case inImage && typ == FrameImage:
			if len(p) == 0 {
				imgW.Close()
				inImage = false
				continue
			}
			_, _ = imgW.Write(p)
		case !inImage && typ == FrameStdin:
			if stdin == nil || stdinEOF {
				continue
			}
			if len(p) == 0 {
				stdinEOF = true
				stdin <- nil
				continue
			}
			stdin <- append([]byte(nil), p...)
		case !inImage && typ == FrameResize:
			var size Size
			if json.Unmarshal(p, &size) == nil {
				select {
				case resize <- size:
				default:
				}
			}
		case !inImage && typ == FrameDone:
			return nil
		default:
			err := fmt.Errorf("%w %q", ErrUnexpectedFrame, typ)
			cancel()
			imgW.CloseWithError(err)
			return err
		}
	}
}
