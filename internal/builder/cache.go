package builder

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/ai4-labs/aic/internal/store"
)

// Build cache.
//
// Every stage carries a cache key that identifies its state: the base
// image digest and platform, then one link per instruction. Metadata
// instructions hash the resulting image config (and ARG values, which
// RUN sees); RUN hashes its command, environment, working directory, user
// and network; COPY and ADD hash the content and metadata of everything
// they write. A step whose key is in the cache reuses the recorded layer
// instead of running. Like Docker, RUN is keyed by its definition, not by
// what it would download.

// chainKey hashes parts into a cache key. Parts are length-prefixed so
// that they can't run into each other.
func chainKey(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v) // maps are encoded with sorted keys
	if err != nil {
		panic(err)
	}
	return string(b)
}

// configKey advances s.key past a metadata instruction.
func (s *stage) configKey() {
	s.key = chainKey(s.key, "config", mustJSON(s.config), s.author)
}

// argsKey advances s.key past an ARG instruction.
func (s *stage) argsKey() {
	var kv []string
	for k, v := range s.args {
		if v != nil {
			kv = append(kv, k+"="+*v)
		} else {
			kv = append(kv, k)
		}
	}
	sort.Strings(kv)
	s.key = chainKey(append([]string{s.key, "args"}, kv...)...)
}

// fromCache applies the cached result of the step with key, if any. It
// reports whether it did.
func (b *builder) fromCache(s *stage, key string) bool {
	if b.opts.NoCache {
		return false
	}
	e, layer, err := b.opts.Store.CacheGet(key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			b.say("[Warning] ignoring build cache entry: %v", err)
		}
		return false
	}
	s.appendLayer(layer, e.History)
	s.key = key
	if layer != nil {
		d, _ := layer.Digest()
		b.say("--> Using cache %s", d.Hex[:12])
	} else {
		b.say("--> Using cache")
	}
	return true
}

// toCache records the result of the step with key.
func (b *builder) toCache(key string, layer v1.Layer, h v1.History) {
	if err := b.opts.Store.CachePut(key, layer, h); err != nil {
		b.say("[Warning] could not save to the build cache: %v", err)
	}
}
