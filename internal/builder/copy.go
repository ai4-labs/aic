package builder

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/rakyll/ai-container/internal/dockerfile"
	"github.com/rakyll/ai-container/internal/rootfs"
)

type copyFlags struct {
	from     string
	chown    string
	chmod    int64 // -1 if unset
	checksum string
}

func (b *builder) copyFlags(s *stage, ins *dockerfile.Instruction) (copyFlags, error) {
	cf := copyFlags{chmod: -1}
	isAdd := ins.Cmd == "add"
	for _, f := range ins.Flags {
		k, v, _ := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		ev, err := b.word(s, v)
		if err != nil {
			return cf, err
		}
		switch {
		case k == "from" && !isAdd:
			cf.from = ev
		case k == "chown":
			cf.chown = ev
		case k == "chmod":
			m, err := strconv.ParseUint(ev, 8, 32)
			if err != nil || m > 0o7777 {
				return cf, fmt.Errorf("--chmod=%s: must be an octal mode", ev)
			}
			cf.chmod = int64(m)
		case k == "checksum" && isAdd:
			if !strings.HasPrefix(ev, "sha256:") {
				return cf, fmt.Errorf("--checksum=%s: only sha256:<hex> is supported", ev)
			}
			cf.checksum = ev
		case k == "link":
			// Layers are always independent of their parents here; nothing to do.
		default:
			return cf, fmt.Errorf("--%s is not supported", k)
		}
	}
	return cf, nil
}

var heredocMarker = strings.NewReplacer(`"`, "", `'`, "", "-", "")

// heredocFor returns the heredoc a COPY source like <<EOF refers to.
func heredocFor(ins *dockerfile.Instruction, arg string) *dockerfile.Heredoc {
	if !strings.HasPrefix(arg, "<<") {
		return nil
	}
	n := heredocMarker.Replace(arg[2:])
	for i := range ins.Heredocs {
		if ins.Heredocs[i].Name == n {
			return &ins.Heredocs[i]
		}
	}
	return nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func isGit(s string) bool {
	return strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "git://") || strings.HasSuffix(strings.SplitN(s, "#", 2)[0], ".git")
}

// copyMatch is one expanded COPY/ADD source.
type copyMatch struct {
	pattern string              // as written
	name    string              // matched path in the source
	heredoc *dockerfile.Heredoc // <<EOF source
	url     string              // ADD <url>
}

func (b *builder) copy(ctx context.Context, s *stage, ins *dockerfile.Instruction) error {
	isAdd := ins.Cmd == "add"
	flags, err := b.copyFlags(s, ins)
	if err != nil {
		return err
	}
	if err := b.ensureTree(s); err != nil {
		return err
	}

	words := make([]string, len(ins.Args))
	for i, a := range ins.Args {
		if heredocFor(ins, a) != nil {
			words[i] = a
			continue
		}
		if words[i], err = b.expandArg(s, ins, a); err != nil {
			return err
		}
	}
	srcs, dest := words[:len(words)-1], words[len(words)-1]

	uid, gid := 0, 0
	if flags.chown != "" {
		if uid, gid, err = parseChown(flags.chown, s.tree); err != nil {
			return err
		}
	}

	var src source = b.ctxSrc
	if flags.from != "" {
		if src, err = b.fromSource(ctx, s, flags.from); err != nil {
			return err
		}
	}

	// Destination: relative to WORKDIR; a trailing slash means directory.
	destDir := strings.HasSuffix(dest, "/") || dest == "." || strings.HasSuffix(dest, "/.")
	if !path.IsAbs(dest) {
		wd := s.config.WorkingDir
		if wd == "" {
			wd = "/"
		}
		dest = path.Join(wd, dest)
	}
	destPath, err := s.tree.Resolve(dest, true)
	if err != nil {
		return err
	}
	if e, ok := s.tree.Get(destPath); ok && e.IsDir() {
		destDir = true
	}

	var matches []copyMatch
	for _, raw := range srcs {
		switch {
		case heredocFor(ins, raw) != nil:
			matches = append(matches, copyMatch{pattern: raw, heredoc: heredocFor(ins, raw)})
		case isAdd && isURL(raw):
			matches = append(matches, copyMatch{pattern: raw, url: raw})
		case isAdd && isGit(raw):
			return fmt.Errorf("%s: git sources are not supported", raw)
		case isURL(raw):
			return fmt.Errorf("%s: COPY does not download URLs; use ADD", raw)
		default:
			names, err := glob(src, rootfs.Clean(raw))
			if err != nil {
				return err
			}
			if len(names) == 0 {
				return fmt.Errorf("%s: no such file or directory", raw)
			}
			for _, n := range names {
				matches = append(matches, copyMatch{pattern: raw, name: n})
			}
		}
	}
	if len(matches) > 1 {
		destDir = true
	}

	lw, err := b.newLayer(s)
	if err != nil {
		return err
	}
	if err := b.copyInto(ctx, s, ins, lw, src, flags, uid, gid, destPath, destDir, matches); err != nil {
		lw.abort()
		return err
	}
	// The key covers exactly what the layer contains, so unchanged
	// sources reuse the cached layer and keep later steps cached.
	key := chainKey(s.key, ins.Cmd, lw.contentKey(), string(s.layerMT))
	if b.fromCache(s, key) {
		lw.abort()
		return nil
	}
	return b.commit(s, lw, ins, key)
}

// copyInto plans and writes the items of one COPY/ADD.
func (b *builder) copyInto(ctx context.Context, s *stage, ins *dockerfile.Instruction, lw *layerWriter, src source, flags copyFlags,
	uid, gid int, destPath string, destDir bool, matches []copyMatch) error {

	isAdd := ins.Cmd == "add"
	_, isContext := src.(*contextSource)
	var items []*copyItem
	target := func(base string) string {
		if destDir {
			return path.Join(destPath, base)
		}
		return destPath
	}
	if destDir {
		if err := lw.mkdirAll(destPath, uid, gid, b.created.Time); err != nil {
			return err
		}
	}

	add := func(m copyMatch) error {
		switch {
		case m.heredoc != nil:
			content := m.heredoc.Content
			if m.heredoc.Expand {
				var err error
				if content, err = dockerfile.ExpandVars(content, s.lookup, b.esc); err != nil {
					return err
				}
			}
			items = append(items, &copyItem{
				dst:    target(m.heredoc.Name),
				e:      rootfs.Entry{Type: rootfs.TypeReg, Mode: 0o644, Size: int64(len(content)), ModTime: b.created.Time},
				inline: []byte(content),
			})
		case m.url != "":
			u, err := url.Parse(m.url)
			if err != nil {
				return err
			}
			base := path.Base(u.Path)
			if destDir && (base == "/" || base == "." || base == "") {
				return fmt.Errorf("%s: cannot determine a file name; give a destination file path", m.url)
			}
			b.say("Downloading %s", u.Redacted())
			file, size, mtime, err := b.download(ctx, u, flags.checksum)
			if err != nil {
				return err
			}
			items = append(items, &copyItem{
				dst:  target(base),
				e:    rootfs.Entry{Type: rootfs.TypeReg, Mode: 0o600, Size: size, ModTime: mtime},
				file: file,
			})
		default:
			r, err := src.resolve(m.name)
			if err != nil {
				return err
			}
			e, ok, err := src.lstat(r)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s: no such file or directory", m.pattern)
			}
			if isAdd && isContext && e.Type == rootfs.TypeReg {
				done, err := b.maybeExtract(s, lw, src.(*contextSource), r, destPath, flags, uid, gid)
				if err != nil || done {
					return err
				}
			}
			if !e.IsDir() {
				items = append(items, &copyItem{dst: target(path.Base(m.name)), src: r, e: *e})
				return nil
			}
			// A directory's contents are copied, not the directory itself.
			if err := lw.mkdirAll(destPath, uid, gid, b.created.Time); err != nil {
				return err
			}
			return walkSource(src, r, "", func(rel, p string, e *rootfs.Entry) error {
				items = append(items, &copyItem{dst: path.Join(destPath, rel), src: p, e: *e})
				return nil
			})
		}
		return nil
	}
	for _, m := range matches {
		if err := add(m); err != nil {
			return err
		}
	}

	header := func(it *copyItem) (*tar.Header, error) {
		// Follow symlinks already in the image for the parent directory.
		parent, err := s.tree.Resolve(path.Dir("/"+it.dst), true)
		if err != nil {
			return nil, err
		}
		it.dst = path.Join(parent, path.Base(it.dst))
		hdr := it.e.Header(it.dst)
		hdr.Uid, hdr.Gid = uid, gid
		if flags.chmod >= 0 && it.e.Type != rootfs.TypeSymlink {
			hdr.Mode = flags.chmod
		}
		if hdr.ModTime.IsZero() {
			hdr.ModTime = b.created.Time
		}
		return hdr, nil
	}

	// Directories, symlinks and inline content first, then file content.
	var files []*copyItem
	for _, it := range items {
		if it.e.Type == rootfs.TypeReg && it.inline == nil && it.file == "" {
			files = append(files, it)
			continue
		}
		hdr, err := header(it)
		if err != nil {
			return err
		}
		var r io.Reader
		switch {
		case it.inline != nil:
			r = bytes.NewReader(it.inline)
		case it.file != "":
			f, err := os.Open(it.file)
			if err != nil {
				return err
			}
			err = lw.write(hdr, f)
			f.Close()
			if err != nil {
				return err
			}
			continue
		}
		if err := lw.write(hdr, r); err != nil {
			return err
		}
	}
	return src.fetch(files, func(it *copyItem, r io.Reader) error {
		hdr, err := header(it)
		if err != nil {
			return err
		}
		return lw.write(hdr, r)
	})
}

// fromSource returns the source for COPY --from: an earlier stage by
// name or index, or else an image.
func (b *builder) fromSource(ctx context.Context, s *stage, from string) (source, error) {
	for _, st := range b.stages[:s.index] {
		if st == nil {
			continue
		}
		if (st.name != "" && st.name == strings.ToLower(from)) || strconv.Itoa(st.index) == from {
			if err := b.ensureTree(st); err != nil {
				return nil, err
			}
			return &treeSource{tree: st.tree, layers: st.layers, tmp: b.tmp}, nil
		}
	}
	for _, d := range b.defs {
		if d.name == strings.ToLower(from) {
			return nil, fmt.Errorf("--from=%s: stage is not before this one", from)
		}
	}
	key := from + "|" + platformString(s.platform)
	st, ok := b.images[key]
	if !ok {
		img, err := b.baseImage(ctx, from, s.platform)
		if err != nil {
			return nil, err
		}
		st = &stage{platform: s.platform}
		if err := st.setBase(img); err != nil {
			return nil, err
		}
		if err := b.ensureTree(st); err != nil {
			return nil, err
		}
		b.images[key] = st
	}
	return &treeSource{tree: st.tree, layers: st.layers, tmp: b.tmp}, nil
}

// parseChown parses user[:group], by number or by name from the image's
// /etc/passwd and /etc/group. Without a group, the GID equals the UID.
func parseChown(spec string, tree *rootfs.Tree) (int, int, error) {
	u, g, hasGroup := strings.Cut(spec, ":")
	uid, err := lookupID(u, tree, "etc/passwd")
	if err != nil {
		return 0, 0, err
	}
	gid := uid
	if hasGroup {
		if gid, err = lookupID(g, tree, "etc/group"); err != nil {
			return 0, 0, err
		}
	}
	return uid, gid, nil
}

func lookupID(v string, tree *rootfs.Tree, file string) (int, error) {
	if v == "" {
		return 0, fmt.Errorf("--chown: empty user or group")
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("--chown: invalid id %d", n)
		}
		return n, nil
	}
	resolved, err := tree.Resolve(file, true)
	if err != nil {
		return 0, err
	}
	data, _ := tree.File(resolved)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 3 && fields[0] == v {
			n, err := strconv.Atoi(fields[2])
			if err != nil {
				return 0, fmt.Errorf("/%s: bad id for %s", file, v)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("--chown: %q not found in /%s", v, file)
}

// download fetches an ADD URL into a temporary file.
func (b *builder) download(ctx context.Context, u *url.URL, checksum string) (string, int64, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	req.Header.Set("User-Agent", "aic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, time.Time{}, fmt.Errorf("%s: %s", u.Redacted(), resp.Status)
	}
	f, err := os.CreateTemp(b.tmp, "download-*")
	if err != nil {
		return "", 0, time.Time{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if checksum != "" {
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != checksum {
			return "", 0, time.Time{}, fmt.Errorf("%s: checksum mismatch: got %s, want %s", u.Redacted(), got, checksum)
		}
	}
	mtime := b.created.Time
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		mtime = lm
	}
	return f.Name(), n, mtime, nil
}

// maybeExtract unpacks a local tar archive (plain, gzip, bzip2 or zstd)
// into destDir, as ADD does. It reports false if p is not an archive.
func (b *builder) maybeExtract(s *stage, lw *layerWriter, c *contextSource, p, destDir string, flags copyFlags, uid, gid int) (bool, error) {
	open := func() (io.ReadCloser, error) { return c.root.Open(rootName(p)) }
	r, closeFn, err := archiveReader(open)
	if err != nil || r == nil {
		return false, err
	}
	defer closeFn()
	if err := lw.mkdirAll(destDir, uid, gid, b.created.Time); err != nil {
		return true, err
	}
	links := map[string]bool{} // symlinks written by this archive
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return true, fmt.Errorf("%s: %w", p, err)
		}
		name, err := rootfs.CleanName(hdr.Name)
		if err != nil {
			return true, fmt.Errorf("%s: %w", p, err)
		}
		if name == "" {
			continue
		}
		e, ok, err := rootfs.EntryFromHeader(hdr)
		if err != nil {
			return true, fmt.Errorf("%s: %w", p, err)
		}
		if !ok {
			continue
		}
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			if links[d] {
				return true, fmt.Errorf("%s: entry %q is beneath symlink %q", p, name, d)
			}
		}
		parent, err := s.tree.Resolve(path.Join(destDir, path.Dir(name)), true)
		if err != nil {
			return true, err
		}
		dst := path.Join(parent, path.Base(name))
		if e.Type == rootfs.TypeLink {
			e.Linkname = path.Join(destDir, e.Linkname)
		}
		if e.Type == rootfs.TypeSymlink {
			links[name] = true
		}
		h := e.Header(dst)
		if flags.chown != "" {
			h.Uid, h.Gid = uid, gid
		}
		if flags.chmod >= 0 && e.Type != rootfs.TypeSymlink {
			h.Mode = flags.chmod
		}
		if err := lw.write(h, tr); err != nil {
			return true, err
		}
	}
}

// archiveReader returns a reader of the decompressed tar stream, or nil
// if the file is not a tar archive. Compressed files that don't contain a
// tar archive are not archives (ADD copies them as-is).
func archiveReader(open func() (io.ReadCloser, error)) (io.Reader, func(), error) {
	f, err := open()
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(f, 1024)
	magic, _ := br.Peek(6)
	var r io.Reader
	closeFn := func() { f.Close() }
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, nil
		}
		r = gz
	case bytes.HasPrefix(magic, []byte("BZh")):
		r = bzip2.NewReader(br)
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, nil
		}
		r = zr
		closeFn = func() { zr.Close(); f.Close() }
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		f.Close()
		return nil, nil, fmt.Errorf("xz archives are not supported by ADD; use gzip, bzip2 or zstd")
	default:
		r = br
	}
	// A tar archive has "ustar" at offset 257 of its first header.
	tb := bufio.NewReaderSize(r, 1024)
	head, _ := tb.Peek(262)
	if len(head) < 262 || !bytes.Equal(head[257:262], []byte("ustar")) {
		closeFn()
		return nil, nil, nil
	}
	return tb, closeFn, nil
}
