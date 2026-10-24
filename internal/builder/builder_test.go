package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/rakyll/ai-container/internal/executor"
	"github.com/rakyll/ai-container/internal/store"
)

// file is a flattened filesystem entry, for assertions.
type file struct {
	typ      byte
	body     string
	mode     int64
	uid, gid int
	link     string
}

func flatten(t *testing.T, img v1.Image) map[string]file {
	t.Helper()
	rc := mutate.Extract(img)
	defer rc.Close()
	out := map[string]file{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "/"), "/")] = file{
			typ: hdr.Typeflag, body: string(b), mode: hdr.Mode, uid: hdr.Uid, gid: hdr.Gid, link: hdr.Linkname,
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type testEnv struct {
	t     *testing.T
	ctx   string
	store *store.Store
	exec  executor.Executor
	out   bytes.Buffer
}

func newEnv(t *testing.T, files map[string]string) *testEnv {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, ctx: t.TempDir(), store: s}
	writeFiles(t, e.ctx, files)
	return e
}

func (e *testEnv) build(opts Options) (v1.Image, error) {
	opts.ContextDir = e.ctx
	opts.Store = e.store
	opts.Platform = v1.Platform{OS: "linux", Architecture: "arm64"}
	opts.Created = time.Unix(1700000000, 0)
	opts.Progress = &e.out
	if e.exec != nil {
		opts.NewExecutor = func() (executor.Executor, error) { return e.exec, nil }
	}
	return Build(context.Background(), opts)
}

func (e *testEnv) mustBuild(opts Options) v1.Image {
	e.t.Helper()
	img, err := e.build(opts)
	if err != nil {
		e.t.Fatalf("Build: %v\noutput:\n%s", err, e.out.String())
	}
	return img
}

func TestBuildWithoutRun(t *testing.T) {
	e := newEnv(t, map[string]string{
		"Dockerfile": `ARG VERSION=1.0
FROM scratch
ARG VERSION
ARG NAME=default
ENV APP_HOME=/srv/app GREETING="hello world"
WORKDIR $APP_HOME
COPY main.txt ./
COPY src/ lib/
COPY *.md docs/
COPY --chown=1000:1001 --chmod=0700 main.txt /owned
COPY <<EOF /etc/motd
v=${VERSION} n=${NAME}
EOF
LABEL version=$VERSION name="$NAME"
USER 1000
EXPOSE 8080 53/udp
VOLUME /data
ENTRYPOINT ["/app"]
CMD serve --port 8080
`,
		".dockerignore": "secret.md\nsrc/tmp\n",
		"main.txt":      "main",
		"src/a/b.txt":   "b",
		"src/tmp/x":     "ignored",
		"README.md":     "readme",
		"secret.md":     "secret",
	})
	ref, _ := store.ParseLocal("app:test")
	img := e.mustBuild(Options{Tags: []name.Reference{ref}, BuildArgs: map[string]string{"NAME": "aic"}})

	got := flatten(t, img)
	for p, want := range map[string]file{
		"srv/app/main.txt":       {typ: tar.TypeReg, body: "main", mode: 0o644},
		"srv/app/lib/a/b.txt":    {typ: tar.TypeReg, body: "b", mode: 0o644},
		"srv/app/docs/README.md": {typ: tar.TypeReg, body: "readme", mode: 0o644},
		"owned":                  {typ: tar.TypeReg, body: "main", mode: 0o700, uid: 1000, gid: 1001},
		"etc/motd":               {typ: tar.TypeReg, body: "v=1.0 n=aic\n", mode: 0o644},
	} {
		if g := got[p]; g != want {
			t.Errorf("%s = %+v, want %+v", p, g, want)
		}
	}
	for _, p := range []string{"srv/app/docs/secret.md", "srv/app/lib/tmp/x", "srv/app/lib/tmp", "Dockerfile"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s should not be in the image", p)
		}
	}
	if g := got["srv/app"]; g.typ != tar.TypeDir {
		t.Errorf("WORKDIR not created: %+v", g)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cf.Config
	if want := []string{"APP_HOME=/srv/app", "GREETING=hello world"}; !reflect.DeepEqual(c.Env, want) {
		t.Errorf("Env = %q, want %q", c.Env, want)
	}
	if c.WorkingDir != "/srv/app" || c.User != "1000" {
		t.Errorf("WorkingDir=%q User=%q", c.WorkingDir, c.User)
	}
	if want := map[string]string{"version": "1.0", "name": "aic"}; !reflect.DeepEqual(c.Labels, want) {
		t.Errorf("Labels = %v", c.Labels)
	}
	if _, ok := c.ExposedPorts["53/udp"]; !ok || len(c.ExposedPorts) != 2 {
		t.Errorf("ExposedPorts = %v", c.ExposedPorts)
	}
	if !reflect.DeepEqual(c.Entrypoint, []string{"/app"}) || !reflect.DeepEqual(c.Cmd, []string{"/bin/sh", "-c", "serve --port 8080"}) {
		t.Errorf("Entrypoint=%q Cmd=%q", c.Entrypoint, c.Cmd)
	}
	if cf.OS != "linux" || cf.Architecture != "arm64" || !cf.Created.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("os/arch/created = %s/%s %v", cf.OS, cf.Architecture, cf.Created)
	}
	// History entries must line up with layers.
	layers, _ := img.Layers()
	nonEmpty := 0
	for _, h := range cf.History {
		if !h.EmptyLayer {
			nonEmpty++
		}
	}
	if nonEmpty != len(layers) {
		t.Errorf("%d non-empty history entries for %d layers", nonEmpty, len(layers))
	}

	stored, err := e.store.Image("app:test")
	if err != nil {
		t.Fatal(err)
	}
	if d1, _ := stored.Digest(); true {
		if d2, _ := img.Digest(); d1 != d2 {
			t.Errorf("stored digest %s != built %s", d1, d2)
		}
	}
}

func TestMultiStageAndPruning(t *testing.T) {
	e := newEnv(t, map[string]string{
		"Dockerfile": `FROM scratch AS build
COPY out/ /dist/
RUN ln -s app /dist/link

FROM scratch AS broken
COPY does-not-exist /

FROM scratch
COPY --from=build /dist/bin/app /usr/bin/
COPY --from=0 /dist /opt
`,
		"out/bin/app": "binary",
	})
	e.exec = &fakeExec{fn: func(fs *memFS, args []string) { fs.symlink("dist/link", "app") }}
	img := e.mustBuild(Options{})
	got := flatten(t, img)
	if got["usr/bin/app"].body != "binary" {
		t.Errorf("usr/bin/app = %+v", got["usr/bin/app"])
	}
	if got["opt/bin/app"].body != "binary" || got["opt/link"].link != "app" {
		t.Errorf("opt = %+v / %+v", got["opt/bin/app"], got["opt/link"])
	}
	if !strings.Contains(e.out.String(), "STEP 1/3: FROM scratch AS build") || strings.Contains(e.out.String(), "does-not-exist") {
		t.Errorf("unexpected progress output:\n%s", e.out.String())
	}

	// --target builds the named stage.
	img = e.mustBuild(Options{Target: "build"})
	if _, ok := flatten(t, img)["dist/bin/app"]; !ok {
		t.Error("target stage missing dist/bin/app")
	}
	if _, err := e.build(Options{Target: "broken"}); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("broken target err = %v", err)
	}
}

// memFS is an in-memory filesystem used by fakeExec.
type memFS struct {
	order []string
	ents  map[string]*memEnt
}

type memEnt struct {
	hdr  tar.Header
	body []byte
}

func (m *memFS) write(p, body string, mode int64) {
	if _, ok := m.ents[p]; !ok {
		m.order = append(m.order, p)
	}
	m.ents[p] = &memEnt{hdr: tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: mode, Size: int64(len(body))}, body: []byte(body)}
}

func (m *memFS) symlink(p, target string) {
	m.order = append(m.order, p)
	m.ents[p] = &memEnt{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: p, Linkname: target, Mode: 0o777}}
}

func (m *memFS) remove(prefix string) {
	for p := range m.ents {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			delete(m.ents, p)
		}
	}
}

// fakeExec "runs" a command by flattening the image, applying fn, and
// exporting the result, like a real container runtime would.
type fakeExec struct {
	fn    func(fs *memFS, args []string)
	specs []executor.Spec
}

func (f *fakeExec) Close() error { return nil }

func (f *fakeExec) Run(_ context.Context, spec executor.Spec, out io.Writer) error {
	f.specs = append(f.specs, spec)
	fs := &memFS{ents: map[string]*memEnt{}}
	rc := mutate.Extract(spec.Image)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(tr)
		p := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "/"), "/")
		fs.order = append(fs.order, p)
		fs.ents[p] = &memEnt{hdr: *hdr, body: b}
	}
	// Runtime-managed files that must not end up in layers.
	fs.write("etc/hosts", "127.0.0.1 localhost\n", 0o644)
	fs.write("proc/self/status", "x", 0o444)
	if spec.Args[len(spec.Args)-1] == "false" {
		return &executor.ExitError{Code: 1}
	}
	f.fn(fs, spec.Args)
	tw := tar.NewWriter(out)
	for _, p := range fs.order {
		ent, ok := fs.ents[p]
		if !ok {
			continue
		}
		h := ent.hdr
		h.Name = p
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
		if _, err := tw.Write(ent.body); err != nil {
			return err
		}
	}
	return tw.Close()
}

func TestRunDiff(t *testing.T) {
	e := newEnv(t, map[string]string{
		"Dockerfile": `FROM scratch
COPY root/ /
ARG TOKEN
ENV MODE=prod
WORKDIR /work
USER 7
RUN --network=none do-things
RUN true
`,
		"root/keep":       "keep",
		"root/modify":     "old",
		"root/gone/a":     "a",
		"root/etc/passwd": "root:x:0:0::/:/bin/sh\n",
	})
	fe := &fakeExec{fn: func(fs *memFS, args []string) {
		if args[len(args)-1] != "do-things" {
			return
		}
		fs.write("modify", "new", 0o644)
		fs.write("added/file", "added", 0o600)
		fs.remove("gone")
	}}
	e.exec = fe
	img := e.mustBuild(Options{BuildArgs: map[string]string{"TOKEN": "s3cret"}})

	got := flatten(t, img)
	if got["modify"].body != "new" || got["added/file"].body != "added" || got["keep"].body != "keep" {
		t.Errorf("filesystem = %+v", got)
	}
	for _, p := range []string{"gone", "gone/a", "etc/hosts", "proc/self/status"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s should not be in the image", p)
		}
	}

	spec := fe.specs[0]
	if want := []string{"/bin/sh", "-c", "do-things"}; !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %q", spec.Args)
	}
	sort.Strings(spec.Env)
	if want := []string{"MODE=prod", "TOKEN=s3cret"}; !reflect.DeepEqual(spec.Env, want) {
		t.Errorf("Env = %q, want %q", spec.Env, want)
	}
	if spec.WorkDir != "/work" || spec.User != "7" || spec.Network != "none" {
		t.Errorf("spec = %+v", spec)
	}
	if n := fe.specs[1].Network; n != "host" {
		t.Errorf("default RUN network = %q, want host", n)
	}

	// The RUN layer holds only the changes, with a whiteout for "gone".
	layers, _ := img.Layers()
	cf, _ := img.ConfigFile()
	var runLayer v1.Layer
	li := 0
	for _, h := range cf.History {
		if h.EmptyLayer {
			continue
		}
		if strings.HasPrefix(h.CreatedBy, "RUN --network=none") {
			runLayer = layers[li]
		}
		li++
	}
	if runLayer == nil {
		t.Fatalf("no RUN layer in history %+v", cf.History)
	}
	names := layerNames(t, runLayer)
	if want := []string{".wh.gone", "added/", "added/file", "modify"}; !reflect.DeepEqual(names, want) {
		t.Errorf("RUN layer = %q, want %q", names, want)
	}
	// ARG values must not leak into the image config.
	for _, kv := range cf.Config.Env {
		if strings.Contains(kv, "s3cret") {
			t.Errorf("build arg leaked into Env: %q", kv)
		}
	}
	// "RUN true" changes nothing and adds no layer.
	if last := cf.History[len(cf.History)-1]; !last.EmptyLayer {
		t.Errorf("RUN true produced a layer: %+v", last)
	}

	// A failing command fails the build.
	writeFiles(t, e.ctx, map[string]string{"Dockerfile": "FROM scratch\nCOPY root/keep /\nRUN false\n"})
	if _, err := e.build(Options{}); err == nil || !strings.Contains(err.Error(), "exited with status 1") {
		t.Errorf("err = %v", err)
	}
}

func layerNames(t *testing.T, l v1.Layer) []string {
	t.Helper()
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var names []string
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}

func TestAddArchiveAndChownByName(t *testing.T) {
	e := newEnv(t, map[string]string{"Dockerfile": `FROM localhost/base:1
ADD files.tar.gz /opt/
ADD --chown=app:staff notes.txt /home/app/
COPY --chown=app notes.txt /home/app/notes2.txt
`, "notes.txt": "n"})

	// files.tar.gz
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "pkg/x", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1, Uid: 5})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if err := os.WriteFile(filepath.Join(e.ctx, "files.tar.gz"), tgz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// A base image with users.
	var lb bytes.Buffer
	ltw := tar.NewWriter(&lb)
	for p, body := range map[string]string{
		"etc/passwd": "root:x:0:0::/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n",
		"etc/group":  "root:x:0:\nstaff:x:50:\n",
	} {
		ltw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		ltw.Write([]byte(body))
	}
	ltw.Close()
	layer, err := tarball.LayerFromReader(&lb)
	if err != nil {
		t.Fatal(err)
	}
	base, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	bcf, _ := base.ConfigFile()
	bcf = bcf.DeepCopy()
	bcf.OS, bcf.Architecture = "linux", "arm64"
	base, _ = mutate.ConfigFile(base, bcf)
	ref, _ := name.ParseReference("localhost/base:1")
	if err := e.store.Put(base, ref); err != nil {
		t.Fatal(err)
	}

	got := flatten(t, e.mustBuild(Options{}))
	if g := got["opt/pkg/x"]; g.body != "x" || g.uid != 5 || g.mode != 0o755 {
		t.Errorf("opt/pkg/x = %+v", g)
	}
	if g := got["home/app/notes.txt"]; g.uid != 1000 || g.gid != 50 {
		t.Errorf("notes.txt = %+v", g)
	}
	if g := got["home/app"]; g.uid != 1000 || g.typ != tar.TypeDir {
		t.Errorf("created dest dir = %+v", g)
	}
	if g := got["home/app/notes2.txt"]; g.uid != 1000 || g.gid != 1000 {
		t.Errorf("notes2.txt = %+v", g)
	}
}

func TestContextSymlinksStayInside(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret": "top secret"})
	e := newEnv(t, map[string]string{"Dockerfile": "FROM scratch\nCOPY esc/secret /x\n"})
	rel, err := filepath.Rel(e.ctx, outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(e.ctx, "esc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.ctx, "abs")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.build(Options{}); err == nil {
		t.Fatal("COPY through a symlink escaping the context succeeded")
	}
	writeFiles(t, e.ctx, map[string]string{"Dockerfile": "FROM scratch\nCOPY abs/secret /x\n"})
	if _, err := e.build(Options{}); err == nil {
		t.Fatal("COPY through an absolute symlink out of the context succeeded")
	}
	// Symlinks inside a copied directory are copied as links.
	writeFiles(t, e.ctx, map[string]string{"Dockerfile": "FROM scratch\nCOPY . /ctx/\n"})
	got := flatten(t, e.mustBuild(Options{}))
	if got["ctx/esc"].typ != tar.TypeSymlink || got["ctx/esc/secret"].body != "" {
		t.Errorf("ctx/esc = %+v", got["ctx/esc"])
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct{ dockerfile, want string }{
		{"RUN echo", "before the first FROM"},
		{"FROM scratch\nCOPY missing /", "missing: no such file"},
		{"FROM scratch\nRUN --mount=type=cache,target=/x echo", "--mount is not supported"},
		{"FROM scratch\nCOPY --chown=nobody a /", `"nobody" not found`},
		{"FROM scratch\nEXPOSE 99999", "invalid port"},
		{"FROM scratch AS first\nCOPY --from=later a /\nFROM scratch AS later\nCOPY --from=first a /", "not before this one"},
	} {
		e := newEnv(t, map[string]string{"Dockerfile": tc.dockerfile, "a": "a"})
		_, err := e.build(Options{Target: ""})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.dockerfile, err, tc.want)
		}
	}
}
