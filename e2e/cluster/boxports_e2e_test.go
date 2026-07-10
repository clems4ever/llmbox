//go:build e2e

package clustere2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// connectSpokeWithCaller is connectSpoke plus an attached HubCaller — the
// spoke-side handle the per-box box-port API forwards through. It mirrors the
// production wiring in internal/spoke (one caller for the spoke's lifetime,
// attached by RunWithCaller after enrollment).
//
// @arg name The spoke name to enroll.
// @return *fakeRemote A handle to the connected spoke.
// @return *cluster.HubCaller The caller attached to the spoke's connection.
func (f *clusterFixture) connectSpokeWithCaller(name string) (*fakeRemote, *cluster.HubCaller) {
	f.t.Helper()
	joinToken, err := cluster.CreateJoinToken(f.store, name, "docker", time.Hour, time.Now())
	if err != nil {
		f.t.Fatalf("create join token: %v", err)
	}
	r := &fakeRemote{t: f.t, fixture: f, name: name, mgr: newFakeSpokeMgr(name, "box:e2e")}
	caller := cluster.NewHubCaller()
	ctx, cancel := context.WithCancel(f.ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	save := func(c cluster.Credentials) error {
		r.mu.Lock()
		r.creds = &c
		r.mu.Unlock()
		return nil
	}
	go func() {
		_ = cluster.RunWithCaller(ctx, cluster.WebSocketDialer(f.wsURL), r.mgr, joinToken, nil, save, caller)
	}()
	f.waitSpokeConnected(name, true)
	return r, caller
}

// callerOpenPort retries an OpenBoxPort briefly, because the caller attaches to
// the connection asynchronously right after enrollment.
func callerOpenPort(t *testing.T, caller *cluster.HubCaller, boxID string, port int, desc string) (cluster.BoxPortInfo, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := caller.OpenBoxPort(context.Background(), boxID, port, desc)
		if err == nil || !strings.Contains(err.Error(), "not connected") || time.Now().After(deadline) {
			return info, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBoxPublishesOwnPort drives the box-originated port-publishing path end to
// end over a real websocket cluster: a spoke enrolls, a box is created on it,
// and the box's port is opened/listed/closed through the spoke's HubCaller —
// exactly what the in-box boxapi socket forwards. It also asserts the scoping
// invariant: a spoke can only ever manage ports of its own boxes.
func TestBoxPublishesOwnPort(t *testing.T) {
	f := newClusterFixture(t)
	_, callerA := f.connectSpokeWithCaller("spoke-a")
	f.connectSpoke("spoke-b")
	f.createBoxViaAPI("web-box", "spoke-a")
	f.createBoxViaAPI("other-box", "spoke-b")

	// Open: the URL comes back and points at the proxy domain.
	info, err := callerOpenPort(t, callerA, "web-box", 3000, "dev server")
	if err != nil {
		t.Fatalf("OpenBoxPort: %v", err)
	}
	if info.Port != 3000 || !strings.Contains(info.URL, ".proxy.example.com/") {
		t.Fatalf("opened port = %+v, want port 3000 with a proxy.example.com URL", info)
	}

	// Idempotent: opening again returns the same URL.
	again, err := callerOpenPort(t, callerA, "web-box", 3000, "ignored")
	if err != nil {
		t.Fatalf("OpenBoxPort again: %v", err)
	}
	if again.URL != info.URL {
		t.Errorf("repeat open URL = %q, want %q", again.URL, info.URL)
	}

	// The hub recorded it for the right box, visible over the admin API too.
	var proxies struct {
		Proxies []struct {
			BoxID string `json:"box_id"`
			Port  int    `json:"port"`
			URL   string `json:"url"`
		} `json:"proxies"`
	}
	if err := f.apiCall("/api/v1/list-proxies", map[string]any{}, &proxies); err != nil {
		t.Fatalf("list-proxies: %v", err)
	}
	if len(proxies.Proxies) != 1 || proxies.Proxies[0].BoxID != "web-box" || proxies.Proxies[0].URL != info.URL {
		t.Fatalf("admin view = %+v, want the one web-box proxy", proxies.Proxies)
	}

	// List through the box path: only this box's ports.
	ports, err := callerA.ListBoxPorts(context.Background(), "web-box")
	if err != nil {
		t.Fatalf("ListBoxPorts: %v", err)
	}
	if len(ports) != 1 || ports[0].URL != info.URL {
		t.Fatalf("box list = %+v, want the one opened port", ports)
	}

	// SCOPING: spoke-a cannot manage a box that lives on spoke-b — same vague
	// error whether the box is unknown or just not its own.
	if _, err := callerA.OpenBoxPort(context.Background(), "other-box", 8080, ""); err == nil || !strings.Contains(err.Error(), "no box") {
		t.Fatalf("cross-spoke open err = %v, want a not-found rejection", err)
	}
	if _, err := callerA.OpenBoxPort(context.Background(), "ghost", 8080, ""); err == nil || !strings.Contains(err.Error(), "no box") {
		t.Fatalf("unknown-box open err = %v, want a not-found rejection", err)
	}

	// Close through the box path; the record is gone for the admin too.
	if err := callerA.CloseBoxPort(context.Background(), "web-box", 3000); err != nil {
		t.Fatalf("CloseBoxPort: %v", err)
	}
	if err := f.apiCall("/api/v1/list-proxies", map[string]any{}, &proxies); err != nil {
		t.Fatalf("list-proxies after close: %v", err)
	}
	if len(proxies.Proxies) != 0 {
		t.Fatalf("proxies after close = %+v, want none", proxies.Proxies)
	}
}
