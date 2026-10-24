// Package rootfs keeps an in-memory index of a container root filesystem
// built from OCI layer tarballs.
//
// The index records metadata and content digests (not content) for every
// path. The builder uses it to resolve symlinks inside the image, to find
// which layer holds a file's bytes, and to diff a container's filesystem
// after RUN against the state before it.
package rootfs

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Entry types. They are the tar type flags.
const (
	TypeReg     = tar.TypeReg
	TypeLink    = tar.TypeLink
	TypeSymlink = tar.TypeSymlink
	TypeChar    = tar.TypeChar
	TypeBlock   = tar.TypeBlock
	TypeDir     = tar.TypeDir
	TypeFifo    = tar.TypeFifo
)

// Entry describes one path.
type Entry struct {
	Type     byte
	Mode     int64 // permission bits plus setuid, setgid and sticky
	UID, GID int
	Size     int64
	// Linkname is the symlink target, or for hard links the cleaned path
	// of the link target.
	Linkname           string
	Devmajor, Devminor int64
	ModTime            time.Time
	Xattrs             map[string]string
	// Digest is the SHA-256 of a regular file's content. Hard links carry
	// the digest of their target.
	Digest [32]byte
	// Layer is the index of the layer that last wrote this path.
	Layer int
}

func (e *Entry) IsDir() bool { return e.Type == TypeDir }

type node struct {
	Entry
	children map[string]*node
}

// keepFiles are small files whose content is retained, for resolving user
// and group names in COPY --chown.
var keepFiles = map[string]bool{"etc/passwd": true, "etc/group": true}

const maxKeep = 1 << 20

// Tree is a filesystem index.
type Tree struct {
	root *node
	kept map[string][]byte
}

// New returns an empty tree with a root directory.
func New() *Tree {
	return &Tree{
		root: &node{Entry: Entry{Type: TypeDir, Mode: 0o755}, children: map[string]*node{}},
		kept: map[string][]byte{},
	}
}

// Clone returns a deep copy.
func (t *Tree) Clone() *Tree {
	c := &Tree{root: cloneNode(t.root), kept: make(map[string][]byte, len(t.kept))}
	for k, v := range t.kept {
		c.kept[k] = v
	}
	return c
}

func cloneNode(n *node) *node {
	c := &node{Entry: n.Entry}
	if n.children != nil {
		c.children = make(map[string]*node, len(n.children))
		for k, v := range n.children {
			c.children[k] = cloneNode(v)
		}
	}
	return c
}

// Clean normalizes p to the tree's path form: slash-separated, relative to
// the root, without "." or ".." elements. The root is "".
func Clean(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func split(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (t *Tree) lookup(p string) *node {
	n := t.root
	for _, c := range split(p) {
		if n.children == nil {
			return nil
		}
		if n = n.children[c]; n == nil {
			return nil
		}
	}
	return n
}

// Get returns the entry at p without following symlinks.
func (t *Tree) Get(p string) (*Entry, bool) {
	n := t.lookup(Clean(p))
	if n == nil {
		return nil, false
	}
	return &n.Entry, true
}

// Children returns the sorted names in directory p.
func (t *Tree) Children(p string) []string {
	n := t.lookup(Clean(p))
	if n == nil || n.children == nil {
		return nil
	}
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// File returns the retained content of /etc/passwd or /etc/group.
func (t *Tree) File(p string) ([]byte, bool) {
	b, ok := t.kept[Clean(p)]
	return b, ok
}

// ensureDir returns the directory node at p, creating missing directories.
// A non-directory in the way is replaced, as an overlay filesystem would.
func (t *Tree) ensureDir(p string) *node {
	n := t.root
	for _, c := range split(p) {
		child := n.children[c]
		if child == nil || child.Type != TypeDir {
			child = &node{Entry: Entry{Type: TypeDir, Mode: 0o755, Layer: -1}, children: map[string]*node{}}
			n.children[c] = child
		}
		n = child
	}
	return n
}

func (t *Tree) put(p string, e Entry) {
	dir, base := path.Split(p)
	parent := t.ensureDir(strings.TrimSuffix(dir, "/"))
	if old := parent.children[base]; old != nil && old.Type == TypeDir && e.Type == TypeDir {
		old.Entry = e // keep the children
		return
	}
	n := &node{Entry: e}
	if e.Type == TypeDir {
		n.children = map[string]*node{}
	}
	parent.children[base] = n
	t.dropKept(p)
}

func (t *Tree) remove(p string) {
	dir, base := path.Split(p)
	if parent := t.lookup(strings.TrimSuffix(dir, "/")); parent != nil && parent.children != nil {
		delete(parent.children, base)
	}
	t.dropKept(p)
}

func (t *Tree) dropKept(p string) {
	for k := range t.kept {
		if k == p || strings.HasPrefix(k, p+"/") || p == "" {
			delete(t.kept, k)
		}
	}
}

// CleanName validates and normalizes a tar entry name. Names that try to
// climb out of the root are rejected rather than clamped.
func CleanName(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return "", fmt.Errorf("invalid path %q in layer", name)
		}
	}
	return Clean(name), nil
}

// Apply applies an uncompressed OCI layer tarball. layer is recorded in
// the entries it writes.
func (t *Tree) Apply(r io.Reader, layer int) error {
	tr := tar.NewReader(r)
	added := map[string]bool{} // whiteouts only hide lower layers
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := CleanName(hdr.Name)
		if err != nil {
			return err
		}
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")
		if base == ".wh..wh..opq" {
			d := t.ensureDir(dir)
			for k := range d.children {
				if !added[join(dir, k)] {
					delete(d.children, k)
					t.dropKept(join(dir, k))
				}
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			if target := join(dir, base[len(".wh."):]); !added[target] {
				t.remove(target)
			}
			continue
		}

		e, ok, err := EntryFromHeader(hdr)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		e.Layer = layer
		switch e.Type {
		case TypeReg:
			h := sha256.New()
			var w io.Writer = h
			var buf bytes.Buffer
			keep := keepFiles[name] && hdr.Size <= maxKeep
			if keep {
				w = io.MultiWriter(h, &buf)
			}
			if _, err := io.Copy(w, tr); err != nil {
				return err
			}
			copy(e.Digest[:], h.Sum(nil))
			if name == "" {
				continue
			}
			t.put(name, e)
			if keep {
				t.kept[name] = buf.Bytes()
			}
		case TypeLink:
			if name == "" {
				continue
			}
			var kept []byte
			if target := t.lookup(e.Linkname); target != nil && target.Type == TypeReg {
				e.Digest, e.Size = target.Digest, target.Size
				kept = t.kept[e.Linkname]
			}
			t.put(name, e)
			if kept != nil && keepFiles[name] {
				t.kept[name] = kept
			}
		default:
			if name == "" {
				if e.Type == TypeDir {
					t.root.Entry = e
				}
				continue
			}
			t.put(name, e)
		}
		added[name] = true
	}
}

// EntryFromHeader converts a tar header. ok is false for entry types that
// have no place in a filesystem (e.g. global PAX headers).
func EntryFromHeader(hdr *tar.Header) (e Entry, ok bool, err error) {
	e = Entry{
		Mode:     hdr.Mode & 0o7777,
		UID:      hdr.Uid,
		GID:      hdr.Gid,
		ModTime:  hdr.ModTime,
		Devmajor: hdr.Devmajor,
		Devminor: hdr.Devminor,
	}
	for k, v := range hdr.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			if e.Xattrs == nil {
				e.Xattrs = map[string]string{}
			}
			e.Xattrs[k] = v
		}
	}
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00':
		e.Type, e.Size = TypeReg, hdr.Size
	case tar.TypeLink:
		e.Type = TypeLink
		if e.Linkname, err = CleanName(hdr.Linkname); err != nil {
			return e, false, err
		}
	case tar.TypeSymlink:
		e.Type, e.Linkname = TypeSymlink, hdr.Linkname
	case tar.TypeDir, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		e.Type = hdr.Typeflag
	default:
		return e, false, nil
	}
	return e, true, nil
}

// Header returns a tar header for e at name.
func (e *Entry) Header(name string) *tar.Header {
	hdr := &tar.Header{
		Typeflag: e.Type,
		Name:     name,
		Mode:     e.Mode,
		Uid:      e.UID,
		Gid:      e.GID,
		ModTime:  e.ModTime,
		Devmajor: e.Devmajor,
		Devminor: e.Devminor,
		Format:   tar.FormatPAX,
	}
	switch e.Type {
	case TypeReg:
		hdr.Size = e.Size
	case TypeDir:
		hdr.Name += "/"
	case TypeSymlink, TypeLink:
		hdr.Linkname = e.Linkname
	}
	if len(e.Xattrs) > 0 {
		hdr.PAXRecords = map[string]string{}
		for k, v := range e.Xattrs {
			hdr.PAXRecords[k] = v
		}
	}
	return hdr
}

// ErrTooManyLinks is returned when symlink resolution does not terminate.
var ErrTooManyLinks = errors.New("too many levels of symbolic links")

// LstatFunc describes a path for Resolve. exists is false if the path
// does not exist; target is the symlink target if isLink.
type LstatFunc func(p string) (isLink bool, target string, exists bool, err error)

// Resolve follows symlinks in p the way the kernel would inside a chroot:
// absolute targets and ".." never leave the root. Missing trailing
// components are kept as-is. If followLast is false, a symlink in the last
// component is not followed.
func Resolve(p string, followLast bool, lstat LstatFunc) (string, error) {
	todo := split(Clean(p))
	resolved := ""
	hops := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			resolved = Clean(path.Dir("/" + resolved))
			continue
		}
		next := join(resolved, c)
		if len(todo) == 0 && !followLast {
			return next, nil
		}
		isLink, target, exists, err := lstat(next)
		if err != nil {
			return "", err
		}
		if !exists {
			return Clean(path.Join(append([]string{"/", next}, todo...)...)), nil
		}
		if !isLink {
			resolved = next
			continue
		}
		if hops++; hops > 255 {
			return "", fmt.Errorf("%s: %w", p, ErrTooManyLinks)
		}
		if path.IsAbs(target) {
			resolved = ""
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	return resolved, nil
}

// Resolve follows symlinks in p within the tree.
func (t *Tree) Resolve(p string, followLast bool) (string, error) {
	return Resolve(p, followLast, func(q string) (bool, string, bool, error) {
		n := t.lookup(q)
		if n == nil {
			return false, "", false, nil
		}
		return n.Type == TypeSymlink, n.Linkname, true, nil
	})
}

// SkipDir can be returned from a WalkFunc to skip a directory's contents.
var SkipDir = fs.SkipDir

// Walk calls fn for p and everything below it, in lexical order, parents
// before children. Symlinks are not followed.
func (t *Tree) Walk(p string, fn func(p string, e *Entry) error) error {
	p = Clean(p)
	n := t.lookup(p)
	if n == nil {
		return fs.ErrNotExist
	}
	err := walk(p, n, fn)
	if err == SkipDir {
		return nil
	}
	return err
}

func walk(p string, n *node, fn func(string, *Entry) error) error {
	if err := fn(p, &n.Entry); err != nil {
		return err
	}
	if n.children == nil {
		return nil
	}
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := walk(join(p, k), n.children[k], fn); err != nil && err != SkipDir {
			return err
		}
	}
	return nil
}

// Same reports whether two entries describe the same file. Modification
// times and directory sizes are ignored.
func Same(a, b *Entry) bool {
	if a.Type != b.Type || a.Mode != b.Mode || a.UID != b.UID || a.GID != b.GID ||
		a.Devmajor != b.Devmajor || a.Devminor != b.Devminor || len(a.Xattrs) != len(b.Xattrs) {
		return false
	}
	for k, v := range a.Xattrs {
		if b.Xattrs[k] != v {
			return false
		}
	}
	switch a.Type {
	case TypeReg:
		return a.Size == b.Size && a.Digest == b.Digest
	case TypeLink:
		return a.Linkname == b.Linkname && a.Digest == b.Digest
	case TypeSymlink:
		return a.Linkname == b.Linkname
	}
	return true
}

// Diff compares two trees. changed lists paths in next that are new or
// differ from prev, parents before children. deleted lists the topmost
// paths in prev that are gone from next. Paths for which skip returns
// true, and everything below them, are ignored.
func Diff(prev, next *Tree, skip func(string) bool) (changed, deleted []string) {
	_ = walk("", next.root, func(p string, e *Entry) error {
		if p == "" {
			return nil
		}
		if skip(p) {
			return SkipDir
		}
		if old := prev.lookup(p); old == nil || !Same(&old.Entry, e) {
			changed = append(changed, p)
		}
		return nil
	})
	_ = walk("", prev.root, func(p string, e *Entry) error {
		if p == "" {
			return nil
		}
		if skip(p) {
			return SkipDir
		}
		n := next.lookup(p)
		if n == nil {
			deleted = append(deleted, p)
			return SkipDir
		}
		if n.Type != TypeDir {
			return SkipDir // replaced by a non-directory; its children are gone with it
		}
		return nil
	})
	return changed, deleted
}
