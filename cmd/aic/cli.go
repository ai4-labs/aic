package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ai4-labs/aic"
	"github.com/ai4-labs/aic/internal/builder"
	"github.com/ai4-labs/aic/internal/store"
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
	var es exitStatus
	if errors.As(err, &es) {
		if es.err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmd.CommandPath(), es.err)
		}
		return es.code
	}
	fmt.Fprintf(stderr, "%s: %v\n", cmd.CommandPath(), err)
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, aic.ErrInvalid) || strings.HasPrefix(err.Error(), "unknown command") {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return exitUsage
	}
	return exitError
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "aic",
		Short:   "Build and run container images from Dockerfiles",
		Long:    "aic builds OCI images from Dockerfiles, runs them in isolated containers, and pushes and pulls them to and from registries.",
		Version: version(),
		// Errors are printed by run, with the right exit code.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newBuildCmd(), newCleanCmd(), newImagesCmd(), newLoginCmd(), newLogoutCmd(), newPruneCmd(), newPullCmd(), newPushCmd(), newRmiCmd(), newRunCmd(), newTagCmd(), newVersionCmd())
	return root
}

// newClient opens the image store; progress receives pull and VM setup
// messages (nil discards them).
func newClient(progress io.Writer) (*aic.Client, error) {
	return aic.New(aic.Options{Progress: progress, UserAgent: "aic/" + version()})
}

type buildOptions struct {
	tags      []string
	file      string
	buildArgs []string
	target    string
	platform  string
	pull      bool
	noCache   bool
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
boots itself. RUN shares the host's network; use RUN --network=none to
isolate a step.`,
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
	f.StringArrayVarP(&o.tags, "tag", "t", nil, "name the image (repeatable); without a tag it is tagged sha256-<digest>; unqualified names go under localhost/")
	f.StringVarP(&o.file, "file", "f", "", "path to the Dockerfile (default CONTEXT/Dockerfile)")
	f.StringArrayVar(&o.buildArgs, "build-arg", nil, "set a build argument `KEY=VALUE`, or KEY to take it from the environment (repeatable)")
	f.StringVar(&o.target, "target", "", "build up to this `stage`")
	f.StringVar(&o.platform, "platform", "", "target platform `OS/ARCH[/VARIANT]` (default linux/<host arch>)")
	f.BoolVar(&o.pull, "pull", false, "always pull base images, even if present locally")
	f.BoolVar(&o.noCache, "no-cache", false, "run every step instead of using the build cache")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "print only the image digest")
	return cmd
}

func runBuild(cmd *cobra.Command, contextDir string, o buildOptions) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
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
	progress, runOut := stderr, stdout
	if o.quiet {
		progress, runOut = io.Discard, io.Discard
	}
	c, err := newClient(progress)
	if err != nil {
		return err
	}
	img, err := c.Build(cmd.Context(), aic.BuildOptions{
		ContextDir: contextDir,
		Dockerfile: o.file,
		Tags:       o.tags,
		BuildArgs:  bargs,
		Target:     o.target,
		Platform:   o.platform,
		Pull:       o.pull,
		NoCache:    o.noCache,
		Progress:   progress,
		Stdout:     runOut,
		Stderr:     progress,
	})
	if err != nil {
		return err
	}
	for _, n := range img.Names {
		fmt.Fprintf(progress, "Successfully tagged %s\n", n)
	}
	fmt.Fprintln(stdout, img.Digest)
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
			progress := cmd.ErrOrStderr()
			if quiet {
				progress = nil
			}
			c, err := newClient(progress)
			if err != nil {
				return err
			}
			d, err := c.Pull(cmd.Context(), a[0], platform)
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
registry. A destination without a tag is tagged sha256-<digest>; aic
never pushes "latest".`,
		Example: "  aic push hello registry.example.com/team/hello:1\n  aic push ghcr.io/org/app:1",
		Args:    args(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			progress := cmd.ErrOrStderr()
			if quiet {
				progress = nil
			}
			c, err := newClient(progress)
			if err != nil {
				return err
			}
			dst := ""
			if len(a) == 2 {
				dst = a[1]
			}
			_, d, err := c.Push(cmd.Context(), a[0], dst)
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
				version(), aic.PlatformString(builder.DefaultPlatform()), dir)
			return nil
		},
	}
}
