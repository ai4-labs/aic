package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"golang.org/x/term"

	"github.com/ai4-labs/aic/internal/auth"
)

// prompter asks questions on a terminal. Its parts are replaceable for
// tests.
type prompter struct {
	in     *bufio.Reader
	out    io.Writer
	secret func() (string, error)                                                // reads without echo
	run    func(ctx context.Context, bin string, args ...string) (string, error) // runs a helper CLI
	look   func(bin string) bool                                                 // is bin installed?
	open   func(url string) error                                                // opens a browser
	verify func(ctx context.Context, reg name.Registry, user, pass string) error
}

func terminalPrompter(stdin *os.File, out io.Writer) *prompter {
	fd := int(stdin.Fd())
	return &prompter{
		in:  bufio.NewReader(stdin),
		out: out,
		secret: func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(out)
			return string(b), err
		},
		run: func(ctx context.Context, bin string, args ...string) (string, error) {
			c := exec.CommandContext(ctx, bin, args...)
			c.Stderr = out
			b, err := c.Output()
			return strings.TrimSpace(string(b)), err
		},
		look: func(bin string) bool { _, err := exec.LookPath(bin); return err == nil },
		open: func(url string) error {
			opener := "xdg-open"
			if runtime.GOOS == "darwin" {
				opener = "open"
			}
			return exec.Command(opener, url).Start()
		},
		verify: auth.Verify,
	}
}

func (p *prompter) line(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", prompt)
	}
	s, err := p.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		return "", err
	}
	if s = strings.TrimSpace(s); s == "" {
		s = def
	}
	return s, nil
}

func (p *prompter) yes(prompt string, def bool) (bool, error) {
	d := "Y/n"
	if !def {
		d = "y/N"
	}
	s, err := p.line(prompt+" ("+d+")", "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// registryKind describes how to log in to a kind of registry.
type registryKind struct {
	title    string
	host     string // fixed host, or "" if it must be asked
	user     string // fixed user name, or "" to ask
	userHint string
	tokenURL string
	tokenFor string // how to get a token, shown to the user
	// cli fetches a token with an installed tool (gcloud, aws).
	cli func(p *prompter, ctx context.Context, host string) (string, error)
}

var (
	gcpHostRE = regexp.MustCompile(`^[a-z0-9-]+-docker\.pkg\.dev$|^(us|eu|asia)?\.?gcr\.io$`)
	ecrHostRE = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$`)
	regionRE  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	accountRE = regexp.MustCompile(`^\d{12}$`)
)

var registryKinds = []registryKind{
	{
		title: "Docker Hub", host: "docker.io", userHint: "your Docker ID",
		tokenURL: "https://app.docker.com/settings/personal-access-tokens",
		tokenFor: "a personal access token (Read & Write)",
	},
	{
		title: "GitHub Container Registry", host: "ghcr.io", userHint: "your GitHub user name",
		tokenURL: "https://github.com/settings/tokens/new?scopes=write:packages&description=aic",
		tokenFor: "a personal access token (classic) with the write:packages scope",
	},
	{
		title: "Google Artifact Registry", user: "oauth2accesstoken",
		tokenFor: "an access token from `gcloud auth print-access-token`",
		cli: func(p *prompter, ctx context.Context, host string) (string, error) {
			if !p.look("gcloud") {
				return "", nil
			}
			ok, err := p.yes("Get a token with `gcloud auth print-access-token`?", true)
			if err != nil || !ok {
				return "", err
			}
			return p.run(ctx, "gcloud", "auth", "print-access-token")
		},
	},
	{
		title: "Amazon ECR", user: "AWS",
		tokenFor: "a password from `aws ecr get-login-password`",
		cli: func(p *prompter, ctx context.Context, host string) (string, error) {
			m := ecrHostRE.FindStringSubmatch(host)
			if m == nil || !p.look("aws") {
				return "", nil
			}
			ok, err := p.yes("Get a password with `aws ecr get-login-password --region "+m[2]+"`?", true)
			if err != nil || !ok {
				return "", err
			}
			return p.run(ctx, "aws", "ecr", "get-login-password", "--region", m[2])
		},
	},
	{
		title: "Quay.io", host: "quay.io", userHint: "your Quay user name or robot account (org+robot)",
		tokenURL: "https://quay.io/user/?tab=settings",
		tokenFor: "an encrypted password or robot token",
	},
	{title: "Another registry", userHint: "your user name", tokenFor: "your password or an access token"},
}

// kindFor guesses the kind of a registry given on the command line.
func kindFor(host string) *registryKind {
	switch {
	case host == "docker.io" || host == name.DefaultRegistry:
		return &registryKinds[0]
	case host == "ghcr.io":
		return &registryKinds[1]
	case gcpHostRE.MatchString(host):
		return &registryKinds[2]
	case ecrHostRE.MatchString(host):
		return &registryKinds[3]
	case host == "quay.io":
		return &registryKinds[4]
	}
	return &registryKinds[5]
}

// askHost asks for the registry host of kinds that have many.
func (p *prompter) askHost(k *registryKind) (string, error) {
	switch k.title {
	case "Google Artifact Registry":
		loc, err := p.line("Repository location (e.g. us, europe-west1)", "us")
		if err != nil {
			return "", err
		}
		host := loc + "-docker.pkg.dev"
		if !gcpHostRE.MatchString(host) {
			return "", fmt.Errorf("invalid location %q", loc)
		}
		return host, nil
	case "Amazon ECR":
		acct, err := p.line("AWS account ID (12 digits)", "")
		if err != nil {
			return "", err
		}
		region, err := p.line("Region", "us-east-1")
		if err != nil {
			return "", err
		}
		if !accountRE.MatchString(acct) || !regionRE.MatchString(region) {
			return "", fmt.Errorf("invalid account ID %q or region %q", acct, region)
		}
		return acct + ".dkr.ecr." + region + ".amazonaws.com", nil
	}
	return p.line("Registry (e.g. registry.example.com or localhost:5000)", "")
}

const maxLoginAttempts = 3

// interactiveLogin guides the user through logging in. registry may be
// empty, in which case the user picks one.
func interactiveLogin(ctx context.Context, p *prompter, f *auth.File, registry string) error {
	var k *registryKind
	host := registry
	if host == "" {
		fmt.Fprintln(p.out, "Which registry do you want to log in to?")
		for i, rk := range registryKinds {
			label := rk.title
			if rk.host != "" {
				label += " (" + rk.host + ")"
			}
			fmt.Fprintf(p.out, "  %d) %s\n", i+1, label)
		}
		choice, err := p.line("Choose", "1")
		if err != nil {
			return err
		}
		i, err := strconv.Atoi(choice)
		if err != nil || i < 1 || i > len(registryKinds) {
			return usagef("invalid choice %q", choice)
		}
		k = &registryKinds[i-1]
		if host = k.host; host == "" {
			if host, err = p.askHost(k); err != nil {
				return err
			}
		}
	}
	reg, err := auth.ParseRegistry(host)
	if err != nil {
		return usageError{err}
	}
	if k == nil {
		k = kindFor(reg.RegistryStr())
	}
	display := registryName(reg)

	fmt.Fprintf(p.out, "\nLogging in to %s.\n", display)
	fmt.Fprintf(p.out, "You need %s.\n", k.tokenFor)
	if k.tokenURL != "" {
		fmt.Fprintf(p.out, "Create one at %s\n", k.tokenURL)
		if ok, err := p.yes("Open that page in your browser?", false); err != nil {
			return err
		} else if ok {
			if err := p.open(k.tokenURL); err != nil {
				fmt.Fprintf(p.out, "Could not open a browser: %v\n", err)
			}
		}
	}
	fmt.Fprintln(p.out)

	user := k.user
	if user == "" {
		prev, err := f.Username(reg)
		if err != nil {
			return err
		}
		prompt := "Username"
		if k.userHint != "" {
			prompt += " (" + k.userHint + ")"
		}
		if user, err = p.line(prompt, prev); err != nil {
			return err
		}
		if user == "" {
			return usagef("a user name is required")
		}
	} else {
		fmt.Fprintf(p.out, "Username: %s\n", user)
	}

	for attempt := 1; ; attempt++ {
		var password string
		if k.cli != nil {
			if password, err = k.cli(p, ctx, reg.RegistryStr()); err != nil {
				return fmt.Errorf("getting a token: %w", err)
			}
		}
		if password == "" {
			fmt.Fprint(p.out, "Password or token (hidden): ")
			if password, err = p.secret(); err != nil {
				return err
			}
		}
		if password == "" {
			return usagef("a password or token is required")
		}
		fmt.Fprintf(p.out, "Checking with %s... ", display)
		err := p.verify(ctx, reg, user, password)
		if err == nil {
			fmt.Fprintln(p.out, "ok")
			if err := f.Login(reg, user, password); err != nil {
				return fmt.Errorf("saving credentials: %w", err)
			}
			where := f.Path()
			if f.Store() != "" {
				where = "the " + f.Store() + " credential helper"
			}
			fmt.Fprintf(p.out, "\nLogin Succeeded! Logged in to %s as %s (saved in %s).\n", display, user, where)
			fmt.Fprintf(p.out, "Push an image with: aic push IMAGE %s\n", pushExample(reg, user))
			return nil
		}
		fmt.Fprintln(p.out, "failed")
		if !errors.Is(err, auth.ErrUnauthorized) || attempt == maxLoginAttempts {
			return fmt.Errorf("logging in to %s: %w", display, err)
		}
		fmt.Fprintf(p.out, "The registry rejected the user name or token. Try again (%d of %d).\n", attempt+1, maxLoginAttempts)
	}
}

// pushExample suggests a destination in reg for the user.
func pushExample(reg name.Registry, user string) string {
	switch h := reg.RegistryStr(); {
	case h == name.DefaultRegistry:
		return "docker.io/" + user + "/NAME"
	case h == "ghcr.io" || h == "quay.io":
		return h + "/" + strings.ToLower(user) + "/NAME"
	case strings.HasSuffix(h, "-docker.pkg.dev"):
		return h + "/PROJECT/REPOSITORY/NAME"
	default:
		return h + "/NAME"
	}
}
