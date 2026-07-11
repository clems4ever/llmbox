package testutils

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
)

// GuestFixture is a running guest with a connected host-side client. It lets any
// package drive the box-control surface (Init, Exec, Dial) over a real Unix socket
// without a real backend. Use NewGuestFixture to build one; its teardown is
// registered on the test automatically.
type GuestFixture struct {
	// Client is the host-side client connected to the guest's control socket.
	Client *guest.Client
	// SocketPath is the filesystem path of the guest's control socket.
	SocketPath string

	cancel    context.CancelFunc
	errc      chan error
	closeOnce sync.Once
}

// NewGuestFixture starts a guest serving a temporary Unix control socket and
// returns it with a connected client. The serve loop is drained via t.Cleanup, so
// callers need no manual teardown.
//
// @arg t The test the fixture's lifetime and temp files are scoped to.
// @return *GuestFixture A running guest with a connected client.
//
// @testcase TestGuestFixtureDrivesLifecycle drives a box through a fixture built here.
func NewGuestFixture(t testing.TB) *GuestFixture {
	t.Helper()
	a := guest.New(guest.Options{})
	sock := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(ctx, sock) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("guest control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	f := &GuestFixture{
		Client:     guest.NewUnixClient(sock),
		SocketPath: sock,
		cancel:     cancel,
		errc:       errc,
	}
	t.Cleanup(f.Close)
	return f
}

// BoxEnv returns an environment to pass in InitReq.Env that points HOME at a
// fresh temp dir, so the box's processes run from a writable, isolated home.
//
// @arg t The test the temp HOME is scoped to.
// @return []string The HOME (and PATH) environment for the box.
//
// @testcase TestGuestFixtureDrivesLifecycle uses a BoxEnv-scoped HOME.
func (f *GuestFixture) BoxEnv(t testing.TB) []string {
	t.Helper()
	return []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
}

// Close cancels the guest's serve loop and waits for it to return. It is
// registered with t.Cleanup by NewGuestFixture, so tests rarely call it directly;
// it is safe to call more than once.
//
// @testcase TestGuestFixtureDrivesLifecycle tears the fixture down via Close.
func (f *GuestFixture) Close() {
	f.closeOnce.Do(func() {
		f.cancel()
		<-f.errc
	})
}
