//go:build linux

package container

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/rakyll/ai-container/internal/rootfs"
)

// Top-level paths that hold mounts, not image content.
var mountPoints = map[string]bool{"proc": true, "sys": true, "dev": true}

func underMount(name string) bool {
	top, _, _ := strings.Cut(name, "/")
	return mountPoints[top]
}

// intended is metadata from the image that could not be applied, e.g. an
// owner that isn't mapped in the user namespace. If the file's ctime is
// unchanged at export time, the command didn't touch it and the intended
// metadata is reported instead of what is on disk.
type intended struct {
	ctime    unix.Timespec
	uid, gid int
	xattrs   map[string]string
}

type extractState struct {
	meta    map[uint64]intended // by inode
	skipped []*tar.Header       // entries that could not be created at all
}

// extract unpacks a flattened image tar into /. It runs after pivot_root,
// so symlinks in the image can't point outside of the container.
func extract(r io.Reader) (*extractState, error) {
	st := &extractState{meta: map[uint64]intended{}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return st, nil
		}
		if err != nil {
			return nil, err
		}
		name, err := rootfs.CleanName(hdr.Name)
		if err != nil {
			return nil, err
		}
		if name == "" || underMount(name) {
			continue
		}
		if err := extractOne(st, tr, hdr, "/"+name); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
}

func extractOne(st *extractState, tr *tar.Reader, hdr *tar.Header, p string) error {
	if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil && !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	case tar.TypeReg, '\x00':
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := os.Symlink(hdr.Linkname, p); err != nil {
			return err
		}
	case tar.TypeLink:
		target, err := rootfs.CleanName(hdr.Linkname)
		if err != nil {
			return err
		}
		// Ownership and mode are shared with the target, already applied.
		return os.Link("/"+target, p)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		mode := uint32(hdr.Mode & 0o7777)
		switch hdr.Typeflag {
		case tar.TypeChar:
			mode |= unix.S_IFCHR
		case tar.TypeBlock:
			mode |= unix.S_IFBLK
		default:
			mode |= unix.S_IFIFO
		}
		if err := unix.Mknod(p, mode, int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))); err != nil {
			// Not permitted in a user namespace: carry the entry through
			// to the export unchanged.
			h := *hdr
			h.Name = strings.TrimPrefix(p, "/")
			st.skipped = append(st.skipped, &h)
			return nil
		}
	default:
		return nil
	}

	failed := false
	if err := unix.Lchown(p, hdr.Uid, hdr.Gid); err != nil {
		failed = true
	}
	if hdr.Typeflag != tar.TypeSymlink {
		if err := unix.Fchmodat(unix.AT_FDCWD, p, uint32(hdr.Mode&0o7777), 0); err != nil {
			return err
		}
	}
	xattrs := map[string]string{}
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			xattrs[name] = v
			if err := unix.Lsetxattr(p, name, []byte(v), 0); err != nil {
				failed = true
			}
		}
	}
	if hdr.Typeflag != tar.TypeDir {
		ts := []unix.Timespec{unix.NsecToTimespec(hdr.ModTime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
		_ = unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
	if failed {
		var s unix.Stat_t
		if err := unix.Lstat(p, &s); err != nil {
			return err
		}
		st.meta[s.Ino] = intended{ctime: s.Ctim, uid: hdr.Uid, gid: hdr.Gid, xattrs: xattrs}
	}
	return nil
}

// exportTree writes dir (the whole filesystem for "/") as a tar stream,
// with names relative to dir.
func exportTree(w io.Writer, st *extractState, dir string) error {
	tw := tar.NewWriter(w)
	links := map[uint64]string{} // first path seen for multiply-linked inodes
	dir = filepath.Clean("/" + dir)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return tw.Close() // nothing to send back
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if name == "" {
			return nil
		}
		if dir == "/" && mountPoints[name] {
			return filepath.SkipDir
		}
		var s unix.Stat_t
		if err := unix.Lstat(p, &s); err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    int64(s.Mode & 0o7777),
			Uid:     int(s.Uid),
			Gid:     int(s.Gid),
			ModTime: timeOf(s.Mtim),
			Format:  tar.FormatPAX,
		}
		switch s.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
		case unix.S_IFREG:
			hdr.Typeflag, hdr.Size = tar.TypeReg, s.Size
			if s.Nlink > 1 {
				if first, ok := links[s.Ino]; ok {
					hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
				} else {
					links[s.Ino] = name
				}
			}
		case unix.S_IFLNK:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
		case unix.S_IFCHR, unix.S_IFBLK:
			hdr.Typeflag = tar.TypeChar
			if s.Mode&unix.S_IFMT == unix.S_IFBLK {
				hdr.Typeflag = tar.TypeBlock
			}
			hdr.Devmajor, hdr.Devminor = int64(unix.Major(uint64(s.Rdev))), int64(unix.Minor(uint64(s.Rdev)))
		case unix.S_IFIFO:
			hdr.Typeflag = tar.TypeFifo
		default:
			return nil // sockets
		}
		xattrs := readXattrs(p)
		if m, ok := st.meta[s.Ino]; ok && m.ctime == s.Ctim {
			hdr.Uid, hdr.Gid, xattrs = m.uid, m.gid, m.xattrs
		}
		for k, v := range xattrs {
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords["SCHILY.xattr."+k] = v
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size == 0 {
			return nil
		}
		f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
	if err != nil {
		return err
	}
	for _, hdr := range st.skipped {
		if dir != "/" {
			break
		}
		if _, err := os.Lstat("/" + hdr.Name); err == nil || !errors.Is(err, fs.ErrNotExist) {
			continue // replaced by the command
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
	}
	return tw.Close()
}

func readXattrs(p string) map[string]string {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil || sz <= 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil
	}
	var out map[string]string
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		if name == "" || name == "security.selinux" {
			continue
		}
		vsz, err := unix.Lgetxattr(p, name, nil)
		if err != nil || vsz < 0 {
			continue
		}
		v := make([]byte, vsz)
		if vsz, err = unix.Lgetxattr(p, name, v); err != nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = string(v[:vsz])
	}
	return out
}
