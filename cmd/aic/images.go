package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/ai4-labs/aic/internal/store"
)

func newImagesCmd() *cobra.Command {
	var quiet, noTrunc bool
	cmd := &cobra.Command{
		Use:   "images [flags]",
		Short: "List images in the local store",
		Long: `List images in the local store.

SIZE is the size of the image as stored and pushed: its config plus
compressed layers.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			list, err := st.List()
			if err != nil {
				return err
			}
			printImages(cmd.OutOrStdout(), list, quiet, noTrunc, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only image IDs")
	cmd.Flags().BoolVar(&noTrunc, "no-trunc", false, "print full image IDs")
	return cmd
}

func imageID(d string, noTrunc bool) string {
	if noTrunc {
		return d
	}
	return strings.TrimPrefix(d, "sha256:")[:12]
}

func printImages(w io.Writer, list []store.ImageInfo, quiet, noTrunc bool, now time.Time) {
	if quiet {
		for _, info := range list {
			fmt.Fprintln(w, imageID(info.Digest.String(), noTrunc))
		}
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE")
	for _, info := range list {
		id := imageID(info.Digest.String(), noTrunc)
		created, size := humanAgo(now, info.Created), humanSize(info.Size)
		if len(info.Names) == 0 {
			fmt.Fprintf(tw, "<none>\t<none>\t%s\t%s\t%s\n", id, created, size)
		}
		for _, n := range info.Names {
			repo, tag := splitName(n)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", repo, tag, id, created, size)
		}
	}
	tw.Flush()
}

// splitName splits a stored name into repository and tag for display.
func splitName(n string) (repo, tag string) {
	ref, err := name.ParseReference(n)
	if err != nil {
		return n, "<none>"
	}
	repo = strings.Replace(ref.Context().Name(), name.DefaultRegistry+"/", "docker.io/", 1)
	if t, ok := ref.(name.Tag); ok {
		return repo, t.TagStr()
	}
	return repo, "<none>"
}

func humanAgo(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			if unit == "hour" {
				return "an hour ago"
			}
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "Less than a minute ago"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 14*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24/7), "week")
	case d < 2*365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	}
	return plural(int(d.Hours()/24/365), "year")
}

func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.3g %cB", v, "kMGT"[exp-1])
}

func newRmiCmd() *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{
		Use:   "rmi [flags] IMAGE...",
		Short: "Remove images from the local store",
		Long: `Remove images from the local store.

Removing a name only deletes the image once it has no other names. An
image ID removes the image with all its names; if it has several, --force
is required. Layers no longer used by any image are deleted.`,
		Example: "  aic rmi hello\n  aic rmi 1c8cd8f6ca12\n  aic rmi --all",
		Args: args(func(cmd *cobra.Command, a []string) error {
			if all && len(a) > 0 {
				return errors.New("--all takes no image arguments")
			}
			if !all && len(a) == 0 {
				return errors.New("requires at least 1 image")
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, a []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			results, err := st.Remove(a, all, force)
			if err != nil {
				return err
			}
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			failed := 0
			for _, r := range results {
				if r.Err != nil {
					fmt.Fprintf(errOut, "Error: %v\n", r.Err)
					failed++
					continue
				}
				for _, n := range r.Untagged {
					fmt.Fprintf(out, "Untagged: %s\n", n)
				}
				for _, d := range r.Deleted {
					fmt.Fprintf(out, "Deleted: %s\n", d.Hex)
				}
			}
			if failed > 0 {
				return fmt.Errorf("failed to remove %d of %d images", failed, len(results))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "remove all images")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove an image by ID even if it has several names")
	return cmd
}

func newPruneCmd() *cobra.Command {
	var vmFiles bool
	cmd := &cobra.Command{
		Use:   "prune [flags]",
		Short: "Remove the build cache",
		Long: `Remove the build cache: the recorded results of build steps, and the
layers no image uses. The next build runs every step again.

With --vm, also remove what the macOS VM caches (kernel, initramfs and
guest); it is downloaded or rebuilt when next needed.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			n, err := st.PruneCache()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d build cache entries\n", n)
			if vmFiles {
				dir, err := vmDir()
				if err != nil {
					return err
				}
				if err := os.RemoveAll(dir); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", dir)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&vmFiles, "vm", false, "also remove the VM's cached kernel, initramfs and guest")
	return cmd
}

func newTagCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tag IMAGE TARGET...",
		Short: "Add names to a local image",
		Long: `Add names to a local image.

IMAGE is a name, digest or ID. Each TARGET is a name like "app:1.2" or
"ghcr.io/org/app:1.2"; unqualified names go under localhost/. A TARGET
without a tag is tagged sha256-<image digest>; "latest" is not allowed.
A name that already points at another image is moved.`,
		Example: "  aic tag hello hello:1.0\n  aic tag 1c8cd8f6ca12 ghcr.io/org/hello:1.0",
		Args:    args(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			var tags []name.Tag
			var repos []name.Repository
			for _, t := range a[1:] {
				tag, repo, ok, err := store.ParseTag(t)
				if err != nil {
					return usageError{err}
				}
				if ok {
					tags = append(tags, tag)
				} else {
					repos = append(repos, repo)
				}
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			img, err := st.Image(a[0])
			if err != nil {
				return err
			}
			d, err := img.Digest()
			if err != nil {
				return err
			}
			var refs []name.Reference
			for _, t := range tags {
				refs = append(refs, t)
			}
			for _, r := range repos {
				refs = append(refs, store.DigestTag(r, d))
			}
			return st.Put(img, refs...)
		},
	}
}
