package firecracker

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
)

// vmmProbeTimeout bounds the aliveness probe of a rehydrated box's VMM.
const vmmProbeTimeout = time.Second

// rehydrate reloads every box persisted under the state directory into the
// in-memory registry, so List/Find/Destroy see boxes created by a previous run
// of the spoke (e.g. before a crash). Rehydrated instances carry no machine
// handle: their VMM is probed once — a VMM orphaned by a crashed spoke keeps
// running — and reported running or stopped accordingly. Control still works
// for a live VMM (it only needs the vsock UDS path), Destroy always works (it
// cleans the state dir and frees the slot, halting a live orphan first), and
// each rehydrated box gets its box-port API listener back. Boxes of every
// namespace are loaded — List/Find filter by namespace at read time — so their
// TAP slots are marked used and never double-allocated.
//
// @error error if the state directory exists but cannot be read.
//
// @testcase TestRehydrateListsPriorBoxes lists boxes persisted by a previous provisioner.
// @testcase TestRehydrateDestroysDeadBox destroys a rehydrated box, cleaning its dir and slot.
// @testcase TestRehydrateRestartsBoxAPIListeners restarts the box-port listeners of rehydrated boxes.
func (p *Provisioner) rehydrate() error {
	metas, err := loadMetas(p.stateDir)
	if err != nil {
		return err
	}
	for _, m := range metas {
		dir := boxDir(p.stateDir, m.Token)
		vsockUDS := filepath.Join(dir, "vsock.sock")
		inst := &fcInstance{
			prov:     p,
			meta:     m,
			vsockUDS: vsockUDS,
			net:      netFor(m.NetIndex),
			alive:    vmmAlive(filepath.Join(dir, "fc.sock")),
		}
		if p.ports != nil {
			api, err := boxapi.ServeUnix(boxAPISocketPath(vsockUDS), m.BoxID, p.ports, p.log)
			if err != nil {
				p.log.Warn("failed to recover box-port API", "box", m.Token, "err", err)
			} else {
				inst.api = api
			}
		}
		p.mu.Lock()
		p.boxes[m.Token] = inst
		p.used[m.NetIndex] = true
		p.mu.Unlock()
		p.log.Info("rehydrated firecracker box", "box", m.Token, "box_id", m.BoxID, "alive", inst.alive)
	}
	return nil
}

// vmmAlive reports whether a box's Firecracker VMM still answers on its API
// socket. A VMM started by a previous spoke process survives that process's
// crash (it is orphaned, not killed), so a rehydrated box may well be running.
//
// @arg apiSock The box's Firecracker API Unix-socket path.
// @return bool True when the VMM accepts a connection on its API socket.
//
// @testcase TestVMMAlive distinguishes a live API socket from a dead or missing one.
func vmmAlive(apiSock string) bool {
	conn, err := net.DialTimeout("unix", apiSock, vmmProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// haltVMM asks a box's VMM to shut down over its API socket by injecting
// Ctrl-Alt-Del (the guest kernel boots with reboot=k, so the reboot exits the
// VMM). It is the best-effort stop for a rehydrated box whose SDK machine
// handle is gone — without it, destroying such a box would delete the rootfs
// under a still-running VM.
//
// @arg apiSock The box's Firecracker API Unix-socket path.
// @error error if the API socket cannot be reached or the action is rejected.
//
// @testcase TestHaltVMM sends the Ctrl-Alt-Del action to a fake API socket.
func haltVMM(apiSock string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost/actions", strings.NewReader(`{"action_type":"SendCtrlAltDel"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
