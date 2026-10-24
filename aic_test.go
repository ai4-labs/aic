package aic_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai4-labs/aic"
)

func newClient(t *testing.T) *aic.Client {
	t.Helper()
	dir := t.TempDir()
	c, err := aic.New(aic.Options{
		StoreDir: filepath.Join(dir, "store"),
		AuthFile: filepath.Join(dir, "auth.json"),
		VMDir:    filepath.Join(dir, "vm"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClient(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\nCOPY hello /\n"), 0o644)
	os.WriteFile(filepath.Join(src, "hello"), []byte("hi"), 0o644)

	img, err := c.Build(ctx, aic.BuildOptions{ContextDir: src, Tags: []string{"app:1", "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.Digest, "sha256:") || len(img.Names) != 2 ||
		img.Names[1] != "localhost/app:sha256-"+strings.TrimPrefix(img.Digest, "sha256:") {
		t.Fatalf("Build = %+v", img)
	}
	if err := c.Tag("app:1", "other:2"); err != nil {
		t.Fatal(err)
	}
	list, err := c.Images()
	if err != nil || len(list) != 1 || len(list[0].Names) != 3 || list[0].Digest != img.Digest {
		t.Fatalf("Images = %+v, %v", list, err)
	}
	res, err := c.Remove([]string{"other:2"}, false, false)
	if err != nil || len(res) != 1 || res[0].Err != nil || len(res[0].Untagged) != 1 {
		t.Fatalf("Remove = %+v, %v", res, err)
	}
	n, freed, err := c.Clean()
	if err != nil || n != 1 || freed == 0 {
		t.Fatalf("Clean = %d, %d, %v", n, freed, err)
	}
	if list, _ := c.Images(); len(list) != 0 {
		t.Errorf("%d images after Clean", len(list))
	}
}

func TestInvalidArguments(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	for name, err := range map[string]error{
		"tag latest":  c.Tag("x", "app:latest"),
		"bad context": func() error { _, err := c.Build(ctx, aic.BuildOptions{ContextDir: "/does/not/exist"}); return err }(),
		"bad network": func() error { _, err := c.Run(ctx, aic.RunOptions{Image: "x", Network: "bridge"}); return err }(),
		"copy back":   func() error { _, err := c.Run(ctx, aic.RunOptions{Image: "x", CopyBack: true}); return err }(),
		"push local":  func() error { _, _, err := c.Push(ctx, "app", ""); return err }(),
		"platform":    func() error { _, err := c.Pull(ctx, "alpine", "nonsense!"); return err }(),
	} {
		if !errors.Is(err, aic.ErrInvalid) {
			t.Errorf("%s: error %v does not wrap ErrInvalid", name, err)
		}
	}
	// Failures that aren't the caller's fault don't.
	if err := c.Tag("missing", "app:1"); err == nil || errors.Is(err, aic.ErrInvalid) {
		t.Errorf("Tag of a missing image: %v", err)
	}
}

func Example() {
	c, err := aic.New(aic.Options{Progress: os.Stderr})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	img, err := c.Build(ctx, aic.BuildOptions{ContextDir: ".", Tags: []string{"app"},
		Progress: os.Stderr, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		panic(err)
	}
	code, err := c.Run(ctx, aic.RunOptions{
		Image:     img.Digest,
		Command:   []string{"make", "test"},
		Workspace: ".",
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	})
	if err != nil {
		panic(err)
	}
	os.Exit(code)
}
