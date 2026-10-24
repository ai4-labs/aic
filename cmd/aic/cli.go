package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"

	"github.com/rakyll/ai-container/internal/auth"
	"github.com/rakyll/ai-container/internal/builder"
	"github.com/rakyll/ai-container/internal/executor"
	"github.com/rakyll/ai-container/internal/store"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError marks errors caused by how aic was invoked (exit code 2).
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }

// args wraps a cobra positional-argument validator so that its errors
// count as usage errors.
func args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return usageError{err}
		}
		return nil
	}
}

func run(argv []string, stdout, stderr io.Writer) int {
	return runWithInput(argv, os.Stdin, stdout, stderr)
}

func runWithInput(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := newRootCmd()
	root.SetArgs(argv)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(stderr, "%s: %v\n", cmd.CommandPath(), err)
	var ue usageError
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return exitUsage
	}
	return exitError
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "aic",
		Short:   "Build container images from Dockerfiles",
		Long:    "aic builds OCI images from Dockerfiles and pushes and pulls them to and from registries.",
		Version: version(),
		// Errors are printed by run, with the right exit code.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newBuildCmd(), newImagesCmd(), newLoginCmd(), newLogoutCmd(), newPullCmd(), newPushCmd(), newRmiCmd(), newVMCmd(), newVersionCmd())
	return root
}

func openStore() (*store.Store, error) {
	dir, err := store.DefaultDir()
	if err != nil {
		return nil, err
	}
	s, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	s.UserAgent = "aic/" + version()
	authPath, err := auth.DefaultPath()
	if err != nil {
		return nil, err
	}
	s.Keychain = auth.Keychain(authPath)
	return s, nil
}

// vmDir is where the macOS build VM keeps its files: $AIC_VM_DIR, or
// "vm" next to the image store.
func vmDir() (string, error) {
	if d := os.Getenv("AIC_VM_DIR"); d != "" {
		return d, nil
	}
	st, err := store.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(st), "vm"), nil
}

// executorOptions configures RUN. $AIC_VM_CPUS and $AIC_VM_MEMORY (MiB)
// size a VM booted for a build on macOS.
func executorOptions(progress io.Writer) (executor.Options, error) {
	dir, err := vmDir()
	if err != nil {
		return executor.Options{}, err
	}
	o := executor.Options{Dir: dir, Progress: progress}
	if n, err := strconv.ParseUint(os.Getenv("AIC_VM_CPUS"), 10, 32); err == nil {
		o.CPUs = uint(n)
	}
	if n, err := strconv.ParseUint(os.Getenv("AIC_VM_MEMORY"), 10, 64); err == nil {
		o.MemoryMiB = n
	}
	return o, nil
}

func parsePlatform(s string) (v1.Platform, error) {
	if s == "" {
		return builder.DefaultPlatform(), nil
	}
	p, err := v1.ParsePlatform(s)
	if err != nil {
		return v1.Platform{}, usageError{err}
	}
	if p.OS == "" || p.Architecture == "" {
		return v1.Platform{}, usagef("invalid platform %q: want OS/ARCH[/VARIANT]", s)
	}
	return *p, nil
}

func platformString(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

type buildOptions struct {
	tags      []string
	file      string
	buildArgs []string
	target    string
	platform  string
	pull      bool
	quiet     bool
}

func newBuildCmd() *cobra.Command {
	var o buildOptions
	cmd := &cobra.Command{
		Use:   "build [flags] [CONTEXT]",
		Short: "Build an image from a Dockerfile",
		Long: `Build an image from a Dockerfile.

CONTEXT is the build context directory (default "."). The Dockerfile is
CONTEXT/Dockerfile unless --file is given. RUN instructions run in
rootless Linux containers; on macOS, inside a small Linux VM that aic
boots itself. RUN has no network access unless --network=host is given.`,
		Example: `  aic build -t hello .
  aic build -f build/Dockerfile --build-arg VERSION=1.2 -t app:1.2 .
  aic build --target test --platform linux/amd64 .`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			contextDir := "."
			if len(a) == 1 {
				contextDir = a[0]
			}
			return runBuild(cmd, contextDir, o)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&o.tags, "tag", "t", nil, "name and optionally tag the image (repeatable); unqualified names go under localhost/")
	f.StringVarP(&o.file, "file", "f", "", "path to the Dockerfile (default CONTEXT/Dockerfile)")
	f.StringArrayVar(&o.buildArgs, "build-arg", nil, "set a build argument `KEY=VALUE`, or KEY to take it from the environment (repeatable)")
	f.StringVar(&o.target, "target", "", "build up to this `stage`")
	f.StringVar(&o.platform, "platform", "", "target platform `OS/ARCH[/VARIANT]` (default linux/<host arch>)")
	f.BoolVar(&o.pull, "pull", false, "always pull base images, even if present locally")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "print only the image digest")
	return cmd
}

func runBuild(cmd *cobra.Command, contextDir string, o buildOptions) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	plat, err := parsePlatform(o.platform)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(contextDir); err != nil || !fi.IsDir() {
		return usagef("context %q is not a directory", contextDir)
	}
	dockerfile := o.file
	if dockerfile == "" {
		dockerfile = filepath.Join(contextDir, "Dockerfile")
	}
	if _, err := os.Stat(dockerfile); err != nil {
		return usagef("no Dockerfile: %v", err)
	}

	var refs []name.Reference
	for _, t := range o.tags {
		r, err := store.ParseLocal(t)
		if err != nil {
			return usagef("invalid tag %q: %v", t, err)
		}
		refs = append(refs, r)
	}
	bargs := map[string]string{}
	for _, a := range o.buildArgs {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			if v, ok = os.LookupEnv(k); !ok {
				continue // like docker: KEY without a value and not in the environment is ignored
			}
		}
		bargs[k] = v
	}
	var created time.Time
	if sde := os.Getenv("SOURCE_DATE_EPOCH"); sde != "" {
		sec, err := strconv.ParseInt(sde, 10, 64)
		if err != nil {
			return usagef("invalid SOURCE_DATE_EPOCH %q", sde)
		}
		created = time.Unix(sec, 0)
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	progress, runOut := stderr, stdout
	if o.quiet {
		progress, runOut = io.Discard, io.Discard
	}
	img, err := builder.Build(cmd.Context(), builder.Options{
		ContextDir: contextDir,
		Dockerfile: dockerfile,
		Tags:       refs,
		BuildArgs:  bargs,
		Target:     o.target,
		Platform:   plat,
		Pull:       o.pull,
		Created:    created,
		Store:      st,
		NewExecutor: func() (executor.Executor, error) {
			o, err := executorOptions(progress)
			if err != nil {
				return nil, err
			}
			return executor.NewNative(o)
		},
		Progress: progress,
		Stdout:   runOut,
		Stderr:   progress,
	})
	if err != nil {
		return err
	}
	d, err := img.Digest()
	if err != nil {
		return err
	}
	for _, r := range refs {
		fmt.Fprintf(progress, "Successfully tagged %s\n", builder.DisplayName(r))
	}
	fmt.Fprintln(stdout, d)
	return nil
}

func newPullCmd() *cobra.Command {
	var platform string
	var quiet bool
	cmd := &cobra.Command{
		Use:     "pull [flags] IMAGE",
		Short:   "Pull an image from a registry",
		Example: "  aic pull alpine:3.20\n  aic pull --platform linux/amd64 ghcr.io/org/app:1",
		Args:    args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ref, err := name.ParseReference(a[0])
			if err != nil {
				return usageError{err}
			}
			plat, err := parsePlatform(platform)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "Pulling %s (%s)...\n", builder.DisplayName(ref), platformString(plat))
			}
			img, err := st.Pull(cmd.Context(), ref, plat)
			if err != nil {
				return err
			}
			d, err := img.Digest()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), d)
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "platform to pull `OS/ARCH[/VARIANT]` (default linux/<host arch>)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the image digest")
	return cmd
}

func newPushCmd() *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "push [flags] IMAGE [DESTINATION]",
		Short: "Push an image to a registry",
		Long: `Push an image to a registry.

IMAGE is a local image name or digest (prefix). Without DESTINATION the
image is pushed to the name it is stored under, which must include a
registry.`,
		Example: "  aic push hello registry.example.com/team/hello:1\n  aic push ghcr.io/org/app:1",
		Args:    args(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			src := a[0]
			var dst name.Reference
			var err error
			if len(a) == 2 {
				dst, err = name.ParseReference(a[1])
			} else {
				// Unqualified build tags are stored under localhost/.
				dst, err = store.ParseLocal(src)
			}
			if err != nil {
				return usageError{err}
			}
			if dst.Context().RegistryStr() == "localhost" {
				return usagef("%s has no registry; give a destination, e.g. aic push %s registry.example.com/%s",
					dst.Name(), src, strings.TrimPrefix(dst.Context().RepositoryStr(), "localhost/"))
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "Pushing %s to %s...\n", src, builder.DisplayName(dst))
			}
			d, err := st.Push(cmd.Context(), src, dst)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), d)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the image digest")
	return cmd
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := store.DefaultDir()
			fmt.Fprintf(cmd.OutOrStdout(), "aic %s\ndefault platform: %s\nimage store: %s\n",
				version(), platformString(builder.DefaultPlatform()), dir)
			return nil
		},
	}
}
