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

func TestClean(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := ParseLocal("app:1")
	if err := s.Put(img, ref); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Flat(img)
	io.ReadAll(r)
	r.Close()
	n, freed, err := s.Clean()
	if err != nil || n != 1 || freed == 0 {
		t.Fatalf("Clean = %d, %d, %v", n, freed, err)
	}
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Errorf("after Clean: %d images, %v", len(list), err)
	}
	for _, d := range []string{"blobs/sha256", "flat"} {
		if ents, _ := os.ReadDir(filepath.Join(s.Dir(), d)); len(ents) != 0 {
			t.Errorf("%s has %d entries after Clean", d, len(ents))
		}
	}
	// The store still works.
	if err := s.Put(img, ref); err != nil {
		t.Fatal(err)
	}
}
