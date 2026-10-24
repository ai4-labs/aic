// Package vm boots and talks to aic's Linux build VM on macOS, using
// Apple's Virtualization framework. The VM runs a LinuxKit kernel and an
// initramfs whose /init is aic-guest; RUN requests travel over a virtio
// console port (see package proto).
package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The guest kernel comes from a LinuxKit kernel image, pinned by digest.
// Only the "kernel" file is read from it, and it is verified against a
// pinned SHA-256 (the layer as a whole is never fully downloaded).
const (
	kernelVersion = "6.12.59"
	kernelRef     = "index.docker.io/linuxkit/kernel:" + kernelVersion + "@sha256:9b6408a5ca773c4d15b7c872625dedfce7a085480f93ac4b0c3e1f4619c2449f"
	maxKernelSize = 128 << 20
)

var kernelSHA256 = map[string]string{
	"arm64": "764624b367909c87335d838c07c6a34b81dafd009784008fc30f00f5f2a68165",
}

// EnsureKernel returns the path of a raw kernel Image for arch in dir,
// downloading and unpacking it the first time.
func EnsureKernel(ctx context.Context, dir, arch string, progress io.Writer) (string, error) {
	want, ok := kernelSHA256[arch]
	if !ok {
		return "", fmt.Errorf("the build VM is not available on %s yet", arch)
	}
	p := filepath.Join(dir, "kernel-"+kernelVersion+"-"+arch)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	fmt.Fprintf(progress, "Downloading Linux %s kernel for the build VM (one time)...\n", kernelVersion)
	ref, err := name.ParseReference(kernelRef)
	if err != nil {
		return "", err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil {
		return "", fmt.Errorf("fetching kernel image: %w", err)
	}
	zimg, err := findKernel(img)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(zimg)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("kernel checksum mismatch: got %s, want %s", got, want)
	}
	raw, err := Unzboot(zimg)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, raw, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// findKernel streams the image's layers until it finds /kernel.
func findKernel(img v1.Image) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		b, err := func() ([]byte, error) {
			rc, err := l.Uncompressed()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			tr := tar.NewReader(rc)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				if hdr.Name == "kernel" || hdr.Name == "./kernel" || hdr.Name == "/kernel" {
					if hdr.Size > maxKernelSize {
						return nil, fmt.Errorf("kernel is %d bytes, more than expected", hdr.Size)
					}
					return io.ReadAll(tr)
				}
			}
		}()
		if err != nil {
			return nil, err
		}
		if b != nil {
			return b, nil
		}
	}
	return nil, errors.New("kernel image has no /kernel")
}

// Unzboot unpacks an EFI zboot kernel (as built with CONFIG_EFI_ZBOOT)
// into the raw arm64 Image that Virtualization.framework boots. A raw
// Image is returned unchanged.
func Unzboot(b []byte) ([]byte, error) {
	if len(b) >= 64 && string(b[56:60]) == "ARMd" {
		return b, nil
	}
	if len(b) < 32 || string(b[0:2]) != "MZ" || string(b[4:8]) != "zimg" {
		return nil, errors.New("kernel is neither a raw arm64 Image nor an EFI zboot image")
	}
	off := binary.LittleEndian.Uint32(b[8:12])
	size := binary.LittleEndian.Uint32(b[12:16])
	comp := string(bytes.TrimRight(b[24:32], "\x00"))
	if uint64(off)+uint64(size) > uint64(len(b)) {
		return nil, errors.New("zboot payload out of bounds")
	}
	if comp != "gzip" {
		return nil, fmt.Errorf("zboot compression %q is not supported", comp)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b[off : off+size]))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxKernelSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxKernelSize || len(raw) < 64 || string(raw[56:60]) != "ARMd" {
		return nil, errors.New("zboot payload is not an arm64 Image")
	}
	return raw, nil
}

func writeFileAtomic(p string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
