package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/docker"
)

// errPipeClosed is returned by a memTransport once either end is closed.
var errPipeClosed = errors.New("pipe closed")

// memTransport is one end of an in-memory, full-duplex frame pipe used to test
// the hub-side remoteSpoke and the spoke-side dispatch loop without a real
// WebSocket. Closing either end fails both.
type memTransport struct {
	recv    <-chan frame
	send    chan<- frame
	done    chan struct{}
	closeFn func()
}

// newPipe returns the two connected ends of an in-memory transport.
func newPipe() (a, b *memTransport) {
	ab := make(chan frame, 64)
	ba := make(chan frame, 64)
	done := make(chan struct{})
	var once sync.Once
	closeFn := func() { once.Do(func() { close(done) }) }
	a = &memTransport{recv: ba, send: ab, done: done, closeFn: closeFn}
	b = &memTransport{recv: ab, send: ba, done: done, closeFn: closeFn}
	return a, b
}

func (t *memTransport) Send(ctx context.Context, f frame) error {
	select {
	case <-t.done:
		return errPipeClosed
	case <-ctx.Done():
		return ctx.Err()
	case t.send <- f:
		return nil
	}
}

func (t *memTransport) Recv(ctx context.Context) (frame, error) {
	select {
	case <-t.done:
		return frame{}, errPipeClosed
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case f := <-t.recv:
		return f, nil
	}
}

func (t *memTransport) Close() error {
	t.closeFn()
	return nil
}

// fakeManager is a configurable, recording BoxManager used to assert that verbs
// arrive at the spoke with the right arguments and that their results round-trip
// back. When err is set, every verb returns it.
type fakeManager struct {
	mu sync.Mutex

	// configured results
	createID   string
	createURL  string
	sessionURL string
	boxes      []docker.Box
	logsOut    string
	execResult docker.ExecResult
	reaped     []string
	err        error

	// recorded inputs
	lastCreate  docker.CreateOptions
	lastSubmit  [2]string // id, code
	lastDestroy string
	lastLogs    [2]any // idOrName, tail
	lastExec    struct {
		idOrName string
		cmd      []string
	}
	lastReap time.Duration
}

func (f *fakeManager) Create(_ context.Context, opts docker.CreateOptions) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreate = opts
	if f.err != nil {
		return "", "", f.err
	}
	return f.createID, f.createURL, nil
}

func (f *fakeManager) SubmitCode(_ context.Context, id, code string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSubmit = [2]string{id, code}
	if f.err != nil {
		return "", f.err
	}
	return f.sessionURL, nil
}

func (f *fakeManager) List(_ context.Context) ([]docker.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.boxes, nil
}

func (f *fakeManager) Destroy(_ context.Context, idOrName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDestroy = idOrName
	return f.err
}

func (f *fakeManager) Logs(_ context.Context, idOrName string, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLogs = [2]any{idOrName, tail}
	if f.err != nil {
		return "", f.err
	}
	return f.logsOut, nil
}

func (f *fakeManager) Exec(_ context.Context, idOrName string, cmd []string) (docker.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastExec.idOrName = idOrName
	f.lastExec.cmd = cmd
	if f.err != nil {
		return docker.ExecResult{}, f.err
	}
	return f.execResult, nil
}

func (f *fakeManager) ReapOrphans(_ context.Context, ttl time.Duration) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReap = ttl
	if f.err != nil {
		return nil, f.err
	}
	return f.reaped, nil
}

// memStore is an in-memory cluster.Store for tests.
type memStore struct {
	mu     sync.Mutex
	join   map[string]JoinTokenRecord
	spokes map[string]SpokeRecord
}

func newMemStore() *memStore {
	return &memStore{join: map[string]JoinTokenRecord{}, spokes: map[string]SpokeRecord{}}
}

func (m *memStore) PutJoinToken(hash string, rec JoinTokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.join[hash] = rec
	return nil
}

func (m *memStore) TakeJoinToken(hash string) (JoinTokenRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.join[hash]
	if ok {
		delete(m.join, hash)
	}
	return rec, ok, nil
}

func (m *memStore) PutSpoke(name string, rec SpokeRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spokes[name] = rec
	return nil
}

func (m *memStore) GetSpoke(name string) (SpokeRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.spokes[name]
	return rec, ok, nil
}

func (m *memStore) ListSpokes() ([]SpokeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SpokeRecord, 0, len(m.spokes))
	for _, r := range m.spokes {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) DeleteSpoke(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.spokes, name)
	return nil
}
