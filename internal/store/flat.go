package store

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Unpacking an image's layers takes seconds for large images (it is
// mostly decompression), and aic run does it for every run. The store
// keeps each run image's filesystem as one uncompressed tar stream in
// <store>/flat/<image digest>.tar; images that are removed lose theirs
// at the next garbage collection.

func (s *Store) flatDir() string { return filepath.Join(s.dir, "flat") }

// Flat returns img's filesystem as a single tar stream with whiteouts
// applied, like mutate.Extract. It reads the store's copy if there is
// one; otherwise it extracts the image and saves a copy as the stream is
// read to the end.
func (s *Store) Flat(img v1.Image) (io.ReadCloser, error) {
	d, err := img.Digest()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(s.flatDir(), d.Hex+".tar")
	if f, err := os.Open(p); err == nil {
		return f, nil
	}
	if err := os.MkdirAll(s.flatDir(), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.flatDir(), ".tmp-*")
	if err != nil {
		return nil, err
	}
	return &teeFlat{src: mutate.Extract(img), tmp: tmp, path: p}, nil
}

// teeFlat copies the stream it reads to tmp, which becomes the store's
// copy if the stream is read to the end without errors.
type teeFlat struct {
	mu   sync.Mutex // a reader may still be running when the run ends
	src  io.ReadCloser
	tmp  *os.File
	path string
	err  error // first error writing tmp
	eof  bool
}

func (t *teeFlat) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.src.Read(p)
	if n > 0 && t.err == nil {
		_, t.err = t.tmp.Write(p[:n])
	}
	if err == io.EOF {
		t.eof = true
	}
	return n, err
}

func (t *teeFlat) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	err := t.src.Close()
	name := t.tmp.Name()
	if cerr := t.tmp.Close(); t.err == nil {
		t.err = cerr
	}
	if t.eof && t.err == nil {
		if os.Rename(name, t.path) == nil {
			return err
		}
	}
	os.Remove(name)
	return err
}

// gcFlat removes the copies of images not in keep (manifest digests) and
// leftovers of interrupted copies.
func (s *Store) gcFlat(keep map[string]bool) error {
	ents, err := os.ReadDir(s.flatDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		hex, ok := strings.CutSuffix(e.Name(), ".tar")
		if ok && keep[hex] {
			continue
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			// Possibly being written by a run right now.
			if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < time.Hour {
				continue
			}
		}
		if err := os.Remove(filepath.Join(s.flatDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
