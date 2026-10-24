//go:build !darwin

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func newVMCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "vm",
		Short:              "Manage the build VM (macOS only)",
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("aic only uses a VM on macOS; here RUN runs natively in rootless containers")
		},
	}
}
