package box_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/box/conformance"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// TestBoxManager runs the backend-neutral box contract against the in-process
// Fake provisioner, validating the Manager and the agent-protocol path it drives
// without Docker. The Docker backend reuses the same conformance.Run.
func TestBoxManager(t *testing.T) {
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		return conformance.NewFake(t)
	})
}

// TestBoxManagerDialBox checks DialBox reaches a listener on the box's localhost
// through the agent's Dial splice. It uses the in-process Fake, where the box's
// localhost is the host's, so a host listener stands in for an in-box service.
// (Container localhost differs, so this is not part of the shared contract; the
// Docker backend proves the host→socket→agent path via Exec/Logs instead.)
func TestBoxManagerDialBox(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 64)
				n, _ := conn.Read(buf)
				_, _ = conn.Write(buf[:n])
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	m := box.NewManager(conformance.NewFake(t), box.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, _, err := m.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	conn, err := m.DialBox(ctx, id, port)
	if err != nil {
		t.Fatalf("DialBox: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
}
