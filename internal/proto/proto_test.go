package proto

import (
	"bytes"
	"io"
	"testing"
)

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	fw := NewWriter(&buf)
	big := bytes.Repeat([]byte("x"), MaxPayload+10)
	if _, err := fw.Stream(FrameImage).Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Stream(FrameImage).Write(nil); err != nil {
		t.Fatal(err)
	}
	if err := fw.Frame(FrameImage, nil); err != nil {
		t.Fatal(err)
	}
	if err := fw.Frame(FrameResult, []byte(`{"exitCode":3}`)); err != nil {
		t.Fatal(err)
	}

	var got []int
	var b []byte
	for {
		typ, p, err := ReadFrame(&buf, b)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if typ == FrameResult && string(p) != `{"exitCode":3}` {
			t.Errorf("result = %q", p)
		}
		got = append(got, len(p))
	}
	if want := []int{MaxPayload, 10, 0, 14}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != 0 || got[3] != 14 {
		t.Errorf("frame sizes = %v, want %v", got, want)
	}
	if err := WriteFrame(io.Discard, FrameImage, big); err == nil {
		t.Error("oversized frame accepted")
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{'I', 0, 0, 0, 5, 'a'}), nil); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated frame err = %v", err)
	}
}
