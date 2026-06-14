// Package server ties the Docker box manager to two front-ends that share one
// process:
//
//   - an MCP server (streamable HTTP), used by a chatbot to create/list/destroy
//     boxes. It only ever exchanges box IDs and the *auth page URL* — never the
//     OAuth secret.
//   - a small web server that serves the auth page where the user pastes their
//     OAuth code. The code goes browser -> this server -> container stdin, so it
//     never enters the chat/MCP context.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
)

// boxManager is the behaviour Server needs from the Docker layer (real impl is
// *docker.Manager; tests fake it).
type boxManager interface {
	CreateLLMBox(ctx context.Context, opts docker.CreateOptions) (id, authorizeURL string, err error)
	SubmitCode(ctx context.Context, id, code string) (sessionURL string, err error)
	List(ctx context.Context) ([]docker.Box, error)
	Destroy(ctx context.Context, idOrName string) error
	ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error)
}

// session tracks one box through the auth handshake.
type session struct {
	Token        string
	BoxID        string
	AuthorizeURL string
	CreatedAt    time.Time

	// Hostname and Description are caller-supplied at creation and immutable,
	// so they need no locking.
	Hostname    string
	Description string

	mu         sync.Mutex
	Status     string // "pending" | "ready" | "error"
	SessionURL string
	Err        string
}

// snapshot reads the session's mutable state under its lock.
//
// @return status The current auth status: pending, ready, or error.
// @return sessionURL The remote-control session URL, set once ready.
// @return errMsg The error detail, set when status is error.
//
// @testcase TestSubmitCodeSuccess reads the session state via snapshot.
func (s *session) snapshot() (status, sessionURL, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status, s.SessionURL, s.Err
}

// persistLocked builds the on-disk form of the session. The caller must hold
// s.mu, as it reads the mutable status fields.
//
// @return persistedSession A snapshot of the session's durable fields.
//
// @testcase TestCreateBoxPersistsSession persists a session built via persistLocked.
func (s *session) persistLocked() persistedSession {
	return persistedSession{
		Token:        s.Token,
		BoxID:        s.BoxID,
		AuthorizeURL: s.AuthorizeURL,
		CreatedAt:    s.CreatedAt,
		Hostname:     s.Hostname,
		Description:  s.Description,
		Status:       s.Status,
		SessionURL:   s.SessionURL,
		Err:          s.Err,
	}
}

// persist builds the on-disk form of the session, taking the lock itself.
//
// @return persistedSession A snapshot of the session's durable fields.
//
// @testcase TestCreateBoxPersistsSession persists a freshly created session.
func (s *session) persist() persistedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// sessionFromPersisted reconstructs a live session from its on-disk form.
//
// @arg ps The persisted session to rehydrate.
// @return *session A live session carrying the persisted fields.
//
// @testcase TestRestoreLoadsAndReconciles rehydrates sessions from the store.
func sessionFromPersisted(ps persistedSession) *session {
	return &session{
		Token:        ps.Token,
		BoxID:        ps.BoxID,
		AuthorizeURL: ps.AuthorizeURL,
		CreatedAt:    ps.CreatedAt,
		Hostname:     ps.Hostname,
		Description:  ps.Description,
		Status:       ps.Status,
		SessionURL:   ps.SessionURL,
		Err:          ps.Err,
	}
}

// Server orchestrates boxes and owns the session registry.
type Server struct {
	mgr       boxManager
	publicURL string // external base URL, e.g. https://boxes.example.com
	authTTL   time.Duration
	store     Store // persists the registry across restarts

	mu      sync.Mutex
	byToken map[string]*session
}

// New builds a Server. publicURL is the externally reachable base URL used to
// construct auth page links; authTTL is how long a box may stay un-authenticated
// before the reaper destroys it. store persists the session registry; pass
// noopStore{} to disable persistence. Call Restore to load any saved sessions.
//
// @arg mgr The box manager the server delegates Docker operations to.
// @arg publicURL The externally reachable base URL for auth page links.
// @arg authTTL How long a box may stay un-authenticated before being reaped.
// @arg store The session store used to persist the registry; noopStore{} disables it.
// @return *Server A ready-to-use Server with an empty in-memory session registry.
//
// @testcase TestCreateBoxRegistersSession builds a Server via New.
func New(mgr boxManager, publicURL string, authTTL time.Duration, store Store) *Server {
	return &Server{
		mgr:       mgr,
		publicURL: strings.TrimRight(publicURL, "/"),
		authTTL:   authTTL,
		store:     store,
		byToken:   make(map[string]*session),
	}
}

// Restore loads persisted sessions into the registry and reconciles them with
// Docker: sessions whose box no longer exists are dropped (and deleted from the
// store) so a stale token can't linger. It returns the number of sessions
// restored. Call it once at startup, before serving.
//
// @arg ctx Context for the Docker list used to reconcile.
// @return int The number of sessions restored into the registry.
// @error error if the store cannot be read or boxes cannot be listed.
//
// @testcase TestRestoreLoadsAndReconciles restores live sessions and drops dead ones.
func (s *Server) Restore(ctx context.Context) (int, error) {
	saved, err := s.store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("loading sessions: %w", err)
	}
	boxes, err := s.mgr.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing boxes to reconcile sessions: %w", err)
	}
	// List returns short (12-char) IDs; a session stores the full box ID.
	isAlive := func(boxID string) bool {
		for _, b := range boxes {
			if strings.HasPrefix(boxID, b.ID) {
				return true
			}
		}
		return false
	}

	s.mu.Lock()
	for _, ps := range saved {
		if !isAlive(ps.BoxID) {
			_ = s.store.Delete(ps.Token)
			continue
		}
		s.byToken[ps.Token] = sessionFromPersisted(ps)
	}
	n := len(s.byToken)
	s.mu.Unlock()
	return n, nil
}

// CreateBox launches a new box and registers an auth session for it. It returns
// the session so callers can build the auth page URL. opts carries the optional
// image, hostname, and description for the box.
//
// @arg ctx Context for the box creation.
// @arg opts The optional image, hostname, and description for the box.
// @return *session The registered auth session for the new box.
// @error error if the box cannot be created or a session token cannot be generated.
//
// @testcase TestCreateBoxRegistersSession checks the session is registered with hostname/description.
// @testcase TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
func (s *Server) CreateBox(ctx context.Context, opts docker.CreateOptions) (*session, error) {
	id, authorizeURL, err := s.mgr.CreateLLMBox(ctx, opts)
	if err != nil {
		return nil, err
	}
	tok, err := newToken()
	if err != nil {
		// Best effort: don't leave the box dangling if we can't track it.
		_ = s.mgr.Destroy(context.Background(), id)
		return nil, fmt.Errorf("generating session token: %w", err)
	}
	sess := &session{
		Token:        tok,
		BoxID:        id,
		AuthorizeURL: authorizeURL,
		CreatedAt:    time.Now(),
		Status:       "pending",
		Hostname:     opts.Hostname,
		Description:  opts.Description,
	}
	s.mu.Lock()
	s.byToken[tok] = sess
	s.mu.Unlock()
	// Best-effort persist: a disk hiccup shouldn't fail an otherwise-good box.
	_ = s.store.Save(sess.persist())
	return sess, nil
}

// AuthPageURL is the URL the user opens to finish authentication.
//
// @arg tok The session token identifying the auth session.
// @return string The absolute auth page URL for the token.
//
// @testcase TestCreateBoxRegistersSession checks the auth page URL format.
func (s *Server) AuthPageURL(tok string) string {
	return s.publicURL + "/auth/" + tok
}

// lookup returns the session for a token, or nil.
//
// @arg tok The session token to look up.
// @return *session The matching session, or nil if none is registered.
//
// @testcase TestCreateBoxRegistersSession checks a created session is found by lookup.
func (s *Server) lookup(tok string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[tok]
}

// lookupByHostname returns the session for a box's hostname (case-insensitive),
// or nil. Hostnames are unique across boxes, so at most one matches.
//
// @arg hostname The box hostname to look up.
// @return *session The matching session, or nil if none has that hostname.
//
// @testcase TestGetByHostname looks a box up by its hostname.
func (s *Server) lookupByHostname(hostname string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byToken {
		if sess.Hostname != "" && strings.EqualFold(sess.Hostname, hostname) {
			return sess
		}
	}
	return nil
}

// SubmitCode feeds the user's OAuth code to the box's login process and waits
// for the box to become ready. It is called by the web handler, never by MCP.
//
// @arg ctx Context for the code submission.
// @arg tok The session token identifying the box.
// @arg code The OAuth code pasted by the user.
// @error error if the session is unknown, the code is empty, or login fails.
//
// @testcase TestSubmitCodeSuccess marks the session ready on success.
// @testcase TestSubmitCodeFailureRecorded records the error on failure.
// @testcase TestSubmitCodeUnknownToken errors for an unknown token.
// @testcase TestSubmitCodeEmpty errors for an empty code.
func (s *Server) SubmitCode(ctx context.Context, tok, code string) error {
	sess := s.lookup(tok)
	if sess == nil {
		return fmt.Errorf("unknown or expired auth session")
	}
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("the code is empty")
	}

	url, err := s.mgr.SubmitCode(ctx, sess.BoxID, code)
	sess.mu.Lock()
	if err != nil {
		sess.Status = "error"
		sess.Err = err.Error()
	} else {
		sess.Status = "ready"
		sess.SessionURL = url
		sess.Err = ""
	}
	ps := sess.persistLocked()
	sess.mu.Unlock()
	// Persist the new status so a restart sees the box as ready (or errored).
	_ = s.store.Save(ps)
	return err
}

// ListBoxes returns all managed boxes.
//
// @arg ctx Context for the list request.
// @return []docker.Box The boxes managed by this server.
// @error error if listing boxes fails.
//
// @testcase TestMCPToolsRegisteredAndCreate exercises the server's box wiring.
func (s *Server) ListBoxes(ctx context.Context) ([]docker.Box, error) {
	return s.mgr.List(ctx)
}

// DestroyBox destroys a box and forgets any session pointing at it.
//
// @arg ctx Context for the destroy request.
// @arg idOrName The ID or name identifying the box to destroy.
// @error error if the box cannot be destroyed.
//
// @testcase TestDestroyForgetsSession checks the session is forgotten after destroy.
func (s *Server) DestroyBox(ctx context.Context, idOrName string) error {
	if err := s.mgr.Destroy(ctx, idOrName); err != nil {
		return err
	}
	s.mu.Lock()
	var dropped []string
	for tok, sess := range s.byToken {
		if strings.HasPrefix(sess.BoxID, idOrName) || strings.HasPrefix(idOrName, sess.BoxID) {
			delete(s.byToken, tok)
			dropped = append(dropped, tok)
		}
	}
	s.mu.Unlock()
	for _, tok := range dropped {
		_ = s.store.Delete(tok)
	}
	return nil
}

// ReapLoop periodically destroys orphaned (never-authenticated) boxes and prunes
// their sessions. It blocks until ctx is cancelled.
//
// @arg ctx Context whose cancellation stops the loop.
// @arg every How often to run a reap pass.
// @arg log Optional sink for reaper log messages; may be nil.
//
// @testcase TestPruneSessionsAfterReap covers the session pruning ReapLoop relies on.
func (s *Server) ReapLoop(ctx context.Context, every time.Duration, log func(string)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reaped, err := s.mgr.ReapOrphans(ctx, s.authTTL)
			if err != nil {
				if log != nil {
					log(fmt.Sprintf("reaper: %v", err))
				}
				continue
			}
			if len(reaped) > 0 {
				s.pruneSessions(reaped)
				if log != nil {
					log(fmt.Sprintf("reaper: destroyed %d orphaned box(es): %s", len(reaped), strings.Join(reaped, ", ")))
				}
			}
		}
	}
}

// pruneSessions drops sessions whose box was reaped.
//
// @arg reapedIDs The short IDs of boxes that were reaped.
//
// @testcase TestPruneSessionsAfterReap checks a reaped box's session is pruned.
func (s *Server) pruneSessions(reapedIDs []string) {
	reaped := make(map[string]bool, len(reapedIDs))
	for _, id := range reapedIDs {
		reaped[id] = true
	}
	s.mu.Lock()
	var dropped []string
	for tok, sess := range s.byToken {
		// reaped IDs are short (12 char); BoxID is the full ID.
		for id := range reaped {
			if strings.HasPrefix(sess.BoxID, id) {
				delete(s.byToken, tok)
				dropped = append(dropped, tok)
				break
			}
		}
	}
	s.mu.Unlock()
	for _, tok := range dropped {
		_ = s.store.Delete(tok)
	}
}

// newToken returns a 256-bit unguessable hex token for an auth page URL.
//
// @return string A 64-character hex-encoded random token.
// @error error if the system random source fails.
//
// @testcase TestCreateBoxRegistersSession checks the generated token length.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
