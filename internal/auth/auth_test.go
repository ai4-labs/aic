package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
)

// basicAuthRegistry is a registry that requires user:pass.
func basicAuthRegistry(t *testing.T) (name.Registry, func()) {
	t.Helper()
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	r, err := name.NewRegistry(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return r, srv.Close
}

func TestParseRegistry(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io":                     "ghcr.io",
		"docker.io":                   name.DefaultRegistry,
		"https://index.docker.io/v1/": name.DefaultRegistry,
		"quay.io/org/repo":            "quay.io",
		"localhost:5000":              "localhost:5000",
	} {
		r, err := ParseRegistry(in)
		if err != nil || r.RegistryStr() != want {
			t.Errorf("ParseRegistry(%q) = %q, %v; want %q", in, r.RegistryStr(), err, want)
		}
	}
	if _, err := ParseRegistry(""); err == nil {
		t.Error("ParseRegistry(\"\") succeeded")
	}
}

// isolate keeps the user's Docker and podman credentials out of tests.
func isolate(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "DOCKER_CONFIG", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(k, home)
	}
	t.Setenv("REGISTRY_AUTH_FILE", "")
}

func TestLoginLogout(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "auth.json")
	f, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	ghcr, _ := ParseRegistry("ghcr.io")
	hub, _ := ParseRegistry("docker.io")
	if err := f.Login(ghcr, "alice", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if err := f.Login(hub, "bob", "hunter2"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %v, %v", fi.Mode(), err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), authn.DefaultAuthKey) {
		t.Errorf("Docker Hub not stored under %s:\n%s", authn.DefaultAuthKey, b)
	}

	// A fresh open sees the saved logins, and the keychain resolves them.
	f, _ = Open(path, "")
	if u, _ := f.Username(ghcr); u != "alice" {
		t.Errorf("Username(ghcr) = %q", u)
	}
	kc := Keychain(path)
	for ref, wantUser := range map[string]string{"ghcr.io/org/app:1": "alice", "alpine": "bob", "quay.io/x/y": ""} {
		r, _ := name.ParseReference(ref)
		a, err := kc.Resolve(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := a.Authorization()
		if cfg.Username != wantUser && !(wantUser == "" && a == authn.Anonymous) {
			t.Errorf("%s: user %q, want %q", ref, cfg.Username, wantUser)
		}
	}

	if found, err := f.Logout(ghcr); err != nil || !found {
		t.Errorf("Logout(ghcr) = %v, %v", found, err)
	}
	if found, _ := f.Logout(ghcr); found {
		t.Error("second Logout(ghcr) found credentials")
	}
	removed, err := f.LogoutAll()
	if err != nil || len(removed) != 1 {
		t.Errorf("LogoutAll = %q, %v", removed, err)
	}
	f, _ = Open(path, "")
	if u, _ := f.Username(hub); u != "" {
		t.Errorf("still logged in to Docker Hub as %q", u)
	}
}

func TestVerify(t *testing.T) {
	reg, stop := basicAuthRegistry(t)
	defer stop()
	ctx := context.Background()
	if err := Verify(ctx, reg, "user", "pass"); err != nil {
		t.Errorf("valid credentials: %v", err)
	}
	if err := Verify(ctx, reg, "user", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("invalid credentials: %v", err)
	}
}
