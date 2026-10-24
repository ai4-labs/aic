package aic

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/ai4-labs/aic/internal/builder"
	"github.com/ai4-labs/aic/internal/executor"
	"github.com/ai4-labs/aic/internal/store"
)

// Image identifies an image in the store.
type Image struct {
	Digest string   // "sha256:..."
	Names  []string // full names, e.g. "localhost/app:sha256-..."
}

// BuildOptions configures Build.
type BuildOptions struct {
	// ContextDir is the build context (default ".").
	ContextDir string
	// Dockerfile is the Dockerfile's path (default ContextDir/Dockerfile).
	Dockerfile string
	// Tags name the image. A name without a tag is tagged
	// sha256-<digest>; unqualified names go under localhost/; "latest"
	// is rejected.
	Tags []string
	// BuildArgs sets ARG values.
	BuildArgs map[string]string
	// Target builds up to this stage.
	Target string
	// Platform is "OS/ARCH[/VARIANT]" (default linux/<host arch>).
	Platform string
	// Pull always pulls base images; NoCache runs every step.
	Pull, NoCache bool
	// Created is the image creation time (default $SOURCE_DATE_EPOCH,
	// else now).
	Created time.Time
	// Progress receives the build's steps; Stdout and Stderr the output
	// of RUN instructions. Nil discards.
	Progress, Stdout, Stderr io.Writer
}

// Build builds an image from a Dockerfile and stores it.
func (c *Client) Build(ctx context.Context, o BuildOptions) (Image, error) {
	plat, err := ParsePlatform(o.Platform)
	if err != nil {
		return Image{}, err
	}
	if o.ContextDir == "" {
		o.ContextDir = "."
	}
	if fi, err := os.Stat(o.ContextDir); err != nil || !fi.IsDir() {
		return Image{}, invalidf("context %q is not a directory", o.ContextDir)
	}
	if o.Dockerfile == "" {
		o.Dockerfile = filepath.Join(o.ContextDir, "Dockerfile")
	}
	if _, err := os.Stat(o.Dockerfile); err != nil {
		return Image{}, invalidf("no Dockerfile: %v", err)
	}
	refs, digestRepos, err := parseTags(o.Tags)
	if err != nil {
		return Image{}, err
	}
	if o.Created.IsZero() {
		if sde := os.Getenv("SOURCE_DATE_EPOCH"); sde != "" {
			sec, err := strconv.ParseInt(sde, 10, 64)
			if err != nil {
				return Image{}, invalidf("invalid SOURCE_DATE_EPOCH %q", sde)
			}
			o.Created = time.Unix(sec, 0)
		}
	}
	progress, stdout, stderr := orDiscard(o.Progress), orDiscard(o.Stdout), orDiscard(o.Stderr)
	img, err := builder.Build(ctx, builder.Options{
		ContextDir:  o.ContextDir,
		Dockerfile:  o.Dockerfile,
		Tags:        refs,
		DigestRepos: digestRepos,
		BuildArgs:   o.BuildArgs,
		Target:      o.Target,
		Platform:    plat,
		Pull:        o.Pull,
		NoCache:     o.NoCache,
		Created:     o.Created,
		Store:       c.store,
		NewExecutor: func() (executor.Executor, error) { return c.newExecutor(progress) },
		Progress:    progress,
		Stdout:      stdout,
		Stderr:      stderr,
	})
	if err != nil {
		return Image{}, err
	}
	d, err := img.Digest()
	if err != nil {
		return Image{}, err
	}
	for _, repo := range digestRepos {
		refs = append(refs, store.DigestTag(repo, d))
	}
	out := Image{Digest: d.String()}
	for _, r := range refs {
		out.Names = append(out.Names, builder.DisplayName(r))
	}
	return out, nil
}

// parseTags splits names into tags and repositories to tag by digest.
func parseTags(names []string) ([]name.Reference, []name.Repository, error) {
	var refs []name.Reference
	var repos []name.Repository
	for _, t := range names {
		tag, repo, ok, err := store.ParseTag(t)
		if err != nil {
			return nil, nil, invalidf("invalid tag %q: %v", t, err)
		}
		if ok {
			refs = append(refs, tag)
		} else {
			repos = append(repos, repo)
		}
	}
	return refs, repos, nil
}

// ParsePlatform parses "OS/ARCH[/VARIANT]"; "" is linux/<host arch>.
func ParsePlatform(s string) (v1.Platform, error) {
	if s == "" {
		return builder.DefaultPlatform(), nil
	}
	p, err := v1.ParsePlatform(s)
	if err != nil {
		return v1.Platform{}, invalidError{err}
	}
	if p.OS == "" || p.Architecture == "" {
		return v1.Platform{}, invalidf("invalid platform %q: want OS/ARCH[/VARIANT]", s)
	}
	return *p, nil
}

// PlatformString formats a platform as OS/ARCH[/VARIANT].
func PlatformString(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
