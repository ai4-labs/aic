package aic

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/ai4-labs/aic/internal/auth"
	"github.com/ai4-labs/aic/internal/builder"
	"github.com/ai4-labs/aic/internal/store"
)

// Pull pulls ref for platform ("OS/ARCH[/VARIANT]", default linux/<host
// arch>) into the store and returns its digest.
func (c *Client) Pull(ctx context.Context, ref, platform string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", invalidError{err}
	}
	plat, err := ParsePlatform(platform)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(c.opts.Progress, "Pulling %s (%s)...\n", builder.DisplayName(r), PlatformString(plat))
	img, err := c.pullWith(ctx, r, plat, c.opts.Progress)
	if err != nil {
		return "", err
	}
	d, err := img.Digest()
	return d.String(), err
}

func (c *Client) pullWith(ctx context.Context, r name.Reference, plat v1.Platform, progress io.Writer) (v1.Image, error) {
	return c.store.PullWithProgress(ctx, r, plat, progress)
}

// Push uploads the local image (a name, digest or ID) to destination
// and returns the name it was pushed to and its digest. An empty
// destination pushes to the image's own name, which must include a
// registry. A destination without a tag is tagged sha256-<digest>;
// "latest" is rejected.
func (c *Client) Push(ctx context.Context, image, destination string) (pushed, digest string, err error) {
	parse, target := store.ParseTag, image
	if destination != "" {
		parse, target = store.ParseDestination, destination
	}
	tag, repo, ok, err := parse(target)
	if err != nil {
		return "", "", invalidError{err}
	}
	if repo.RegistryStr() == "localhost" {
		return "", "", invalidf("%s has no registry; give a destination, e.g. aic push %s registry.example.com/%s",
			repo.Name(), image, strings.TrimPrefix(repo.RepositoryStr(), "localhost/"))
	}
	img, err := c.store.Image(image)
	if err != nil {
		return "", "", err
	}
	d, err := img.Digest()
	if err != nil {
		return "", "", err
	}
	if !ok {
		tag = store.DigestTag(repo, d) // never "latest"
	}
	fmt.Fprintf(c.opts.Progress, "Pushing %s to %s...\n", image, builder.DisplayName(tag))
	pd, err := c.store.Push(ctx, d.String(), tag)
	if err != nil {
		return "", "", err
	}
	return builder.DisplayName(tag), pd.String(), nil
}

// LoginOptions configures Login.
type LoginOptions struct {
	Registry, Username, Password string
}

// Login checks the credentials with the registry and saves them to the
// Client's AuthFile (or a Docker credential helper, if one was installed
// when the file was created).
func (c *Client) Login(ctx context.Context, o LoginOptions) error {
	reg, err := auth.ParseRegistry(o.Registry)
	if err != nil {
		return invalidError{err}
	}
	if o.Username == "" || o.Password == "" {
		return invalidf("a user name and a password are required")
	}
	f, err := auth.OpenDefault(c.opts.AuthFile)
	if err != nil {
		return err
	}
	if err := auth.Verify(ctx, reg, o.Username, o.Password); err != nil {
		return fmt.Errorf("logging in to %s: %w", RegistryName(reg), err)
	}
	if err := f.Login(reg, o.Username, o.Password); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	return nil
}

// Logout removes the credentials Login saved for registry. It reports
// whether there were any.
func (c *Client) Logout(registry string) (bool, error) {
	reg, err := auth.ParseRegistry(registry)
	if err != nil {
		return false, invalidError{err}
	}
	f, err := auth.OpenDefault(c.opts.AuthFile)
	if err != nil {
		return false, err
	}
	return f.Logout(reg)
}

// LogoutAll removes all credentials Login saved.
func (c *Client) LogoutAll() error {
	f, err := auth.OpenDefault(c.opts.AuthFile)
	if err != nil {
		return err
	}
	_, err = f.LogoutAll()
	return err
}

// LoggedInUser returns the user name used for registry: from Login's
// credentials, else Docker's or podman's. It is "" if there are none.
func (c *Client) LoggedInUser(registry string) (string, error) {
	reg, err := auth.ParseRegistry(registry)
	if err != nil {
		return "", invalidError{err}
	}
	f, err := auth.OpenDefault(c.opts.AuthFile)
	if err != nil {
		return "", err
	}
	if u, err := f.Username(reg); err != nil || u != "" {
		return u, err
	}
	a, err := auth.Keychain(c.opts.AuthFile).Resolve(reg)
	if err != nil || a == authn.Anonymous {
		return "", err
	}
	cfg, err := a.Authorization()
	if err != nil {
		return "", err
	}
	return cfg.Username, nil
}

// RegistryName is a registry's name as users write it ("docker.io" for
// Docker Hub).
func RegistryName(reg name.Registry) string {
	if reg.RegistryStr() == name.DefaultRegistry {
		return "docker.io"
	}
	return reg.RegistryStr()
}
