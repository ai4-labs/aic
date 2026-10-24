//go:build darwin

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/ai4-labs/aic/internal/proto"
)

// Options configures the VM.
type Options struct {
	// Dir caches the kernel and initramfs and holds the console log.
	Dir string
	// CPUs and MemoryMiB size the VM. Zero picks a default.
	CPUs      uint
	MemoryMiB uint64
	// Progress receives one-time setup messages.
	Progress io.Writer
}

// Machine is a running build VM.
type Machine struct {
	vm        *vz.VirtualMachine
	toGuest   *os.File // host end of the guest's /dev/hvc1, write side
	fromGuest *os.File // host end of the guest's /dev/hvc1, read side
	fw        *proto.Writer
	console   *os.File
	logPath   string
	cpus      uint
	memMiB    uint64

	files    []*os.File // everything to close when stopping
	mu       sync.Mutex // serializes runs
	stopOnce sync.Once
	closed   atomic.Bool
}

const bootTimeout = 60 * time.Second

// Start boots a VM and waits until the guest is serving.
func Start(ctx context.Context, o Options) (*Machine, error) {
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := EnsureSigned(o.Progress); err != nil {
		return nil, err
	}
	guest, err := guestBinary(ctx, o.Dir, o.Progress)
	if err != nil {
		return nil, err
	}
	kernel, err := EnsureKernel(ctx, o.Dir, runtime.GOARCH, o.Progress)
	if err != nil {
		return nil, err
	}
	initrd, err := ensureInitrd(o.Dir, guest)
	if err != nil {
		return nil, err
	}

	cpus := o.CPUs
	if cpus == 0 {
		cpus = uint(min(runtime.NumCPU(), 4))
	}
	cpus = max(min(cpus, vz.VirtualMachineConfigurationMaximumAllowedCPUCount()), vz.VirtualMachineConfigurationMinimumAllowedCPUCount())
	mem := o.MemoryMiB
	if mem == 0 {
		mem = 4096
	}

	boot, err := vz.NewLinuxBootLoader(kernel,
		vz.WithCommandLine("console=hvc0 ip=dhcp panic=-1 quiet loglevel=3"),
		vz.WithInitrd(initrd))
	if err != nil {
		return nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, cpus, mem<<20)
	if err != nil {
		return nil, err
	}

	m := &Machine{logPath: filepath.Join(o.Dir, "console.log"), cpus: cpus, memMiB: mem}
	ok := false
	var files []*os.File
	defer func() {
		if !ok {
			for _, f := range files {
				f.Close()
			}
		}
	}()

	// hvc0: kernel and guest log, to a file. hvc1: the request channel.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	files = append(files, devnull)
	if m.console, err = os.Create(m.logPath); err != nil {
		return nil, err
	}
	files = append(files, m.console)
	consoleAtt, err := vz.NewFileHandleSerialPortAttachment(devnull, m.console)
	if err != nil {
		return nil, err
	}
	guestIn, toGuest, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromGuest, guestOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	files = append(files, guestIn, toGuest, fromGuest, guestOut)
	m.toGuest, m.fromGuest = toGuest, fromGuest
	m.fw = proto.NewWriter(toGuest)
	chanAtt, err := vz.NewFileHandleSerialPortAttachment(guestIn, guestOut)
	if err != nil {
		return nil, err
	}
	var ports []*vz.VirtioConsoleDeviceSerialPortConfiguration
	for _, a := range []vz.SerialPortAttachment{consoleAtt, chanAtt} {
		p, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(a)
		if err != nil {
			return nil, err
		}
		ports = append(ports, p)
	}
	cfg.SetSerialPortsVirtualMachineConfiguration(ports)

	// Outbound network through NAT, shared by RUN containers unless
	// they use --network=none.
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, err
	}
	nic.SetMACAddress(mac)
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	if _, err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("VM configuration: %w", err)
	}
	if m.vm, err = vz.NewVirtualMachine(cfg); err != nil {
		return nil, entitlementHint(err)
	}
	if err := m.vm.Start(); err != nil {
		return nil, entitlementHint(err)
	}

	ready := make(chan error, 1)
	go func() {
		typ, _, err := proto.ReadFrame(fromGuest, nil)
		if err == nil && typ != proto.FrameReady {
			err = fmt.Errorf("%w %q from guest", proto.ErrUnexpectedFrame, typ)
		}
		ready <- err
	}()
	timer := time.NewTimer(bootTimeout)
	defer timer.Stop()
	select {
	case err = <-ready:
	case <-timer.C:
		err = errors.New("timed out")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		m.files = files
		ok = true // stop closes the files
		m.stop()
		return nil, fmt.Errorf("booting the build VM: %v%s", err, m.logTail())
	}
	ok = true
	m.files = files
	// If the guest powers off (it does on a protocol error), fail the
	// current run instead of waiting on a channel nobody serves.
	go func() {
		for st := range m.vm.StateChangedNotify() {
			if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
				m.stop()
				return
			}
		}
	}()
	return m, nil
}

func entitlementHint(err error) error {
	if strings.Contains(err.Error(), "entitlement") {
		return fmt.Errorf("%w\naic must be signed with the com.apple.security.virtualization entitlement; build it with `make`", err)
	}
	return err
}

func (m *Machine) logTail() string {
	b, err := os.ReadFile(m.logPath)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	return "\nVM console (" + m.logPath + "):\n" + string(b)
}

// Run executes spec in a fresh container in the VM. image is the
// flattened root filesystem; the container's filesystem afterwards is
// written to rio.Export.
func (m *Machine) Run(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return proto.Result{}, errors.New("build VM is stopped")
	}
	// Cancelling ctx asks the guest to kill the container. If it doesn't
	// answer, stop the VM: the channel can't be trusted anymore.
	done := make(chan struct{})
	defer close(done)
	stopOnCancel := context.AfterFunc(ctx, func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			m.stop()
		}
	})
	defer stopOnCancel()

	res, err := proto.HostRun(ctx, m.fromGuest, m.fw, spec, image, rio)
	if err == nil && ctx.Err() != nil {
		return proto.Result{}, ctx.Err()
	}
	var ce *proto.ChannelError
	if errors.As(err, &ce) {
		err = m.channelErr(ctx, ce.Err)
		m.stop() // the channel is out of sync and unusable
		return proto.Result{}, err
	}
	return res, err
}

// Closed reports whether the VM has stopped.
func (m *Machine) Closed() bool { return m.closed.Load() }

// CPUs and MemoryMiB report the VM's size.
func (m *Machine) CPUs() uint        { return m.cpus }
func (m *Machine) MemoryMiB() uint64 { return m.memMiB }

func (m *Machine) channelErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("build VM channel: %v%s", err, m.logTail())
}

// Close stops the VM.
func (m *Machine) Close() error {
	m.stop()
	return nil
}

func (m *Machine) stop() {
	m.stopOnce.Do(func() {
		m.closed.Store(true)
		if m.vm != nil && m.vm.CanStop() {
			_ = m.vm.Stop()
		}
		for _, f := range m.files {
			f.Close()
		}
	})
}
