package hub

import (
	"context"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/testutils"
)

// TestOpenBoxPortCreatesProxy checks a box-originated open publishes a proxy
// for the box, stamps the box as creator, and returns the public URL.
func TestOpenBoxPortCreatesProxy(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	info, err := s.OpenBoxPort(context.Background(), testSpoke, "web-box", 3000, "dev server")
	if err != nil {
		t.Fatalf("OpenBoxPort: %v", err)
	}
	if info.Port != 3000 || info.Description != "dev server" {
		t.Errorf("info = %+v", info)
	}
	list, err := st.ListProxies()
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("proxy count = %d, want 1", len(list))
	}
	rec := list[0]
	if rec.BoxID != "web-box" || rec.Port != 3000 {
		t.Errorf("record = %+v", rec)
	}
	if rec.Owner != "box:web-box" {
		t.Errorf("Owner = %q, want box:web-box", rec.Owner)
	}
	if want := s.proxyURL(rec.Slug); info.URL != want || info.URL == "" {
		t.Errorf("URL = %q, want %q", info.URL, want)
	}
}

// TestOpenBoxPortWrongSpoke checks a request arriving from a spoke that does
// not own the box is rejected with the same vague error as an unknown box.
func TestOpenBoxPortWrongSpoke(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	_, err := s.OpenBoxPort(context.Background(), "other-spoke", "web-box", 3000, "")
	if err == nil {
		t.Fatal("expected an error for a box owned by another spoke")
	}
	if !strings.Contains(err.Error(), `no box "web-box" found on spoke "other-spoke"`) {
		t.Errorf("err = %v, want the vague not-found message", err)
	}
}

// TestOpenBoxPortUnknownBox checks an unknown box ID is rejected with the same
// message shape as the wrong-spoke case.
func TestOpenBoxPortUnknownBox(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	_, err := s.OpenBoxPort(context.Background(), testSpoke, "ghost", 3000, "")
	if err == nil || !strings.Contains(err.Error(), `no box "ghost" found on spoke`) {
		t.Fatalf("err = %v, want the vague not-found message", err)
	}
}

// TestOpenBoxPortEmptyBoxID checks a box created without a box ID cannot
// publish ports and gets a clear explanation.
func TestOpenBoxPortEmptyBoxID(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	_, err := s.OpenBoxPort(context.Background(), testSpoke, "", 3000, "")
	if err == nil || !strings.Contains(err.Error(), "no box ID") {
		t.Fatalf("err = %v, want the no-box-ID message", err)
	}
}

// TestOpenBoxPortProxyDisabled checks the box gets a clear message when the hub
// has no proxy base domain configured.
func TestOpenBoxPortProxyDisabled(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	_, err := s.OpenBoxPort(context.Background(), testSpoke, "web-box", 3000, "")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want the disabled message", err)
	}
}

// TestCloseBoxPortDeletesProxy checks a box can close its own published port,
// and closing a port with no proxy errors.
func TestCloseBoxPortDeletesProxy(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	if _, err := s.OpenBoxPort(context.Background(), testSpoke, "web-box", 3000, ""); err != nil {
		t.Fatalf("OpenBoxPort: %v", err)
	}
	if err := s.CloseBoxPort(context.Background(), testSpoke, "web-box", 3000); err != nil {
		t.Fatalf("CloseBoxPort: %v", err)
	}
	list, _ := st.ListProxies()
	if len(list) != 0 {
		t.Errorf("proxy count after close = %d, want 0", len(list))
	}
	if err := s.CloseBoxPort(context.Background(), testSpoke, "web-box", 3000); err == nil {
		t.Error("expected an error closing an unpublished port")
	}
}

// TestCloseBoxPortWrongSpoke checks a spoke cannot close another spoke's box
// ports.
func TestCloseBoxPortWrongSpoke(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	if _, err := s.OpenBoxPort(context.Background(), testSpoke, "web-box", 3000, ""); err != nil {
		t.Fatalf("OpenBoxPort: %v", err)
	}

	if err := s.CloseBoxPort(context.Background(), "other-spoke", "web-box", 3000); err == nil {
		t.Fatal("expected an error from the wrong spoke")
	}
	list, _ := st.ListProxies()
	if len(list) != 1 {
		t.Errorf("proxy count = %d, want 1 (untouched)", len(list))
	}
}

// TestListBoxPortsScopedToBox checks a box's list never contains another box's
// ports.
func TestListBoxPortsScopedToBox(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")
	registerBox(t, s, "other-box", "")

	if _, err := s.OpenBoxPort(context.Background(), testSpoke, "web-box", 3000, "mine"); err != nil {
		t.Fatalf("OpenBoxPort web-box: %v", err)
	}
	if _, err := s.createProxy("other-box", 8080, "admin@corp.com", "not mine"); err != nil {
		t.Fatalf("createProxy other-box: %v", err)
	}

	ports, err := s.ListBoxPorts(context.Background(), testSpoke, "web-box")
	if err != nil {
		t.Fatalf("ListBoxPorts: %v", err)
	}
	if len(ports) != 1 || ports[0].Port != 3000 || ports[0].Description != "mine" {
		t.Errorf("ports = %+v, want only web-box's port 3000", ports)
	}
}
