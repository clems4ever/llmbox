package hub

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// dialMgr is a FakeMgr that also satisfies boxDialer by dialing a fixed address,
// standing in for the in-process docker manager reaching a box's port.
type dialMgr struct {
	*testutils.FakeMgr
	target   string // host:port DialBox connects to (a real test listener)
	gotBoxID string // identifier the last DialBox call received, recorded for assertions
}

// DialBox records the identifier it was called with and dials the fixed target.
// The recorded identifier lets tests assert the proxy dials by container ID (what
// the real docker manager resolves), not the user-facing box ID.
func (d *dialMgr) DialBox(_ context.Context, boxID string, _ int) (net.Conn, error) {
	d.gotBoxID = boxID
	return net.Dial("tcp", d.target)
}

// newProxyServer builds a proxy-enabled Server backed by a real store and the
// given manager and authenticator, and registers a "web-box" session on the
// local spoke so proxies can be created for it.
func newProxyServer(t *testing.T, mgr boxManager, a *auth.Authenticator) (*Server, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := wireSpoke(New(nil, "https://boxes.example.com", st, a), mgr)
	s.SetProxyBaseDomain("proxy.example.com")
	return s, st
}

// registerBox creates a tracked session for boxID on the given spoke.
func registerBox(t *testing.T, s *Server, boxID, spoke string) {
	t.Helper()
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: boxID, SpokeName: spoke}); err != nil {
		t.Fatalf("createBox: %v", err)
	}
}

// TestCreateProxyRegistersAndBuildsURL checks a proxy is registered with a slug,
// the local spoke, and a sub-domain URL built from the base domain.
func TestCreateProxyRegistersAndBuildsURL(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	rec, err := s.createProxy("web-box", 8000, "dev@corp.com", "web preview")
	if err != nil {
		t.Fatalf("createProxy: %v", err)
	}
	if rec.Slug == "" || rec.Port != 8000 || rec.BoxID != "web-box" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.Spoke != testSpoke {
		t.Errorf("spoke = %q, want %q", rec.Spoke, testSpoke)
	}
	if rec.Description != "web preview" {
		t.Errorf("description = %q, want %q", rec.Description, "web preview")
	}
	if got, want := s.proxyURL(rec.Slug), "https://"+rec.Slug+".proxy.example.com/"; got != want {
		t.Errorf("proxyURL = %q, want %q", got, want)
	}
}

// TestCreateBoxPublishesConfiguredPorts checks a box created on a spoke that
// configured --publish-port comes up with an HTTP proxy already registered for
// each of those ports, recorded as created by the spoke.
func TestCreateBoxPublishesConfiguredPorts(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID: "abcdef0123456789",
		CreatePublishPorts: []sandbox.PublishPort{
			{Port: 8080, Description: "web-app"},
			{Port: 3000},
		},
	}
	s, st := newProxyServer(t, f, nil)

	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "cc-box", SpokeName: ""}); err != nil {
		t.Fatalf("createBox: %v", err)
	}

	proxies, err := st.ListProxies()
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	byPort := map[int]store.ProxyRecord{}
	for _, p := range proxies {
		byPort[p.Port] = p
	}
	if len(byPort) != 2 {
		t.Fatalf("published %d proxies, want 2: %+v", len(byPort), proxies)
	}
	for _, want := range []struct {
		port int
		desc string
	}{{8080, "web-app"}, {3000, ""}} {
		p, ok := byPort[want.port]
		if !ok {
			t.Fatalf("no proxy for port %d", want.port)
		}
		if p.BoxID != "cc-box" || p.InstanceID != "abcdef0123456789" {
			t.Errorf("proxy %d bound to %q/%q, want cc-box/abcdef0123456789", want.port, p.BoxID, p.InstanceID)
		}
		if p.Description != want.desc {
			t.Errorf("proxy %d description = %q, want %q", want.port, p.Description, want.desc)
		}
		if p.Owner != "spoke:"+testSpoke {
			t.Errorf("proxy %d owner = %q, want spoke:%s", want.port, p.Owner, testSpoke)
		}
	}
}

// TestCreateBoxPublishPortsProxyDisabled checks that configured publish ports on
// a hub without proxying enabled are skipped without failing box creation.
func TestCreateBoxPublishPortsProxyDisabled(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID:           "abcdef0123456789",
		CreatePublishPorts: []sandbox.PublishPort{{Port: 8080}},
	}
	// newTestServer does not enable proxying (no base domain).
	s := newTestServer(f)
	if s.ProxyEnabled() {
		t.Fatal("test server unexpectedly has proxying enabled")
	}
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "cc-box"}); err != nil {
		t.Fatalf("createBox should not fail when proxying is disabled: %v", err)
	}
}

// TestProxyURLCarriesPublicURLPort checks proxyURL appends the public URL's port
// so the advertised URL is reachable when the hub runs on a non-standard port,
// while the base domain itself stays port-free.
func TestProxyURLCarriesPublicURLPort(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(nil, "https://boxes.example.com:8443", st, nil)
	s.SetProxyBaseDomain("proxy.example.com")
	if got, want := s.proxyURL("abc123"), "https://abc123.proxy.example.com:8443/"; got != want {
		t.Errorf("proxyURL = %q, want %q", got, want)
	}
}

// TestCreateProxyDisabled checks createProxy and ProxyEnabled report disabled
// when no base domain is configured.
func TestCreateProxyDisabled(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	if s.ProxyEnabled() {
		t.Fatal("ProxyEnabled = true without a base domain")
	}
	if _, err := s.createProxy("web-box", 8000, "", ""); err == nil {
		t.Error("expected an error when proxying is disabled")
	}
}

// TestCreateProxyUnknownBox checks createProxy refuses a box with no session.
func TestCreateProxyUnknownBox(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	if _, err := s.createProxy("nope", 8000, "", ""); err == nil {
		t.Error("expected an error for an unknown box ID")
	}
}

// TestCreateProxyRejectsBadPort checks createProxy validates the port range.
func TestCreateProxyRejectsBadPort(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	for _, port := range []int{0, -1, 70000} {
		if _, err := s.createProxy("web-box", port, "", ""); err == nil {
			t.Errorf("port %d: expected an error", port)
		}
	}
}

// TestCreateProxyIdempotent checks a repeated create for the same box/port
// returns the existing proxy rather than a duplicate.
func TestCreateProxyIdempotent(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	first, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatalf("createProxy #1: %v", err)
	}
	second, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatalf("createProxy #2: %v", err)
	}
	if first.Slug != second.Slug {
		t.Errorf("slug changed on repeat: %q vs %q", first.Slug, second.Slug)
	}
	list, _ := st.ListProxies()
	if len(list) != 1 {
		t.Errorf("got %d proxies, want 1 (idempotent)", len(list))
	}
}

// TestCreateProxyIdempotentKeepsDescription checks that a repeated create for the
// same box/port and container returns the original record unchanged, so a
// description supplied only on the second call is ignored (and an original
// description is preserved).
func TestCreateProxyIdempotentKeepsDescription(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	first, err := s.createProxy("web-box", 8000, "", "original note")
	if err != nil {
		t.Fatalf("createProxy #1: %v", err)
	}
	second, err := s.createProxy("web-box", 8000, "", "ignored note")
	if err != nil {
		t.Fatalf("createProxy #2: %v", err)
	}
	if second.Slug != first.Slug {
		t.Errorf("slug changed on repeat: %q vs %q", first.Slug, second.Slug)
	}
	if second.Description != "original note" {
		t.Errorf("description = %q, want the original %q", second.Description, "original note")
	}
	stored, ok, _ := st.GetProxy(first.Slug)
	if !ok || stored.Description != "original note" {
		t.Errorf("stored description = %q (ok=%v), want %q", stored.Description, ok, "original note")
	}
}

// TestCreateProxyEmptyDescription checks an empty description is accepted and
// stored as the zero value (so the field is omitted from on-disk JSON).
func TestCreateProxyEmptyDescription(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatalf("createProxy: %v", err)
	}
	if rec.Description != "" {
		t.Errorf("description = %q, want empty", rec.Description)
	}
}

// TestCreateProxyReplacesStaleContainer checks that when a proxy already exists
// for a box ID/port but points at a different (destroyed) container, createProxy
// mints a fresh slug and drops the stale one — so a new box that reuses a box ID
// never inherits the old box's proxy URL.
func TestCreateProxyReplacesStaleContainer(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "newcontainer00000"}, nil)
	// Pre-seed a proxy from an earlier box generation (a different container).
	if err := st.SaveProxy(store.ProxyRecord{
		Slug: "oldslug", BoxID: "web-box", InstanceID: "oldcontainer00000", Port: 8000, Spoke: testSpoke,
	}); err != nil {
		t.Fatal(err)
	}
	registerBox(t, s, "web-box", "") // session's container is "newcontainer00000"

	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatalf("createProxy: %v", err)
	}
	if rec.Slug == "oldslug" {
		t.Error("createProxy reused the stale slug from the destroyed container")
	}
	if rec.InstanceID != "newcontainer00000" {
		t.Errorf("new proxy container = %q, want the current box's container", rec.InstanceID)
	}
	// The stale record is gone, and only the fresh one remains.
	if _, ok, _ := st.GetProxy("oldslug"); ok {
		t.Error("stale proxy slug still resolvable")
	}
	if list, _ := st.ListProxies(); len(list) != 1 {
		t.Errorf("got %d proxies, want 1", len(list))
	}
}

// TestDestroySessionlessBoxRemovesProxies checks that destroying a box by its box
// ID clears its proxies even when no session is tracked (defence in depth).
func TestDestroySessionlessBoxRemovesProxies(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{}, nil)
	if err := st.SaveProxy(store.ProxyRecord{
		Slug: "slug1", BoxID: "web-box", InstanceID: "c1", Port: 8000, Spoke: testSpoke,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.destroyBox(context.Background(), "web-box"); err != nil {
		t.Fatalf("destroyBox: %v", err)
	}
	if list, _ := st.ListProxies(); len(list) != 0 {
		t.Errorf("sessionless box destroy left proxies: %+v", list)
	}
}

// TestCreateProxyRefusesTerminatedBox checks a proxy cannot be enabled for a
// terminated tombstone: the container is gone, so the proxy could never route.
func TestCreateProxyRefusesTerminatedBox(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	s.regSession("tok", &session{
		BoxID: "dead-box", Generation: "cccccccccccc1111", SpokeName: testSpoke,
		Phase: "ready", BoxState: boxStateTerminated,
	})

	if _, err := s.createProxy("dead-box", 8000, "", ""); err == nil {
		t.Fatal("creating a proxy for a terminated box should be refused")
	}
}

// TestSyncReconcilesProxies checks the sync pass drops a proxy whose box
// generation no longer exists on its spoke while keeping a proxy whose box is
// still alive — closing the reuse window where a box vanishes out of band.
func TestSyncReconcilesProxies(t *testing.T) {
	mgr := &testutils.FakeMgr{ListResult: []sandbox.Box{
		{InstanceID: "live123", BoxID: "live-box", State: "running", Phase: "ready"},
	}}
	s, st := newProxyServer(t, mgr, nil)
	// One proxy for a live box, one for a box that no longer exists.
	if err := st.SaveProxy(store.ProxyRecord{Slug: "live", BoxID: "live-box", InstanceID: "live123", Port: 8000, Spoke: testSpoke}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProxy(store.ProxyRecord{Slug: "dead", BoxID: "dead-box", InstanceID: "dead999", Port: 8000, Spoke: testSpoke}); err != nil {
		t.Fatal(err)
	}

	s.syncSpokes(context.Background())

	if _, ok, _ := st.GetProxy("live"); !ok {
		t.Error("live box's proxy was wrongly dropped")
	}
	if _, ok, _ := st.GetProxy("dead"); ok {
		t.Error("stale proxy for a gone box survived the sync pass")
	}
}

// TestListProxiesFiltersByBox checks listProxies returns all proxies or only one
// box's.
func TestListProxiesFiltersByBox(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	registerBox(t, s, "api-box", "")
	if _, err := s.createProxy("web-box", 8000, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createProxy("api-box", 9000, "", ""); err != nil {
		t.Fatal(err)
	}

	all, _ := s.listProxies("")
	if len(all) != 2 {
		t.Errorf("listProxies(\"\") = %d, want 2", len(all))
	}
	one, _ := s.listProxies("web-box")
	if len(one) != 1 || one[0].BoxID != "web-box" {
		t.Errorf("listProxies(web-box) = %+v", one)
	}
}

// TestDeleteProxyRemoves checks deleteProxy removes a proxy by box and port.
func TestDeleteProxyRemoves(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	rec, _ := s.createProxy("web-box", 8000, "", "")

	slug, err := s.deleteProxy("web-box", 8000)
	if err != nil {
		t.Fatalf("deleteProxy: %v", err)
	}
	if slug != rec.Slug {
		t.Errorf("deleted slug = %q, want %q", slug, rec.Slug)
	}
	if list, _ := st.ListProxies(); len(list) != 0 {
		t.Errorf("proxy still present after delete: %+v", list)
	}
}

// TestDeleteProxyUnknown checks deleteProxy errors when no proxy matches.
func TestDeleteProxyUnknown(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	if _, err := s.deleteProxy("web-box", 8000); err == nil {
		t.Error("expected an error deleting a non-existent proxy")
	}
}

// TestDeleteProxyBySlug checks deleteProxyBySlug removes a proxy by its slug.
func TestDeleteProxyBySlug(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	rec, _ := s.createProxy("web-box", 8000, "", "")
	if err := s.deleteProxyBySlug(rec.Slug); err != nil {
		t.Fatalf("deleteProxyBySlug: %v", err)
	}
	if list, _ := st.ListProxies(); len(list) != 0 {
		t.Errorf("proxy still present: %+v", list)
	}
}

// TestDestroyBoxRemovesProxies checks destroying a box also removes its proxies.
func TestDestroyBoxRemovesProxies(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	if _, err := s.createProxy("web-box", 8000, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.destroyBox(context.Background(), "web-box"); err != nil {
		t.Fatalf("destroyBox: %v", err)
	}
	if list, _ := st.ListProxies(); len(list) != 0 {
		t.Errorf("proxies survived box destroy: %+v", list)
	}
}

// TestProxySlugFromHost checks slug extraction matches proxy sub-domains and
// rejects the main host, deeper sub-domains, and foreign domains.
func TestProxySlugFromHost(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	s.SetProxyBaseDomain("proxy.example.com")
	cases := map[string]struct {
		host     string
		wantSlug string
		wantOK   bool
	}{
		"match":      {"ab12.proxy.example.com", "ab12", true},
		"match-port": {"ab12.proxy.example.com:8080", "ab12", true},
		"uppercase":  {"AB12.Proxy.Example.com", "ab12", true},
		"base-only":  {"proxy.example.com", "", false},
		"deep":       {"a.b.proxy.example.com", "", false},
		"foreign":    {"ab12.evil.com", "", false},
		"main-host":  {"boxes.example.com", "", false},
	}
	for name, tc := range cases {
		slug, ok := s.proxySlugFromHost(tc.host)
		if ok != tc.wantOK || slug != tc.wantSlug {
			t.Errorf("%s: proxySlugFromHost(%q) = (%q,%v), want (%q,%v)", name, tc.host, slug, ok, tc.wantSlug, tc.wantOK)
		}
	}
}

// TestHandleProxyForwards checks an authorized request to a proxy sub-domain is
// reverse-proxied to the box's port and the upstream response is returned.
func TestHandleProxyForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "box")
		_, _ = w.Write([]byte("hello from box at " + r.URL.Path))
	}))
	defer upstream.Close()

	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, _ := newProxyServer(t, mgr, nil) // auth nil => proxy open
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/some/path", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "box" {
		t.Errorf("missing upstream header; got %v", resp.Header)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "hello from box at /some/path" {
		t.Errorf("body = %q", got)
	}
}

// TestHandleProxyDialsByBoxID checks the proxy dials the box by its user-facing
// box ID, not by the opaque generation token stamped on the proxy record. The
// hub addresses boxes only by (spoke, box ID); the spoke's Find resolves the box
// ID to its current incarnation. The box ID and generation token are deliberately
// distinct here so dialing the wrong identifier is detectable.
func TestHandleProxyDialsByBoxID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "generation-9999"}, target: upstream.Listener.Addr().String()}
	s, _ := newProxyServer(t, mgr, nil) // auth nil => proxy open
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.BoxID == rec.InstanceID {
		t.Fatalf("test setup invalid: box ID and generation token must differ (both %q)", rec.BoxID)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if mgr.gotBoxID != rec.BoxID {
		t.Errorf("DialBox dialed %q, want box ID %q (the hub addresses boxes by box ID, not the generation token)", mgr.gotBoxID, rec.BoxID)
	}
}

// TestHandleProxyUnknownSlug checks a request for a slug with no proxy 404s.
func TestHandleProxyUnknownSlug(t *testing.T) {
	s, _ := newProxyServer(t, &dialMgr{FakeMgr: &testutils.FakeMgr{}}, nil)
	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "deadbeef.proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleProxyRequiresLogin checks that, with activation auth enabled, an
// unauthenticated proxy request is refused.
func TestHandleProxyRequiresLogin(t *testing.T) {
	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}}
	s, _ := newProxyServer(t, mgr, a)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleProxyAuthorizedForwards checks a signed-in box-activator reaches the
// box through the proxy when auth is enabled.
func TestHandleProxyAuthorizedForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, st := newProxyServer(t, mgr, a)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a signed-in admin session and present its cookie.
	if err := st.PutIdentitySession(hashTok("SID"), store.IdentitySession{Email: "dev@corp.com", CanAdmin: true, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	req.AddCookie(&http.Cookie{Name: auth.LoginCookie, Value: "SID"})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandleProxyNamedSpokeForwards checks a proxy whose box runs on a specific
// (non-default) spoke is forwarded to that spoke over the streaming tunnel, with
// the query string preserved and the box dialed by its container ID.
func TestHandleProxyNamedSpokeForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Spoke", "remote")
		_, _ = w.Write([]byte("remote box at " + r.URL.RequestURI()))
	}))
	defer upstream.Close()

	remote := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, _ := newProxyServer(t, &dialMgr{FakeMgr: &testutils.FakeMgr{}}, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"remote1": remote}})
	registerBox(t, s, "web-box", "remote1")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/page?x=1", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Spoke") != "remote" {
		t.Errorf("missing spoke header; got %v", resp.Header)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "remote box at /page?x=1" {
		t.Errorf("body = %q", got)
	}
	if remote.gotBoxID != rec.BoxID {
		t.Errorf("remote spoke dialed box id %q, want box ID %q", remote.gotBoxID, rec.BoxID)
	}
}

// TestHandleProxyUnsupportedSpoke checks a proxy whose box runs on a spoke that
// can neither dial boxes nor proxy HTTP is refused with 502.
func TestHandleProxyUnsupportedSpoke(t *testing.T) {
	hub := &testutils.FakeHub{Connected: map[string]boxManager{
		"remote1": &testutils.FakeMgr{CreateID: "abcdef0123456789"},
	}}
	// The local manager can dial; the remote one (a plain FakeMgr) cannot.
	s, _ := newProxyServer(t, &dialMgr{FakeMgr: &testutils.FakeMgr{}}, nil)
	s.SetHub(hub)
	registerBox(t, s, "web-box", "remote1")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Spoke != "remote1" {
		t.Fatalf("proxy spoke = %q, want remote1", rec.Spoke)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// TestBackendProxies drives the box-control backend's proxy methods — enabling,
// listing, and disabling a proxy through the adapter the API handlers call.
func TestBackendProxies(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	b := s.boxBackend()

	if !b.ProxyEnabled() {
		t.Fatal("ProxyEnabled() = false, want true")
	}
	info, err := b.CreateProxy(context.Background(), "web-box", 8000, "preview server")
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if info.BoxID != "web-box" || info.Port != 8000 || info.URL == "" {
		t.Errorf("proxy info = %+v", info)
	}
	if info.Description != "preview server" {
		t.Errorf("CreateProxy description = %q, want %q", info.Description, "preview server")
	}
	list, err := b.ListProxies(context.Background(), "web-box")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProxies = %+v (err %v), want 1", list, err)
	}
	if list[0].Description != "preview server" {
		t.Errorf("ListProxies description = %q, want %q", list[0].Description, "preview server")
	}
	if err := b.DeleteProxy(context.Background(), "web-box", 8000); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if rest, _ := b.ListProxies(context.Background(), ""); len(rest) != 0 {
		t.Errorf("proxy survived delete: %+v", rest)
	}
}

// TestPingProxyReportsServing checks a proxy whose box port answers an HTTP
// request is reported OK with the box's status code, and that the box is dialed
// by its box ID.
func TestPingProxyReportsServing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, _ := newProxyServer(t, mgr, nil)
	registerBox(t, s, "web-box", "")
	if _, err := s.createProxy("web-box", 8000, "", ""); err != nil {
		t.Fatal(err)
	}

	ping, err := s.pingProxy(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("pingProxy: %v", err)
	}
	if !ping.OK {
		t.Errorf("ping OK = false (%q), want true", ping.Error)
	}
	if ping.Status != http.StatusTeapot {
		t.Errorf("ping status = %d, want %d", ping.Status, http.StatusTeapot)
	}
	if mgr.gotBoxID != "web-box" {
		t.Errorf("DialBox dialed %q, want box ID web-box", mgr.gotBoxID)
	}
}

// TestPingProxyDownWhenNoServer checks a proxy whose box port refuses the
// connection is reported not-OK (with a reason), not surfaced as an error.
func TestPingProxyDownWhenNoServer(t *testing.T) {
	// A listener closed right away yields an address nothing serves, so the dial
	// is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()

	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: dead}
	s, _ := newProxyServer(t, mgr, nil)
	registerBox(t, s, "web-box", "")
	if _, err := s.createProxy("web-box", 8000, "", ""); err != nil {
		t.Fatal(err)
	}

	ping, err := s.pingProxy(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("pingProxy: %v", err)
	}
	if ping.OK {
		t.Errorf("ping OK = true, want false for a down box")
	}
	if ping.Error == "" {
		t.Errorf("ping Error empty, want a reason for a down box")
	}
}

// TestPingProxyUnknownProxy checks pinging a box/port with no proxy is reported
// not-OK with a reason, not an error.
func TestPingProxyUnknownProxy(t *testing.T) {
	s, _ := newProxyServer(t, &dialMgr{FakeMgr: &testutils.FakeMgr{}}, nil)
	ping, err := s.pingProxy(context.Background(), "nope", 9999)
	if err != nil {
		t.Fatalf("pingProxy: %v", err)
	}
	if ping.OK || ping.Error == "" {
		t.Errorf("ping = %+v, want not-OK with a reason", ping)
	}
}

// TestPingProxyUnsupportedSpoke checks a proxy on a spoke that cannot dial boxes
// is reported not-OK rather than erroring.
func TestPingProxyUnsupportedSpoke(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil) // plain FakeMgr: not a boxDialer
	registerBox(t, s, "web-box", "")
	if _, err := s.createProxy("web-box", 8000, "", ""); err != nil {
		t.Fatal(err)
	}
	ping, err := s.pingProxy(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("pingProxy: %v", err)
	}
	if ping.OK {
		t.Errorf("ping OK = true, want false for a spoke that cannot dial boxes")
	}
}

// TestBackendPingProxy checks the box-control backend surfaces a proxy probe
// through the adapter the API handler calls.
func TestBackendPingProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, _ := newProxyServer(t, mgr, nil)
	registerBox(t, s, "web-box", "")
	b := s.boxBackend()
	if _, err := b.CreateProxy(context.Background(), "web-box", 8000, ""); err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	ping, err := b.PingProxy(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("PingProxy: %v", err)
	}
	if !ping.OK || ping.Status != http.StatusOK {
		t.Errorf("ping = %+v, want OK with status 200", ping)
	}
}
