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
	CreateLLMBox(ctx context.Context, image string) (id, authorizeURL string, err error)
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

	mu         sync.Mutex
	Status     string // "pending" | "ready" | "error"
	SessionURL string
	Err        string
}

func (s *session) snapshot() (status, sessionURL, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status, s.SessionURL, s.Err
}

// Server orchestrates boxes and owns the session registry.
type Server struct {
	mgr       boxManager
	publicURL string // external base URL, e.g. https://boxes.example.com
	authTTL   time.Duration

	mu      sync.Mutex
	byToken map[string]*session
}

// New builds a Server. publicURL is the externally reachable base URL used to
// construct auth page links; authTTL is how long a box may stay un-authenticated
// before the reaper destroys it.
func New(mgr boxManager, publicURL string, authTTL time.Duration) *Server {
	return &Server{
		mgr:       mgr,
		publicURL: strings.TrimRight(publicURL, "/"),
		authTTL:   authTTL,
		byToken:   make(map[string]*session),
	}
}

// CreateBox launches a new box and registers an auth session for it. It returns
// the session so callers can build the auth page URL.
func (s *Server) CreateBox(ctx context.Context, image string) (*session, error) {
	id, authorizeURL, err := s.mgr.CreateLLMBox(ctx, image)
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
	}
	s.mu.Lock()
	s.byToken[tok] = sess
	s.mu.Unlock()
	return sess, nil
}

// AuthPageURL is the URL the user opens to finish authentication.
func (s *Server) AuthPageURL(tok string) string {
	return s.publicURL + "/auth/" + tok
}

// lookup returns the session for a token, or nil.
func (s *Server) lookup(tok string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[tok]
}

// SubmitCode feeds the user's OAuth code to the box's login process and waits
// for the box to become ready. It is called by the web handler, never by MCP.
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
	defer sess.mu.Unlock()
	if err != nil {
		sess.Status = "error"
		sess.Err = err.Error()
		return err
	}
	sess.Status = "ready"
	sess.SessionURL = url
	sess.Err = ""
	return nil
}

// ListBoxes returns all managed boxes.
func (s *Server) ListBoxes(ctx context.Context) ([]docker.Box, error) {
	return s.mgr.List(ctx)
}

// DestroyBox destroys a box and forgets any session pointing at it.
func (s *Server) DestroyBox(ctx context.Context, idOrName string) error {
	if err := s.mgr.Destroy(ctx, idOrName); err != nil {
		return err
	}
	s.mu.Lock()
	for tok, sess := range s.byToken {
		if strings.HasPrefix(sess.BoxID, idOrName) || strings.HasPrefix(idOrName, sess.BoxID) {
			delete(s.byToken, tok)
		}
	}
	s.mu.Unlock()
	return nil
}

// ReapLoop periodically destroys orphaned (never-authenticated) boxes and prunes
// their sessions. It blocks until ctx is cancelled.
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
func (s *Server) pruneSessions(reapedIDs []string) {
	reaped := make(map[string]bool, len(reapedIDs))
	for _, id := range reapedIDs {
		reaped[id] = true
	}
	s.mu.Lock()
	for tok, sess := range s.byToken {
		// reaped IDs are short (12 char); BoxID is the full ID.
		for id := range reaped {
			if strings.HasPrefix(sess.BoxID, id) {
				delete(s.byToken, tok)
				break
			}
		}
	}
	s.mu.Unlock()
}

// newToken returns a 256-bit unguessable hex token for an auth page URL.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
