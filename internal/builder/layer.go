package builder

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/rakyll/ai-container/internal/dockerfile"
	"github.com/rakyll/ai-container/internal/rootfs"
)

// layerWriter writes a layer tarball to a temporary file.
type layerWriter struct {
	f       *os.File
	tw      *tar.Writer
	tree    *rootfs.Tree // the stage's filesystem before this layer
	written map[string]bool
	count   int
	mtime   time.Time
}

func (b *builder) newLayer(s *stage) (*layerWriter, error) {
	if err := b.ensureTree(s); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(b.tmp, "layer-*.tar")
	if err != nil {
		return nil, err
	}
	return &layerWriter{f: f, tw: tar.NewWriter(f), tree: s.tree, written: map[string]bool{}, mtime: b.created.Time}, nil
}

// exists reports whether p is a directory, either already in the image
// or written to this layer.
func (w *layerWriter) isDir(p string) bool {
	if w.written[p] {
		return true
	}
	e, ok := w.tree.Get(p)
	return ok && e.IsDir()
}

// mkdirAll writes directory entries for dir and any missing parents.
// Directories that already exist are left alone.
func (w *layerWriter) mkdirAll(dir string, uid, gid int, mtime time.Time) error {
	dir = rootfs.Clean(dir)
	if dir == "" || w.isDir(dir) {
		return nil
	}
	if err := w.mkdirAll(path.Dir(dir), uid, gid, mtime); err != nil {
		return err
	}
	return w.write(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Uid: uid, Gid: gid, ModTime: mtime, Format: tar.FormatPAX}, nil)
}

// write writes an entry, creating missing parent directories as root.
func (w *layerWriter) write(hdr *tar.Header, r io.Reader) error {
	p := rootfs.Clean(hdr.Name)
	if p == "" {
		return nil
	}
	if parent := path.Dir(p); parent != "." && !w.isDir(parent) {
		if err := w.mkdirAll(parent, 0, 0, w.mtime); err != nil {
			return err
		}
	}
	if hdr.Typeflag == tar.TypeDir && !strings.HasSuffix(hdr.Name, "/") {
		hdr.Name += "/"
	}
	hdr.Uname, hdr.Gname = "", ""
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if r != nil && hdr.Typeflag == tar.TypeReg {
		n, err := io.Copy(w.tw, r)
		if err != nil {
			return err
		}
		if n != hdr.Size {
			return fmt.Errorf("%s: file changed while being copied", p)
		}
	}
	if hdr.Typeflag == tar.TypeDir {
		w.written[p] = true
	}
	w.count++
	return nil
}

// whiteout records the deletion of p.
func (w *layerWriter) whiteout(p string) error {
	dir, base := path.Split(p)
	if err := w.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: dir + ".wh." + base, ModTime: w.mtime, Format: tar.FormatPAX}); err != nil {
		return err
	}
	w.count++
	return nil
}

func (w *layerWriter) abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// commit closes the layer and appends it to the stage. A layer with no
// entries is recorded as an empty history entry instead.
func (b *builder) commit(s *stage, w *layerWriter, ins *dockerfile.Instruction) error {
	if err := w.tw.Close(); err != nil {
		w.abort()
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if w.count == 0 {
		os.Remove(w.f.Name())
		s.adds = append(s.adds, mutate.Addendum{History: b.history(ins, true)})
		return nil
	}
	layer, err := tarball.LayerFromFile(w.f.Name(), tarball.WithMediaType(s.layerMT))
	if err != nil {
		return err
	}
	f, err := os.Open(w.f.Name())
	if err != nil {
		return err
	}
	defer f.Close()
	idx := len(s.layers)
	if err := s.tree.Apply(f, idx); err != nil {
		return err
	}
	s.layers = append(s.layers, layer)
	s.adds = append(s.adds, mutate.Addendum{Layer: layer, History: b.history(ins, false), MediaType: s.layerMT})
	return nil
}
