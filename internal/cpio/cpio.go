// Package cpio writes "newc" cpio archives, the format of Linux initramfs
// images.
package cpio

import (
	"fmt"
	"io"
)

// Mode bits for Entry.Mode.
const (
	TypeDir  = 0o040000
	TypeReg  = 0o100000
	TypeChar = 0o020000
	TypeLink = 0o120000
)

// Entry is one archive member.
type Entry struct {
	Name         string
	Mode         uint32 // type and permission bits
	Data         []byte // content for regular files, target for symlinks
	Major, Minor uint32 // device numbers for TypeChar
}

// Writer writes a newc archive.
type Writer struct {
	w   io.Writer
	ino uint32
	err error
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w, ino: 1} }

func (cw *Writer) write(b []byte) {
	if cw.err == nil {
		_, cw.err = cw.w.Write(b)
	}
}

func (cw *Writer) pad(n int) {
	if r := n % 4; r != 0 {
		cw.write(make([]byte, 4-r))
	}
}

// Write adds an entry.
func (cw *Writer) Write(e Entry) error {
	name := e.Name + "\x00"
	nlink := 1
	if e.Mode&0o170000 == TypeDir {
		nlink = 2
	}
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		cw.ino, e.Mode, 0, 0, nlink, 0, len(e.Data), 0, 0, e.Major, e.Minor, len(name), 0)
	cw.ino++
	cw.write([]byte(hdr))
	cw.write([]byte(name))
	cw.pad(len(hdr) + len(name))
	cw.write(e.Data)
	cw.pad(len(e.Data))
	return cw.err
}

// Close writes the trailer.
func (cw *Writer) Close() error {
	cw.ino = 0
	return cw.Write(Entry{Name: "TRAILER!!!"})
}
