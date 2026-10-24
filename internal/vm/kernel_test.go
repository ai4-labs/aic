package vm

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
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
