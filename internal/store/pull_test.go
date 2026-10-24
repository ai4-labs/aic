package store

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestPullProgress(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	img, err := random.Image(2048, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/test/img:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	plat := v1.Platform{OS: cf.OS, Architecture: cf.Architecture}

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Progress = &out
	if _, err := s.Pull(context.Background(), ref, plat); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "pulled in"); n != 3 {
		t.Errorf("first pull reported %d pulled layers, want 3:\n%s", n, out.String())
	}
	out.Reset()
	if _, err := s.Pull(context.Background(), ref, plat); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "already present"); n != 3 {
		t.Errorf("second pull reported %d present layers, want 3:\n%s", n, out.String())
	}
}
