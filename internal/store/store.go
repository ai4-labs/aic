// Package store is aic's local image store: an OCI image layout on disk,
// plus pulling from and pushing to registries.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// RefAnnotation holds an image's name in the layout's index.json.
const RefAnnotation = "org.opencontainers.image.ref.name"

// ErrNotFound is returned when an image is not in the store.
var ErrNotFound = errors.New("image not known")

// Store is a local OCI image layout.
type Store struct {
	dir string
	lp  layout.Path
	// UserAgent is sent to registries.
	UserAgent string
	// Keychain supplies registry credentials (default: Docker's config).
	Keychain authn.Keychain
}

// DefaultDir returns $AIC_STORE, or ~/.aic/store.
func DefaultDir() (string, error) {
	if d := os.Getenv("AIC_STORE"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aic", "store"), nil
}

// Open opens the store at dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lp: layout.Path(dir), UserAgent: "aic"}
	err := s.withLock(func() error {
		if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
			return nil
		}
		_, err := layout.Write(dir, empty.Index)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("opening image store %s: %w", dir, err)
	}
	return s, nil
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// Put stores img under the given names, moving each name from any image
// that had it. With no names the image is stored untagged.
func (s *Store) Put(img v1.Image, refs ...name.Reference) error {
	d, err := img.Digest()
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		if len(refs) == 0 {
			return s.lp.ReplaceImage(img, func(desc v1.Descriptor) bool {
				return desc.Digest == d && desc.Annotations[RefAnnotation] == ""
			})
		}
		for _, ref := range refs {
			n := ref.Name()
			err := s.lp.ReplaceImage(img, func(desc v1.Descriptor) bool {
				return desc.Annotations[RefAnnotation] == n ||
					(desc.Digest == d && desc.Annotations[RefAnnotation] == "")
			}, layout.WithAnnotations(map[string]string{RefAnnotation: n}))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Image looks up an image by name ("alpine", "localhost/app:1"), by
// digest ("sha256:..."), or by a unique digest prefix of at least 12 hex
// characters.
func (s *Store) Image(ref string) (v1.Image, error) {
	var img v1.Image
	err := s.withLock(func() error {
		idx, err := s.lp.ImageIndex()
		if err != nil {
			return err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return err
		}
		desc, _, err := find(im.Manifests, ref)
		if err != nil {
			return err
		}
		img, err = s.lp.Image(desc.Digest)
		return err
	})
	return img, err
}

// find resolves ref to a descriptor. byName is the stored name if ref
// matched a name, or "" if it matched a digest.
func find(descs []v1.Descriptor, ref string) (desc *v1.Descriptor, byName string, err error) {
	hex := strings.TrimPrefix(ref, "sha256:")
	if isHex(hex) && (len(hex) >= 12 || strings.HasPrefix(ref, "sha256:")) {
		var found *v1.Descriptor
		for i, d := range descs {
			if strings.HasPrefix(d.Digest.Hex, hex) {
				if found != nil && found.Digest != d.Digest {
					return nil, "", fmt.Errorf("%s: ambiguous digest prefix", ref)
				}
				found = &descs[i]
			}
		}
		if found != nil {
			return found, "", nil
		}
	}
	// Names: try as given (docker.io default), then with localhost/.
	var names []string
	if r, err := name.ParseReference(ref); err == nil {
		names = append(names, r.Name())
	}
	if r, err := ParseLocal(ref); err == nil {
		names = append(names, r.Name())
	}
	for _, n := range names {
		for i, d := range descs {
			if d.Annotations[RefAnnotation] == n {
				return &descs[i], n, nil
			}
		}
	}
	return nil, "", fmt.Errorf("%s: %w", ref, ErrNotFound)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ParseLocal parses an image name for tagging a local build. Like
// podman, a name without a registry is placed under localhost/ rather
// than docker.io.
func ParseLocal(s string) (name.Reference, error) {
	first, _, hasSlash := strings.Cut(s, "/")
	if !hasSlash || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		s = "localhost/" + s
	}
	return name.ParseReference(s)
}

func (s *Store) remoteOptions(ctx context.Context) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(s.keychain()),
		remote.WithUserAgent(s.UserAgent),
	}
}

func (s *Store) keychain() authn.Keychain {
	if s.Keychain != nil {
		return s.Keychain
	}
	return authn.DefaultKeychain
}

// Pull fetches ref for platform and stores it under ref. It returns the
// stored image.
func (s *Store) Pull(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Image, error) {
	opts := append(s.remoteOptions(ctx), remote.WithPlatform(platform))
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	if cf.OS != platform.OS || cf.Architecture != platform.Architecture {
		return nil, fmt.Errorf("%s: image is for %s/%s, not %s", ref, cf.OS, cf.Architecture, platform)
	}
	if err := s.Put(img, ref); err != nil {
		return nil, err
	}
	return s.Image(ref.Name())
}

// Push uploads the local image src to dst.
func (s *Store) Push(ctx context.Context, src string, dst name.Reference) (v1.Hash, error) {
	img, err := s.Image(src)
	if err != nil {
		return v1.Hash{}, err
	}
	if err := remote.Write(dst, img, s.remoteOptions(ctx)...); err != nil {
		return v1.Hash{}, err
	}
	return img.Digest()
}
