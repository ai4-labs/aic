package vm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai4-labs/aic/internal/cpio"
)

// BuildInitrd returns an initramfs whose /init is the guest binary, with
// the kernel modules in /lib/modules (loaded by the guest in that order,
// listed in /lib/modules/order).
func BuildInitrd(guest []byte, mods []module) ([]byte, error) {
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	var order []string
	entries := []cpio.Entry{
		{Name: "dev", Mode: cpio.TypeDir | 0o755},
		{Name: "dev/console", Mode: cpio.TypeChar | 0o600, Major: 5, Minor: 1},
		{Name: "etc", Mode: cpio.TypeDir | 0o755},
		{Name: "proc", Mode: cpio.TypeDir | 0o755},
		{Name: "run", Mode: cpio.TypeDir | 0o755},
		{Name: "sys", Mode: cpio.TypeDir | 0o755},
		{Name: "tmp", Mode: cpio.TypeDir | 0o1777},
		{Name: "init", Mode: cpio.TypeReg | 0o755, Data: guest},
		{Name: "lib", Mode: cpio.TypeDir | 0o755},
		{Name: "lib/modules", Mode: cpio.TypeDir | 0o755},
	}
	for _, m := range mods {
		entries = append(entries, cpio.Entry{Name: "lib/modules/" + m.name, Mode: cpio.TypeReg | 0o644, Data: m.data})
		order = append(order, m.name)
	}
	entries = append(entries, cpio.Entry{Name: "lib/modules/order", Mode: cpio.TypeReg | 0o644,
		Data: []byte(strings.Join(order, "\n"))})
	for _, e := range entries {
		if err := w.Write(e); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ensureInitrd writes the initramfs for guest and mods to dir, named by
// content.
func ensureInitrd(dir string, guest []byte, mods []module) (string, error) {
	h := sha256.New()
	h.Write(guest)
	for _, m := range mods {
		h.Write([]byte(m.name))
		h.Write(m.data)
	}
	p := filepath.Join(dir, "initrd-"+hex.EncodeToString(h.Sum(nil)[:8])+".cpio")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	b, err := BuildInitrd(guest, mods)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, b, 0o644); err != nil {
		return "", err
	}
	// Initramfs files of other aic versions are rebuilt when needed.
	old, _ := filepath.Glob(filepath.Join(dir, "initrd-*.cpio"))
	for _, o := range old {
		if o != p {
			os.Remove(o)
		}
	}
	return p, nil
}
