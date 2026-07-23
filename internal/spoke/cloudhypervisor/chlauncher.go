package cloudhypervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// defaultCHBinary is the Cloud Hypervisor executable resolved from PATH when the
// spoke sets no explicit path.
const defaultCHBinary = "cloud-hypervisor"

// guestBootTimeout bounds how long Launch waits for a freshly booted guest to answer
// on its vsock control channel before giving up and tearing the VM down.
const guestBootTimeout = 45 * time.Second

// vmmProbeTimeout bounds the liveness/halt calls to an orphaned VMM's API socket, so
// a dead socket fails fast during rehydrate.
const vmmProbeTimeout = 2 * time.Second

// chLauncher is the real launcher: it boots each box as a cloud-hypervisor process
// and drives it over the REST API. It holds no per-box state — a box's identity and
// sockets all come from the vmSpec — so one launcher serves every box on a spoke.
type chLauncher struct {
	// chBinary is the cloud-hypervisor executable (path or PATH name).
	chBinary string
	log      *slog.Logger
}

// newCHLauncher builds a launcher that starts VMs with the given cloud-hypervisor
// binary; empty resolves "cloud-hypervisor" from PATH.
//
// @arg chBinary The cloud-hypervisor executable path or PATH name; empty uses the default.
// @return *chLauncher A launcher ready to boot boxes.
//
// @testcase TestConformanceCloudHypervisor boots live boxes through a launcher built here.
func newCHLauncher(chBinary string) *chLauncher {
	if chBinary == "" {
		chBinary = defaultCHBinary
	}
	return &chLauncher{chBinary: chBinary, log: slog.Default()}
}

// chHandle is a live box VMM: it stops the box by asking the whole cloud-hypervisor
// process to shut down over its API socket, after which the API socket disappears.
type chHandle struct {
	apiSock string
	// proc is the cloud-hypervisor process; kept only to release it on Stop (its
	// lifetime is deliberately decoupled from the spoke — see Launch).
	proc *os.Process
}

// Stop asks the box's VMM to shut down over its API socket, then reaps the process
// handle. It is best-effort: a VMM that already exited (socket gone) is treated as
// stopped.
//
// @error error Always nil; shutdown failures are swallowed because a gone VMM is a
// successful stop from the caller's point of view.
//
// @testcase TestConformanceCloudHypervisor stops live boxes through this handle.
func (h *chHandle) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), vmmProbeTimeout)
	defer cancel()
	_ = newAPIClient(h.apiSock).shutdownVMM(ctx)
	if h.proc != nil {
		// Reap the child so it does not linger as a zombie once it exits.
		_, _ = h.proc.Wait()
	}
	return nil
}

// Launch copies the box's rootfs, starts a cloud-hypervisor process on the box's API
// socket, defines and boots the VM (including any GPU passthrough devices), and waits
// for the guest to answer on vsock. The VMM is started in its own process group so a
// signal to the spoke does not stop running boxes; a box therefore survives a spoke
// restart and is re-adopted by rehydrate. On any failure it tears the half-booted VMM
// down so nothing leaks.
//
// @arg ctx Context bounding the boot and guest wait.
// @arg spec The box's VM parameters.
// @return vmHandle A handle to the running VMM.
// @error error if the rootfs cannot be prepared, the VMM cannot be started, the VM cannot be created/booted, or the guest never answers.
//
// @testcase TestConformanceCloudHypervisor launches every live box through this method.
func (l *chLauncher) Launch(ctx context.Context, spec vmSpec) (vmHandle, error) {
	if err := os.MkdirAll(spec.BoxDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating box dir: %w", err)
	}
	rootfs := filepath.Join(spec.BoxDir, "rootfs.ext4")
	// Copy+grow the base rootfs only on first boot: a resumed box reuses its existing
	// per-box rootfs so its on-disk state (auth, workspace, anything written before
	// the pause) survives. Re-copying here would silently reset the box's disk.
	if _, err := os.Stat(rootfs); os.IsNotExist(err) {
		if err := prepareRootfs(spec.RootfsSrc, rootfs, spec.DiskBytes); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat box rootfs: %w", err)
	}

	// A stale API socket from a previous boot would make the VMM refuse to bind; a
	// resumed box reuses the same path, so clear it first.
	_ = os.Remove(spec.APISock)
	cmd := exec.Command(l.chBinary, "--api-socket", "path="+spec.APISock)
	// Own process group: a Ctrl-C/SIGTERM delivered to the spoke's group must not
	// stop running boxes (they outlive the spoke and are rehydrated).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting cloud-hypervisor: %w", err)
	}
	handle := &chHandle{apiSock: spec.APISock, proc: cmd.Process}

	if err := waitForSocket(ctx, spec.APISock, guestBootTimeout); err != nil {
		_ = handle.Stop()
		return nil, fmt.Errorf("cloud-hypervisor API socket never appeared: %w", err)
	}

	cfg := buildVMConfig(vmConfigParams{
		Kernel:      spec.Kernel,
		Rootfs:      rootfs,
		VsockUDS:    spec.VsockUDS,
		VCPUs:       spec.VCPUs,
		MemoryBytes: spec.MemoryBytes,
		GPUs:        spec.GPUs,
		MDEVs:       spec.MDEVs,
		TapName:     spec.TapName,
		MAC:         spec.MAC,
		IPArg:       spec.IPArg,
	})
	client := newAPIClient(spec.APISock)
	if err := client.createVM(ctx, cfg); err != nil {
		_ = handle.Stop()
		return nil, err
	}
	if err := client.bootVM(ctx); err != nil {
		_ = handle.Stop()
		return nil, err
	}
	if err := waitForGuest(ctx, spec.VsockUDS, guestBootTimeout); err != nil {
		_ = handle.Stop()
		return nil, fmt.Errorf("box %s guest never answered: %w", spec.Token, err)
	}
	return handle, nil
}

// Dial opens the box's guest control channel over its vsock UDS.
//
// @arg ctx Context for the dial and handshake.
// @arg vsockUDS The box's vsock Unix-socket path.
// @return net.Conn A control connection to the guest.
// @error error if the vsock cannot be dialled or the handshake fails.
//
// @testcase TestConformanceCloudHypervisor drives guests through Dial.
func (l *chLauncher) Dial(ctx context.Context, vsockUDS string) (net.Conn, error) {
	return dialVsock(ctx, vsockUDS, guestVsockPort)
}

// Alive reports whether an orphaned VMM still answers on its API socket.
//
// @arg apiSock The box's API Unix-socket path.
// @return bool True when the VMM responds to a ping.
//
// @testcase TestConformanceCloudHypervisor probes rehydrated boxes through Alive.
func (l *chLauncher) Alive(apiSock string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), vmmProbeTimeout)
	defer cancel()
	return newAPIClient(apiSock).ping(ctx)
}

// Halt best-effort stops an orphaned VMM by its API socket.
//
// @arg apiSock The box's API Unix-socket path.
// @error error if the VMM cannot be reached or refuses to shut down.
//
// @testcase TestConformanceCloudHypervisor halts orphaned boxes through Halt.
func (l *chLauncher) Halt(apiSock string) error {
	ctx, cancel := context.WithTimeout(context.Background(), vmmProbeTimeout)
	defer cancel()
	return newAPIClient(apiSock).shutdownVMM(ctx)
}

// prepareRootfs copies the base rootfs to the box's writable copy and grows it to
// diskBytes with a sparse truncate (the added space costs no host blocks until
// written; the guest's boot-time resize grows the ext4 to fill it). It mirrors the
// Firecracker backend's per-box rootfs handling.
//
// @arg src The base rootfs image path.
// @arg dst The box's per-box rootfs copy path.
// @arg diskBytes The size to grow the copy to (a no-op when not larger than the copy).
// @error error if the copy or resize fails.
//
// @testcase TestConformanceCloudHypervisor grows each live box's rootfs through this.
func prepareRootfs(src, dst string, diskBytes int64) error {
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("copying rootfs: %w", err)
	}
	if diskBytes > 0 {
		info, err := os.Stat(dst)
		if err != nil {
			return fmt.Errorf("stat rootfs copy: %w", err)
		}
		if diskBytes > info.Size() {
			if err := os.Truncate(dst, diskBytes); err != nil {
				return fmt.Errorf("resizing rootfs to %d bytes: %w", diskBytes, err)
			}
		}
	}
	return nil
}

// copyFile copies src to dst, creating dst 0600.
//
// @arg src The source file path.
// @arg dst The destination file path.
// @error error if either file cannot be opened or the copy fails.
//
// @testcase TestConformanceCloudHypervisor copies each live box's rootfs through this.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// waitForSocket blocks until path exists (a VMM's API socket appearing) or the
// timeout elapses.
//
// @arg ctx Context whose cancellation aborts the wait.
// @arg path The socket path to wait for.
// @arg timeout The overall wait bound.
// @error error if the socket does not appear before the deadline or ctx is cancelled.
//
// @testcase TestConformanceCloudHypervisor waits for each live VMM's API socket.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitForGuest blocks until the box's guest answers the vsock CONNECT handshake on
// guestVsockPort or the timeout elapses, so Launch returns only once the guest is
// actually reachable.
//
// @arg ctx Context whose cancellation aborts the wait.
// @arg vsockUDS The box's vsock Unix-socket path.
// @arg timeout The overall wait bound.
// @error error if the guest does not answer before the deadline or ctx is cancelled.
//
// @testcase TestConformanceCloudHypervisor waits for each live box's guest.
func waitForGuest(ctx context.Context, vsockUDS string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := dialVsock(dctx, vsockUDS, guestVsockPort)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
