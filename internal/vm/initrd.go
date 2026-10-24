package vm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/rakyll/ai-container/internal/cpio"
)

// BuildInitrd returns an initramfs whose /init is the guest binary.
func BuildInitrd(guest []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	for _, e := range []cpio.Entry{
		{Name: "dev", Mode: cpio.TypeDir | 0o755},
		{Name: "dev/console", Mode: cpio.TypeChar | 0o600, Major: 5, Minor: 1},
		{Name: "etc", Mode: cpio.TypeDir | 0o755},
		{Name: "proc", Mode: cpio.TypeDir | 0o755},
		{Name: "run", Mode: cpio.TypeDir | 0o755},
		{Name: "sys", Mode: cpio.TypeDir | 0o755},
		{Name: "tmp", Mode: cpio.TypeDir | 0o1777},
		{Name: "init", Mode: cpio.TypeReg | 0o755, Data: guest},
	} {
		if err := w.Write(e); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ensureInitrd writes the initramfs for guest to dir, named by content.
func ensureInitrd(dir string, guest []byte) (string, error) {
	sum := sha256.Sum256(guest)
	p := filepath.Join(dir, "initrd-"+hex.EncodeToString(sum[:8])+".cpio")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	b, err := BuildInitrd(guest)
	if err != nil {
		return "", err
	}
	return p, writeFileAtomic(p, b, 0o644)
}
