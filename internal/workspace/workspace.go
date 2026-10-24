// Package workspace copies a host directory into a container and the
// container's changes back out.
//
// Both directions go through tar streams and an os.Root, so neither a
// symlink in the workspace nor a name in the container's export can
// reach files outside of the directory. Changes are computed against a
// snapshot taken while packing, so files edited on the host during the
// run are left alone unless the container changed them too.
package workspace

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/rakyll/ai-container/internal/rootfs"
)

// state identifies an entry's type, permissions and contents.
type state struct {
	typ  byte
	mode fs.FileMode
	sum  [sha256.Size]byte // regular files
	link string            // symlinks
}

// Snapshot records the workspace as it was copied into the container.
type Snapshot map[string]state

// Pack writes the contents of dir to tw with names under prefix (which
// ends in a slash), starting with prefix itself, and returns what it
// wrote. Only directories, regular files and symlinks are copied; owners
// are left to the container.
func Pack(tw *tar.Writer, dir, prefix string) (Snapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snap := Snapshot{}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := root.Lstat(name)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    prefix + name,
			Mode:    int64(fi.Mode().Perm()),
			ModTime: fi.ModTime(),
			Format:  tar.FormatPAX,
		}
		if name == "." {
			hdr.Name = prefix
		}
		st := state{mode: fi.Mode().Perm()}
		switch {
		case fi.IsDir():
			hdr.Typeflag = tar.TypeDir
			if name != "." {
				hdr.Name += "/"
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
			st.link = target
		case fi.Mode().IsRegular():
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.CopyN(io.MultiWriter(tw, h), f, hdr.Size); err != nil {
				return err
			}
			st.typ = tar.TypeReg
			h.Sum(st.sum[:0])
			snap[name] = st
			return nil
		default:
			return nil // sockets, devices, pipes
		}
		st.typ = hdr.Typeflag
		if name != "." {
			snap[name] = st
		}
		return tw.WriteHeader(hdr)
	})
	return snap, err
}

// Changes counts what Apply did.
type Changes struct {
	Written, Removed int
}

// Apply copies the container's changes in export (a tar stream with
// names relative to the workspace) to dir: entries that differ from base
// are written, and entries in base that are missing from export are
// removed. The whole stream is read and checked before anything changes.
func Apply(dir string, export io.ReadSeeker, base Snapshot) (Changes, error) {
	var ch Changes
	got := Snapshot{}
	err := entries(export, func(name string, hdr *tar.Header, r io.Reader) error {
		st := state{typ: hdr.Typeflag, mode: fs.FileMode(hdr.Mode).Perm(), link: hdr.Linkname}
		switch hdr.Typeflag {
		case tar.TypeReg:
			h := sha256.New()
			if _, err := io.Copy(h, r); err != nil {
				return err
			}
			h.Sum(st.sum[:0])
			st.link = ""
		case tar.TypeLink:
			// A copy of its target, which comes first.
			target, _ := rootfs.CleanName(hdr.Linkname)
			t, ok := got[target]
			if !ok || t.typ != tar.TypeReg {
				return fmt.Errorf("%s: hard link to unknown file %s", name, target)
			}
			st.typ, st.sum, st.link = tar.TypeReg, t.sum, ""
		case tar.TypeDir, tar.TypeSymlink:
		default:
			return nil // not copied back
		}
		got[name] = st
		return nil
	})
	if err != nil {
		return ch, fmt.Errorf("reading the workspace from the container: %w", err)
	}
	if _, err := export.Seek(0, io.SeekStart); err != nil {
		return ch, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ch, err
	}
	defer root.Close()
	err = entries(export, func(name string, hdr *tar.Header, r io.Reader) error {
		if b, ok := base[name]; ok && b == got[name] {
			return nil // unchanged by the container
		}
		changed, err := applyOne(root, name, hdr, r)
		if changed {
			ch.Written++
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return ch, err
	}
	// Remove what the container deleted, topmost paths first.
	var gone []string
	for name := range base {
		if _, ok := got[name]; !ok {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	var removed string
	for _, name := range gone {
		if removed != "" && strings.HasPrefix(name, removed+"/") {
			continue
		}
		if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := root.RemoveAll(name); err != nil {
			return ch, err
		}
		removed = name
		ch.Removed++
	}
	return ch, nil
}

// entries calls fn for each entry with a valid, non-empty name, and
// fails on names that would leave the directory.
func entries(r io.Reader, fn func(name string, hdr *tar.Header, r io.Reader) error) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := rootfs.CleanName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if hdr.Typeflag == tar.TypeLink {
			if _, err := rootfs.CleanName(hdr.Linkname); err != nil {
				return err
			}
		}
		if err := fn(name, hdr, tr); err != nil {
			return err
		}
	}
}

func applyOne(root *os.Root, name string, hdr *tar.Header, r io.Reader) (bool, error) {
	mode := fs.FileMode(hdr.Mode).Perm()
	cur, err := root.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if exists && cur.IsDir() {
			if cur.Mode().Perm() != mode {
				return true, root.Chmod(name, mode)
			}
			return false, nil
		}
		if exists {
			if err := root.RemoveAll(name); err != nil {
				return false, err
			}
		}
		if err := root.Mkdir(name, 0o700); err != nil {
			return false, err
		}
		return true, root.Chmod(name, mode)
	case tar.TypeSymlink:
		if exists && cur.Mode()&fs.ModeSymlink != 0 {
			if target, err := root.Readlink(name); err == nil && target == hdr.Linkname {
				return false, nil
			}
		}
		if exists {
			if err := root.RemoveAll(name); err != nil {
				return false, err
			}
		}
		return true, root.Symlink(hdr.Linkname, name)
	case tar.TypeLink:
		// Copy the target, which is already in place.
		target, _ := rootfs.CleanName(hdr.Linkname)
		f, err := root.Open(target)
		if err != nil {
			return false, err
		}
		defer f.Close()
		return writeFile(root, name, cur, exists, mode, f)
	case tar.TypeReg:
		return writeFile(root, name, cur, exists, mode, r)
	}
	return false, nil
}

// writeFile replaces name with the contents of r, unless it already has
// them, through a temporary file so a failure leaves the old one intact.
func writeFile(root *os.Root, name string, cur fs.FileInfo, exists bool, mode fs.FileMode, r io.Reader) (bool, error) {
	tmp := path.Join(path.Dir(name), fmt.Sprintf(".%s.aic-%d", path.Base(name), os.Getpid()))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		root.Remove(tmp)
		return false, err
	}
	if exists && cur.Mode().IsRegular() && sameContents(root, name, cur.Size(), h.Sum(nil)) {
		root.Remove(tmp)
		if cur.Mode().Perm() != mode {
			return true, root.Chmod(name, mode)
		}
		return false, nil
	}
	if err := root.Chmod(tmp, mode); err != nil {
		root.Remove(tmp)
		return false, err
	}
	if exists && (cur.IsDir() || cur.Mode()&fs.ModeSymlink != 0) {
		if err := root.RemoveAll(name); err != nil {
			root.Remove(tmp)
			return false, err
		}
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		return false, err
	}
	return true, nil
}

func sameContents(root *os.Root, name string, size int64, sum []byte) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() != size {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), sum)
}
