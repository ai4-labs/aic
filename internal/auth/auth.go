// Package auth manages registry credentials for aic login and logout,
// and resolves credentials for pulls and pushes.
//
// aic keeps its own credentials file, ~/.aic/auth.json, in the format of
// Docker's config.json. If a Docker credential helper is configured
// ("credsStore" or "credHelpers"), or the platform's default helper is
// installed when the file is created (docker-credential-osxkeychain on
// macOS, -secretservice or -pass on Linux), secrets are kept there instead
// of in the file. Lookups fall back to Docker's and podman's files.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// DefaultPath returns $AIC_AUTH_FILE, or ~/.aic/auth.json.
func DefaultPath() (string, error) {
	if p := os.Getenv("AIC_AUTH_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aic", "auth.json"), nil
}

// File is a credentials file.
type File struct {
	cf *configfile.ConfigFile
}

// Open loads the credentials file at path. A missing file is treated as
// empty; if it is created, newStore is recorded as its credentials store
// ("" keeps secrets in the file).
func Open(path, newStore string) (*File, error) {
	cf := configfile.New(path)
	f, err := os.Open(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cf.CredentialsStore = newStore
	case err != nil:
		return nil, err
	default:
		defer f.Close()
		if err := cf.LoadFromReader(f); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return &File{cf: cf}, nil
}

// OpenDefault opens the default credentials file, using the platform's
// credential helper for new files when one is installed.
func OpenDefault(path string) (*File, error) {
	return Open(path, credentials.DetectDefaultStore(""))
}

// Path returns the file's location.
func (f *File) Path() string { return f.cf.Filename }

// Store returns where secrets are kept: a credential helper name, or "".
func (f *File) Store() string { return f.cf.CredentialsStore }

// ParseRegistry accepts "ghcr.io", "docker.io", "localhost:5000",
// "https://index.docker.io/v1/" or "quay.io/org/repo" and returns the
// registry.
func ParseRegistry(s string) (name.Registry, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	host, _, _ := strings.Cut(s, "/")
	if host == "" {
		return name.Registry{}, fmt.Errorf("invalid registry %q", s)
	}
	return name.NewRegistry(host)
}

// keys returns the config keys a registry's credentials may be stored
// under. The first is the one aic writes. Docker Hub has historical
// aliases.
func keys(reg name.Registry) []string {
	r := reg.RegistryStr()
	if r == name.DefaultRegistry {
		return []string{authn.DefaultAuthKey, name.DefaultRegistry, "docker.io"}
	}
	return []string{r}
}

func (f *File) get(reg name.Registry) (types.AuthConfig, error) {
	for _, k := range keys(reg) {
		ac, err := f.cf.GetAuthConfig(k)
		if err != nil {
			return types.AuthConfig{}, err
		}
		if ac.Username != "" || ac.Password != "" || ac.Auth != "" || ac.IdentityToken != "" || ac.RegistryToken != "" {
			return ac, nil
		}
	}
	return types.AuthConfig{}, nil
}

// Username returns the user logged in to reg, or "".
func (f *File) Username(reg name.Registry) (string, error) {
	ac, err := f.get(reg)
	return ac.Username, err
}

// Login saves credentials for reg.
func (f *File) Login(reg name.Registry, username, password string) error {
	k := keys(reg)[0]
	return f.cf.GetCredentialsStore(k).Store(types.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: k,
	})
}

// Logout removes the credentials for reg. It reports whether there were
// any.
func (f *File) Logout(reg name.Registry) (bool, error) {
	found := false
	for _, k := range keys(reg) {
		ac, err := f.cf.GetAuthConfig(k)
		if err != nil {
			return found, err
		}
		if ac.Username == "" && ac.Password == "" && ac.Auth == "" && ac.IdentityToken == "" {
			if _, ok := f.cf.AuthConfigs[k]; !ok {
				continue
			}
		}
		found = true
		if err := f.cf.GetCredentialsStore(k).Erase(k); err != nil {
			return found, err
		}
	}
	return found, nil
}

// LogoutAll removes all credentials and returns the registries they were for.
func (f *File) LogoutAll() ([]string, error) {
	all, err := f.cf.GetAllCredentials()
	if err != nil {
		return nil, err
	}
	var removed []string
	for k := range all {
		if err := f.cf.GetCredentialsStore(k).Erase(k); err != nil {
			return removed, err
		}
		removed = append(removed, k)
	}
	return removed, nil
}

// fileKeychain resolves credentials from a credentials file.
type fileKeychain struct {
	open func() (*File, error)
}

func (k fileKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	f, err := k.open()
	if err != nil || f == nil {
		return authn.Anonymous, err
	}
	reg, err := name.NewRegistry(target.RegistryStr())
	if err != nil {
		return authn.Anonymous, nil
	}
	ac, err := f.get(reg)
	if err != nil {
		return nil, err
	}
	if ac == (types.AuthConfig{}) {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(authn.AuthConfig{
		Username:      ac.Username,
		Password:      ac.Password,
		Auth:          ac.Auth,
		IdentityToken: ac.IdentityToken,
		RegistryToken: ac.RegistryToken,
	}), nil
}

// podmanAuthFile returns podman's credentials file, if there is one.
func podmanAuthFile() string {
	var candidates []string
	if p := os.Getenv("REGISTRY_AUTH_FILE"); p != "" {
		candidates = append(candidates, p)
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		candidates = append(candidates, filepath.Join(d, "containers", "auth.json"))
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		candidates = append(candidates, filepath.Join(d, "containers", "auth.json"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "containers", "auth.json"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Keychain resolves credentials from aic's file at path first, then from
// Docker's config (and its credential helpers), then from podman's.
func Keychain(path string) authn.Keychain {
	return authn.NewMultiKeychain(
		fileKeychain{open: func() (*File, error) { return Open(path, "") }},
		authn.DefaultKeychain,
		fileKeychain{open: func() (*File, error) {
			p := podmanAuthFile()
			if p == "" {
				return nil, nil
			}
			return Open(p, "")
		}},
	)
}

// ErrUnauthorized is returned by Verify for rejected credentials.
var ErrUnauthorized = errors.New("invalid username or password")

// Verify checks credentials against the registry: it authenticates and
// requests the API root.
func Verify(ctx context.Context, reg name.Registry, username, password string) error {
	auth := authn.FromConfig(authn.AuthConfig{Username: username, Password: password})
	tr, err := transport.NewWithContext(ctx, reg, auth, http.DefaultTransport, []string{reg.Scope(transport.PullScope)})
	if err != nil {
		return unauthorized(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Scheme()+"://"+reg.RegistryStr()+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return unauthorized(err)
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode >= 300:
		return fmt.Errorf("%s: unexpected status %s", reg.RegistryStr(), resp.Status)
	}
	return nil
}

func unauthorized(err error) error {
	var te *transport.Error
	if errors.As(err, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden) {
		return ErrUnauthorized
	}
	return err
}
