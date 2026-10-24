package workspace

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, entries ...tarEntry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestPack(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.txt", "a")
	write(t, dir, "sub/b.txt", "bb")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if _, err := Pack(tw, dir, "workspace/"); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(h.Typeflag) + ":" + string(b) + h.Linkname
	}
	want := map[string]string{
		"workspace/":          "5:",
		"workspace/a.txt":     "0:a",
		"workspace/sub/":      "5:",
		"workspace/sub/b.txt": "0:bb",
		"workspace/link":      "2:/etc/passwd", // copied as a link, not followed
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func list(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += "->" + target
		} else if !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel += "=" + string(b)
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

func snapshot(t *testing.T, dir string) Snapshot {
	t.Helper()
	snap, err := Pack(tar.NewWriter(io.Discard), dir, "w/")
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestApply(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "same.txt", "same")
	write(t, dir, "changed.txt", "old")
	write(t, dir, "deleted.txt", "x")
	write(t, dir, "olddir/f", "x")
	write(t, dir, "becomes-link", "x")
	write(t, dir, "host-edit.txt", "before")
	sameBefore, _ := os.Stat(filepath.Join(dir, "same.txt"))
	base := snapshot(t, dir)
	// Edited on the host while the container ran, and not by the container.
	write(t, dir, "host-edit.txt", "after")
	write(t, dir, "host-new.txt", "new")

	ch, err := Apply(dir, mkTar(t,
		tarEntry{name: "host-edit.txt", typ: tar.TypeReg, body: "before"},
		tarEntry{name: "same.txt", typ: tar.TypeReg, body: "same"},
		tarEntry{name: "changed.txt", typ: tar.TypeReg, body: "new"},
		tarEntry{name: "new/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "new/script.sh", typ: tar.TypeReg, body: "#!/bin/sh", mode: 0o755},
		tarEntry{name: "becomes-link", typ: tar.TypeSymlink, link: "same.txt"},
		tarEntry{name: "hard", typ: tar.TypeLink, link: "changed.txt"},
	), base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"becomes-link->same.txt", "changed.txt=new", "hard=new", "host-edit.txt=after", "host-new.txt=new", "new", "new/script.sh=#!/bin/sh", "same.txt=same"}
	if got := list(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("dir = %v, want %v", got, want)
	}
	if ch.Written != 5 || ch.Removed != 2 {
		t.Errorf("changes = %+v, want 5 written (changed, new, script, link, hard), 2 removed", ch)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "new/script.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("script mode = %v", fi.Mode())
	}
	if after, _ := os.Stat(filepath.Join(dir, "same.txt")); !os.SameFile(sameBefore, after) {
		t.Error("an unchanged file was rewritten")
	}
}

func TestApplyRejectsEscapes(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ws")
	write(t, dir, "keep", "k")
	for _, tc := range []struct {
		name string
		tar  *bytes.Reader
	}{
		{"dotdot", mkTar(t, tarEntry{name: "../evil", typ: tar.TypeReg, body: "x"})},
		{"through symlink", mkTar(t,
			tarEntry{name: "keep", typ: tar.TypeReg, body: "k"},
			tarEntry{name: "out", typ: tar.TypeSymlink, link: parent},
			tarEntry{name: "out/evil", typ: tar.TypeReg, body: "x"})},
		{"hard link out", mkTar(t, tarEntry{name: "h", typ: tar.TypeLink, link: "../../etc/passwd"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Apply(dir, tc.tar, snapshot(t, dir)); err == nil {
				t.Error("Apply succeeded")
			}
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Fatal("wrote outside of the workspace")
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "keep")); string(b) != "k" {
				t.Error("workspace was changed")
			}
		})
	}
}
