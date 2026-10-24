package builder

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/rakyll/ai-container/internal/ignore"
	"github.com/rakyll/ai-container/internal/rootfs"
)

// copyItem is one entry to write in a COPY/ADD layer.
type copyItem struct {
	dst    string       // destination path in the image (resolved, relative to /)
	src    string       // canonical path in the source
	e      rootfs.Entry // source metadata; hard links are presented as regular files
	inline []byte       // content for heredocs
	file   string       // host file with the content (ADD <url>)
}

// source is where COPY reads files from: the build context, or the
// filesystem of another stage or image.
type source interface {
	// resolve follows symlinks in p without leaving the source's root.
	resolve(p string) (string, error)
	// lstat describes the canonical path p without following symlinks.
	lstat(p string) (*rootfs.Entry, bool, error)
	// readDir lists directory p, sorted.
	readDir(p string) ([]string, error)
	// fetch calls emit with the content of each regular-file item, in
	// any order.
	fetch(items []*copyItem, emit func(*copyItem, io.Reader) error) error
}

// contextSource reads the build context through an os.Root, so neither
// ".." nor symlinks can reach files outside of it.
type contextSource struct {
	root *os.Root
	ign  *ignore.Matcher
}

func openContext(dir, dockerfilePath string) (*contextSource, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	c := &contextSource{root: root}
	// A Dockerfile-specific ignore file (Dockerfile.dockerignore) wins
	// over the context's .dockerignore.
	var f io.ReadCloser
	if df, err := os.Open(dockerfilePath + ".dockerignore"); err == nil {
		f = df
	} else if rf, err := root.Open(".dockerignore"); err == nil {
		f = rf
	}
	if f != nil {
		defer f.Close()
		if c.ign, err = ignore.Parse(f); err != nil {
			root.Close()
			return nil, err
		}
	}
	return c, nil
}

func rootName(p string) string {
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

func (c *contextSource) rawLstat(p string) (bool, string, bool, error) {
	fi, err := c.root.Lstat(rootName(p))
	if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return false, "", true, nil
	}
	target, err := c.root.Readlink(rootName(p))
	if err != nil {
		return false, "", false, err
	}
	return true, filepath.ToSlash(target), true, nil
}

func isNotDir(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

func (c *contextSource) resolve(p string) (string, error) {
	return rootfs.Resolve(p, true, c.rawLstat)
}

func (c *contextSource) excluded(p string, isDir bool) bool {
	return c.ign.Excluded(p) && !(isDir && c.ign.HasExceptions())
}

func (c *contextSource) lstat(p string) (*rootfs.Entry, bool, error) {
	fi, err := c.root.Lstat(rootName(p))
	if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	e := &rootfs.Entry{Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}
	if fi.Mode()&fs.ModeSetuid != 0 {
		e.Mode |= 0o4000
	}
	if fi.Mode()&fs.ModeSetgid != 0 {
		e.Mode |= 0o2000
	}
	if fi.Mode()&fs.ModeSticky != 0 {
		e.Mode |= 0o1000
	}
	switch {
	case fi.Mode().IsRegular():
		e.Type, e.Size = rootfs.TypeReg, fi.Size()
	case fi.IsDir():
		e.Type = rootfs.TypeDir
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = rootfs.TypeSymlink
		if e.Linkname, err = c.root.Readlink(rootName(p)); err != nil {
			return nil, false, err
		}
		e.Linkname = filepath.ToSlash(e.Linkname)
	default:
		return nil, false, nil // sockets, devices and pipes are not copied
	}
	if p != "" && c.excluded(p, e.IsDir()) {
		return nil, false, nil
	}
	return e, true, nil
}

func (c *contextSource) readDir(p string) ([]string, error) {
	f, err := c.root.Open(rootName(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, de := range ents {
		if !c.excluded(path.Join(p, de.Name()), de.IsDir()) {
			names = append(names, de.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (c *contextSource) fetch(items []*copyItem, emit func(*copyItem, io.Reader) error) error {
	for _, it := range items {
		if err := c.fetchOne(it, emit); err != nil {
			return err
		}
	}
	return nil
}

func (c *contextSource) fetchOne(it *copyItem, emit func(*copyItem, io.Reader) error) error {
	f, err := c.root.Open(rootName(it.src))
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", it.src)
	}
	return emit(it, io.LimitReader(f, it.e.Size))
}

// treeSource reads from the filesystem of a stage or image. Content is
// streamed out of the layer that holds each file.
type treeSource struct {
	tree   *rootfs.Tree
	layers []v1.Layer
	tmp    string
}

func (t *treeSource) resolve(p string) (string, error) { return t.tree.Resolve(p, true) }

func (t *treeSource) lstat(p string) (*rootfs.Entry, bool, error) {
	e, ok := t.tree.Get(p)
	if !ok {
		return nil, false, nil
	}
	c := *e
	if c.Type == rootfs.TypeLink {
		c.Type = rootfs.TypeReg
	}
	return &c, true, nil
}

func (t *treeSource) readDir(p string) ([]string, error) { return t.tree.Children(p), nil }

func (t *treeSource) fetch(items []*copyItem, emit func(*copyItem, io.Reader) error) error {
	// Group items by the layer and tar path holding their bytes. A hard
	// link's bytes are under its target's name, in the link's layer.
	want := map[int]map[string][]*copyItem{}
	for _, it := range items {
		e, ok := t.tree.Get(it.src)
		if !ok {
			return fmt.Errorf("%s: not found", it.src)
		}
		holder := it.src
		if e.Type == rootfs.TypeLink {
			holder = e.Linkname
		}
		if e.Layer < 0 || e.Layer >= len(t.layers) {
			return fmt.Errorf("%s: no layer holds its content", it.src)
		}
		if want[e.Layer] == nil {
			want[e.Layer] = map[string][]*copyItem{}
		}
		want[e.Layer][holder] = append(want[e.Layer][holder], it)
	}
	idx := make([]int, 0, len(want))
	for i := range want {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, li := range idx {
		if err := t.scanLayer(li, want[li], emit); err != nil {
			return err
		}
	}
	return nil
}

func (t *treeSource) scanLayer(li int, want map[string][]*copyItem, emit func(*copyItem, io.Reader) error) error {
	rc, err := t.layers[li].Uncompressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for len(want) > 0 {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p, err := rootfs.CleanName(hdr.Name)
		if err != nil {
			return err
		}
		items := want[p]
		if len(items) == 0 || (hdr.Typeflag != tar.TypeReg && hdr.Typeflag != '\x00') {
			continue
		}
		delete(want, p)
		if len(items) == 1 {
			if err := emit(items[0], tr); err != nil {
				return err
			}
			continue
		}
		// Several destinations share this content: spool it once.
		if err := t.emitMany(items, tr, emit); err != nil {
			return err
		}
	}
	for p := range want {
		return fmt.Errorf("%s: content not found in layer %d", p, li)
	}
	return nil
}

func (t *treeSource) emitMany(items []*copyItem, r io.Reader, emit func(*copyItem, io.Reader) error) error {
	f, err := os.CreateTemp(t.tmp, "spool-*")
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := emit(it, f); err != nil {
			return err
		}
	}
	return nil
}

func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

// glob expands a source pattern. Matches keep the names as written
// (not symlink-resolved) so that destination names come out right.
func glob(src source, pat string) ([]string, error) {
	exists := func(p string) (bool, error) {
		r, err := src.resolve(p)
		if err != nil {
			return false, err
		}
		_, ok, err := src.lstat(r)
		return ok, err
	}
	if !hasMeta(pat) {
		ok, err := exists(pat)
		if err != nil || !ok {
			return nil, err
		}
		return []string{pat}, nil
	}
	cands := []string{""}
	for _, comp := range strings.Split(pat, "/") {
		var next []string
		for _, c := range cands {
			if !hasMeta(comp) {
				next = append(next, path.Join(c, comp))
				continue
			}
			r, err := src.resolve(c)
			if err != nil {
				return nil, err
			}
			names, err := src.readDir(r)
			if err != nil {
				continue // not a directory
			}
			for _, n := range names {
				ok, err := path.Match(comp, n)
				if err != nil {
					return nil, fmt.Errorf("bad pattern %q: %w", pat, err)
				}
				if ok {
					next = append(next, path.Join(c, n))
				}
			}
		}
		cands = next
	}
	var out []string
	for _, c := range cands {
		ok, err := exists(c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// walkSource calls fn for everything below dir (not dir itself), parents
// first. rel is the path relative to dir. Symlinks are not followed.
func walkSource(src source, dir, rel string, fn func(rel, p string, e *rootfs.Entry) error) error {
	names, err := src.readDir(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		p, r := path.Join(dir, n), path.Join(rel, n)
		e, ok, err := src.lstat(p)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := fn(r, p, e); err != nil {
			return err
		}
		if e.IsDir() {
			if err := walkSource(src, p, r, fn); err != nil {
				return err
			}
		}
	}
	return nil
}
