package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func blobCount(t *testing.T, s *Store) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Dir(), "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func TestListAndRemove(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := testImage(t), testImage(t)
	ra1, _ := ParseLocal("a:1")
	ra2, _ := ParseLocal("a:2")
	rb, _ := name.ParseReference("example.com/b:latest")
	if err := s.Put(a, ra1, ra2); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(b, rb); err != nil {
		t.Fatal(err)
	}
	da, _ := a.Digest()
	db, _ := b.Digest()

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %+v", list)
	}
	for _, info := range list {
		switch info.Digest {
		case da:
			if !reflect.DeepEqual(info.Names, []string{"localhost/a:1", "localhost/a:2"}) {
				t.Errorf("a names = %q", info.Names)
			}
		case db:
			if !reflect.DeepEqual(info.Names, []string{"example.com/b:latest"}) {
				t.Errorf("b names = %q", info.Names)
			}
		default:
			t.Errorf("unexpected image %s", info.Digest)
		}
		if info.Size <= 0 || info.Platform != "linux/arm64" {
			t.Errorf("info = %+v", info)
		}
	}
	blobsBefore := blobCount(t, s)

	// By ID with two names: refused without force.
	res, err := s.Remove([]string{da.Hex[:12]}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res[0].Err, ErrMultipleNames) {
		t.Fatalf("remove by ID = %+v", res[0])
	}

	// By name: untag only, the image stays.
	res, _ = s.Remove([]string{"a:1"}, false, false)
	if res[0].Err != nil || !reflect.DeepEqual(res[0].Untagged, []string{"localhost/a:1"}) || len(res[0].Deleted) != 0 {
		t.Fatalf("remove a:1 = %+v", res[0])
	}
	if _, err := s.Image("a:2"); err != nil {
		t.Errorf("a:2 gone after removing a:1: %v", err)
	}
	if blobCount(t, s) != blobsBefore {
		t.Error("blobs removed while the image is still named")
	}

	// Last name: the image is deleted and its blobs collected.
	res, _ = s.Remove([]string{"a:2", "nope"}, false, false)
	if res[0].Err != nil || len(res[0].Deleted) != 1 || res[0].Deleted[0] != da {
		t.Fatalf("remove a:2 = %+v", res[0])
	}
	if !errors.Is(res[1].Err, ErrNotFound) {
		t.Errorf("remove nope = %+v", res[1])
	}
	if got := blobCount(t, s); got >= blobsBefore {
		t.Errorf("blobs = %d, want fewer than %d", got, blobsBefore)
	}
	if _, err := s.Image(db.String()); err != nil {
		t.Errorf("b damaged: %v", err)
	}
	if img, _ := s.Image("example.com/b:latest"); img == nil {
		t.Fatal("b missing")
	} else if _, err := img.Layers(); err != nil {
		t.Errorf("b layers: %v", err)
	}

	// --all empties the store.
	if err := s.Put(a, ra1); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Remove(nil, true, false)
	if len(res[0].Deleted) != 2 {
		t.Errorf("remove --all = %+v", res[0])
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Errorf("List after --all = %+v", list)
	}
	if n := blobCount(t, s); n != 0 {
		t.Errorf("%d blobs left after --all", n)
	}
}
