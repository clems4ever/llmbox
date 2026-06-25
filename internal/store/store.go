// Package store persists llmbox's durable state — the auth-session registry, the
// activation login state, and the cluster enrollment records — behind a small set
// of interfaces. bbolt is the only implementation today (see Open), but the
// interfaces are deliberately backend-agnostic so another engine (SQLite,
// Postgres, …) can be added without touching the server.
package store

import (
	"io"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
)

// LoginSession is a completed activation login, keyed in the store by an opaque
// random session ID (the value of the browser cookie). Its presence means the
// user authenticated and was authorized; CSRF guards the activation POST.
type LoginSession struct {
	Email     string    `json:"email"`
	Provider  string    `json:"provider"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`

	// Activate reports whether this identity may activate boxes (i.e. it passed
	// the provider's box-activation allow rule). Admin reports whether it may use
	// the admin UI. The two capabilities are independent and both decided once at
	// sign-in, so each surface can enforce its own gate from the stored session.
	Activate bool `json:"activate"`
	Admin    bool `json:"admin"`
}

// LoginFlow is the short-lived state of an in-flight OIDC handshake, keyed in the
// store by the OAuth state parameter. It is consumed (deleted) on callback.
type LoginFlow struct {
	Provider    string    `json:"provider"`
	ReturnToken string    `json:"return_token"`
	ReturnTo    string    `json:"return_to"`
	Nonce       string    `json:"nonce"`
	Verifier    string    `json:"verifier"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// PersistedSession is the on-disk form of a box's auth session. It mirrors the
// durable fields of the server's live session so the registry survives a restart.
type PersistedSession struct {
	Token        string            `json:"token"`
	ContainerID  string            `json:"container_id"`
	AuthorizeURL string            `json:"authorize_url"`
	CreatedAt    time.Time         `json:"created_at"`
	HookState    map[string]string `json:"hook_state,omitempty"`
	BoxID        string            `json:"box_id,omitempty"`
	Description  string            `json:"description,omitempty"`
	SpokeName    string            `json:"spoke_name,omitempty"`
	Status       string            `json:"status"`
	SessionURL   string            `json:"session_url,omitempty"`
	Err          string            `json:"err,omitempty"`
	ActivatedBy  string            `json:"activated_by,omitempty"`
}

// SessionStore persists the box auth-session registry across restarts. It is the
// store's own concern (logins and cluster enrollment are separate contracts), so
// a backend can implement and test it in isolation. All methods must be safe for
// concurrent use.
type SessionStore interface {
	// Save writes (creating or replacing) one session keyed by its token.
	Save(ps PersistedSession) error
	// Delete removes the session for a token; deleting a missing token is a no-op.
	Delete(token string) error
	// LoadAll returns every persisted session.
	LoadAll() ([]PersistedSession, error)
}

// Store is the aggregate persistence contract the server depends on: the session
// registry, the activation login state, and the cluster enrollment records, plus
// a Close that releases the backend. All methods must be safe for concurrent use.
// Use Open for a bbolt-backed implementation.
type Store interface {
	SessionStore
	LoginStore
	cluster.Store
	io.Closer
}

// LoginStore persists the activation login state across restarts: completed
// login sessions (keyed by an opaque cookie ID) and the short-lived in-flight
// OIDC handshake state (keyed by the OAuth state parameter). All methods must be
// safe for concurrent use.
type LoginStore interface {
	// SaveLoginFlow stores the in-flight handshake state under the OAuth state key.
	SaveLoginFlow(state string, f LoginFlow) error
	// TakeLoginFlow returns and removes the flow for state (one-time use); the bool
	// is false when no flow matches.
	TakeLoginFlow(state string) (LoginFlow, bool, error)
	// SaveLoginSession stores a completed login session under its opaque id.
	SaveLoginSession(id string, s LoginSession) error
	// LoginSession returns the session for id; the bool is false when none matches.
	LoginSession(id string) (LoginSession, bool, error)
	// DeleteLoginSession removes a login session; deleting a missing id is a no-op.
	DeleteLoginSession(id string) error
	// PurgeExpiredLogins drops login sessions and flows that expired before now.
	PurgeExpiredLogins(now time.Time) error
}
