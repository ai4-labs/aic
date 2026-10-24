package rootfs

import (
	"archive/tar"
	"bytes"
	"reflect"
	"testing"
)

// tarEntry describes an entry for mkTar.
type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func paths(t *testing.T, tr *Tree) []string {
	var out []string
	if err := tr.Walk("", func(p string, e *Entry) error {
		if p != "" {
			out = append(out, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApplyWhiteouts(t *testing.T) {
	tr := New()
	must(t, tr.Apply(mkTar(t,
		tarEntry{name: "a/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a/x", body: "x"},
		tarEntry{name: "a/y", body: "y"},
		tarEntry{name: "b/c/d", body: "d"}, // implicit parents
		tarEntry{name: "etc/passwd", body: "root:x:0:0::/root:/bin/sh\n"},
	), 0))
	must(t, tr.Apply(mkTar(t,
		tarEntry{name: "a/.wh.x"},
		tarEntry{name: "b/.wh..wh..opq"},
		tarEntry{name: "b/new", body: "n"},
		tarEntry{name: "./c", body: "c"},
	), 1))
	if got, want := paths(t, tr), []string{"a", "a/y", "b", "b/new", "c", "etc", "etc/passwd"}; !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	if e, _ := tr.Get("b/new"); e.Layer != 1 {
		t.Errorf("b/new layer = %d", e.Layer)
	}
	if b, ok := tr.File("/etc/passwd"); !ok || string(b) != "root:x:0:0::/root:/bin/sh\n" {
		t.Errorf("kept passwd = %q, %v", b, ok)
	}
	// Whiteouts must not hide entries added by the same layer.
	tr2 := New()
	must(t, tr2.Apply(mkTar(t, tarEntry{name: "d/keep", body: "k"}, tarEntry{name: "d/.wh..wh..opq"}, tarEntry{name: "f", body: "f"}, tarEntry{name: ".wh.f"}), 0))
	if got, want := paths(t, tr2), []string{"d", "d/keep", "f"}; !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	if err := New().Apply(mkTar(t, tarEntry{name: "../evil", body: "x"}), 0); err == nil {
		t.Error("Apply accepted ../evil")
	}
}

func TestResolve(t *testing.T) {
	tr := New()
	must(t, tr.Apply(mkTar(t,
		tarEntry{name: "usr/lib/libc.so", body: "c"},
		tarEntry{name: "lib", typ: tar.TypeSymlink, link: "usr/lib"},
		tarEntry{name: "abs", typ: tar.TypeSymlink, link: "/usr"},
		tarEntry{name: "up", typ: tar.TypeSymlink, link: "../../../.."},
		tarEntry{name: "loop", typ: tar.TypeSymlink, link: "loop"},
		tarEntry{name: "usr/rel", typ: tar.TypeSymlink, link: "../lib"},
	), 0))
	for in, want := range map[string]string{
		"/lib/libc.so":    "usr/lib/libc.so",
		"abs/lib/libc.so": "usr/lib/libc.so",
		"up/etc/shadow":   "etc/shadow", // cannot escape the root
		"usr/rel/x":       "usr/lib/x",
		"missing/a/../b":  "missing/b",
		"/":               "",
	} {
		got, err := tr.Resolve(in, true)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, _ := tr.Resolve("lib", false); got != "lib" {
		t.Errorf("Resolve(lib, false) = %q", got)
	}
	if _, err := tr.Resolve("loop/x", true); err == nil {
		t.Error("Resolve(loop) succeeded")
	}
}

func TestDiff(t *testing.T) {
	base := New()
	must(t, base.Apply(mkTar(t,
		tarEntry{name: "keep", body: "same"},
		tarEntry{name: "mod", body: "old"},
		tarEntry{name: "chmod", body: "c"},
		tarEntry{name: "gone/a", body: "a"},
		tarEntry{name: "gone/b", body: "b"},
		tarEntry{name: "dir/x", body: "x"},
		tarEntry{name: "proc/", typ: tar.TypeDir},
	), 0))
	next := New()
	must(t, next.Apply(mkTar(t,
		tarEntry{name: "keep", body: "same"},
		tarEntry{name: "mod", body: "new"},
		tarEntry{name: "chmod", body: "c", mode: 0o755},
		tarEntry{name: "dir/y", body: "y"},
		tarEntry{name: "proc/1/status", body: "s"},
		tarEntry{name: "etc/hosts", body: "h"},
	), 0))
	skip := func(p string) bool { return p == "proc" || p == "etc/hosts" }
	changed, deleted := Diff(base, next, skip)
	if want := []string{"chmod", "dir/y", "etc", "mod"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %q, want %q", changed, want)
	}
	if want := []string{"dir/x", "gone"}; !reflect.DeepEqual(deleted, want) {
		t.Errorf("deleted = %q, want %q", deleted, want)
	}
}

func TestCloneIsDeep(t *testing.T) {
	a := New()
	must(t, a.Apply(mkTar(t, tarEntry{name: "d/f", body: "1"}), 0))
	b := a.Clone()
	must(t, b.Apply(mkTar(t, tarEntry{name: "d/g", body: "2"}), 1))
	if _, ok := a.Get("d/g"); ok {
		t.Error("clone shares state with original")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
