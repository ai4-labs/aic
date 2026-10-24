package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/ai4-labs/aic/internal/auth"
)

type fakeTerm struct {
	out      bytes.Buffer
	secrets  []string
	ran      []string
	opened   []string
	tools    map[string]bool
	tokens   map[string]string // tool -> token it prints
	goodPass string
	checks   []string // "host user pass"
}

func (ft *fakeTerm) prompter(input string) *prompter {
	return &prompter{
		in:  bufio.NewReader(strings.NewReader(input)),
		out: &ft.out,
		secret: func() (string, error) {
			if len(ft.secrets) == 0 {
				return "", errors.New("no more secrets")
			}
			s := ft.secrets[0]
			ft.secrets = ft.secrets[1:]
			return s, nil
		},
		run: func(_ context.Context, bin string, args ...string) (string, error) {
			ft.ran = append(ft.ran, bin+" "+strings.Join(args, " "))
			return ft.tokens[bin], nil
		},
		look: func(bin string) bool { return ft.tools[bin] },
		open: func(url string) error { ft.opened = append(ft.opened, url); return nil },
		verify: func(_ context.Context, reg name.Registry, user, pass string) error {
			ft.checks = append(ft.checks, reg.RegistryStr()+" "+user+" "+pass)
			if pass != ft.goodPass {
				return auth.ErrUnauthorized
			}
			return nil
		},
	}
}

func openTestFile(t *testing.T) *auth.File {
	t.Helper()
	f, err := auth.Open(filepath.Join(t.TempDir(), "auth.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestInteractiveLoginGHCR(t *testing.T) {
	ft := &fakeTerm{secrets: []string{"wrong", "ghp_good"}, goodPass: "ghp_good"}
	f := openTestFile(t)
	// Menu choice 2 (ghcr.io), open the token page, user name.
	err := interactiveLogin(context.Background(), ft.prompter("2\ny\noctocat\n"), f, "")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, ft.out.String())
	}
	out := ft.out.String()
	for _, want := range []string{"GitHub Container Registry (ghcr.io)", "write:packages", "rejected the user name or token. Try again (2 of 3)", "Login Succeeded!", "aic push IMAGE ghcr.io/octocat/NAME"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(ft.opened) != 1 || !strings.HasPrefix(ft.opened[0], "https://github.com/settings/tokens") {
		t.Errorf("opened %q", ft.opened)
	}
	reg, _ := auth.ParseRegistry("ghcr.io")
	if u, _ := f.Username(reg); u != "octocat" {
		t.Errorf("saved user %q", u)
	}
}

func TestInteractiveLoginGCPWithGcloud(t *testing.T) {
	ft := &fakeTerm{tools: map[string]bool{"gcloud": true}, tokens: map[string]string{"gcloud": "ya29.token"}, goodPass: "ya29.token"}
	f := openTestFile(t)
	// Menu choice 3, location europe-west1, accept gcloud.
	if err := interactiveLogin(context.Background(), ft.prompter("3\neurope-west1\n\n"), f, ""); err != nil {
		t.Fatalf("login: %v\n%s", err, ft.out.String())
	}
	if len(ft.ran) != 1 || ft.ran[0] != "gcloud auth print-access-token" {
		t.Errorf("ran %q", ft.ran)
	}
	if len(ft.checks) != 1 || ft.checks[0] != "europe-west1-docker.pkg.dev oauth2accesstoken ya29.token" {
		t.Errorf("checks %q", ft.checks)
	}
}

func TestInteractiveLoginECRWithRegistryArg(t *testing.T) {
	ft := &fakeTerm{tools: map[string]bool{"aws": true}, tokens: map[string]string{"aws": "ecr-pass"}, goodPass: "ecr-pass"}
	f := openTestFile(t)
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	if err := interactiveLogin(context.Background(), ft.prompter("y\n"), f, host); err != nil {
		t.Fatalf("login: %v\n%s", err, ft.out.String())
	}
	if len(ft.ran) != 1 || ft.ran[0] != "aws ecr get-login-password --region eu-west-1" {
		t.Errorf("ran %q", ft.ran)
	}
	if !strings.Contains(ft.out.String(), "Username: AWS") {
		t.Errorf("output:\n%s", ft.out.String())
	}
}

func TestInteractiveLoginGivesUp(t *testing.T) {
	ft := &fakeTerm{secrets: []string{"a", "b", "c"}, goodPass: "never"}
	f := openTestFile(t)
	err := interactiveLogin(context.Background(), ft.prompter("\nn\nbob\n"), f, "")
	if !errors.Is(err, auth.ErrUnauthorized) || len(ft.checks) != maxLoginAttempts {
		t.Errorf("err = %v after %d checks", err, len(ft.checks))
	}
	reg, _ := auth.ParseRegistry("docker.io")
	if u, _ := f.Username(reg); u != "" {
		t.Errorf("saved credentials after failing: %q", u)
	}
}

func TestInteractiveLoginBadInput(t *testing.T) {
	for _, input := range []string{"9\n", "x\n", "4\nnot-an-account\nus-east-1\n", "3\nbad location!\n"} {
		ft := &fakeTerm{}
		if err := interactiveLogin(context.Background(), ft.prompter(input), openTestFile(t), ""); err == nil {
			t.Errorf("input %q: no error", input)
		}
		if len(ft.ran) != 0 {
			t.Errorf("input %q ran %q", input, ft.ran)
		}
	}
}

func TestLoginInteractiveFlags(t *testing.T) {
	for _, args := range [][]string{
		{"login", "-i", "-u", "x"},
		{"login", "-i", "a", "b"},
		{"login", "-i"}, // stdin is not a terminal in tests
	} {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Errorf("aic %q = %d, want %d", args, code, exitUsage)
		}
	}
}
