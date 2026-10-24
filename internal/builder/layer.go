package builder

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	sum     hash.Hash // over entries without times; see contentKey
}

func (b *builder) newLayer(s *stage) (*layerWriter, error) {
	if err := b.ensureTree(s); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(b.tmp, "layer-*.tar")
	if err != nil {
		return nil, err
	}
	return &layerWriter{f: f, tw: tar.NewWriter(f), tree: s.tree, written: map[string]bool{}, mtime: b.created.Time, sum: sha256.New()}, nil
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
	w.hashHeader(hdr)
	if r != nil && hdr.Typeflag == tar.TypeReg {
		n, err := io.Copy(io.MultiWriter(w.tw, w.sum), r)
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
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: dir + ".wh." + base, ModTime: w.mtime, Format: tar.FormatPAX}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	w.hashHeader(hdr)
	w.count++
	return nil
}

func (w *layerWriter) abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// finish closes the layer and returns it, or nil if it has no entries.
func (b *builder) finish(s *stage, w *layerWriter) (v1.Layer, error) {
	if err := w.tw.Close(); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.f.Close(); err != nil {
		return nil, err
	}
	if w.count == 0 {
		os.Remove(w.f.Name())
		return nil, nil
	}
	return tarball.LayerFromFile(w.f.Name(), tarball.WithMediaType(s.layerMT))
}

// appendLayer adds a layer to the stage, or just a history entry if
// layer is nil. The filesystem index catches up lazily (ensureTree).
func (s *stage) appendLayer(layer v1.Layer, h v1.History) {
	h.EmptyLayer = layer == nil
	if layer == nil {
		s.adds = append(s.adds, mutate.Addendum{History: h})
		return
	}
	s.layers = append(s.layers, layer)
	s.adds = append(s.adds, mutate.Addendum{Layer: layer, History: h, MediaType: s.layerMT})
}

// commit finishes the layer of the step with cache key key, appends it
// to the stage and records it in the build cache.
func (b *builder) commit(s *stage, w *layerWriter, ins *dockerfile.Instruction, key string) error {
	layer, err := b.finish(s, w)
	if err != nil {
		return err
	}
	h := b.history(ins, layer == nil)
	s.appendLayer(layer, h)
	s.key = key
	b.toCache(key, layer, h)
	return nil
}

// hashHeader adds an entry's metadata, except times, to the content key.
func (w *layerWriter) hashHeader(hdr *tar.Header) {
	fmt.Fprintf(w.sum, "%q %c %o %d %d %q %d %d %d", hdr.Name, hdr.Typeflag, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Linkname, hdr.Size, hdr.Devmajor, hdr.Devminor)
	keys := make([]string, 0, len(hdr.PAXRecords))
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w.sum, " %q=%q", k, hdr.PAXRecords[k])
	}
	w.sum.Write([]byte{'\n'})
}

// contentKey identifies what the layer writes, ignoring modification
// times: same files and metadata, same key.
func (w *layerWriter) contentKey() string { return hex.EncodeToString(w.sum.Sum(nil)) }
