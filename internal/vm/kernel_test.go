package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func fakeImage() []byte {
	raw := make([]byte, 4096)
	copy(raw[56:], "ARMd")
	return raw
}

func TestUnzboot(t *testing.T) {
	raw := fakeImage()
	if got, err := Unzboot(raw); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("raw image: %v", err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()
	z := make([]byte, 64)
	copy(z[0:], "MZ")
	copy(z[4:], "zimg")
	binary.LittleEndian.PutUint32(z[8:], 64)
	binary.LittleEndian.PutUint32(z[12:], uint32(gz.Len()))
	copy(z[24:], "gzip")
	z = append(z, gz.Bytes()...)
	got, err := Unzboot(z)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("zboot: %v", err)
	}

	bad := append([]byte(nil), z...)
	binary.LittleEndian.PutUint32(bad[12:], 1<<30)
	if _, err := Unzboot(bad); err == nil {
		t.Error("out-of-bounds payload accepted")
	}
	if _, err := Unzboot([]byte("not a kernel")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestScanKernelImage(t *testing.T) {
	tarOf := func(files map[string][]byte) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, b := range files {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
			tw.Write(b)
		}
		tw.Close()
		return buf.Bytes()
	}
	modTar := tarOf(map[string][]byte{
		"./" + moduleDir + "a.ko": []byte("module a"),
		"./" + moduleDir + "b.ko": []byte("module b"),
		"./lib/modules/other.ko":  []byte("other"),
	})
	layerTar := tarOf(map[string][]byte{"kernel": []byte("the kernel"), "kernel.tar": modTar})
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(layerTar)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, l)
	if err != nil {
		t.Fatal(err)
	}
	k, mods, err := scanKernelImage(img, true, []module{{name: "a.ko"}, {name: "b.ko"}})
	if err != nil || string(k) != "the kernel" || string(mods[0].data) != "module a" || string(mods[1].data) != "module b" {
		t.Fatalf("scan = %q, %+v, %v", k, mods, err)
	}
	if _, _, err := scanKernelImage(img, false, []module{{name: "missing.ko"}}); err == nil {
		t.Error("missing module not reported")
	}
}
