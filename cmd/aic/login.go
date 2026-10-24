package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/rakyll/ai-container/internal/auth"
)

func registryName(reg name.Registry) string {
	if reg.RegistryStr() == name.DefaultRegistry {
		return "docker.io"
	}
	return reg.RegistryStr()
}

func authFile(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return auth.DefaultPath()
}

type loginOptions struct {
	username, password, authfile         string
	passwordStdin, getLogin, interactive bool
}

func newLoginCmd() *cobra.Command {
	var o loginOptions
	cmd := &cobra.Command{
		Use:   "login [flags] REGISTRY",
		Short: "Log in to a container registry",
		Long: `Log in to a container registry.

The credentials are checked with the registry, then saved to ~/.aic/auth.json
($AIC_AUTH_FILE). If a Docker credential helper is installed when that file
is created (docker-credential-osxkeychain on macOS, -secretservice or -pass
on Linux), the secret is stored there instead of in the file.`,
		Example: `  aic login -i
  aic login ghcr.io
  aic login -u myuser docker.io
  echo "$TOKEN" | aic login -u myuser --password-stdin ghcr.io`,
		Args: args(func(cmd *cobra.Command, a []string) error {
			if o.interactive {
				return cobra.MaximumNArgs(1)(cmd, a)
			}
			return cobra.ExactArgs(1)(cmd, a)
		}),
		RunE: func(cmd *cobra.Command, a []string) error {
			if o.interactive {
				registry := ""
				if len(a) == 1 {
					registry = a[0]
				}
				return runInteractiveLogin(cmd, registry, o)
			}
			return runLogin(cmd, a[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.username, "username", "u", "", "user name")
	f.StringVarP(&o.password, "password", "p", "", "password (insecure: visible to other processes; prefer --password-stdin)")
	f.BoolVar(&o.passwordStdin, "password-stdin", false, "read the password or token from stdin")
	f.BoolVar(&o.getLogin, "get-login", false, "print the user name logged in to REGISTRY")
	f.StringVar(&o.authfile, "authfile", "", "credentials file (default ~/.aic/auth.json)")
	f.BoolVarP(&o.interactive, "interactive", "i", false, "guided login: pick a registry, get help creating a token, retry on mistakes")
	return cmd
}

func runInteractiveLogin(cmd *cobra.Command, registry string, o loginOptions) error {
	if o.username != "" || o.password != "" || o.passwordStdin || o.getLogin {
		return usagef("--interactive can't be combined with --username, --password, --password-stdin or --get-login")
	}
	stdin, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(stdin.Fd())) {
		return usagef("--interactive needs a terminal")
	}
	path, err := authFile(o.authfile)
	if err != nil {
		return err
	}
	f, err := auth.OpenDefault(path)
	if err != nil {
		return err
	}
	return interactiveLogin(cmd.Context(), terminalPrompter(stdin, cmd.ErrOrStderr()), f, registry)
}

func runLogin(cmd *cobra.Command, registry string, o loginOptions) error {
	reg, err := auth.ParseRegistry(registry)
	if err != nil {
		return usageError{err}
	}
	path, err := authFile(o.authfile)
	if err != nil {
		return err
	}
	f, err := auth.OpenDefault(path)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	if o.getLogin {
		user, err := loggedInUser(f, path, reg)
		if err != nil {
			return err
		}
		if user == "" {
			return fmt.Errorf("not logged in to %s", registryName(reg))
		}
		fmt.Fprintln(out, user)
		return nil
	}

	if o.password != "" && o.passwordStdin {
		return usagef("--password and --password-stdin are mutually exclusive")
	}
	if o.password != "" {
		fmt.Fprintln(errOut, "WARNING! Using --password via the CLI is insecure. Use --password-stdin.")
	}
	in := cmd.InOrStdin()
	user, password := o.username, o.password
	if o.passwordStdin {
		if user == "" {
			return usagef("--username is required with --password-stdin")
		}
		b, err := io.ReadAll(io.LimitReader(in, 1<<20))
		if err != nil {
			return err
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	stdin, isFile := in.(*os.File)
	interactive := isFile && term.IsTerminal(int(stdin.Fd()))
	if user == "" {
		if !interactive {
			return usagef("--username is required when stdin is not a terminal")
		}
		prev, _ := f.Username(reg)
		if prev != "" {
			fmt.Fprintf(errOut, "Username (%s): ", prev)
		} else {
			fmt.Fprint(errOut, "Username: ")
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if user = strings.TrimSpace(line); user == "" {
			user = prev
		}
	}
	if password == "" && !o.passwordStdin {
		if !interactive {
			return usagef("a password is required: use --password-stdin when stdin is not a terminal")
		}
		fmt.Fprint(errOut, "Password: ")
		b, err := term.ReadPassword(int(stdin.Fd()))
		fmt.Fprintln(errOut)
		if err != nil {
			return err
		}
		password = string(b)
	}
	if user == "" || password == "" {
		return usagef("a user name and a password are required")
	}

	if err := auth.Verify(cmd.Context(), reg, user, password); err != nil {
		return fmt.Errorf("logging in to %s: %w", registryName(reg), err)
	}
	if err := f.Login(reg, user, password); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	if f.Store() == "" {
		fmt.Fprintf(errOut, "Credentials saved unencrypted in %s (readable only by you). Install a Docker credential helper to use the system keychain.\n", f.Path())
	}
	fmt.Fprintln(out, "Login Succeeded!")
	return nil
}

// loggedInUser finds the user for reg in aic's file, then in Docker's and
// podman's credentials.
func loggedInUser(f *auth.File, path string, reg name.Registry) (string, error) {
	if u, err := f.Username(reg); err != nil || u != "" {
		return u, err
	}
	a, err := auth.Keychain(path).Resolve(reg)
	if err != nil || a == authn.Anonymous {
		return "", err
	}
	cfg, err := a.Authorization()
	if err != nil {
		return "", err
	}
	return cfg.Username, nil
}

func newLogoutCmd() *cobra.Command {
	var all bool
	var authfile string
	cmd := &cobra.Command{
		Use:     "logout [flags] REGISTRY",
		Short:   "Log out of a container registry",
		Long:    `Log out of a container registry by removing the credentials saved by "aic login".`,
		Example: "  aic logout ghcr.io\n  aic logout --all",
		Args: args(func(cmd *cobra.Command, a []string) error {
			if all && len(a) > 0 {
				return errors.New("--all takes no registry argument")
			}
			if !all && len(a) != 1 {
				return errors.New("requires a registry (or --all)")
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, a []string) error {
			path, err := authFile(authfile)
			if err != nil {
				return err
			}
			f, err := auth.OpenDefault(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if all {
				if _, err := f.LogoutAll(); err != nil {
					return err
				}
				fmt.Fprintln(out, "Removed login credentials for all registries")
				return nil
			}
			reg, err := auth.ParseRegistry(a[0])
			if err != nil {
				return usageError{err}
			}
			found, err := f.Logout(reg)
			if err != nil {
				return err
			}
			if !found {
				if u, _ := loggedInUser(f, path, reg); u != "" {
					return fmt.Errorf("not logged in to %s with aic login; the credentials in use come from another tool's configuration", registryName(reg))
				}
				return fmt.Errorf("not logged in to %s", registryName(reg))
			}
			fmt.Fprintf(out, "Removed login credentials for %s\n", registryName(reg))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "remove credentials for all registries")
	cmd.Flags().StringVar(&authfile, "authfile", "", "credentials file (default ~/.aic/auth.json)")
	return cmd
}
