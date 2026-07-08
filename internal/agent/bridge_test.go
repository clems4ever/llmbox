package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startBridge runs RunBoxAPIBridge with the given dialer and waits for its
// socket to accept connections.
func startBridge(t *testing.T, dial func(ctx context.Context) (net.Conn, error)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "boxapi.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = RunBoxAPIBridge(ctx, sock, dial, log) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			_ = conn.Close()
			return sock
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBoxAPIBridgeSplices checks bytes flow both ways through the bridge, for
// concurrent connections.
func TestBoxAPIBridgeSplices(t *testing.T) {
	// The "host" side: a Unix echo server standing in for the vsock peer.
	hostSock := filepath.Join(t.TempDir(), "host.sock")
	ln, err := net.Listen("unix", hostSock)
	if err != nil {
		t.Fatalf("host listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := fmt.Fprintf(c, "echo:%s", line); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", hostSock)
	}
	sock := startBridge(t, dial)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Errorf("dial bridge: %v", err)
				return
			}
			defer conn.Close()
			msg := fmt.Sprintf("hello-%d\n", i)
			if _, err := conn.Write([]byte(msg)); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			got, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if got != "echo:"+msg {
				t.Errorf("got %q, want %q", got, "echo:"+msg)
			}
		}(i)
	}
	wg.Wait()
}

// TestDialHostVsockDialer checks DialHostVsock returns a usable dialer that,
// on a host without AF_VSOCK (the normal test environment), fails cleanly with
// an error rather than panicking or hanging — and that a returned connection,
// if the host does have vsock, is non-nil and closable. The end-to-end vsock
// path is proven by the live-microVM TestBoxAPIOverVsock.
func TestDialHostVsockDialer(t *testing.T) {
	dial := DialHostVsock(5001)
	if dial == nil {
		t.Fatal("DialHostVsock returned a nil dialer")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := dial(context.Background())
		if err != nil {
			return // no hypervisor here: a clean error is the expected outcome
		}
		if conn == nil {
			t.Error("dial returned a nil conn with a nil error")
			return
		}
		_ = conn.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dial neither returned nor failed within 5s")
	}
}

// TestBoxAPIBridgeDialError checks the client connection is closed when the
// host-side dial fails.
func TestBoxAPIBridgeDialError(t *testing.T) {
	dial := func(context.Context) (net.Conn, error) {
		return nil, errors.New("no hypervisor here")
	}
	sock := startBridge(t, dial)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read err = %v, want EOF (connection closed)", err)
	}
}
