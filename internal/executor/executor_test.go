package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/rakyll/ai-container/internal/proto"
)

func layer(t *testing.T, names ...string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		h := &tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
		if strings.HasSuffix(n, "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	return &buf
}

func TestRunWorkspace(t *testing.T) {
	b := layer(t, "etc/", "etc/os-release", "workspace/", "workspace/from-image")
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, l)
	if err != nil {
		t.Fatal(err)
	}

	var gotSpec proto.Spec
	var names []string
	fn := func(_ context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error) {
		gotSpec = spec
		tr := tar.NewReader(image)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return proto.Result{}, err
			}
			names = append(names, h.Name)
		}
		io.WriteString(rio.Stdout, "ok")
		return proto.Result{ExitCode: 4}, nil
	}
	var stdout bytes.Buffer
	ws := &Workspace{Path: "/workspace/", Write: func(tw *tar.Writer, prefix string) error {
		tw.WriteHeader(&tar.Header{Name: prefix, Typeflag: tar.TypeDir, Mode: 0o755})
		return tw.WriteHeader(&tar.Header{Name: prefix + "host.txt", Typeflag: tar.TypeReg, Mode: 0o644})
	}}
	err = run(context.Background(), fn, Spec{Image: img, Args: []string{"x"}, Network: "none", Stdout: &stdout,
		Stdin: strings.NewReader(""), Workspace: ws}, io.Discard)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 4 {
		t.Fatalf("run = %v, want exit status 4", err)
	}
	sort.Strings(names)
	if want := "etc etc/os-release workspace/ workspace/host.txt"; strings.Join(names, " ") != want {
		t.Errorf("image = %v, want %s", names, want)
	}
	if gotSpec.Export != "/workspace" || gotSpec.Chown != "/workspace" || gotSpec.Network != "none" || !gotSpec.Stdin {
		t.Errorf("spec = %+v", gotSpec)
	}
	if stdout.String() != "ok" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunWorkspaceError(t *testing.T) {
	fn := func(_ context.Context, _ proto.Spec, image io.Reader, _ proto.IO) (proto.Result, error) {
		_, err := io.Copy(io.Discard, image)
		return proto.Result{Error: "extracting: unexpected EOF"}, err
	}
	ws := &Workspace{Path: "/w", Write: func(*tar.Writer, string) error { return errors.New("permission denied") }}
	err := run(context.Background(), fn, Spec{Image: empty.Image, Workspace: ws}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "copying the workspace: permission denied") {
		t.Errorf("run = %v, want the workspace error", err)
	}
	for _, p := range []string{"relative", "/", "/a/../.."} {
		ws.Path = p
		if err := run(context.Background(), fn, Spec{Image: empty.Image, Workspace: ws}, io.Discard); err == nil {
			t.Errorf("workspace path %q accepted", p)
		}
	}
}
