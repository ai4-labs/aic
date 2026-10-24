package store

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func testImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "arm64"
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPutAndLookup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img := testImage(t)
	d, _ := img.Digest()
	ref, _ := ParseLocal("app:v1")
	if ref.Name() != "localhost/app:v1" {
		t.Fatalf("ParseLocal = %s", ref.Name())
	}
	if err := s.Put(img, ref); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"app:v1", "localhost/app:v1", d.String(), d.Hex[:12]} {
		got, err := s.Image(q)
		if err != nil {
			t.Errorf("Image(%q): %v", q, err)
			continue
		}
		if gd, _ := got.Digest(); gd != d {
			t.Errorf("Image(%q) digest = %s, want %s", q, gd, d)
		}
	}
	if _, err := s.Image("app:v2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Image(app:v2) err = %v, want ErrNotFound", err)
	}

	// Re-tagging moves the name.
	img2 := testImage(t)
	if err := s.Put(img2, ref); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Image("app:v1")
	d2, _ := img2.Digest()
	if gd, _ := got.Digest(); gd != d2 {
		t.Errorf("after retag digest = %s, want %s", gd, d2)
	}
	idx, _ := s.lp.ImageIndex()
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 1 {
		t.Errorf("index has %d manifests, want 1", len(im.Manifests))
	}
}

func TestPushPull(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img := testImage(t)
	local, _ := ParseLocal("app")
	if err := s.Put(img, local); err != nil {
		t.Fatal(err)
	}
	dst, err := name.ParseReference(host + "/team/app:1")
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := s.Push(ctx, "app", dst)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if _, err := remote.Head(dst); err != nil {
		t.Fatalf("image not in registry: %v", err)
	}

	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pulled, err := s2.Pull(ctx, dst, v1.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if d, _ := pulled.Digest(); d != pushed {
		t.Errorf("pulled digest %s, pushed %s", d, pushed)
	}
	if _, err := s2.Pull(ctx, dst, v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Error("Pull with mismatched platform succeeded")
	}
}
