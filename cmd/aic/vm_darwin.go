//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ai4-labs/aic/internal/vm"
)

func newVMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Manage the Linux VM that runs RUN instructions on macOS",
		Long: `Manage the Linux VM that runs RUN instructions on macOS.

Without a running VM, each build boots its own VM and stops it afterwards
(about a second). "aic vm start" keeps one running in the background so
builds can use it right away.`,
		Args: args(cobra.NoArgs),
	}
	cmd.AddCommand(newVMStartCmd(), newVMStopCmd(), newVMStatusCmd(), newVMLogsCmd(), newVMRmCmd(), newVMServeCmd())
	return cmd
}

func envUint(k string) uint64 {
	n, _ := strconv.ParseUint(os.Getenv(k), 10, 64)
	return n
}

func newVMStartCmd() *cobra.Command {
	var cpus uint
	var memory uint64
	cmd := &cobra.Command{
		Use:   "start [flags]",
		Short: "Start the VM in the background",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			out := cmd.ErrOrStderr()
			if vm.Running(dir) {
				fmt.Fprintln(out, "The VM is already running.")
				return nil
			}
			// Download the kernel here, where progress is visible.
			if _, err := vm.EnsureKernel(cmd.Context(), dir, runtime.GOARCH, out); err != nil {
				return err
			}
			return startDaemon(cmd.Context(), dir, cpus, memory, out)
		},
	}
	cmd.Flags().UintVar(&cpus, "cpus", uint(envUint("AIC_VM_CPUS")), "number of CPUs (default min(4, host CPUs); env AIC_VM_CPUS)")
	cmd.Flags().Uint64Var(&memory, "memory", envUint("AIC_VM_MEMORY"), "memory in MiB (default 4096; env AIC_VM_MEMORY)")
	return cmd
}

func startDaemon(ctx context.Context, dir string, cpus uint, memory uint64, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "daemon.log")
	logf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logf.Close()
	c := exec.Command(self, "vm", "serve", "--cpus", strconv.FormatUint(uint64(cpus), 10), "--memory", strconv.FormatUint(memory, 10))
	c.Env = append(os.Environ(), "AIC_VM_DIR="+dir)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive this terminal
	if err := c.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()

	fmt.Fprint(out, "Starting the VM...")
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(90 * time.Second)
	for {
		select {
		case <-tick.C:
			if st, err := vm.DaemonStatus(dir); err == nil {
				fmt.Fprintf(out, " done (pid %d, %d CPUs, %d MiB).\n", st.PID, st.CPUs, st.MemoryMiB)
				return nil
			}
		case err := <-exited:
			fmt.Fprintln(out)
			b, _ := os.ReadFile(logPath)
			return fmt.Errorf("the VM exited: %v\n%s", err, b)
		case <-timeout:
			fmt.Fprintln(out)
			c.Process.Kill()
			return fmt.Errorf("timed out starting the VM; see %s", logPath)
		case <-ctx.Done():
			fmt.Fprintln(out)
			c.Process.Kill()
			return ctx.Err()
		}
	}
}

func newVMStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background VM",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			err = vm.StopDaemon(dir)
			if errors.Is(err, vm.ErrNotRunning) {
				fmt.Fprintln(cmd.ErrOrStderr(), "The VM is not running.")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "The VM is stopped.")
			return nil
		},
	}
}

func newVMStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the background VM is running",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			st, err := vm.DaemonStatus(dir)
			if errors.Is(err, vm.ErrNotRunning) {
				fmt.Fprintf(out, "State:   stopped (builds boot their own VM)\nDir:     %s\n", dir)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "State:   running since %s (%s)\nPID:     %d\nCPUs:    %d\nMemory:  %d MiB\nKernel:  Linux %s\nRuns:    %d\nDir:     %s\n",
				st.Started.Format(time.DateTime), humanAgo(time.Now(), st.Started), st.PID, st.CPUs, st.MemoryMiB, st.Kernel, st.Runs, dir)
			return nil
		},
	}
}

func newVMLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [flags]",
		Short: "Print the VM's console log",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			f, err := os.Open(filepath.Join(dir, "console.log"))
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("no VM log yet")
			}
			if err != nil {
				return err
			}
			defer f.Close()
			out := cmd.OutOrStdout()
			if _, err := io.Copy(out, f); err != nil {
				return err
			}
			for follow {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(200 * time.Millisecond):
				}
				if _, err := io.Copy(out, f); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output")
	return cmd
}

func newVMRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm [flags]",
		Short: "Remove the VM's cached kernel, initramfs and logs",
		Long: `Remove the VM's cached kernel, initramfs and logs. They are downloaded
or rebuilt the next time a VM starts. Images are not affected.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			if vm.Running(dir) {
				if !force {
					return usagef("the VM is running; stop it first or use --force")
				}
				if err := vm.StopDaemon(dir); err != nil {
					return err
				}
			}
			var removed int
			for _, pat := range []string{"kernel-*", "initrd-*.cpio", "console.log", "daemon.log"} {
				matches, _ := filepath.Glob(filepath.Join(dir, pat))
				for _, m := range matches {
					if err := os.Remove(m); err != nil {
						return err
					}
					removed++
				}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Removed %d file(s) from %s.\n", removed, dir)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "stop the VM first if it is running")
	return cmd
}

// newVMServeCmd is the background process started by "aic vm start".
func newVMServeCmd() *cobra.Command {
	var cpus uint
	var memory uint64
	cmd := &cobra.Command{
		Use:    "serve",
		Hidden: true,
		Args:   args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := vmDir()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			return vm.Serve(ctx, vm.Options{Dir: dir, CPUs: cpus, MemoryMiB: memory, Progress: cmd.ErrOrStderr()})
		},
	}
	cmd.Flags().UintVar(&cpus, "cpus", 0, "")
	cmd.Flags().Uint64Var(&memory, "memory", 0, "")
	return cmd
}
