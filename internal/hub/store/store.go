// Package store persists llmbox's durable state — the auth-session registry, the
// activation login state, and the cluster enrollment records — behind a small set
// of interfaces. SQLite is the only implementation today (see Open), but the
// interfaces are deliberately backend-agnostic so another engine (Postgres, …)
// can be added without touching the server.
package store

import (
	"io"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
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

// ProxyRecord is the on-disk form of an enabled HTTP proxy: a stable, unguessable
// slug (the subdomain label the user's browser requests) bound to a box's port on
// a given spoke. Proxies are default-deny — a request only reaches a box's port
// when a record with a matching slug exists — so this registry is the allowlist.
type ProxyRecord struct {
	// Slug is the unguessable DNS label the proxy is reached at
	// (https://<slug>.<base-domain>/) and the store key.
	Slug string `json:"slug"`
	// BoxID is the box whose port is exposed (the caller-assigned box ID).
	BoxID string `json:"box_id"`
	// ContainerID is the container the proxy was created for. It pins the proxy to
	// one box *generation*: a box destroyed and later recreated with the same box
	// ID gets a different container, so a stale proxy is never silently reused for
	// the new box (it is replaced, and reconciliation drops it).
	ContainerID string `json:"container_id,omitempty"`
	// Port is the TCP port inside the box that requests are forwarded to.
	Port int `json:"port"`
	// Spoke is the cluster spoke the box runs on.
	Spoke string `json:"spoke,omitempty"`
	// CreatedAt is when the proxy was enabled.
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is the identity (email) that enabled the proxy, when known (e.g.
	// an admin acting through the UI); empty for proxies enabled over MCP.
	CreatedBy string `json:"created_by,omitempty"`
	// Description is an optional human-readable note about the proxy, supplied at
	// creation. It is omitted from the on-disk JSON when empty, so records written
	// before this field existed decode with an empty Description (backward compatible).
	Description string `json:"description,omitempty"`
}

// ProxyStore persists the enabled-proxy registry across restarts, keyed by the
// proxy's slug. It is the store's own concern (separate from sessions, logins,
// and cluster enrollment) so a backend can implement and test it in isolation.
// All methods must be safe for concurrent use.
type ProxyStore interface {
	// SaveProxy writes (creating or replacing) one proxy keyed by its slug.
	SaveProxy(rec ProxyRecord) error
	// GetProxy returns the proxy for a slug; the bool is false when none matches.
	GetProxy(slug string) (ProxyRecord, bool, error)
	// ListProxies returns every enabled proxy.
	ListProxies() ([]ProxyRecord, error)
	// DeleteProxy removes the proxy for a slug; deleting a missing slug is a no-op.
	DeleteProxy(slug string) error
}

// APIKeyRecord is the on-disk form of one API key: its human-readable name and
// its validity window. The key's secret is never stored — only its SHA-256 hash,
// which is the store key — so a leaked database cannot be replayed as keys.
type APIKeyRecord struct {
	// Name is the operator-chosen label identifying what the key is for
	// (e.g. "ci", "mcp-prod").
	Name string `json:"name"`
	// CreatedAt is when the key was minted.
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the key stops authenticating; keys always expire.
	ExpiresAt time.Time `json:"expires_at"`
}

// APIKeyInfo describes one stored API key for listing/revocation. ID is the
// key's secret hash (an opaque handle the operator can delete by); the secret
// itself is never recoverable.
type APIKeyInfo struct {
	ID        string
	Name      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// APIKeyStore persists the API keys that authenticate box-control API callers.
// Keys are keyed by the SHA-256 hash of their secret, so the store never holds
// a usable credential. All methods must be safe for concurrent use.
type APIKeyStore interface {
	// PutAPIKey writes (creating or replacing) one API key keyed by its secret hash.
	PutAPIKey(hash string, rec APIKeyRecord) error
	// GetAPIKey returns the API key for a secret hash; the bool is false when none matches.
	GetAPIKey(hash string) (APIKeyRecord, bool, error)
	// ListAPIKeys returns every stored API key.
	ListAPIKeys() ([]APIKeyInfo, error)
	// DeleteAPIKey removes the API key for a hash; deleting a missing hash is a no-op.
	DeleteAPIKey(hash string) error
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

// SettingsStore persists small hub-wide settings as opaque key/value strings
// (e.g. the name of the default spoke an admin picked in the UI). It is a
// deliberately generic key/value contract — one row per setting — so operator
// choices that belong in the database rather than the config file can be added
// without a schema change. All methods must be safe for concurrent use.
type SettingsStore interface {
	// PutSetting writes (creating or replacing) the value for key.
	PutSetting(key, value string) error
	// GetSetting returns the value for key; the bool is false when key is unset.
	GetSetting(key string) (string, bool, error)
}

// Store is the aggregate persistence contract the server depends on: the session
// registry, the activation login state, the cluster enrollment records, API
// keys, and hub-wide settings, plus a Close that releases the backend. All
// methods must be safe for concurrent use. Use Open for a SQLite-backed
// implementation.
type Store interface {
	SessionStore
	LoginStore
	ProxyStore
	SettingsStore
	APIKeyStore
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
