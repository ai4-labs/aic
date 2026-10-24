// Package vm boots and talks to aic's Linux build VM on macOS, using
// Apple's Virtualization framework. The VM runs a LinuxKit kernel and an
// initramfs whose /init is aic-guest; RUN requests travel over vsock, or
// a virtio console port if vsock is unavailable (see package proto).
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
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The guest kernel comes from a LinuxKit kernel image, pinned by digest.
// Only the "kernel" file and the vsock modules (from "kernel.tar") are
// read from it, each verified against a pinned SHA-256.
const (
	kernelVersion = "6.12.59"
	kernelRef     = "index.docker.io/linuxkit/kernel:" + kernelVersion + "@sha256:9b6408a5ca773c4d15b7c872625dedfce7a085480f93ac4b0c3e1f4619c2449f"
	maxKernelSize = 128 << 20
	maxModuleSize = 8 << 20
)

var kernelSHA256 = map[string]string{
	"arm64": "764624b367909c87335d838c07c6a34b81dafd009784008fc30f00f5f2a68165",
}

// module is a kernel module loaded by the guest, in load order.
type module struct {
	name   string
	sha256 string
	data   []byte
}

// The vsock modules give the guest a fast channel to the host (see
// guest.Main); the LinuxKit kernel builds them as modules.
var vsockModules = map[string][]module{
	"arm64": {
		{name: "vsock.ko", sha256: "396eb1c3c69c62d58f48712273833b9c69fe7f493177270a49df9b21aaf68dfe"},
		{name: "vmw_vsock_virtio_transport_common.ko", sha256: "a5382288b210a716f078f263804b0ab90bd2f7442a47fd262964317b2f8c07c1"},
		{name: "vmw_vsock_virtio_transport.ko", sha256: "e4ca724dcb931d170999f4c80ad198838479fa0f0e8db70c724f9503ca47ae65"},
	},
}

const moduleDir = "lib/modules/" + kernelVersion + "-linuxkit/kernel/net/vmw_vsock/"

// EnsureKernel returns the path of a raw kernel Image for arch in dir and
// the guest's kernel modules, downloading them the first time.
func EnsureKernel(ctx context.Context, dir, arch string, progress io.Writer) (string, []module, error) {
	want, ok := kernelSHA256[arch]
	if !ok {
		return "", nil, fmt.Errorf("the build VM is not available on %s yet", arch)
	}
	kpath := filepath.Join(dir, "kernel-"+kernelVersion+"-"+arch)
	mdir := filepath.Join(dir, "modules-"+kernelVersion+"-"+arch)
	mods := append([]module(nil), vsockModules[arch]...)
	haveKernel := fileExists(kpath)
	haveMods := true
	for i := range mods {
		b, err := os.ReadFile(filepath.Join(mdir, mods[i].name))
		if err != nil {
			haveMods = false
			break
		}
		mods[i].data = b
	}
	if haveKernel && haveMods {
		return kpath, mods, nil
	}

	fmt.Fprintf(progress, "Downloading the Linux %s kernel for the VM (one time, about 200 MB)...\n", kernelVersion)
	ref, err := name.ParseReference(kernelRef)
	if err != nil {
		return "", nil, err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil {
		return "", nil, fmt.Errorf("fetching kernel image: %w", err)
	}
	zimg, mods, err := scanKernelImage(img, !haveKernel, mods)
	if err != nil {
		return "", nil, err
	}
	if !haveKernel {
		sum := sha256.Sum256(zimg)
		if got := hex.EncodeToString(sum[:]); got != want {
			return "", nil, fmt.Errorf("kernel checksum mismatch: got %s, want %s", got, want)
		}
		raw, err := Unzboot(zimg)
		if err != nil {
			return "", nil, err
		}
		if err := writeFileAtomic(kpath, raw, 0o644); err != nil {
			return "", nil, err
		}
	}
	for _, m := range mods {
		sum := sha256.Sum256(m.data)
		if got := hex.EncodeToString(sum[:]); got != m.sha256 {
			return "", nil, fmt.Errorf("kernel module %s checksum mismatch: got %s, want %s", m.name, got, m.sha256)
		}
		if err := writeFileAtomic(filepath.Join(mdir, m.name), m.data, 0o644); err != nil {
			return "", nil, err
		}
	}
	return kpath, mods, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// scanKernelImage streams the image's layers for /kernel (if wantKernel)
// and the modules, which are inside /kernel.tar. It stops as soon as it
// has everything.
func scanKernelImage(img v1.Image, wantKernel bool, mods []module) ([]byte, []module, error) {
	var kernel []byte
	missing := func() int {
		n := 0
		if wantKernel && kernel == nil {
			n++
		}
		for _, m := range mods {
			if m.data == nil {
				n++
			}
		}
		return n
	}
	for i := range mods {
		mods[i].data = nil // fetch all of them, so they match
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, err
	}
	for _, l := range layers {
		err := func() error {
			rc, err := l.Uncompressed()
			if err != nil {
				return err
			}
			defer rc.Close()
			tr := tar.NewReader(rc)
			for missing() > 0 {
				hdr, err := tr.Next()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				switch strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/") {
				case "kernel":
					if !wantKernel {
						continue
					}
					if hdr.Size > maxKernelSize {
						return fmt.Errorf("kernel is %d bytes, more than expected", hdr.Size)
					}
					if kernel, err = io.ReadAll(tr); err != nil {
						return err
					}
				case "kernel.tar":
					if err := readModules(tar.NewReader(tr), mods); err != nil {
						return err
					}
				}
			}
			return nil
		}()
		if err != nil {
			return nil, nil, err
		}
		if missing() == 0 {
			return kernel, mods, nil
		}
	}
	if wantKernel && kernel == nil {
		return nil, nil, errors.New("kernel image has no /kernel")
	}
	return nil, nil, errors.New("kernel image is missing the vsock modules")
}

func readModules(tr *tar.Reader, mods []module) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		for i := range mods {
			if name == moduleDir+mods[i].name {
				if hdr.Size > maxModuleSize {
					return fmt.Errorf("module %s is %d bytes, more than expected", name, hdr.Size)
				}
				if mods[i].data, err = io.ReadAll(tr); err != nil {
					return err
				}
			}
		}
		done := true
		for _, m := range mods {
			done = done && m.data != nil
		}
		if done {
			return nil
		}
	}
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
