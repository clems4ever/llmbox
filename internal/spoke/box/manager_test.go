package box_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// TestBoxManager runs the backend-neutral box contract against the in-process
// Fake provisioner, validating the Manager and the guest-protocol path it drives
// without Docker. The Docker backend reuses the same conformance.Run.
func TestBoxManager(t *testing.T) {
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		return conformance.NewFake(t)
	})
}

// TestBoxManagerDialBox checks DialBox reaches a listener on the box's localhost
// through the guest's Dial splice. It uses the in-process Fake, where the box's
// localhost is the host's, so a host listener stands in for an in-box service.
// (Container localhost differs, so this is not part of the shared contract; the
// Docker backend proves the host→socket→guest path via Exec/Logs instead.)
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

// stubProv is a box.Provisioner that returns canned results/errors, for driving
// the Manager's error paths without a real backend.
type stubProv struct {
	listInsts []box.Instance
	listErr   error
	provInst  box.Instance
	provErr   error
	findInst  box.Instance
	findErr   error
}

// Provision returns the stub's canned instance/error.
func (s *stubProv) Provision(context.Context, sandbox.CreateOptions) (box.Instance, error) {
	return s.provInst, s.provErr
}

// List returns the stub's canned instances/error.
func (s *stubProv) List(context.Context) ([]box.Instance, error) { return s.listInsts, s.listErr }

// Find returns the stub's canned instance/error.
func (s *stubProv) Find(context.Context, string) (box.Instance, error) { return s.findInst, s.findErr }

// stubInst is a box.Instance whose operations can be made to fail. A non-nil
// controlErr makes every guest call (Init/Start/Exec/...) fail at connect.
type stubInst struct {
	meta         sandbox.Box
	controlErr   error
	markReadyErr error
	destroyErr   error
	pauseErr     error
	resumeErr    error
}

// Meta returns the stub's canned box view.
func (s *stubInst) Meta() sandbox.Box { return s.meta }

// Control returns the stub's control error (or a default error).
func (s *stubInst) Control(context.Context) (net.Conn, error) { return nil, errOr(s.controlErr) }

// MarkReady returns the stub's mark-ready error.
func (s *stubInst) MarkReady(context.Context) error { return s.markReadyErr }

// Pause returns the stub's pause error.
func (s *stubInst) Pause(context.Context) error { return s.pauseErr }

// Resume returns the stub's resume error.
func (s *stubInst) Resume(context.Context) error { return s.resumeErr }

// Destroy returns the stub's destroy error.
func (s *stubInst) Destroy(context.Context) error { return s.destroyErr }

// errOr returns e, or a default non-nil error when e is nil.
func errOr(e error) error {
	if e != nil {
		return e
	}
	return errors.New("no control channel in stub")
}

// TestManagerCreateUniquenessListError surfaces a provisioner List error during
// the uniqueness check.
func TestManagerCreateUniquenessListError(t *testing.T) {
	m := box.NewManager(&stubProv{listErr: errors.New("list boom")}, box.Config{})
	if _, _, err := m.Create(context.Background(), sandbox.CreateOptions{BoxID: "x"}); err == nil {
		t.Fatal("Create should fail when the uniqueness check cannot list boxes")
	}
}

// TestManagerCreateProvisionError surfaces a provisioning error.
func TestManagerCreateProvisionError(t *testing.T) {
	m := box.NewManager(&stubProv{provErr: errors.New("prov boom")}, box.Config{})
	if _, _, err := m.Create(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Create should fail when provisioning fails")
	}
}

// TestManagerCreateGuestError destroys the box and errors when the guest cannot
// be reached for Init.
func TestManagerCreateGuestError(t *testing.T) {
	inst := &stubInst{controlErr: errors.New("no guest")}
	m := box.NewManager(&stubProv{provInst: inst}, box.Config{})
	if _, _, err := m.Create(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Create should fail when the guest is unreachable")
	}
}

// TestManagerSubmitCodeFindError surfaces a Find error.
func TestManagerSubmitCodeFindError(t *testing.T) {
	m := box.NewManager(&stubProv{findErr: sandbox.ErrBoxNotFound}, box.Config{})
	if _, err := m.SubmitCode(context.Background(), "x", "code"); err == nil {
		t.Fatal("SubmitCode should fail when the box is not found")
	}
}

// TestManagerVerbsFindError checks Exec, Logs, and DialBox surface a Find error.
func TestManagerVerbsFindError(t *testing.T) {
	m := box.NewManager(&stubProv{findErr: sandbox.ErrBoxNotFound}, box.Config{})
	ctx := context.Background()
	if _, err := m.Exec(ctx, "x", []string{"echo"}); err == nil {
		t.Fatal("Exec should fail when the box is not found")
	}
	if _, err := m.Logs(ctx, "x", 0); err == nil {
		t.Fatal("Logs should fail when the box is not found")
	}
	if _, err := m.DialBox(ctx, "x", 80); err == nil {
		t.Fatal("DialBox should fail when the box is not found")
	}
}

// TestManagerListError surfaces a provisioner List error.
func TestManagerListError(t *testing.T) {
	m := box.NewManager(&stubProv{listErr: errors.New("list boom")}, box.Config{})
	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("List should surface the provisioner error")
	}
}

// TestManagerDestroyErrors checks Destroy surfaces a non-not-found Find error and
// a destroy error, while treating a not-found box as success.
func TestManagerDestroyErrors(t *testing.T) {
	ctx := context.Background()

	if err := box.NewManager(&stubProv{findErr: sandbox.ErrBoxNotFound}, box.Config{}).Destroy(ctx, "x"); err != nil {
		t.Fatalf("Destroy of a missing box should be a no-op, got %v", err)
	}
	if err := box.NewManager(&stubProv{findErr: errors.New("find boom")}, box.Config{}).Destroy(ctx, "x"); err == nil {
		t.Fatal("Destroy should surface a non-not-found Find error")
	}
	inst := &stubInst{destroyErr: errors.New("destroy boom")}
	if err := box.NewManager(&stubProv{findInst: inst}, box.Config{}).Destroy(ctx, "x"); err == nil {
		t.Fatal("Destroy should surface an instance destroy error")
	}
}
