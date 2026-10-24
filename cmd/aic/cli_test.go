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

	"github.com/google/go-containerregistry/pkg/registry"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
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
