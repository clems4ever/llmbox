package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestNewRootCmd checks the command wiring: its name and the three flags.
func TestNewRootCmd(t *testing.T) {
	cmd := newRootCmd()
	if cmd.Use != name {
		t.Errorf("Use = %q, want %q", cmd.Use, name)
	}
	for _, flag := range []string{"upstream", "addr", "stdio"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("flag --%s not registered", flag)
		}
	}
}

// TestRunRequiresUpstream checks run refuses to start without an upstream URL.
func TestRunRequiresUpstream(t *testing.T) {
	if err := run(context.Background(), "", "", ":0", false); err == nil {
		t.Fatal("run with empty upstream = nil, want an error")
	}
}

// TestRunHTTPServesAndStops checks run serves the HTTP MCP endpoint and returns
// cleanly once its context is cancelled. The upstream is never contacted here
// (nothing dials it), so a placeholder URL is fine.
func TestRunHTTPServesAndStops(t *testing.T) {
	// Reserve an ephemeral port, then hand its address to run.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, "http://upstream.invalid", "", addr, false) }()

	// Wait until the listener is accepting connections.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("MCP HTTP endpoint did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned %v, want nil on cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}
