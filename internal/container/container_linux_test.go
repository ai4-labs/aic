//go:build linux

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rakyll/ai-container/internal/proto"
)

// The test binary doubles as the container child (Init) and as the
// command run inside the container (helper).
func TestMain(m *testing.M) {
	Init()
	if os.Getenv("AIC_TEST_HELPER") == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

func helper() int {
	switch os.Getenv("MODE") {
	case "cat":
		io.Copy(os.Stdout, os.Stdin)
		return 0
	case "tty":
		ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
		if err != nil {
			fmt.Println("not a terminal:", err)
			return 1
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("size=%dx%d line=%s", ws.Col, ws.Row, line)
		return 0
	case "ws":
		wd, _ := os.Getwd()
		in, err := os.ReadFile("in")
		if err != nil {
			fmt.Println(err)
			return 1
		}
		fmt.Printf("wd=%s in=%s uid=%d\n", wd, in, os.Getuid())
		os.WriteFile("out", []byte("out"), 0o644)
		os.Remove("gone")
		return 3
	}
	fmt.Printf("uid=%d\n", os.Getuid())
	up := 0
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp != 0 {
			up++
		}
	}
	fmt.Printf("interfaces up=%d\n", up)
	if b, err := os.ReadFile("/proc/self/stat"); err == nil {
		fmt.Printf("procpid=%s getpid=%d\n", strings.Fields(string(b))[0], os.Getpid())
	}
	if err := os.WriteFile("/new", []byte("new"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile("/mod", []byte("modified"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Remove("/del"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if os.Getenv("CHOWN") == "1" {
		if err := os.Chown("/new", 1000, 1000); err != nil {
			fmt.Fprintln(os.Stderr, "chown:", err)
			return 1
		}
	}
	if os.Getenv("EXIT") == "3" {
		return 3
	}
	return 0
}

// testImage is a flattened rootfs containing this test binary.
func testImage(t *testing.T) []byte {
	t.Helper()
	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	// The test binary runs inside a bare image, so it must be static.
	if f, err := elf.NewFile(bytes.NewReader(self)); err == nil {
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Skip("the test binary is dynamically linked (e.g. -race); run with CGO_ENABLED=0")
			}
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body []byte) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	add(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(&tar.Header{Name: "bin/helper", Typeflag: tar.TypeReg, Mode: 0o755}, self)
	add(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("root:x:0:0::/root:/bin/sh\n"))
	add(&tar.Header{Name: "owned", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1000, Gid: 1001}, []byte("o"))
	add(&tar.Header{Name: "mod", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("m"))
	add(&tar.Header{Name: "del", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("d"))
	add(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/owned"}, nil)
	add(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "mod"}, nil)
	add(&tar.Header{Name: "work/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(&tar.Header{Name: "work/in", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("in"))
	add(&tar.Header{Name: "work/gone", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("g"))
	tw.Close()
	return buf.Bytes()
}

type entry struct {
	typ      byte
	uid, gid int
	mode     int64
	body     string
	link     string
}

func runHelper(t *testing.T, env ...string) (proto.Result, map[string]entry, string) {
	t.Helper()
	spec := proto.Spec{Args: []string{"helper"}, Env: append([]string{"AIC_TEST_HELPER=1", "PATH=/bin"}, env...), Network: "none"}
	return runSpec(t, spec, nil, testImage(t))
}

func runSpec(t *testing.T, spec proto.Spec, stdin io.Reader, image []byte) (proto.Result, map[string]entry, string) {
	t.Helper()
	var stdout, stderr, export bytes.Buffer
	res, err := Run(context.Background(), spec, bytes.NewReader(image),
		proto.IO{Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Export: &export})
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("user namespaces unavailable: %v", err)
		}
		t.Fatalf("Run: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(res.Error, "setting up the root filesystem") && strings.Contains(res.Error, "not permitted") {
		t.Skipf("namespaces are restricted on this host: %s", res.Error)
	}
	files := map[string]entry{}
	tr := tar.NewReader(&export)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[strings.TrimSuffix(h.Name, "/")] = entry{h.Typeflag, h.Uid, h.Gid, h.Mode, string(b), h.Linkname}
	}
	return res, files, stdout.String() + stderr.String()
}

func TestRun(t *testing.T) {
	res, files, out := runHelper(t)
	if res.Error != "" || res.ExitCode != 0 {
		t.Fatalf("result = %+v\noutput: %s", res, out)
	}
	if !strings.Contains(out, "uid=0") || !strings.Contains(out, "interfaces up=1") {
		t.Errorf("output = %q, want uid=0 and only loopback", out)
	}
	if e := files["owned"]; e.uid != 1000 || e.gid != 1001 || e.mode != 0o640 || e.body != "o" {
		t.Errorf("owned = %+v, want uid 1000 gid 1001 mode 0640 preserved", e)
	}
	// mod and hard are one inode; either may be exported first.
	m, h := files["mod"], files["hard"]
	if !(m.body == "modified" && h.typ == tar.TypeLink && h.link == "mod") &&
		!(h.body == "modified" && m.typ == tar.TypeLink && m.link == "hard") {
		t.Errorf("mod = %+v, hard = %+v; want one modified file and a hard link to it", m, h)
	}
	if e := files["link"]; e.typ != tar.TypeSymlink || e.link != "/owned" {
		t.Errorf("link = %+v", e)
	}
	if e := files["new"]; e.body != "new" || e.uid != 0 {
		t.Errorf("new = %+v", e)
	}
	if _, ok := files["del"]; ok {
		t.Error("del still exists")
	}
	for _, p := range []string{"proc", "sys", "dev", "dev/null"} {
		if _, ok := files[p]; ok {
			t.Errorf("%s exported", p)
		}
	}
}

func TestRunExitCode(t *testing.T) {
	res, files, out := runHelper(t, "EXIT=3")
	if res.ExitCode != 3 || res.Error != "" {
		t.Fatalf("result = %+v\noutput: %s", res, out)
	}
	if len(files) != 0 {
		t.Errorf("a failed command exported %d files", len(files))
	}
}

func TestRunChown(t *testing.T) {
	res, files, out := runHelper(t, "CHOWN=1")
	if os.Geteuid() != 0 && findSubIDs() == nil {
		// Only root is mapped: chown to another user must fail cleanly.
		if res.ExitCode != 1 || !strings.Contains(out, "chown") {
			t.Fatalf("result = %+v, output %q; want chown failure", res, out)
		}
		return
	}
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("result = %+v\noutput: %s", res, out)
	}
	if e := files["new"]; e.uid != 1000 || e.gid != 1000 {
		t.Errorf("new = %+v, want owner 1000:1000", e)
	}
}

func TestRunSetupError(t *testing.T) {
	var stdout, stderr, export bytes.Buffer
	spec := proto.Spec{Args: []string{"does-not-exist"}, Network: "none"}
	res, err := Run(context.Background(), spec, bytes.NewReader(testImage(t)), proto.IO{Stdout: &stdout, Stderr: &stderr, Export: &export})
	if err != nil {
		t.Skipf("Run: %v", err)
	}
	if strings.Contains(res.Error, "setting up the root filesystem") {
		t.Skipf("namespaces are restricted on this host: %s", res.Error)
	}
	if !strings.Contains(res.Error, "executable file not found") {
		t.Errorf("result = %+v", res)
	}
}

func TestRunStdin(t *testing.T) {
	input := strings.Repeat("stdin data\n", 10_000)
	spec := proto.Spec{Args: []string{"helper"}, Env: []string{"AIC_TEST_HELPER=1", "MODE=cat"}, Network: "none", Stdin: true}
	res, _, out := runSpec(t, spec, strings.NewReader(input), testImage(t))
	if res.Error != "" || res.ExitCode != 0 || out != input {
		t.Fatalf("result = %+v, output %d bytes, want %d", res, len(out), len(input))
	}
}

func TestRunTTY(t *testing.T) {
	spec := proto.Spec{Args: []string{"helper"}, Env: []string{"AIC_TEST_HELPER=1", "MODE=tty"}, Network: "none",
		Stdin: true, TTY: true, Size: proto.Size{Rows: 33, Cols: 101}}
	// Keep stdin open until the command has read its line, as a terminal would.
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("hello\n"))
		time.Sleep(2 * time.Second)
		pw.Close()
	}()
	res, _, out := runSpec(t, spec, pr, testImage(t))
	if res.Error != "" || res.ExitCode != 0 || !strings.Contains(out, "size=101x33 line=hello") {
		t.Fatalf("result = %+v, output %q", res, out)
	}
}

func TestRunExportDir(t *testing.T) {
	spec := proto.Spec{Args: []string{"helper"}, Env: []string{"AIC_TEST_HELPER=1", "MODE=ws"}, Network: "none",
		WorkDir: "/work", Export: "/work", Chown: "/work"}
	res, files, out := runSpec(t, spec, nil, testImage(t))
	if res.Error != "" || res.ExitCode != 3 {
		t.Fatalf("result = %+v, output %q", res, out)
	}
	if !strings.Contains(out, "wd=/work in=in") {
		t.Errorf("output = %q", out)
	}
	if len(files) != 2 || files["in"].body != "in" || files["out"].body != "out" {
		t.Errorf("exported %v, want in and out (relative to /work), even though the command failed", files)
	}
}
