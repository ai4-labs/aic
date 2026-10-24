package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The build cache maps a step's cache key to the layer it produced. Each
// entry is a small JSON file in <store>/cache/; the layer blob lives with
// the image blobs and is kept by garbage collection while an entry
// refers to it.

// CacheEntry is the recorded result of a build step.
type CacheEntry struct {
	// Empty is set for steps that produced no layer.
	Empty     bool            `json:"empty,omitempty"`
	Digest    *v1.Hash        `json:"digest,omitempty"` // compressed layer
	DiffID    *v1.Hash        `json:"diffID,omitempty"` // uncompressed layer
	Size      int64           `json:"size,omitempty"`
	MediaType types.MediaType `json:"mediaType,omitempty"`
	History   v1.History      `json:"history"`
}

func (s *Store) cacheDir() string { return filepath.Join(s.dir, "cache") }

func validKey(key string) error {
	if len(key) != 64 || !isHex(key) {
		return fmt.Errorf("invalid cache key %q", key)
	}
	return nil
}

// CacheGet returns the entry for key, and its layer unless the step
// produced none. It returns ErrNotFound for unknown keys and for entries
// whose layer blob is gone.
func (s *Store) CacheGet(key string) (*CacheEntry, v1.Layer, error) {
	if err := validKey(key); err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.cacheDir(), key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var e CacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, nil, fmt.Errorf("cache entry %s: %w", key[:12], err)
	}
	if e.Empty {
		return &e, nil, nil
	}
	if e.Digest == nil || e.DiffID == nil {
		return nil, nil, fmt.Errorf("cache entry %s: no layer digest", key[:12])
	}
	p := filepath.Join(s.dir, "blobs", e.Digest.Algorithm, e.Digest.Hex)
	if fi, err := os.Stat(p); err != nil || fi.Size() != e.Size {
		return nil, nil, ErrNotFound
	}
	l, err := partial.CompressedToLayer(&blobLayer{path: p, e: e})
	if err != nil {
		return nil, nil, err
	}
	return &e, l, nil
}

// CachePut records the result of a step: layer (nil for none) and its
// history entry. The layer blob is written to the store.
func (s *Store) CachePut(key string, layer v1.Layer, h v1.History) error {
	if err := validKey(key); err != nil {
		return err
	}
	e := CacheEntry{Empty: layer == nil, History: h}
	if layer != nil {
		d, err := layer.Digest()
		if err != nil {
			return err
		}
		diff, err := layer.DiffID()
		if err != nil {
			return err
		}
		e.Digest, e.DiffID = &d, &diff
		if e.Size, err = layer.Size(); err != nil {
			return err
		}
		if e.MediaType, err = layer.MediaType(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		if layer != nil {
			rc, err := layer.Compressed()
			if err != nil {
				return err
			}
			if err := s.lp.WriteBlob(*e.Digest, rc); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(s.cacheDir(), 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(s.cacheDir(), ".tmp-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(b); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), filepath.Join(s.cacheDir(), key))
	})
}

// PruneCache removes the build cache and the layers only it used. It
// returns the number of entries removed.
func (s *Store) PruneCache() (int, error) {
	n := 0
	err := s.withLock(func() error {
		ents, err := os.ReadDir(s.cacheDir())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, e := range ents {
			if err := os.Remove(filepath.Join(s.cacheDir(), e.Name())); err != nil {
				return err
			}
			if !strings.HasPrefix(e.Name(), ".") {
				n++
			}
		}
		im, err := s.indexManifest()
		if err != nil {
			return err
		}
		return s.gc(im)
	})
	return n, err
}

// cachedBlobs returns the layer digests (hex) the cache refers to.
func (s *Store) cachedBlobs() (map[string]bool, error) {
	keep := map[string]bool{}
	ents, err := os.ReadDir(s.cacheDir())
	if errors.Is(err, fs.ErrNotExist) {
		return keep, nil
	}
	if err != nil {
		return nil, err
	}
	for _, de := range ents {
		if validKey(de.Name()) != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.cacheDir(), de.Name()))
		if err != nil {
			return nil, err
		}
		var e CacheEntry
		if json.Unmarshal(b, &e) == nil && !e.Empty && e.Digest != nil {
			keep[e.Digest.Hex] = true
		}
	}
	return keep, nil
}

// blobLayer is a compressed layer stored as a blob.
type blobLayer struct {
	path string
	e    CacheEntry
}

func (l *blobLayer) Digest() (v1.Hash, error)            { return *l.e.Digest, nil }
func (l *blobLayer) DiffID() (v1.Hash, error)            { return *l.e.DiffID, nil }
func (l *blobLayer) Size() (int64, error)                { return l.e.Size, nil }
func (l *blobLayer) MediaType() (types.MediaType, error) { return l.e.MediaType, nil }
func (l *blobLayer) Compressed() (io.ReadCloser, error)  { return os.Open(l.path) }
