package store

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestCache(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, _, err := s.CacheGet(k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CacheGet(missing) = %v", err)
	}
	if _, _, err := s.CacheGet("../../etc/passwd"); err == nil {
		t.Error("CacheGet accepted a path")
	}

	layer, err := random.Layer(512, types.OCILayer)
	if err != nil {
		t.Fatal(err)
	}
	h := v1.History{CreatedBy: "RUN make"}
	if err := s.CachePut(k1, layer, h); err != nil {
		t.Fatal(err)
	}
	if err := s.CachePut(k2, nil, v1.History{CreatedBy: "RUN true"}); err != nil {
		t.Fatal(err)
	}

	e, l, err := s.CacheGet(k1)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := layer.Digest()
	wantDiff, _ := layer.DiffID()
	got, _ := l.Digest()
	gotDiff, _ := l.DiffID()
	if got != want || gotDiff != wantDiff || e.History.CreatedBy != "RUN make" {
		t.Errorf("cached layer %s/%s, want %s/%s; history %+v", got, gotDiff, want, wantDiff, e.History)
	}
	if mt, _ := l.MediaType(); mt != types.OCILayer {
		t.Errorf("media type %s", mt)
	}
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if e, l, err := s.CacheGet(k2); err != nil || !e.Empty || l != nil {
		t.Errorf("empty entry = %+v, %v, %v", e, l, err)
	}

	// Garbage collection (e.g. aic rmi) keeps cached layers...
	if _, err := s.Remove(nil, true, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CacheGet(k1); err != nil {
		t.Errorf("cached layer collected: %v", err)
	}
	// ...until the cache is pruned.
	if n, err := s.PruneCache(); err != nil || n != 2 {
		t.Errorf("PruneCache = %d, %v", n, err)
	}
	if _, _, err := s.CacheGet(k1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CacheGet after prune = %v", err)
	}
	if n := blobCount(t, s); n != 0 {
		t.Errorf("%d blobs left after prune", n)
	}
}
