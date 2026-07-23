package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePortService is a recording BoxPortService used to assert that box-port
// verbs arrive on the hub with the connection's spoke name and the spoke-stamped
// box ID. When err is set, every verb returns it.
type fakePortService struct {
	mu sync.Mutex

	// configured results
	info  BoxPortInfo
	ports []BoxPortInfo
	err   error

	// recorded inputs
	lastSpoke   string
	lastBoxID   string
	lastPort    int
	lastDesc    string
	lastDomain  string
	lastVerdict string
}

// OpenBoxPort is a test helper.
func (f *fakePortService) OpenBoxPort(_ context.Context, spokeName, boxID string, port int, description string) (BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSpoke, f.lastBoxID, f.lastPort, f.lastDesc = spokeName, boxID, port, description
	if f.err != nil {
		return BoxPortInfo{}, f.err
	}
	return f.info, nil
}

// RecordDNSAudit is a test helper.
func (f *fakePortService) RecordDNSAudit(_ context.Context, spokeName, boxID, domain, verdict string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSpoke, f.lastBoxID, f.lastDomain, f.lastVerdict = spokeName, boxID, domain, verdict
	return f.err
}

// CloseBoxPort is a test helper.
func (f *fakePortService) CloseBoxPort(_ context.Context, spokeName, boxID string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSpoke, f.lastBoxID, f.lastPort = spokeName, boxID, port
	return f.err
}

// ListBoxPorts is a test helper.
func (f *fakePortService) ListBoxPorts(_ context.Context, spokeName, boxID string) ([]BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSpoke, f.lastBoxID = spokeName, boxID
	if f.err != nil {
		return nil, f.err
	}
	return f.ports, nil
}

// startCallerSpoke runs RunWithCaller over an in-memory pipe, completes the
// enrollment handshake hub-side, and wraps the hub end in a remoteSpoke backed
// by svc — the same order ConnectHandler uses. It returns the attached caller
// and the hub-side remoteSpoke.
func startCallerSpoke(t *testing.T, ctx context.Context, svc BoxPortService) (*HubCaller, *remoteSpoke, chan error) {
	t.Helper()
	spokeEnd, hubEnd := newPipe()
	dial := func(context.Context) (transport, error) { return spokeEnd, nil }
	caller := NewHubCaller()

	done := make(chan error, 1)
	go func() {
		done <- RunWithCaller(ctx, dial, &fakeManager{}, "tok", nil, nil, caller)
	}()

	// Hub side: consume the enroll frame and welcome the spoke.
	enroll := recvWithin(t, hubEnd)
	if enroll.Type != frameEnroll {
		t.Fatalf("first frame type = %q, want enroll", enroll.Type)
	}
	wp, _ := encodePayload(welcomeResp{Name: "edge", Credential: "cred-1"})
	if err := hubEnd.Send(ctx, frame{Type: frameWelcome, Payload: wp}); err != nil {
		t.Fatalf("send welcome: %v", err)
	}
	return caller, newRemoteSpoke("edge", hubEnd, svc), done
}

// TestSpokeCallerRoundTrip is a package test.
func TestSpokeCallerRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc := &fakePortService{
		info:  BoxPortInfo{Port: 3000, URL: "https://ab12.example.com/", Description: "dev server"},
		ports: []BoxPortInfo{{Port: 3000, URL: "https://ab12.example.com/"}},
	}
	caller, _, _ := startCallerSpoke(t, ctx, svc)

	// The caller attaches asynchronously once enrollment completes; retry the
	// first call briefly rather than sleeping.
	var info BoxPortInfo
	var err error
	for start := time.Now(); ; {
		info, err = caller.OpenBoxPort(ctx, "box-a", 3000, "dev server")
		if !errors.Is(err, errNotConnected) || time.Since(start) > 2*time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("OpenBoxPort: %v", err)
	}
	if info != svc.info {
		t.Errorf("OpenBoxPort info = %+v, want %+v", info, svc.info)
	}
	svc.mu.Lock()
	if svc.lastSpoke != "edge" || svc.lastBoxID != "box-a" || svc.lastPort != 3000 || svc.lastDesc != "dev server" {
		t.Errorf("service saw spoke=%q box=%q port=%d desc=%q", svc.lastSpoke, svc.lastBoxID, svc.lastPort, svc.lastDesc)
	}
	svc.mu.Unlock()

	ports, err := caller.ListBoxPorts(ctx, "box-a")
	if err != nil {
		t.Fatalf("ListBoxPorts: %v", err)
	}
	if len(ports) != 1 || ports[0] != svc.ports[0] {
		t.Errorf("ListBoxPorts = %+v, want %+v", ports, svc.ports)
	}

	if err := caller.CloseBoxPort(ctx, "box-a", 3000); err != nil {
		t.Fatalf("CloseBoxPort: %v", err)
	}
	svc.mu.Lock()
	if svc.lastSpoke != "edge" || svc.lastBoxID != "box-a" || svc.lastPort != 3000 {
		t.Errorf("close saw spoke=%q box=%q port=%d", svc.lastSpoke, svc.lastBoxID, svc.lastPort)
	}
	svc.mu.Unlock()
}

// TestSpokeCallerServiceError is a package test.
func TestSpokeCallerServiceError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc := &fakePortService{err: errors.New("no box \"box-a\" found on spoke \"edge\"")}
	caller, _, _ := startCallerSpoke(t, ctx, svc)

	var err error
	for start := time.Now(); ; {
		_, err = caller.OpenBoxPort(ctx, "box-a", 3000, "")
		if !errors.Is(err, errNotConnected) || time.Since(start) > 2*time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "no box") {
		t.Fatalf("OpenBoxPort err = %v, want the service's error", err)
	}
}

// TestSpokeCallerDisconnected is a package test.
func TestSpokeCallerDisconnected(t *testing.T) {
	// A caller with no connection fails immediately.
	caller := NewHubCaller()
	if _, err := caller.OpenBoxPort(context.Background(), "box-a", 3000, ""); !errors.Is(err, errNotConnected) {
		t.Fatalf("detached OpenBoxPort err = %v, want errNotConnected", err)
	}

	// An in-flight call fails once the connection drops: attach a transport,
	// issue a call nobody answers, then close the pipe.
	spokeEnd, hubEnd := newPipe()
	caller.attach(spokeEnd)
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(context.Background(), spokeEnd, &fakeManager{}, caller) }()

	callErr := make(chan error, 1)
	go func() {
		_, err := caller.OpenBoxPort(context.Background(), "box-a", 3000, "")
		callErr <- err
	}()
	_ = recvWithin(t, hubEnd) // the request reached the hub side; never answer it
	_ = hubEnd.Close()
	<-serveDone
	caller.detach()

	select {
	case err := <-callErr:
		if !errors.Is(err, errNotConnected) {
			t.Fatalf("in-flight err = %v, want errNotConnected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight call did not fail after disconnect")
	}

	// And subsequent calls keep failing while detached.
	if _, err := caller.OpenBoxPort(context.Background(), "box-a", 3000, ""); !errors.Is(err, errNotConnected) {
		t.Fatalf("post-disconnect err = %v, want errNotConnected", err)
	}
}

// TestSpokeCallerReconnects is a package test.
func TestSpokeCallerReconnects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc := &fakePortService{info: BoxPortInfo{Port: 8080, URL: "https://x.example.com/"}}

	// First connection: drive it up, then drop it.
	caller, rs1, done := startCallerSpoke(t, ctx, svc)
	for start := time.Now(); ; {
		if _, err := caller.OpenBoxPort(ctx, "box-a", 8080, ""); err == nil || time.Since(start) > 2*time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = rs1.Close()
	<-done // RunWithCaller returned and detached the caller

	// Second connection: the same caller rides the new pipe.
	spokeEnd2, hubEnd2 := newPipe()
	dial2 := func(context.Context) (transport, error) { return spokeEnd2, nil }
	go func() {
		_ = RunWithCaller(ctx, dial2, &fakeManager{}, "", &Credentials{Name: "edge", Credential: "cred-1"}, nil, caller)
	}()
	enroll := recvWithin(t, hubEnd2)
	if enroll.Type != frameEnroll {
		t.Fatalf("first frame on reconnect = %q, want enroll", enroll.Type)
	}
	wp, _ := encodePayload(welcomeResp{Name: "edge"})
	if err := hubEnd2.Send(ctx, frame{Type: frameWelcome, Payload: wp}); err != nil {
		t.Fatalf("send welcome: %v", err)
	}
	newRemoteSpoke("edge", hubEnd2, svc)

	var err error
	for start := time.Now(); ; {
		_, err = caller.OpenBoxPort(ctx, "box-a", 8080, "")
		if !errors.Is(err, errNotConnected) || time.Since(start) > 2*time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("OpenBoxPort after reconnect: %v", err)
	}
}

// TestHubWithoutBoxPortServiceRejects is a package test.
func TestHubWithoutBoxPortServiceRejects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spokeEnd, hubEnd := newPipe()
	newRemoteSpoke("edge", hubEnd, nil)

	payload, _ := encodePayload(openBoxPortReq{BoxID: "box-a", Port: 3000})
	if err := spokeEnd.Send(ctx, frame{Type: frameSpokeReq, ID: 7, Method: methodOpenBoxPort, Payload: payload}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := recvWithin(t, spokeEnd)
	if resp.Type != frameSpokeResp || resp.ID != 7 {
		t.Fatalf("resp = %+v, want spoke_resp id 7", resp)
	}
	if !strings.Contains(resp.Error, "does not support box-port requests") {
		t.Errorf("resp error = %q, want unsupported message", resp.Error)
	}
}

// TestSpokeCallerUnknownMethod is a package test.
func TestSpokeCallerUnknownMethod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spokeEnd, hubEnd := newPipe()
	newRemoteSpoke("edge", hubEnd, &fakePortService{})

	if err := spokeEnd.Send(ctx, frame{Type: frameSpokeReq, ID: 9, Method: "bogus"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := recvWithin(t, spokeEnd)
	if resp.Type != frameSpokeResp || resp.ID != 9 || !strings.Contains(resp.Error, "unknown method") {
		t.Fatalf("resp = %+v, want unknown-method error", resp)
	}
}
