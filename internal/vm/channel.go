package vm

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/rakyll/ai-container/internal/proto"
)

// channelError is a failure of the channel itself (as opposed to a
// failed run), after which the channel is unusable.
type channelError struct{ err error }

func (e *channelError) Error() string { return e.err.Error() }
func (e *channelError) Unwrap() error { return e.err }

// hostRun performs one run over a frame channel: it sends the spec and
// the image, and dispatches output frames until the result arrives. It is
// the host side of the protocol, used both against the guest's serial
// port and against the VM daemon's socket.
func hostRun(r io.Reader, fw *proto.Writer, spec proto.Spec, image io.Reader, stdout, stderr, export io.Writer) (proto.Result, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return proto.Result{}, err
	}
	if err := fw.Frame(proto.FrameSpec, b); err != nil {
		return proto.Result{}, &channelError{err}
	}
	sendErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 256<<10)
		_, err := io.CopyBuffer(fw.Stream(proto.FrameImage), image, buf)
		// Always end the stream so the other side finishes the request.
		if ferr := fw.Frame(proto.FrameImage, nil); err == nil {
			err = ferr
		}
		sendErr <- err
	}()

	var buf []byte
	var werr error // first failure writing output; reported after the result
	for {
		typ, p, err := proto.ReadFrame(r, buf)
		if err != nil {
			return proto.Result{}, &channelError{err}
		}
		buf = p[:0]
		var w io.Writer
		switch typ {
		case proto.FrameStdout:
			w = stdout
		case proto.FrameStderr:
			w = stderr
		case proto.FrameExport:
			if len(p) > 0 {
				w = export
			}
		case proto.FrameResult:
			var res proto.Result
			if err := json.Unmarshal(p, &res); err != nil {
				return proto.Result{}, err
			}
			if err := <-sendErr; err != nil {
				return proto.Result{}, fmt.Errorf("sending the image: %w", err)
			}
			if werr != nil {
				return proto.Result{}, werr
			}
			return res, nil
		default:
			return proto.Result{}, &channelError{fmt.Errorf("%w %q", proto.ErrUnexpectedFrame, typ)}
		}
		// After a write error, keep draining so the channel stays in sync.
		if w != nil && werr == nil {
			_, werr = w.Write(p)
		}
	}
}
