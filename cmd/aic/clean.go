package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newCleanCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "clean [flags]",
		Short: "Remove all images, caches and VM files",
		Long: `Remove everything aic has stored: all images, the build cache, the
unpacked copies of images, and the macOS VM's kernel, modules and
initramfs. Registry credentials (aic login) are kept.

aic asks for confirmation unless --force is given.`,
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
			vm, err := vmDir()
			if err != nil {
				return err
			}
			if !force {
				ok, err := confirm(cmd, fmt.Sprintf("Remove %d image(s), all caches in %s and the VM files in %s?", len(list), st.Dir(), vm))
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("cancelled")
				}
			}
			n, freed, err := st.Clean()
			if err != nil {
				return err
			}
			vmFreed := dirSize(vm)
			if err := os.RemoveAll(vm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d image(s); freed %s\n", n, humanSize(freed+vmFreed))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "don't ask for confirmation")
	return cmd
}

// confirm asks a yes/no question on the terminal. Without a terminal it
// fails, so that scripts have to pass --force.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return false, usagef("not a terminal; use --force to skip the confirmation")
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
