// Package store persists llmbox's durable state — the box registry, the admin
// sign-in (identity) state, the cluster enrollment records, API keys, and hub-wide
// settings — behind a small set of interfaces. SQLite is the only implementation
// today (see Open), but the interfaces are deliberately backend-agnostic so another
// engine (Postgres, …) can be added without touching the server.
package store

import (
	"io"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// Lifecycle is a box's authoritative runtime state as the hub records it. It is
// the hub's own view, distinct from the backend's observed state (see
// Box.ObservedState): a box is either believed to exist (LifecycleRunning) or
// confirmed gone from its spoke (LifecycleTerminated — a tombstone kept so the UI
// can show what happened until the record is removed). "Unreachable" is
// deliberately NOT a Lifecycle value: it is a live property (is the box's spoke
// connected right now?) computed at read time, so it can never go stale.
type Lifecycle string

const (
	// LifecycleRunning marks a box believed to exist on its spoke.
	LifecycleRunning Lifecycle = "running"
	// LifecycleTerminated marks a box confirmed absent from its (reachable) spoke:
	// it exited or was removed out of band. The record is kept as a tombstone.
	LifecycleTerminated Lifecycle = "terminated"
)

// Box is the persisted form of one box: its stable identity, where it runs, its
// hub-recorded lifecycle, and the backend facts last observed for it. It is keyed
// in the store by its Token (the box's bearer credential to the hub), because a
// box ID is unique only per spoke while the token is globally unique. Fields
// grouped by concern:
//
//   - identity/placement: Token, InstanceID, BoxID, Spoke, Description.
//   - provisioning: Status, LastError, HookState.
//   - hub lifecycle: Lifecycle, CreatedAt.
//   - last-observed backend facts (Observed*): what the sync pass last saw on the
//     spoke, stored so the record renders in full while its spoke is offline.
type Box struct {
	// Token is the box's bearer credential to the hub and the store key.
	Token string `json:"token"`
	// InstanceID is the box's opaque backend generation token (its current
	// incarnation) — spoke-minted, never a native container/VM handle. A box
	// destroyed and recreated with the same BoxID gets a new InstanceID, so it pins
	// a record to one generation. It is compared only by equality, never parsed.
	InstanceID string `json:"instance_id"`
	// BoxID is the caller-assigned logical id, unique per spoke.
	BoxID string `json:"box_id,omitempty"`
	// Spoke is the cluster spoke the box runs on.
	Spoke string `json:"spoke,omitempty"`
	// Description is the caller-supplied human note.
	Description string `json:"description,omitempty"`

	// Status is the box's provisioning phase: "broken" when its init script failed
	// during creation, "ready" otherwise.
	Status string `json:"status"`
	// LastError is the init script's captured output, set when Status is "broken".
	LastError string `json:"last_error,omitempty"`
	// HookState is the opaque per-hook state returned by the box.create hooks,
	// replayed to box.destroy. It is the one field without a natural columnar
	// shape, so it is held as JSON text.
	HookState map[string]string `json:"hook_state,omitempty"`

	// Lifecycle is the hub's authoritative runtime state for the box.
	Lifecycle Lifecycle `json:"lifecycle,omitempty"`
	// CreatedAt is when the box was created.
	CreatedAt time.Time `json:"created_at"`

	// ObservedName, ObservedImage, and ObservedState mirror the backend metadata
	// (instance name, image/rootfs, and backend state such as "running"/"exited")
	// as last seen by the sync pass. ObservedAt is when that observation was made
	// (zero when the box has never been observed).
	ObservedName  string    `json:"observed_name,omitempty"`
	ObservedImage string    `json:"observed_image,omitempty"`
	ObservedState string    `json:"observed_state,omitempty"`
	ObservedAt    time.Time `json:"observed_at,omitempty"`
}

// IdentitySession is a completed sign-in, keyed in the store by an opaque random
// session ID (the value of the browser cookie). Its presence means the user
// authenticated and was authorized; the CSRF token guards state-changing POSTs.
type IdentitySession struct {
	Email     string    `json:"email"`
	Provider  string    `json:"provider"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`

	// CanAdmin reports whether this identity may use the admin UI and reach the
	// per-box HTTP proxies. It is decided once at sign-in (from the admin
	// allow-list) and enforced from the stored session.
	CanAdmin bool `json:"can_admin"`
}

// OIDCFlow is the short-lived state of an in-flight OIDC handshake, keyed in the
// store by the OAuth state parameter. It is consumed (deleted) on callback.
type OIDCFlow struct {
	Provider     string    `json:"provider"`
	ReturnTo     string    `json:"return_to"`
	Nonce        string    `json:"nonce"`
	PKCEVerifier string    `json:"pkce_verifier"`
	ExpiresAt    time.Time `json:"expires_at"`
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
	// InstanceID is the backend generation the proxy was created for. It pins the
	// proxy to one box generation: a box destroyed and later recreated with the
	// same box ID gets a different instance, so a stale proxy is never silently
	// reused for the new box (it is replaced, and reconciliation drops it).
	InstanceID string `json:"instance_id,omitempty"`
	// Port is the TCP port inside the box that requests are forwarded to.
	Port int `json:"port"`
	// Spoke is the cluster spoke the box runs on.
	Spoke string `json:"spoke,omitempty"`
	// CreatedAt is when the proxy was enabled.
	CreatedAt time.Time `json:"created_at"`
	// Owner is the identity (email) that enabled the proxy, when known (e.g. an
	// admin acting through the UI); empty for proxies enabled over the API.
	Owner string `json:"owner,omitempty"`
	// Description is an optional human-readable note about the proxy.
	Description string `json:"description,omitempty"`
}

// APIKeyRecord is the on-disk form of one API key: its human-readable name and
// its validity window. The key's secret is never stored — only its SHA-256 hash,
// which is the store key — so a leaked database cannot be replayed as keys.
type APIKeyRecord struct {
	// Name is the operator-chosen label identifying what the key is for
	// (e.g. "ci", "prod").
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

// BoxStore persists the box registry across restarts. It is the store's own
// concern (identities, cluster enrollment, and proxies are separate contracts),
// so a backend can implement and test it in isolation. All methods must be safe
// for concurrent use.
type BoxStore interface {
	// PutBox writes (creating or replacing) one box keyed by its token.
	PutBox(b Box) error
	// DeleteBox removes the box for a token; deleting a missing token is a no-op.
	DeleteBox(token string) error
	// ListBoxes returns every persisted box.
	ListBoxes() ([]Box, error)
}

// IdentityStore persists the sign-in state across restarts: completed identity
// sessions (keyed by an opaque cookie ID) and the short-lived in-flight OIDC
// handshake state (keyed by the OAuth state parameter). All methods must be safe
// for concurrent use.
type IdentityStore interface {
	// PutOIDCFlow stores the in-flight handshake state under the OAuth state key.
	PutOIDCFlow(state string, f OIDCFlow) error
	// TakeOIDCFlow returns and removes the flow for state (one-time use); the bool
	// is false when no flow matches.
	TakeOIDCFlow(state string) (OIDCFlow, bool, error)
	// PutIdentitySession stores a completed identity session under its opaque id.
	PutIdentitySession(id string, s IdentitySession) error
	// GetIdentitySession returns the session for id; the bool is false when none matches.
	GetIdentitySession(id string) (IdentitySession, bool, error)
	// DeleteIdentitySession removes an identity session; deleting a missing id is a no-op.
	DeleteIdentitySession(id string) error
	// PurgeExpiredIdentities drops identity sessions and flows that expired before now.
	PurgeExpiredIdentities(now time.Time) error
}

// ProxyStore persists the enabled-proxy registry across restarts, keyed by the
// proxy's slug. All methods must be safe for concurrent use.
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

// AllowlistGroup is a named set of egress domains a box may reach, plus how long
// each DNS-resolved IP stays pinned in the box's firewall after a lookup
// (TTLSeconds). IsGlobal marks a group applied to every box on an
// isolation-enabled runner; non-global groups apply only to the boxes they are
// assigned to. Domains support exact hosts and leading-wildcard patterns
// (e.g. "*.github.com"). The group is the unit the UI creates, assigns, and
// import/exports.
type AllowlistGroup struct {
	// ID is the group's stable slug identity (kebab-case of the name at creation).
	ID string `json:"id"`
	// Name is the human-facing unique label.
	Name string `json:"name"`
	// Description is the operator's note about what the group is for.
	Description string `json:"description"`
	// TTLSeconds is how long a DNS-resolved IP stays open after a lookup before it
	// must be re-resolved; it bounds the window an IP reallocated to a rogue
	// service could be reached. Zero means the store default is used at read time.
	TTLSeconds int `json:"ttl_seconds"`
	// IsGlobal applies the group to every box on an isolation-enabled runner.
	IsGlobal bool `json:"is_global"`
	// Domains are the exact/wildcard hosts the group permits. Always sorted.
	Domains []string `json:"domains"`
	// CreatedAt and UpdatedAt track the group's lifecycle for the UI.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AllowlistStore persists the network-isolation allowlist configuration: the
// named domain groups, and which non-global groups are attached to which box.
// It is hub-local control-plane state (no secrets); a spoke reads the computed
// effective allowlist over the cluster transport, never this store directly. All
// methods must be safe for concurrent use.
type AllowlistStore interface {
	// SaveAllowlistGroup writes (creating or replacing) one group and its domains,
	// keyed by ID. It replaces the group's domain set wholesale.
	SaveAllowlistGroup(g AllowlistGroup) error
	// GetAllowlistGroup returns the group for an ID; the bool is false on a miss.
	GetAllowlistGroup(id string) (AllowlistGroup, bool, error)
	// ListAllowlistGroups returns every group with its domains, ordered by name.
	ListAllowlistGroups() ([]AllowlistGroup, error)
	// DeleteAllowlistGroup removes a group, its domains, and every box assignment
	// referencing it; deleting a missing group is a no-op.
	DeleteAllowlistGroup(id string) error
	// SetBoxGroups replaces the set of non-global groups assigned to boxID.
	// Passing an empty slice clears the box's assignments.
	SetBoxGroups(boxID string, groupIDs []string) error
	// GetBoxGroups returns the non-global group IDs assigned to boxID, sorted.
	GetBoxGroups(boxID string) ([]string, error)
	// ListBoxGroups returns every box-to-groups assignment, keyed by box ID.
	ListBoxGroups() (map[string][]string, error)
}

// DNSAuditEntry is one aggregated row of the DNS audit trail: a (box, domain,
// verdict) triple with how many times it was seen and when. The hub aggregates
// per triple rather than storing every lookup, so a chatty box does not grow the
// table without bound while the UI can still show counts and recency.
type DNSAuditEntry struct {
	BoxID     string    `json:"box_id"`
	Domain    string    `json:"domain"`
	Verdict   string    `json:"verdict"`
	Hits      int64     `json:"hits"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// DNSAuditFilter narrows a DNS audit query. A zero field is "any"; Limit 0 uses
// the store's default cap.
type DNSAuditFilter struct {
	BoxID   string
	Verdict string
	Domain  string
	Limit   int
}

// DNSAuditStore persists the DNS lookups boxes make under network isolation, for
// the audit view. All methods must be safe for concurrent use.
type DNSAuditStore interface {
	// RecordDNSLookup folds one lookup into the aggregate: it inserts a new
	// (box, domain, verdict) row or bumps an existing row's hit count and last-seen.
	RecordDNSLookup(boxID, domain, verdict string, at time.Time) error
	// ListDNSAudit returns audit rows matching filter, most-recent first.
	ListDNSAudit(filter DNSAuditFilter) ([]DNSAuditEntry, error)
	// DeleteDNSAuditForBox drops a box's audit rows (called when it is destroyed).
	DeleteDNSAuditForBox(boxID string) error
}

// Store is the aggregate persistence contract the server depends on: the box
// registry, the sign-in (identity) state, the cluster enrollment records, API
// keys, and hub-wide settings, plus a Close that releases the backend. All
// methods must be safe for concurrent use. Use Open for a SQLite-backed
// implementation.
type Store interface {
	BoxStore
	IdentityStore
	ProxyStore
	SettingsStore
	APIKeyStore
	AllowlistStore
	DNSAuditStore
	cluster.Store
	io.Closer
}
