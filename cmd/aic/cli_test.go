package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithInput(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIUsage(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	for _, tc := range []struct {
		args []string
		code int
		want string // substring of stdout+stderr
	}{
		{[]string{"--help"}, exitOK, "build"},
		{[]string{"build", "--help"}, exitOK, "--build-arg"},
		{[]string{"version"}, exitOK, "image store:"},
		{[]string{"--version"}, exitOK, "aic version"},
		{[]string{"bogus"}, exitUsage, "unknown command"},
		{[]string{"build", "--nope"}, exitUsage, "unknown flag"},
		{[]string{"build", "a", "b"}, exitUsage, "accepts at most 1 arg"},
		{[]string{"build", "/does/not/exist"}, exitUsage, "not a directory"},
		{[]string{"pull"}, exitUsage, "accepts 1 arg"},
		{[]string{"push", "hello"}, exitUsage, "has no registry"},
		{[]string{"push", "registry.example.com/x:1"}, exitError, "image not known"},
		{[]string{"build", "--platform", "nonsense!", "."}, exitUsage, "platform"},
		{[]string{"run"}, exitUsage, "requires at least 1 arg"},
		{[]string{"run", "--network", "bridge", "x"}, exitUsage, "invalid --network"},
		{[]string{"run", "-w", "/does/not/exist", "x"}, exitUsage, "no such file"},
		{[]string{"run", "-w", ".:/", "x"}, exitUsage, "invalid workspace path"},
		{[]string{"run", "-it", "x"}, exitUsage, "needs a terminal"},
		{[]string{"run", "--copy-back", "x"}, exitUsage, "needs a workspace"},
		{[]string{"run", "127.0.0.1:1/x:1"}, exitRunFailed, "127.0.0.1:1"},
	} {
		code, stdout, stderr := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(stdout+stderr, tc.want) {
			t.Errorf("aic %q = %d\nstdout: %s\nstderr: %s\nwant code %d containing %q",
				tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
}

func TestCLIBuild(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nARG V\nLABEL v=$V\nCOPY f /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Flags after the positional argument, repeated -t.
	code, stdout, stderr := runCLI(t, "build", dir, "-t", "a", "--tag=b:2", "--build-arg", "V=1", "-q")
	if code != exitOK || !strings.HasPrefix(stdout, "sha256:") {
		t.Fatalf("build = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	digest := strings.TrimSpace(stdout)
	for _, ref := range []string{"a", "localhost/b:2", digest[:19]} {
		if code, _, stderr := runCLI(t, "push", ref, "127.0.0.1:1/x:1", "-q"); code != exitError || strings.Contains(stderr, "image not known") {
			t.Errorf("push %s: code %d, stderr %s (want a network error, not a lookup failure)", ref, code, stderr)
		}
	}
}

func TestCLIImagesAndRmi(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "build", "-q", "-t", "app:1", "-t", "registry.example.com/team/app:2", dir)
	if code != exitOK {
		t.Fatalf("build: %s", stderr)
	}
	id := strings.TrimPrefix(strings.TrimSpace(stdout), "sha256:")[:12]

	_, stdout, _ = runCLI(t, "images")
	for _, want := range []string{"REPOSITORY", "localhost/app", "registry.example.com/team/app", id, "Less than a minute ago"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("images output missing %q:\n%s", want, stdout)
		}
	}
	if _, stdout, _ = runCLI(t, "images", "-q"); strings.TrimSpace(stdout) != id {
		t.Errorf("images -q = %q, want %q", stdout, id)
	}

	if code, _, stderr = runCLI(t, "rmi", id); code != exitError || !strings.Contains(stderr, "--force") {
		t.Errorf("rmi by ID with two names: code %d, %s", code, stderr)
	}
	if code, stdout, _ = runCLI(t, "rmi", "app:1"); code != exitOK || stdout != "Untagged: localhost/app:1\n" {
		t.Errorf("rmi app:1: code %d, %q", code, stdout)
	}
	if code, stdout, _ = runCLI(t, "rmi", "-f", id); code != exitOK || !strings.Contains(stdout, "Deleted: "+id) {
		t.Errorf("rmi -f: code %d, %q", code, stdout)
	}
	if _, stdout, _ = runCLI(t, "images", "-q"); stdout != "" {
		t.Errorf("images after rmi = %q", stdout)
	}
	if code, _, stderr = runCLI(t, "rmi", "nope"); code != exitError || !strings.Contains(stderr, "image not known") {
		t.Errorf("rmi nope: code %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "rmi"); code != exitUsage {
		t.Errorf("rmi without args: code %d", code)
	}
	if code, _, _ = runCLI(t, "rmi", "-a", "x"); code != exitUsage {
		t.Errorf("rmi -a x: code %d", code)
	}
}

func TestHumanFormats(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		30 * time.Second:     "Less than a minute ago",
		5 * time.Minute:      "5 minutes ago",
		time.Hour:            "an hour ago",
		3 * 24 * time.Hour:   "3 days ago",
		21 * 24 * time.Hour:  "3 weeks ago",
		400 * 24 * time.Hour: "13 months ago",
	} {
		if got := humanAgo(now, now.Add(-d)); got != want {
			t.Errorf("humanAgo(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{512: "512 B", 7_840_000: "7.84 MB", 1_200_000_000: "1.2 GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCLILoginPushLogout(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "DOCKER_CONFIG", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(k, home)
	}
	t.Setenv("REGISTRY_AUTH_FILE", "")
	t.Setenv("AIC_STORE", t.TempDir())
	authPath := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("AIC_AUTH_FILE", authPath)

	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	login := func(input string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runWithInput(append([]string{"login"}, args...), strings.NewReader(input), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	if code, _, stderr := login("wrong\n", "-u", "user", "--password-stdin", host); code != exitError || !strings.Contains(stderr, "invalid username or password") {
		t.Fatalf("bad password: code %d, %s", code, stderr)
	}
	if _, err := os.Stat(authPath); err == nil {
		t.Error("credentials saved after a failed login")
	}
	if code, _, _ := login("", "-u", "user", host); code != exitUsage {
		t.Errorf("non-interactive login without a password: code %d", code)
	}
	code, stdout, stderr := login("pass\n", "-u", "user", "--password-stdin", host)
	if code != exitOK || !strings.Contains(stdout, "Login Succeeded!") {
		t.Fatalf("login: code %d\n%s%s", code, stdout, stderr)
	}
	if code, stdout, _ = runCLI(t, "login", "--get-login", host); code != exitOK || strings.TrimSpace(stdout) != "user" {
		t.Errorf("--get-login = %d %q", code, stdout)
	}

	// Pushing uses the saved credentials.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644)
	if code, _, stderr = runCLI(t, "build", "-q", "-t", "app", dir); code != exitOK {
		t.Fatalf("build: %s", stderr)
	}
	if code, _, stderr = runCLI(t, "push", "-q", "app", host+"/team/app:1"); code != exitOK {
		t.Fatalf("push with credentials: %s", stderr)
	}

	if code, stdout, _ = runCLI(t, "logout", host); code != exitOK || !strings.Contains(stdout, "Removed login credentials for "+host) {
		t.Errorf("logout = %d %q", code, stdout)
	}
	if code, _, stderr = runCLI(t, "logout", host); code != exitError || !strings.Contains(stderr, "not logged in") {
		t.Errorf("second logout = %d %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "push", "-q", "app", host+"/team/app:2"); code != exitError {
		t.Errorf("push after logout succeeded")
	}
	if code, stdout, _ = runCLI(t, "logout", "--all"); code != exitOK {
		t.Errorf("logout --all = %d %q", code, stdout)
	}
}

func TestCLICacheAndPrune(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644)
	if code, _, stderr := runCLI(t, "build", dir); code != exitOK || strings.Contains(stderr, "Using cache") {
		t.Fatalf("first build: %d %s", code, stderr)
	}
	if _, _, stderr := runCLI(t, "build", dir); !strings.Contains(stderr, "Using cache") {
		t.Errorf("second build did not use the cache:\n%s", stderr)
	}
	if _, _, stderr := runCLI(t, "build", "--no-cache", dir); strings.Contains(stderr, "Using cache") {
		t.Errorf("--no-cache used the cache:\n%s", stderr)
	}
	if code, stdout, _ := runCLI(t, "prune"); code != exitOK || stdout != "Removed 1 build cache entries\n" {
		t.Errorf("prune = %d %q", code, stdout)
	}
	if _, _, stderr := runCLI(t, "build", dir); strings.Contains(stderr, "Using cache") {
		t.Errorf("build after prune used the cache:\n%s", stderr)
	}
}

func TestCLITagging(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644)

	if code, _, stderr := runCLI(t, "build", "-t", "app:latest", dir); code != exitUsage || !strings.Contains(stderr, `does not tag images as "latest"`) {
		t.Errorf("-t app:latest: %d %s", code, stderr)
	}
	code, stdout, stderr := runCLI(t, "build", "-t", "app", "-t", "app:1.0", dir)
	if code != exitOK {
		t.Fatalf("build: %s", stderr)
	}
	digest := strings.TrimSpace(stdout)
	want := "localhost/app:sha256-" + strings.TrimPrefix(digest, "sha256:")
	if !strings.Contains(stderr, "Successfully tagged "+want) || strings.Contains(stderr, ":latest") {
		t.Errorf("tags:\n%s", stderr)
	}
	_, stdout, _ = runCLI(t, "images")
	if strings.Contains(stdout, "latest") || !strings.Contains(stdout, "sha256-") || !strings.Contains(stdout, "1.0") {
		t.Errorf("images:\n%s", stdout)
	}

	// The repository name alone finds its image...
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	if code, _, stderr = runCLI(t, "push", "-q", "app", host+"/team/app"); code != exitOK {
		t.Fatalf("push: %s", stderr)
	}
	// ...and a destination without a tag is tagged by digest, not latest.
	if _, err := crane.Digest(host + "/team/app:sha256-" + strings.TrimPrefix(digest, "sha256:")); err != nil {
		t.Errorf("digest tag not in registry: %v", err)
	}
	if _, err := crane.Digest(host + "/team/app:latest"); err == nil {
		t.Error("pushed a latest tag")
	}
	if code, _, stderr = runCLI(t, "push", "app", host+"/team/app:latest"); code != exitUsage {
		t.Errorf("push to :latest: %d %s", code, stderr)
	}

	// After a rebuild, the name alone refers to the newest image.
	os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY other /\n"), 0o644)
	_, stdout, _ = runCLI(t, "build", "-q", "-t", "app", dir)
	newDigest := strings.TrimSpace(stdout)
	if newDigest == digest {
		t.Fatal("rebuild produced the same image")
	}
	if code, _, stderr = runCLI(t, "push", "-q", "app", host+"/team/app"); code != exitOK {
		t.Fatalf("push after rebuild: %d %s", code, stderr)
	}
	if _, err := crane.Digest(host + "/team/app:sha256-" + strings.TrimPrefix(newDigest, "sha256:")); err != nil {
		t.Errorf("pushed an older image: %v", err)
	}
}

func TestCLITag(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644)
	code, stdout, stderr := runCLI(t, "build", "-q", "-t", "app:1", dir)
	if code != exitOK {
		t.Fatalf("build: %s", stderr)
	}
	hex := strings.TrimPrefix(strings.TrimSpace(stdout), "sha256:")

	if code, stdout, stderr = runCLI(t, "tag", "app:1", "app:2", "registry.example.com/team/app:2", "other"); code != exitOK || stdout != "" {
		t.Fatalf("tag: %d %q %s", code, stdout, stderr)
	}
	_, stdout, _ = runCLI(t, "images")
	rows := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			rows[f[0]+":"+f[1]] = true
		}
	}
	for _, want := range []string{"localhost/app:1", "localhost/app:2", "registry.example.com/team/app:2", "localhost/other:sha256-" + hex} {
		if !rows[want] {
			t.Errorf("images missing %s:\n%s", want, stdout)
		}
	}
	if _, stdout, _ = runCLI(t, "images", "-q"); strings.TrimSpace(stdout) != hex[:12] {
		t.Errorf("tagging created another image: %q", stdout)
	}
	// By ID works too.
	if code, _, stderr = runCLI(t, "tag", hex[:12], "app:3"); code != exitOK {
		t.Errorf("tag by ID: %s", stderr)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"tag", "app:1"}, exitUsage, "requires at least 2 arg"},
		{[]string{"tag", "app:1", "app:latest"}, exitUsage, `"latest"`},
		{[]string{"tag", "app:1", "app@sha256:" + hex}, exitUsage, "digest can't be used as a tag"},
		{[]string{"tag", "nope:1", "app:9"}, exitError, "image not known"},
	} {
		if code, _, stderr := runCLI(t, tc.args...); code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("aic %q = %d %s; want %d %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestCLIRunNoCommand(t *testing.T) {
	t.Setenv("AIC_STORE", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nLABEL a=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "build", "-q", "-t", "nocmd", dir); code != exitOK {
		t.Fatalf("build = %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "run", "nocmd"); code != exitUsage || !strings.Contains(stderr, "no default command") {
		t.Errorf("run = %d: %s", code, stderr)
	}
}

func TestParseWorkspace(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ in, dir, path string }{
		{dir, dir, "/workspace"},
		{dir + ":/src", dir, "/src"},
		{dir + ":/src/../app/", dir, "/app"},
	} {
		d, p, err := parseWorkspace(tc.in)
		if err != nil || d != tc.dir || p != tc.path {
			t.Errorf("parseWorkspace(%q) = %q, %q, %v; want %q, %q", tc.in, d, p, err, tc.dir, tc.path)
		}
	}
	for _, in := range []string{dir + ":/", dir + ":/..", filepath.Join(dir, "missing")} {
		if _, _, err := parseWorkspace(in); err == nil {
			t.Errorf("parseWorkspace(%q) succeeded", in)
		}
	}
}

func TestCLIClean(t *testing.T) {
	t.Setenv("AIC_STORE", filepath.Join(t.TempDir(), "store"))
	t.Setenv("AIC_VM_DIR", filepath.Join(t.TempDir(), "vm"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY Dockerfile /\n"), 0o644)
	if code, _, stderr := runCLI(t, "build", "-q", "-t", "app", dir); code != exitOK {
		t.Fatalf("build: %s", stderr)
	}
	os.MkdirAll(os.Getenv("AIC_VM_DIR"), 0o755)
	os.WriteFile(filepath.Join(os.Getenv("AIC_VM_DIR"), "kernel"), []byte("k"), 0o644)

	// Without a terminal, --force is required.
	if code, _, stderr := runCLI(t, "clean"); code != exitUsage || !strings.Contains(stderr, "--force") {
		t.Errorf("clean without --force = %d: %s", code, stderr)
	}
	code, stdout, stderr := runCLI(t, "clean", "--force")
	if code != exitOK || !strings.Contains(stdout, "Removed 1 image") {
		t.Fatalf("clean = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, stdout, _ := runCLI(t, "images", "-q"); strings.TrimSpace(stdout) != "" {
		t.Errorf("images after clean: %q", stdout)
	}
	if _, err := os.Stat(os.Getenv("AIC_VM_DIR")); err == nil {
		t.Error("VM dir survived clean")
	}
}
