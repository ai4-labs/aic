package cpio

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, e := range []Entry{
		{Name: "dev", Mode: TypeDir | 0o755},
		{Name: "dev/console", Mode: TypeChar | 0o600, Major: 5, Minor: 1},
		{Name: "init", Mode: TypeReg | 0o755, Data: []byte("#!/bin/sh\necho hi\n")},
		{Name: "sh", Mode: TypeLink | 0o777, Data: []byte("init")},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len()%4 != 0 {
		t.Errorf("archive length %d is not 4-byte aligned", buf.Len())
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("070701")) || !bytes.Contains(buf.Bytes(), []byte("TRAILER!!!")) {
		t.Fatal("not a newc archive")
	}
	// Cross-check with the system cpio, if there is one.
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("no cpio binary")
	}
	cmd := exec.Command("cpio", "-t")
	cmd.Stdin = &buf
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cpio -t: %v", err)
	}
	if got := strings.Fields(string(out)); strings.Join(got, ",") != "dev,dev/console,init,sh" {
		t.Errorf("cpio -t = %q", got)
	}
}
