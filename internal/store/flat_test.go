package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestFlat(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(mutate.Extract(img))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	copyPath := filepath.Join(s.flatDir(), d.Hex+".tar")

	// A partial read saves nothing.
	r, err := s.Flat(img)
	if err != nil {
		t.Fatal(err)
	}
	io.CopyN(io.Discard, r, 100)
	r.Close()
	if _, err := os.Stat(copyPath); err == nil {
		t.Fatal("partial read was saved")
	}

	// A full read saves the copy, which later reads use.
	for i := 0; i < 2; i++ {
		r, err := s.Flat(img)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read %d: %d bytes, %v; want %d", i, len(got), err, len(want))
		}
		if _, err := os.Stat(copyPath); err != nil {
			t.Fatalf("read %d: no copy saved: %v", i, err)
		}
	}
	if ents, _ := os.ReadDir(s.flatDir()); len(ents) != 1 {
		t.Errorf("flat dir has %d entries, want 1", len(ents))
	}

	// Pruning the cache drops it.
	if _, err := s.PruneCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyPath); err == nil {
		t.Error("copy survived prune")
	}
}
