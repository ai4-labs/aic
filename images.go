package aic

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/ai4-labs/aic/internal/store"
)

// ImageInfo describes a stored image.
type ImageInfo struct {
	Digest  string   // "sha256:..."
	Names   []string // full names, e.g. "localhost/app:1", sorted
	Created time.Time
	Size    int64 // config plus compressed layers, as stored and pushed
}

// Images lists the images in the store, newest first.
func (c *Client) Images() ([]ImageInfo, error) {
	list, err := c.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]ImageInfo, len(list))
	for i, info := range list {
		out[i] = ImageInfo{Digest: info.Digest.String(), Names: info.Names, Created: info.Created, Size: info.Size}
	}
	return out, nil
}

// Tag adds names to a local image (a name, digest or ID). A name without
// a tag is tagged sha256-<digest>; unqualified names go under
// localhost/; "latest" is rejected. A name used by another image moves.
func (c *Client) Tag(image string, names ...string) error {
	tags, repos, err := parseTags(names)
	if err != nil {
		return err
	}
	img, err := c.store.Image(image)
	if err != nil {
		return err
	}
	d, err := img.Digest()
	if err != nil {
		return err
	}
	refs := append([]name.Reference(nil), tags...)
	for _, r := range repos {
		refs = append(refs, store.DigestTag(r, d))
	}
	return c.store.Put(img, refs...)
}

// RemoveResult reports what removing one image did.
type RemoveResult struct {
	Ref      string
	Untagged []string // names removed
	Deleted  []string // digests of images deleted
	Err      error
}

// Remove removes images by name, digest or ID (all of them if all is
// set). A name only deletes the image once it has no other names; an ID
// removes the image with all its names, which requires force if it has
// several. Layers no longer used are deleted.
func (c *Client) Remove(images []string, all, force bool) ([]RemoveResult, error) {
	results, err := c.store.Remove(images, all, force)
	if err != nil {
		return nil, err
	}
	out := make([]RemoveResult, len(results))
	for i, r := range results {
		out[i] = RemoveResult{Ref: r.Ref, Untagged: r.Untagged, Err: r.Err}
		for _, d := range r.Deleted {
			out[i].Deleted = append(out[i].Deleted, d.String())
		}
	}
	return out, nil
}

// Prune removes the build cache, layers no image uses and the unpacked
// copies of images that Run keeps, and returns the number of build cache
// entries removed. With vm, it also removes the macOS VM's cached
// kernel, modules and initramfs.
func (c *Client) Prune(vm bool) (int, error) {
	n, err := c.store.PruneCache()
	if err != nil {
		return n, err
	}
	if vm {
		err = os.RemoveAll(c.opts.VMDir)
	}
	return n, err
}

// Clean removes everything the Client stored: all images, caches and the
// VM's files. Credentials are kept. It returns the number of images
// removed and the bytes freed.
func (c *Client) Clean() (images int, freed int64, err error) {
	images, freed, err = c.store.Clean()
	if err != nil {
		return images, freed, err
	}
	freed += dirSize(c.opts.VMDir)
	return images, freed, os.RemoveAll(c.opts.VMDir)
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
