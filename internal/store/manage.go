package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ImageInfo describes a stored image.
type ImageInfo struct {
	Digest v1.Hash
	// Names are the image's full names, e.g. "localhost/app:1", sorted.
	Names    []string
	Created  time.Time
	Platform string
	// Size is the total size of the config and the compressed layers.
	Size int64
}

// List returns the stored images, newest first.
func (s *Store) List() ([]ImageInfo, error) {
	var out []ImageInfo
	err := s.withLock(func() error {
		im, err := s.indexManifest()
		if err != nil {
			return err
		}
		byDigest := map[v1.Hash]*ImageInfo{}
		for _, d := range im.Manifests {
			info := byDigest[d.Digest]
			if info == nil {
				info = &ImageInfo{Digest: d.Digest}
				if err := s.describe(info); err != nil {
					return fmt.Errorf("image %s: %w", d.Digest.Hex[:12], err)
				}
				byDigest[d.Digest] = info
			}
			if n := d.Annotations[RefAnnotation]; n != "" {
				info.Names = append(info.Names, n)
			}
		}
		for _, info := range byDigest {
			sort.Strings(info.Names)
			out = append(out, *info)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Digest.Hex < out[j].Digest.Hex
	})
	return out, err
}

func (s *Store) describe(info *ImageInfo) error {
	img, err := s.lp.Image(info.Digest)
	if err != nil {
		return err
	}
	m, err := img.Manifest()
	if err != nil {
		return err
	}
	info.Size = m.Config.Size
	for _, l := range m.Layers {
		info.Size += l.Size
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	info.Created = cf.Created.Time
	info.Platform = cf.OS + "/" + cf.Architecture
	if cf.Variant != "" {
		info.Platform += "/" + cf.Variant
	}
	return nil
}

func (s *Store) indexManifest() (*v1.IndexManifest, error) {
	idx, err := s.lp.ImageIndex()
	if err != nil {
		return nil, err
	}
	return idx.IndexManifest()
}

// RemoveResult reports what Remove did for one argument.
type RemoveResult struct {
	Ref      string
	Untagged []string  // names removed
	Deleted  []v1.Hash // images removed entirely
	Err      error
}

// ErrMultipleNames is returned when removing an image by ID that has
// several names, without force.
var ErrMultipleNames = errors.New("image has multiple names (use --force to remove them all)")

// Remove removes images, like podman rmi. A name removes just that name,
// and the image too once it has no names left. A digest or ID removes
// the image with all its names; with several names that requires force.
// With all set, refs are ignored and every image is removed. Blobs no
// longer used by any image are deleted afterwards.
func (s *Store) Remove(refs []string, all, force bool) ([]RemoveResult, error) {
	var results []RemoveResult
	err := s.withLock(func() error {
		im, err := s.indexManifest()
		if err != nil {
			return err
		}
		descs := im.Manifests
		if all {
			seen := map[v1.Hash]bool{}
			var res RemoveResult
			res.Ref = "--all"
			for _, d := range descs {
				if n := d.Annotations[RefAnnotation]; n != "" {
					res.Untagged = append(res.Untagged, n)
				}
				if !seen[d.Digest] {
					seen[d.Digest] = true
					res.Deleted = append(res.Deleted, d.Digest)
				}
			}
			results = append(results, res)
			descs = nil
		} else {
			for _, ref := range refs {
				var res RemoveResult
				res.Ref = ref
				descs, res = removeOne(descs, ref, force, res)
				results = append(results, res)
			}
		}
		im.Manifests = descs
		if err := s.writeIndex(im); err != nil {
			return err
		}
		return s.gc(im)
	})
	return results, err
}

func removeOne(descs []v1.Descriptor, ref string, force bool, res RemoveResult) ([]v1.Descriptor, RemoveResult) {
	desc, byName, err := find(descs, ref)
	if err != nil {
		res.Err = err
		return descs, res
	}
	target := desc.Digest
	var names []string
	for _, d := range descs {
		if d.Digest == target && d.Annotations[RefAnnotation] != "" {
			names = append(names, d.Annotations[RefAnnotation])
		}
	}
	if byName == "" && len(names) > 1 && !force {
		res.Err = fmt.Errorf("%s: %w: %s", ref, ErrMultipleNames, strings.Join(names, ", "))
		return descs, res
	}
	var kept []v1.Descriptor
	remaining := 0
	for _, d := range descs {
		n := d.Annotations[RefAnnotation]
		switch {
		case d.Digest != target:
			kept = append(kept, d)
		case byName != "" && n != byName:
			kept = append(kept, d) // another name of the same image
			remaining++
		case n != "":
			res.Untagged = append(res.Untagged, n)
		}
	}
	if remaining == 0 {
		res.Deleted = append(res.Deleted, target)
	}
	return kept, res
}

func (s *Store) writeIndex(im *v1.IndexManifest) error {
	if im.Manifests == nil {
		im.Manifests = []v1.Descriptor{}
	}
	b, err := json.MarshalIndent(im, "", "   ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".index-*.json")
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
	return os.Rename(tmp.Name(), filepath.Join(s.dir, "index.json"))
}

// gc deletes blobs not referenced by any image in im. It must run with
// the store locked, so no write is in progress.
func (s *Store) gc(im *v1.IndexManifest) error {
	keep := map[string]bool{}
	for _, d := range im.Manifests {
		keep[d.Digest.Hex] = true
		img, err := s.lp.Image(d.Digest)
		if err != nil {
			return err
		}
		m, err := img.Manifest()
		if err != nil {
			return err
		}
		keep[m.Config.Digest.Hex] = true
		for _, l := range m.Layers {
			keep[l.Digest.Hex] = true
		}
	}
	dir := filepath.Join(s.dir, "blobs", "sha256")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
