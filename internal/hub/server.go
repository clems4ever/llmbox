// Package server ties the Docker box manager to the HTTP front-ends that share
// one process and one port:
//
//   - the box-control JSON API (under /api/v1/), used by the UI and by callers
//     like the llmbox-mcp binary to create/list/destroy boxes. It only ever
//     exchanges box IDs and the *auth page URL* — never the OAuth secret.
//   - the auth web page where the user pastes their OAuth code. The code goes
//     browser -> this server -> container stdin, so it never enters the caller's
//     context.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/hooks"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/docker"
)

// defaultSpokeSettingKey is the settings-store key under which the admin-chosen
// default spoke is persisted. A box created with no explicit spoke runs on that
// spoke; until an admin picks one, an unqualified create is refused.
const defaultSpokeSettingKey = "default_spoke"

// boxManager is the behaviour Server needs from a spoke's box layer. A spoke is
// reached over the cluster transport (the concrete type is cluster's remoteSpoke).
// Tests fake it. It is an alias of cluster.BoxManager so the same interface is the
// cluster RPC surface.
type boxManager = cluster.BoxManager

// clusterHub is what Server needs from the cluster hub: resolving connected
// remote spokes by name (for routing) and the HTTP handler spokes connect to.
// The real implementation is *cluster.Hub; tests fake it. The hub is always
// present in a running server — every box runs on a remote spoke.
type clusterHub interface {
	Spoke(name string) (boxManager, bool)
	Spokes() map[string]boxManager
	ConnectHandler(w http.ResponseWriter, r *http.Request)
	// Disconnect force-closes a named spoke's live connection (used when an admin
	// drops a spoke); a no-op when the spoke is not connected.
	Disconnect(name string)
}

// boxHooks is the behaviour Server needs from the hooks layer (real impl is
// *hooks.Runner; tests fake it). A nil boxHooks disables the integration. The
// hooks run external programs at box lifecycle events: box.create may return
// files to inject and opaque per-hook state, which box.destroy is replayed to
// undo what it did.
type boxHooks interface {
	// OnCreate runs the box.create hooks, returning files to inject and the
	// per-hook state to persist with the box.
	OnCreate(ctx context.Context, box hooks.BoxInfo) ([]hooks.File, map[string]string, error)
	// OnDestroy runs the box.destroy hooks, replaying the per-hook state.
	OnDestroy(ctx context.Context, box hooks.BoxInfo, state map[string]string) error
}

// session tracks one box through the auth handshake.
type session struct {
	Token        string
	ContainerID  string
	AuthorizeURL string
	CreatedAt    time.Time

	// HookState is the opaque per-hook state returned by the box.create hooks
	// (nil when no hooks are configured), keyed by hook executable. It is replayed
	// to the box.destroy hooks when the box is destroyed or reaped — e.g. a
	// granular hook stores its minted subject token here so it can revoke it. Set
	// at creation and immutable, so it needs no locking.
	HookState map[string]string

	// BoxID and Description are caller-supplied at creation and immutable,
	// so they need no locking.
	BoxID       string
	Description string

	// SpokeName is the cluster spoke the box runs on (resolved at creation from the
	// request, or the admin-chosen default spoke). Set at creation and immutable, so
	// it needs no locking; per-box verbs route to this spoke.
	SpokeName string

	mu          sync.Mutex
	Status      string // "pending" | "ready" | "error"
	SessionURL  string
	Err         string
	ActivatedBy string // identity (email) that submitted the code, when auth is enabled

	// BoxState is the box's persisted runtime state (boxStateRunning or
	// boxStateTerminated; "" from an older record means running). LastSeen is
	// when the box was last observed on its spoke; Name, Image, and
	// InstanceState mirror the backend metadata observed then, so the record
	// renders in full while its spoke is offline. All are updated by the sync
	// pass and guarded by mu.
	BoxState      string
	LastSeen      time.Time
	Name          string
	Image         string
	InstanceState string
}

// terminated reports whether the box behind this session has been confirmed
// gone from its spoke (the record is a tombstone).
//
// @return bool True when the session's box state is terminated.
//
// @testcase TestDestroyTerminatedRecordSkipsSpoke clears a tombstone without a spoke round-trip.
func (s *session) terminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BoxState == boxStateTerminated
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
		Token:         s.Token,
		ContainerID:   s.ContainerID,
		AuthorizeURL:  s.AuthorizeURL,
		CreatedAt:     s.CreatedAt,
		HookState:     s.HookState,
		BoxID:         s.BoxID,
		Description:   s.Description,
		SpokeName:     s.SpokeName,
		Status:        s.Status,
		SessionURL:    s.SessionURL,
		Err:           s.Err,
		ActivatedBy:   s.ActivatedBy,
		BoxState:      s.BoxState,
		LastSeen:      s.LastSeen,
		Name:          s.Name,
		Image:         s.Image,
		InstanceState: s.InstanceState,
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
		Token:         ps.Token,
		ContainerID:   ps.ContainerID,
		AuthorizeURL:  ps.AuthorizeURL,
		CreatedAt:     ps.CreatedAt,
		HookState:     ps.HookState,
		BoxID:         ps.BoxID,
		Description:   ps.Description,
		SpokeName:     ps.SpokeName,
		Status:        ps.Status,
		SessionURL:    ps.SessionURL,
		Err:           ps.Err,
		ActivatedBy:   ps.ActivatedBy,
		BoxState:      ps.BoxState,
		LastSeen:      ps.LastSeen,
		Name:          ps.Name,
		Image:         ps.Image,
		InstanceState: ps.InstanceState,
	}
}

// Server orchestrates boxes and owns the session registry.
type Server struct {
	hooks     boxHooks // runs lifecycle hooks per box; nil when none configured
	publicURL string   // external base URL, e.g. https://boxes.example.com
	authTTL   time.Duration
	store     Store // persists the registry across restarts

	mu      sync.Mutex
	byToken map[string]*session

	// pendingBoxIDs holds box IDs claimed by in-flight createBox calls (lowercased)
	// that have not yet been registered in byToken. Together with byToken — which
	// holds every already-registered box's ID across all spokes — it enforces
	// hub-wide box-ID uniqueness: two concurrent creates, or a create racing a box
	// still being registered, cannot both take the same ID. Guarded by mu.
	pendingBoxIDs map[string]struct{}

	// hub holds the connected remote spokes; every box runs on one of them. Set
	// once at startup via SetHub before serving (always set in a running server).
	hub clusterHub

	// auth gates box activation behind provider sign-in (Google, …). nil leaves
	// activation unauthenticated (no provider configured).
	auth *auth.Authenticator

	// proxyBaseDomain is the parent domain per-box HTTP proxies hang off (e.g.
	// "proxy.example.com"); a proxy is reached at <slug>.<proxyBaseDomain>. Empty
	// disables the proxy feature entirely. Set once at startup via SetProxyBaseDomain.
	proxyBaseDomain string

	// log records best-effort failures (persistence, cleanup, destroy hooks) that
	// are not propagated to the caller; nil falls back to slog.Default().
	log *slog.Logger
}

// logger returns the Server's logger, or slog.Default() when none was set.
//
// @return *slog.Logger The configured logger, or the slog default.
//
// @testcase TestCreateBoxRegistersSession exercises a Server whose logger defaults.
func (s *Server) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// New builds a Server. publicURL is the externally reachable base URL used to
// construct auth page links; authTTL is how long a box may stay un-authenticated
// before the reaper destroys it. store persists the session registry; pass a
// no-op store to disable persistence. hooks runs lifecycle hooks per box; pass
// nil to disable hook integration. auth gates box activation behind provider
// sign-in; pass nil to leave activation unauthenticated. The server routes every
// box to a remote spoke, so attach the cluster hub via SetHub before serving. Call
// Restore to load any saved sessions.
//
// @arg hooks The box lifecycle hook runner; nil disables hook integration.
// @arg publicURL The externally reachable base URL for auth page links.
// @arg authTTL How long a box may stay un-authenticated before being reaped.
// @arg store The session store used to persist the registry; a no-op store disables it.
// @arg auth The activation authenticator; nil leaves activation unauthenticated.
// @return *Server A ready-to-use Server with an empty in-memory session registry.
//
// @testcase TestCreateBoxRegistersSession builds a Server via New.
// @testcase TestCreateBoxRunsCreateHooks builds a Server with a hook runner.
func New(hooks boxHooks, publicURL string, authTTL time.Duration, store Store, auth *auth.Authenticator) *Server {
	s := &Server{
		hooks:         hooks,
		publicURL:     strings.TrimRight(publicURL, "/"),
		authTTL:       authTTL,
		store:         store,
		auth:          auth,
		byToken:       make(map[string]*session),
		pendingBoxIDs: make(map[string]struct{}),
		log:           slog.Default(),
	}
	// The server owns the canonical store; bind it into the authenticator so its
	// OIDC handlers and CurrentLogin persist to (and read) the same login state.
	auth.Bind(store, s.log)
	return s
}

// SetHub attaches the cluster hub holding connected remote spokes. Call it once
// at startup, before serving. Every box runs on a remote spoke resolved through
// the hub, so a running server always has one.
//
// @arg hub The cluster hub resolving remote spokes by name.
//
// @testcase TestCreateBoxRoutesToSpoke routes a box to a remote spoke via the hub.
func (s *Server) SetHub(hub clusterHub) { s.hub = hub }

// errNoDefaultSpoke is returned when a box is created without naming a spoke and
// no default spoke has been chosen by an admin yet.
var errNoDefaultSpoke = errors.New("no default spoke configured; set one in the admin UI or name a spoke when creating the box")

// DefaultSpoke returns the admin-chosen default spoke a box with no explicit spoke
// runs on, or "" when none has been set.
//
// @return string The default spoke name, or "" when unset.
// @error error if the setting cannot be read from the store.
//
// @testcase TestDefaultSpokeRoundTrip reads back a default spoke set via SetDefaultSpoke.
func (s *Server) DefaultSpoke() (string, error) {
	name, _, err := s.store.GetSetting(defaultSpokeSettingKey)
	if err != nil {
		return "", err
	}
	return name, nil
}

// SetDefaultSpoke persists the default spoke an unqualified box create routes to.
// An empty name clears the default (unqualified creates then error).
//
// @arg name The spoke name to make the default, or "" to clear it.
// @error error if the setting cannot be written to the store.
//
// @testcase TestDefaultSpokeRoundTrip persists and clears the default spoke.
func (s *Server) SetDefaultSpoke(name string) error {
	return s.store.PutSetting(defaultSpokeSettingKey, name)
}

// createSpoke mints a one-time join token that enrolls a new spoke under name,
// valid for ttl. It is the spoke-creation operation admin (and any future
// authorized caller) share, sitting alongside createBox/createProxy as the
// server's single home for the operation; the copy-pasteable run command is
// presentation the caller builds from the returned token.
//
// @arg name The spoke name to enroll; must be non-empty.
// @arg ttl How long the minted join token stays valid.
// @return string The one-time join token.
// @error error if the name is empty or the token cannot be minted.
//
// @testcase TestBackendCreateSpoke mints a token for a named spoke.
func (s *Server) createSpoke(name string, ttl time.Duration) (string, error) {
	if name == "" {
		return "", errors.New("spoke name is required")
	}
	return cluster.CreateJoinToken(s.store, name, ttl, time.Now())
}

// dropSpoke removes a spoke entirely: it deletes the enrollment record, revokes
// every outstanding join token for that name so it cannot re-enroll, force-closes
// its live hub connection, and clears the default spoke if the dropped one was it
// (so an unqualified create fails loudly rather than targeting a spoke that no
// longer exists). Once the record is gone the cleanup steps are best-effort —
// logged, not fatal.
//
// @arg name The spoke name to drop; must be non-empty.
// @error error if the name is empty or the enrollment record cannot be deleted.
//
// @testcase TestDropSpokeRemovesAndKicks deletes the record, revokes its tokens, and disconnects the live link.
// @testcase TestDropDefaultSpokeClearsDefault clears the default when the dropped spoke was it.
func (s *Server) dropSpoke(name string) error {
	if name == "" {
		return errors.New("spoke name is required")
	}
	if err := s.store.DeleteSpoke(name); err != nil {
		return err
	}
	// Revoke any outstanding join tokens for this spoke so it can't re-enroll.
	if tokens, err := s.store.ListJoinTokens(); err == nil {
		for _, t := range tokens {
			if t.Name == name {
				if derr := s.store.DeleteJoinToken(t.ID); derr != nil {
					s.logger().Warn("dropSpoke: deleting join token", "spoke", name, "err", derr)
				}
			}
		}
	}
	if s.hub != nil {
		s.hub.Disconnect(name)
	}
	// A box created with no spoke routes to the default spoke; if the one just
	// dropped was the default, clear it.
	if def, err := s.DefaultSpoke(); err == nil && def == name {
		if cerr := s.SetDefaultSpoke(""); cerr != nil {
			s.logger().Warn("dropSpoke: clearing default spoke after drop", "spoke", name, "err", cerr)
		}
	}
	return nil
}

// chooseDefaultSpoke makes an enrolled spoke the default that unqualified box
// creates run on. It rejects a spoke that is not currently enrolled so a typo
// can't silently disable unqualified box creation.
//
// @arg name The spoke to make the default; must be non-empty and enrolled.
// @error error if the name is empty, the enrolled spokes cannot be read, the spoke is not enrolled, or the setting cannot be written.
//
// @testcase TestBackendSetDefaultSpoke persists the chosen default spoke and rejects an unenrolled one.
func (s *Server) chooseDefaultSpoke(name string) error {
	if name == "" {
		return errors.New("spoke name is required")
	}
	enrolled, err := s.store.ListSpokes()
	if err != nil {
		return fmt.Errorf("listing spokes: %w", err)
	}
	for _, rec := range enrolled {
		if rec.Name == name {
			return s.SetDefaultSpoke(name)
		}
	}
	return fmt.Errorf("spoke %s is not enrolled", name)
}

// revokeJoinToken deletes a single outstanding join token by its opaque ID so it
// can no longer be used to enroll a spoke.
//
// @arg id The token ID (its hash handle); must be non-empty.
// @error error if the id is empty or the token cannot be deleted.
//
// @testcase TestBackendJoinTokens deletes the token by ID.
func (s *Server) revokeJoinToken(id string) error {
	if id == "" {
		return errors.New("token id is required")
	}
	return s.store.DeleteJoinToken(id)
}

// resolveStoredSpoke maps a persisted spoke name to the spoke it belongs to now,
// resolving an empty name (a legacy pre-cluster session or proxy) to the current
// default spoke. When no default is set it stays empty, which the callers treat as
// an unknown/departed spoke.
//
// @arg name The stored spoke name, possibly empty.
// @return string The name, or the default spoke when empty.
//
// @testcase TestRestoreKeepsDisconnectedSpokeSessions keeps a session on an offline spoke.
func (s *Server) resolveStoredSpoke(name string) string {
	if name != "" {
		return name
	}
	def, err := s.DefaultSpoke()
	if err != nil {
		s.logger().Warn("resolving default spoke", "err", err)
		return ""
	}
	return def
}

// SetProxyBaseDomain sets the parent domain per-box HTTP proxies are served
// under (e.g. "proxy.example.com"), enabling the proxy feature. An empty domain
// leaves it disabled. Call it once at startup before serving.
//
// @arg domain The proxy parent domain; empty disables proxying.
//
// @testcase TestCreateProxyRegistersAndBuildsURL enables proxying via this setter.
func (s *Server) SetProxyBaseDomain(domain string) {
	s.proxyBaseDomain = strings.Trim(strings.TrimSpace(domain), ".")
}

// spoke resolves a spoke name to its box manager. An empty name resolves to the
// admin-chosen default spoke; any name is then looked up among the connected
// remote spokes.
//
// @arg name The spoke name, or "" to use the default spoke.
// @return boxManager The resolved spoke's box manager.
// @error error if no default is set (for an empty name), or the named spoke is not connected.
//
// @testcase TestCreateBoxRoutesToSpoke resolves a connected remote spoke.
// @testcase TestCreateBoxUnknownSpoke errors when the named spoke is not connected.
// @testcase TestCreateBoxDefaultsToDefaultSpoke resolves an empty name to the default spoke.
// @testcase TestCreateBoxNoDefaultSpoke errors when an empty name has no default set.
func (s *Server) spoke(name string) (boxManager, error) {
	if name == "" {
		def, err := s.DefaultSpoke()
		if err != nil {
			return nil, err
		}
		if def == "" {
			return nil, errNoDefaultSpoke
		}
		name = def
	}
	if s.hub != nil {
		if bm, ok := s.hub.Spoke(name); ok {
			return bm, nil
		}
	}
	return nil, fmt.Errorf("spoke %q is not connected", name)
}

// allSpokes returns every connected remote spoke to fan a cluster-wide operation
// (list, reap) across, keyed by name.
//
// @return map[string]boxManager The connected remote spokes, keyed by name.
//
// @testcase TestListFansOutAcrossSpokes aggregates boxes from every spoke.
func (s *Server) allSpokes() map[string]boxManager {
	out := map[string]boxManager{}
	if s.hub != nil {
		maps.Copy(out, s.hub.Spokes())
	}
	return out
}

// reserveBoxID claims boxID for an in-flight create so no other box — on this or
// any other spoke — can hold it. Existence is read from the box records (the same
// source of truth as the box list), so uniqueness can never disagree with what the
// dashboard shows: it fails if a record already holds the ID (case-insensitive),
// or if another in-flight create on this hub already claimed it. A terminated
// record does not block the ID — recreating a dead box under its old name is
// allowed, and the tombstone is replaced once the create succeeds. An empty box
// ID is unnamed and exempt. Every successful reserve must be paired with
// releaseBoxID once the create settles. Spokes also reject a duplicate box ID at
// provision time, which backstops boxes the hub has no record of.
//
// @arg boxID The caller-assigned box ID to claim, or "" for an unnamed box.
// @error error if the box ID is already held by a live record or being created.
//
// @testcase TestCreateBoxRejectsDuplicateBoxIDSameSpoke rejects a duplicate on one spoke.
// @testcase TestCreateBoxRejectsDuplicateBoxIDAcrossSpokes rejects a duplicate on another spoke.
// @testcase TestCreateBoxReplacesTerminatedTombstone allows reusing a terminated record's box ID.
func (s *Server) reserveBoxID(boxID string) error {
	if boxID == "" {
		return nil
	}
	key := strings.ToLower(boxID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byToken {
		// terminated() takes the session lock; s.mu → sess.mu is the codebase's
		// lock order, never the reverse.
		if strings.EqualFold(sess.BoxID, boxID) && !sess.terminated() {
			return fmt.Errorf("box ID %q is already in use on spoke %q; choose a different box ID", boxID, sess.SpokeName)
		}
	}
	if _, claimed := s.pendingBoxIDs[key]; claimed {
		return fmt.Errorf("box ID %q is already being created; choose a different box ID", boxID)
	}
	s.pendingBoxIDs[key] = struct{}{}
	return nil
}

// releaseBoxID drops an in-flight box-ID claim made by reserveBoxID. It is a
// no-op for an empty ID. Safe to call even after the box registered, since the
// claim and the registered session are tracked separately.
//
// @arg boxID The box ID whose in-flight claim to release.
//
// @testcase TestCreateBoxConcurrentSameBoxIDOnlyOneWins pairs reserve with release across racing creates.
// @testcase TestCreateBoxRegistersSession runs the deferred release once a box registers.
func (s *Server) releaseBoxID(boxID string) {
	if boxID == "" {
		return
	}
	s.mu.Lock()
	delete(s.pendingBoxIDs, strings.ToLower(boxID))
	s.mu.Unlock()
}

// SpokeStatus describes one enrolled cluster spoke and its health: whether it
// currently holds a live connection to the hub, and whether it is the default
// spoke unqualified box creates route to.
type SpokeStatus struct {
	Name       string    `json:"name" jsonschema:"the spoke's name"`
	Connected  bool      `json:"connected" jsonschema:"whether the spoke currently has a live connection to the hub"`
	Default    bool      `json:"default,omitempty" jsonschema:"true for the default spoke unqualified box creates run on"`
	EnrolledAt time.Time `json:"enrolled_at" jsonschema:"when the spoke enrolled"`
}

// SpokeStatuses reports every enrolled remote spoke and its health, marking which
// are currently connected and which is the default. It returns nothing when no
// hub is attached.
//
// @arg _ Context (unused; the data is in-memory and in the store).
// @return []SpokeStatus One entry per enrolled remote spoke.
// @error error if the enrolled spokes cannot be read from the store.
//
// @testcase TestSpokeStatusesReportsHealth marks enrolled spokes connected or not.
// @testcase TestSpokeStatusesMarksDefault flags the default spoke.
func (s *Server) SpokeStatuses(_ context.Context) ([]SpokeStatus, error) {
	if s.hub == nil {
		return nil, nil
	}
	connected := s.hub.Spokes()
	enrolled, err := s.store.ListSpokes()
	if err != nil {
		return nil, err
	}
	def, err := s.DefaultSpoke()
	if err != nil {
		return nil, err
	}
	out := make([]SpokeStatus, 0, len(enrolled))
	for _, rec := range enrolled {
		_, isConnected := connected[rec.Name]
		out = append(out, SpokeStatus{
			Name:       rec.Name,
			Connected:  isConnected,
			Default:    rec.Name == def,
			EnrolledAt: rec.EnrolledAt,
		})
	}
	return out, nil
}

// Restore loads persisted sessions into the registry. It deliberately talks to
// no spoke: the store is the system of record, so startup only reads it back —
// a box's record is never dropped here just because its spoke is offline at
// boot. Drift between the records and what the spokes actually run is corrected
// continuously by the sync pass (see syncSpokes), which runs the same way at
// startup and an hour later rather than as a one-shot boot step. The only purge
// is store-driven: sessions and proxies pinned to a spoke that was de-enrolled
// (departed, not merely offline) while the hub was down are removed via
// PruneDepartedSpokes. It returns the number of sessions restored. Call it once
// at startup, before serving.
//
// @return int The number of sessions restored into the registry.
// @error error if the store cannot be read.
//
// @testcase TestRestoreLoadsWithoutSpokes restores every record without contacting any spoke.
// @testcase TestRestoreKeepsDisconnectedSpokeSessions keeps a session whose spoke is offline.
func (s *Server) Restore() (int, error) {
	saved, err := s.store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("loading sessions: %w", err)
	}
	s.mu.Lock()
	for _, ps := range saved {
		s.byToken[ps.Token] = sessionFromPersisted(ps)
	}
	n := len(s.byToken)
	s.mu.Unlock()

	// Purge sessions and proxies pinned to spokes that were de-enrolled while the
	// hub was down, so a removed spoke leaves no objects behind to resolve at
	// random. This reads only the store (enrollment records), never a spoke.
	if purged, err := s.PruneDepartedSpokes(); err != nil {
		s.logger().Warn("pruning departed spokes during restore", "err", err)
	} else if len(purged) > 0 {
		s.logger().Info("pruned sessions for departed spokes", "count", len(purged))
	}
	return n, nil
}

// syncTerminateGrace is how old a session must be before the sync pass may
// conclude its box is gone. It closes the race where a spoke listing taken just
// before (or during) a create is folded in just after the session registers —
// without the grace, the brand-new box would be absent from that stale listing
// and wrongly tombstoned.
const syncTerminateGrace = time.Minute

// syncSpokes folds every connected spoke's live box inventory into the session
// records — the continuous convergence that replaces one-shot startup
// reconciliation. For each spoke that answers, records pinned to it are
// refreshed (metadata, last-seen) or marked terminated when their box is gone;
// proxies whose box generation no longer exists are dropped. A spoke that is
// offline or fails to list is skipped entirely: nothing is concluded about
// boxes the hub cannot currently observe. Called periodically from ReapLoop
// and, targeted, after a create.
//
// @arg ctx Context for the per-spoke list requests.
//
// @testcase TestSyncMarksVanishedBoxTerminated tombstones a box gone from a reachable spoke.
// @testcase TestSyncRefreshesObservedMetadata records name/image/state/last-seen from the listing.
// @testcase TestSyncSkipsUnreachableSpoke leaves an offline spoke's records untouched.
// @testcase TestSyncReconcilesProxies drops a proxy whose box is gone from a reachable spoke.
func (s *Server) syncSpokes(ctx context.Context) {
	boxesBySpoke := map[string][]sandbox.Box{}
	for name, bm := range s.allSpokes() {
		boxes, err := bm.List(ctx)
		if err != nil {
			s.logger().Warn("listing spoke for sync failed; leaving its records untouched", "spoke", name, "err", err)
			continue
		}
		boxesBySpoke[name] = boxes
	}
	for name, boxes := range boxesBySpoke {
		s.syncSpokeInventory(name, boxes)
	}
	s.reconcileProxies(boxesBySpoke)
}

// syncSpokeInventory reconciles the records pinned to one spoke against the
// boxes that spoke actually reports. A record whose box appears in the listing
// is refreshed (observed name, image, backend state, last-seen) and re-marked
// running; a record whose box is absent — beyond the create grace period — is
// marked terminated exactly once, its destroy hooks replayed and its proxies
// dropped, and kept as a tombstone so the UI shows what happened until the
// record is removed. Boxes the spoke reports that the hub has no record of are
// logged, never silently ignored.
//
// @arg spokeName The spoke whose inventory boxes is.
// @arg boxes The boxes the spoke reported (short instance IDs).
//
// @testcase TestSyncMarksVanishedBoxTerminated marks a vanished box terminated and runs its destroy hooks once.
// @testcase TestSyncRefreshesObservedMetadata persists the observed metadata on a live record.
// @testcase TestSyncGraceKeepsFreshRecord leaves a just-created record alone when absent from a stale listing.
// @testcase TestSyncRevivesReappearedBox re-marks a tombstone running when its box reappears.
func (s *Server) syncSpokeInventory(spokeName string, boxes []sandbox.Box) {
	now := time.Now()
	s.mu.Lock()
	var sessions []*session
	for _, sess := range s.byToken {
		if s.resolveStoredSpoke(sess.SpokeName) == spokeName {
			sessions = append(sessions, sess)
		}
	}
	s.mu.Unlock()

	matched := map[string]bool{}
	var torn []tornBox
	for _, sess := range sessions {
		var live *sandbox.Box
		for i := range boxes {
			if boxes[i].InstanceID != "" && strings.HasPrefix(sess.ContainerID, boxes[i].InstanceID) {
				live = &boxes[i]
				matched[boxes[i].InstanceID] = true
				break
			}
		}
		sess.mu.Lock()
		if live != nil {
			sess.BoxState = boxStateRunning
			sess.LastSeen = now
			sess.Name = live.Name
			sess.Image = live.Image
			sess.InstanceState = live.State
			ps := sess.persistLocked()
			sess.mu.Unlock()
			if err := s.store.Save(ps); err != nil {
				s.logger().Warn("persisting synced box record", "box", ps.BoxID, "err", err)
			}
			continue
		}
		alreadyTerminated := sess.BoxState == boxStateTerminated
		withinGrace := now.Sub(sess.CreatedAt) < syncTerminateGrace
		if alreadyTerminated || withinGrace {
			sess.mu.Unlock()
			continue
		}
		sess.BoxState = boxStateTerminated
		ps := sess.persistLocked()
		sess.mu.Unlock()
		if err := s.store.Save(ps); err != nil {
			s.logger().Warn("persisting terminated box record", "box", ps.BoxID, "err", err)
		}
		s.logger().Info("box gone from its spoke; record marked terminated", "box", ps.BoxID, "spoke", spokeName)
		torn = append(torn, tornBox{boxID: ps.BoxID, state: ps.HookState})
	}
	// The running→terminated transition happens exactly once (the state is
	// persisted), so hooks fire once per disappearance and proxies stop routing.
	for _, tb := range torn {
		s.runDestroyHooks(hooks.BoxInfo{BoxID: tb.boxID}, tb.state)
		s.deleteProxiesForBox(tb.boxID)
	}
	// Surface managed boxes the hub has no record of (e.g. created out of band,
	// or records lost with a previous database) rather than hiding them.
	for _, b := range boxes {
		if !matched[b.InstanceID] {
			s.logger().Info("spoke reports a box the hub has no record of", "spoke", spokeName, "instance", b.InstanceID, "box", b.BoxID)
		}
	}
}

// syncSpoke folds one spoke's live inventory into the records — the targeted
// form of syncSpokes, run right after a create so the new box's observed
// metadata (name, image, backend state) lands in its record without waiting for
// the next periodic pass. Best-effort: a list failure is logged and the record
// simply stays metadata-less until the next sync.
//
// @arg ctx Context for the list request.
// @arg name The spoke to sync.
// @arg bm The spoke's box manager.
//
// @testcase TestCreateBoxSyncsObservedMetadata populates the new record's metadata at create time.
func (s *Server) syncSpoke(ctx context.Context, name string, bm boxManager) {
	boxes, err := bm.List(ctx)
	if err != nil {
		s.logger().Warn("listing spoke after create failed; record metadata deferred to next sync", "spoke", name, "err", err)
		return
	}
	s.syncSpokeInventory(name, boxes)
}

// reconcileProxies deletes proxies whose box no longer exists on its (listed)
// spoke, matching by container ID so a same-box-ID box of a newer generation
// does not keep an old proxy alive. boxesBySpoke holds the boxes successfully
// listed per spoke; a proxy whose spoke is absent (unreachable) is kept. Called
// from the periodic sync pass.
//
// @arg boxesBySpoke The boxes listed per reachable spoke, keyed by spoke name.
//
// @testcase TestSyncReconcilesProxies drops a proxy whose box is gone and keeps a live one.
func (s *Server) reconcileProxies(boxesBySpoke map[string][]sandbox.Box) {
	proxies, err := s.store.ListProxies()
	if err != nil {
		s.logger().Warn("listing proxies to reconcile during restore", "err", err)
		return
	}
	for _, p := range proxies {
		spokeName := s.resolveStoredSpoke(p.Spoke)
		boxes, listed := boxesBySpoke[spokeName]
		if !listed {
			continue // spoke unreachable; can't verify, so keep the proxy
		}
		alive := false
		for _, b := range boxes {
			// Prefer the container ID (the exact box generation); fall back to box ID
			// for a proxy persisted before container IDs were recorded.
			if p.ContainerID != "" {
				if strings.HasPrefix(p.ContainerID, b.InstanceID) {
					alive = true
					break
				}
			} else if b.BoxID != "" && strings.EqualFold(b.BoxID, p.BoxID) {
				alive = true
				break
			}
		}
		if !alive {
			if derr := s.store.DeleteProxy(p.Slug); derr != nil {
				s.logger().Warn("failed to delete stale proxy during restore", "slug", p.Slug, "err", derr)
			}
		}
	}
}

// createBox launches a new box and registers an auth session for it. When box
// hooks are configured, it first runs the box.create hooks, injects the files
// they return (e.g. a granular hook's subject token, config, and CLIs), and
// records their opaque state on the session; that state is replayed to the
// box.destroy hooks if box creation later fails so nothing is left dangling. It
// returns the session so callers can build the auth page URL. opts carries the
// box ID, description, and the spoke to place the box on (empty resolves to the
// admin-chosen default spoke); the box image is not a hub input — each spoke
// launches its own configured image.
//
// @arg ctx Context for the box creation.
// @arg opts The box ID, description, and target spoke for the box.
// @return *session The registered auth session for the new box.
// @error error if no spoke is named and no default is set, the target spoke is not connected, a box.create hook fails, the box cannot be created, or a session token cannot be generated.
//
// @testcase TestCreateBoxRegistersSession checks the session is registered with box ID/description.
// @testcase TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
// @testcase TestCreateBoxRunsCreateHooks runs the hooks, injects their files, and persists their state.
// @testcase TestCreateBoxRunsDestroyHooksOnCreateFailure replays hook state when box creation fails.
// @testcase TestCreateBoxRoutesToSpoke creates the box on the named remote spoke.
// @testcase TestCreateBoxUnknownSpoke errors when the named spoke is not connected.
// @testcase TestCreateBoxDefaultsToDefaultSpoke creates on the default spoke when the request names none.
// @testcase TestCreateBoxNoDefaultSpoke errors when the request names no spoke and no default is set.
func (s *Server) createBox(ctx context.Context, opts sandbox.CreateOptions) (*session, error) {
	// Resolve an unqualified create to the admin-chosen default spoke, and pin the
	// box to that concrete spoke name so its later verbs route there even if the
	// default changes afterwards.
	spokeName := opts.SpokeName
	if spokeName == "" {
		def, err := s.DefaultSpoke()
		if err != nil {
			return nil, err
		}
		if def == "" {
			return nil, errNoDefaultSpoke
		}
		spokeName = def
	}
	mgr, err := s.spoke(spokeName)
	if err != nil {
		return nil, err
	}

	// Claim the box ID hub-wide before any slow work, so a box ID is unique across
	// every spoke (the per-spoke docker layer only sees its own boxes). The claim
	// is held until the session is registered (or the create fails), then released.
	if err := s.reserveBoxID(opts.BoxID); err != nil {
		return nil, err
	}
	defer s.releaseBoxID(opts.BoxID)

	box := hooks.BoxInfo{BoxID: opts.BoxID, Description: opts.Description}
	var hookState map[string]string
	if s.hooks != nil {
		files, state, err := s.hooks.OnCreate(ctx, box)
		if err != nil {
			// A hook may have done partial work (e.g. minted a token) before failing;
			// replay its state to the destroy hooks so nothing is left dangling.
			s.runDestroyHooks(box, state)
			return nil, err
		}
		hookState = state
		for _, f := range files {
			opts.Files = append(opts.Files, sandbox.InjectFile{
				Path:    f.Path,
				Content: f.Content,
				Mode:    f.Mode,
				UID:     f.UID,
				GID:     f.GID,
			})
		}
	}

	id, authorizeURL, err := mgr.Create(ctx, opts)
	if err != nil {
		s.runDestroyHooks(box, hookState)
		return nil, err
	}
	tok, err := newToken(rand.Reader)
	if err != nil {
		// Best effort: don't leave the box or hook state dangling if we can't track it.
		if derr := mgr.Destroy(context.Background(), id); derr != nil {
			s.logger().Warn("failed to destroy untrackable box", "container", id, "err", derr)
		}
		s.runDestroyHooks(box, hookState)
		return nil, fmt.Errorf("generating session token: %w", err)
	}
	sess := &session{
		Token:        tok,
		ContainerID:  id,
		AuthorizeURL: authorizeURL,
		CreatedAt:    time.Now(),
		HookState:    hookState,
		Status:       "pending",
		BoxID:        opts.BoxID,
		Description:  opts.Description,
		SpokeName:    spokeName,
		// The spoke just created the box, so it was observed alive this instant.
		BoxState: boxStateRunning,
		LastSeen: time.Now(),
	}
	s.mu.Lock()
	s.byToken[tok] = sess
	s.mu.Unlock()
	// Best-effort persist: a disk hiccup shouldn't fail an otherwise-good box.
	if err := s.store.Save(sess.persist()); err != nil {
		s.logger().Warn("failed to persist new session", "box", sess.BoxID, "err", err)
	}
	// Recreating a dead box under its old ID replaces its tombstone: the
	// terminated record's hooks already ran and its proxies are gone, so only the
	// record itself remains to clear.
	s.dropTerminatedRecords(opts.BoxID, tok)
	// Fold the spoke's inventory in right away so the record carries the box's
	// observed name/image/state without waiting for the next periodic sync.
	s.syncSpoke(ctx, spokeName, mgr)
	return sess, nil
}

// dropTerminatedRecords deletes every terminated record holding boxID (there is
// normally at most one), sparing the session registered under keepToken. It is
// how recreating a box under a dead box's ID replaces the tombstone instead of
// listing two boxes with one name. No-op for an unnamed box.
//
// @arg boxID The box ID whose tombstones to clear; "" is a no-op.
// @arg keepToken The token of the just-registered session to spare.
//
// @testcase TestCreateBoxReplacesTerminatedTombstone clears the tombstone when its box ID is reused.
func (s *Server) dropTerminatedRecords(boxID, keepToken string) {
	if boxID == "" {
		return
	}
	s.mu.Lock()
	var dropped []string
	for tok, sess := range s.byToken {
		if tok != keepToken && strings.EqualFold(sess.BoxID, boxID) && sess.terminated() {
			delete(s.byToken, tok)
			dropped = append(dropped, tok)
		}
	}
	s.mu.Unlock()
	for _, tok := range dropped {
		if err := s.store.Delete(tok); err != nil {
			s.logger().Warn("failed to delete replaced tombstone record", "box", boxID, "err", err)
		}
	}
}

// runDestroyHooks best-effort runs the box.destroy hooks for the given per-hook
// state, logging (but not returning) errors and no-op when no hooks are
// configured or the state is empty. It uses a background context so cleanup runs
// even when the caller's context is already cancelled.
//
// @arg box The box the destroy event concerns.
// @arg state The per-hook state to replay; empty is a no-op.
//
// @testcase TestCreateBoxRunsDestroyHooksOnCreateFailure cleans up via this helper on failure.
// @testcase TestDestroyRunsDestroyHooks replays a destroyed box's hook state via this helper.
func (s *Server) runDestroyHooks(box hooks.BoxInfo, state map[string]string) {
	if s.hooks == nil || len(state) == 0 {
		return
	}
	if err := s.hooks.OnDestroy(context.Background(), box, state); err != nil {
		s.logger().Warn("box.destroy hook failed", "box", box.BoxID, "err", err)
	}
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

// lookupByBoxID returns the session for a box's caller-assigned box ID
// (case-insensitive), or nil. Box IDs are unique across live boxes (enforced at
// create time against the spokes' box list), so normally at most one matches.
// Should a duplicate ever linger — e.g. an orphan session for a box the spoke no
// longer has, alongside a freshly created one reusing the ID — the
// choice is made deterministically and health-aware rather than by random map
// order: a session on a currently reachable spoke wins over one on an unreachable
// spoke, then the most recently created, then the lexically smaller token. This
// guarantees a stable, sensible result and never resolves a box to a dead spoke
// while a live one exists.
//
// @arg boxID The box ID to look up.
// @return *session The matching session, or nil if none has that box ID.
//
// @testcase TestGetByBoxID looks a box up by its box ID.
// @testcase TestLookupByBoxIDPrefersReachableSpoke deterministically prefers a reachable spoke.
func (s *Server) lookupByBoxID(boxID string) *session {
	s.mu.Lock()
	var matches []*session
	for _, sess := range s.byToken {
		if sess.BoxID != "" && strings.EqualFold(sess.BoxID, boxID) {
			matches = append(matches, sess)
		}
	}
	s.mu.Unlock()
	if len(matches) == 0 {
		return nil
	}
	// spoke() touches the hub, so it is called outside s.mu. The fields compared
	// (SpokeName, CreatedAt, Token) are immutable after creation; liveness is read
	// under the session's own lock.
	best := matches[0]
	bestRank := boxIDMatchRank{alive: !best.terminated(), reachable: s.spokeReachable(best.SpokeName)}
	for _, c := range matches[1:] {
		cRank := boxIDMatchRank{alive: !c.terminated(), reachable: s.spokeReachable(c.SpokeName)}
		if betterBoxIDMatch(c, cRank, best, bestRank) {
			best, bestRank = c, cRank
		}
	}
	return best
}

// boxIDMatchRank carries the health facts lookupByBoxID orders duplicate box-ID
// matches by: whether the session's box is still alive (not a terminated
// tombstone) and whether its spoke is currently reachable.
type boxIDMatchRank struct {
	alive     bool
	reachable bool
}

// spokeReachable reports whether the named spoke is currently resolvable (the
// local spoke always is; a remote spoke only when connected to the hub).
//
// @arg name The spoke name ("" or "local" for the in-process spoke).
// @return bool True when the spoke can currently be reached.
//
// @testcase TestLookupByBoxIDPrefersReachableSpoke distinguishes a reachable spoke from an unreachable one.
func (s *Server) spokeReachable(name string) bool {
	_, err := s.spoke(name)
	return err == nil
}

// betterBoxIDMatch reports whether candidate c should be preferred over best when
// resolving a box ID, applying the total order: alive (non-terminated) first, then
// reachable spoke, then newer CreatedAt, then lexically smaller Token (a stable
// final tiebreak). Alive outranks reachable so a duplicate ID never resolves to a
// tombstone while a live box exists anywhere.
//
// @arg c The candidate session.
// @arg cRank The candidate's health facts (alive, spoke reachable).
// @arg best The current best session.
// @arg bestRank The current best's health facts.
// @return bool True when c is the better match.
//
// @testcase TestLookupByBoxIDPrefersReachableSpoke exercises the reachable-then-newer-then-token ordering.
// @testcase TestLookupByBoxIDPrefersAliveOverTerminated prefers a live box over a tombstone with the same ID.
func betterBoxIDMatch(c *session, cRank boxIDMatchRank, best *session, bestRank boxIDMatchRank) bool {
	if cRank.alive != bestRank.alive {
		return cRank.alive
	}
	if cRank.reachable != bestRank.reachable {
		return cRank.reachable
	}
	if !c.CreatedAt.Equal(best.CreatedAt) {
		return c.CreatedAt.After(best.CreatedAt)
	}
	return c.Token < best.Token
}

// submitCode feeds the user's OAuth code to the box's login process and waits
// for the box to become ready. It is called by the web handler, never by the API.
//
// @arg ctx Context for the code submission.
// @arg tok The session token identifying the box.
// @arg code The OAuth code pasted by the user.
// @error error if the session is unknown, the code is empty, its spoke is not connected, or login fails.
//
// @testcase TestSubmitCodeSuccess marks the session ready on success.
// @testcase TestSubmitCodeFailureRecorded records the error on failure.
// @testcase TestSubmitCodeUnknownToken errors for an unknown token.
// @testcase TestSubmitCodeEmpty errors for an empty code.
func (s *Server) submitCode(ctx context.Context, tok, code string) error {
	sess := s.lookup(tok)
	if sess == nil {
		return fmt.Errorf("unknown or expired auth session")
	}
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("the code is empty")
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		return err
	}

	url, err := mgr.SubmitCode(ctx, sess.ContainerID, code)
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
	if serr := s.store.Save(ps); serr != nil {
		s.logger().Warn("failed to persist session status", "box", ps.BoxID, "err", serr)
	}
	return err
}

// boxRecords returns a stable snapshot of every tracked session in its
// persisted form, newest first (then by token, so equal timestamps still order
// deterministically). It is the read path of the box list: records come from
// the registry (the store's in-memory mirror), never from a live spoke, so the
// listing works identically whether a spoke is connected or not.
//
// @return []persistedSession One snapshot per tracked session, newest first.
//
// @testcase TestListBoxesFromRecords lists every record without contacting a spoke.
func (s *Server) boxRecords() []persistedSession {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.byToken))
	for _, sess := range s.byToken {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	out := make([]persistedSession, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sess.persist())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// connectedSpokeSet returns the names of the spokes currently holding a live
// connection to the hub. It is the read-time input that turns a stored
// "running" record into a displayed "unreachable" one, so staleness is
// impossible: connectivity is always evaluated at the moment of the read.
//
// @return map[string]bool The connected spoke names.
//
// @testcase TestListBoxesMarksUnreachable marks a record unreachable when its spoke is not in the set.
func (s *Server) connectedSpokeSet() map[string]bool {
	out := map[string]bool{}
	for name := range s.allSpokes() {
		out[name] = true
	}
	return out
}

// boxFromRecord renders one session record as the sandbox.Box view the API and
// UI consume. The displayed state is derived, in priority order: a terminated
// record shows "terminated" (a tombstone — the box is confirmed gone from its
// spoke); a record whose spoke has no live connection shows "unreachable" (the
// box may well still be running, the hub just cannot see it right now); an
// observable record shows the backend state the sync pass last recorded (e.g.
// "running" or "exited", defaulting to "running" before the first sync).
//
// @arg ps The session record to render.
// @arg connected The currently connected spoke names (see connectedSpokeSet).
// @return sandbox.Box The record rendered as a box view.
//
// @testcase TestListBoxesFromRecords renders running records from their stored metadata.
// @testcase TestListBoxesMarksUnreachable renders a disconnected spoke's record as unreachable.
// @testcase TestSyncMarksVanishedBoxTerminated renders a vanished box as terminated.
func (s *Server) boxFromRecord(ps persistedSession, connected map[string]bool) sandbox.Box {
	spoke := s.resolveStoredSpoke(ps.SpokeName)
	state := ps.InstanceState
	if state == "" {
		state = boxStateRunning
	}
	status := state
	switch {
	case ps.BoxState == boxStateTerminated:
		state = boxStateTerminated
		status = "gone from its spoke"
	case !connected[spoke]:
		state = sandbox.StateUnreachable
		status = "spoke offline"
	}
	var lastSeen int64
	if !ps.LastSeen.IsZero() {
		lastSeen = ps.LastSeen.Unix()
	}
	phase := ps.Status
	if phase == "" {
		phase = "pending"
	}
	return sandbox.Box{
		InstanceID:  shortInstanceID(ps.ContainerID),
		Name:        ps.Name,
		BoxID:       ps.BoxID,
		Description: ps.Description,
		Spoke:       spoke,
		Image:       ps.Image,
		State:       state,
		Status:      status,
		Phase:       phase,
		Created:     ps.CreatedAt.Unix(),
		LastSeen:    lastSeen,
	}
}

// shortInstanceID truncates a full container/instance ID to the 12-character
// short form spokes report in their listings, leaving shorter IDs untouched.
//
// @arg id The full instance ID.
// @return string The short (at most 12 character) form.
//
// @testcase TestListBoxesFromRecords lists boxes with short instance IDs.
func shortInstanceID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// listBoxes returns every tracked box rendered from its record — the store is
// the system of record, so a box on an offline spoke stays listed (as
// unreachable) and a terminated box stays listed as a tombstone until its
// record is removed. No spoke is contacted.
//
// @arg _ Context (unused; the data is the in-memory registry).
// @return []sandbox.Box The tracked boxes, newest first.
// @error error Always nil (kept for interface stability).
//
// @testcase TestBoxToolsOverBackend exercises the server's box wiring.
// @testcase TestListBoxesFromRecords lists records across spokes without contacting them.
// @testcase TestListBoxesMarksUnreachable keeps a disconnected spoke's boxes listed.
func (s *Server) listBoxes(_ context.Context) ([]sandbox.Box, error) {
	connected := s.connectedSpokeSet()
	recs := s.boxRecords()
	out := make([]sandbox.Box, 0, len(recs))
	for _, ps := range recs {
		out = append(out, s.boxFromRecord(ps, connected))
	}
	return out, nil
}

// boxLogs returns the recent console output of the box with the given box ID.
// Like get and destroy, it is keyed by the box ID supplied at create time, so
// a box created without one is not reachable here. tail bounds how many trailing
// lines are returned and is passed through to the manager.
//
// @arg ctx Context for the logs request.
// @arg boxID The box ID of the box to read logs from.
// @arg tail The maximum number of trailing log lines to return; the manager applies a default when non-positive.
// @return string The box's recent console output.
// @error error if no box has that box ID, its spoke is not connected, or the logs cannot be read.
//
// @testcase TestBoxLogsByBoxID returns a box's logs looked up by box ID.
func (s *Server) boxLogs(ctx context.Context, boxID string, tail int) (string, error) {
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		return "", fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", boxID)
	}
	if sess.terminated() {
		return "", fmt.Errorf("box %q is terminated (it no longer exists on its spoke); its logs are gone", boxID)
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		return "", err
	}
	return mgr.Logs(ctx, sess.ContainerID, tail)
}

// boxExec runs a shell command inside the box with the given box ID and returns
// its captured output. Like get, logs, and destroy, it is keyed by the box ID
// supplied at create time, so a box created without one is not reachable here. The
// command is run via "/bin/sh -c" so callers can pass an ordinary shell line.
//
// @arg ctx Context for the exec request.
// @arg boxID The box ID of the box to run the command in.
// @arg command The shell command line to run inside the box.
// @return sandbox.ExecResult The command's stdout, stderr, and exit code.
// @error error if the command is empty, no box has that box ID, its spoke is not connected, or the command cannot be run.
//
// @testcase TestBoxExecByBoxID runs a command in a box looked up by box ID.
func (s *Server) boxExec(ctx context.Context, boxID, command string) (sandbox.ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return sandbox.ExecResult{}, fmt.Errorf("command is required")
	}
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		return sandbox.ExecResult{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", boxID)
	}
	if sess.terminated() {
		return sandbox.ExecResult{}, fmt.Errorf("box %q is terminated (it no longer exists on its spoke)", boxID)
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	return mgr.Exec(ctx, sess.ContainerID, []string{"/bin/sh", "-c", command})
}

// idMatchesBox reports whether idOrName identifies a box with the given box ID
// and container ID — by exact box ID (what the admin UI sends) or by container-ID
// prefix in either direction (so a short ID matches the full one, and vice
// versa). It is the shared predicate used to match both tracked sessions and
// live box listings when routing or cleaning up a destroy.
//
// @arg boxID The box's box ID (the caller-assigned identifier), if any.
// @arg containerID The box's container ID, if known.
// @arg idOrName The box ID or container ID to match against.
// @return bool Whether the identifier names that box.
//
// @testcase TestDestroyBoxByBoxIDRoutesToSpoke routes a box-ID destroy to the box's spoke.
// @testcase TestDestroyRoutesToSpoke routes a container-ID destroy to the box's spoke.
func idMatchesBox(boxID, containerID, idOrName string) bool {
	if idOrName == "" {
		return false
	}
	if boxID != "" && boxID == idOrName {
		return true
	}
	return containerID != "" &&
		(strings.HasPrefix(containerID, idOrName) || strings.HasPrefix(idOrName, containerID))
}

// boxMatchesSession reports whether idOrName identifies sess's box, matching on
// its box ID or container ID. Used to route and clean up a destroy regardless of
// which identifier the caller has.
//
// @arg sess The session to test.
// @arg idOrName The box ID or container ID to match against.
// @return bool Whether the identifier names this session's box.
//
// @testcase TestDestroyBoxByBoxIDRoutesToSpoke routes a box-ID destroy to the box's spoke.
// @testcase TestDestroyRoutesToSpoke routes a container-ID destroy to the box's spoke.
func boxMatchesSession(sess *session, idOrName string) bool {
	return idMatchesBox(sess.BoxID, sess.ContainerID, idOrName)
}

// destroyBox destroys a box and forgets any session pointing at it. Removal is
// idempotent: if the box's container is already gone on its spoke (a not-found
// error), the destroy is treated as success and the session is still forgotten,
// so a box a human removed out of band can be cleared from the UI without error.
// A terminated record (a tombstone — the box is confirmed gone) is removed
// without contacting any spoke, and its destroy hooks are not re-run (they fired
// when it was marked terminated). A box whose spoke is offline is refused with
// its record kept: the hub will not guess about a box it cannot observe — retry
// when the spoke reconnects, or drop the spoke to purge everything pinned to it.
//
// @arg ctx Context for the destroy request.
// @arg idOrName The box ID or container ID identifying the box to destroy.
// @error error if the box's spoke is not connected (and the record is not terminated), or the box cannot be destroyed for a reason other than already being gone.
//
// @testcase TestDestroyForgetsSession checks the session is forgotten after destroy.
// @testcase TestDestroyRunsDestroyHooks checks the box's hook state is replayed to the destroy hooks.
// @testcase TestDestroyRoutesToSpoke destroys a box on the spoke its session names.
// @testcase TestDestroyBoxByBoxIDRoutesToSpoke destroys a remote box by its box ID.
// @testcase TestDestroySessionlessBoxFindsSpoke destroys a box with no tracked session on its actual spoke.
// @testcase TestDestroyAlreadyGoneBoxSucceeds treats a not-found from the spoke as a successful, session-clearing removal.
// @testcase TestDestroyUnknownBoxIsIdempotent treats a box no spoke reports as already gone (no-op success).
// @testcase TestDestroyTerminatedRecordSkipsSpoke removes a tombstone without a spoke and without re-running hooks.
// @testcase TestDestroyUnreachableSpokeRefused refuses to destroy a box whose spoke is offline and keeps its record.
func (s *Server) destroyBox(ctx context.Context, idOrName string) error {
	// Route to the spoke the matching session names. idOrName may be a box ID
	// (what the admin UI sends) or a container ID, so match on both.
	var mgr boxManager
	matched := false
	terminated := false
	var spokeErr error
	s.mu.Lock()
	for _, sess := range s.byToken {
		if boxMatchesSession(sess, idOrName) {
			matched = true
			// terminated() takes sess.mu; s.mu → sess.mu is the lock order.
			if terminated = sess.terminated(); terminated {
				// A tombstone needs no spoke: the box is confirmed gone, only the
				// record remains to clear.
				break
			}
			mgr, spokeErr = s.spoke(sess.SpokeName)
			break
		}
	}
	s.mu.Unlock()
	if spokeErr != nil {
		return fmt.Errorf("box %q cannot be destroyed right now: %w; the box record is kept — retry when its spoke reconnects, or drop the spoke to purge it", idOrName, spokeErr)
	}
	// No tracked session named the box. A destroy may still legitimately target a
	// box the hub has no record of (e.g. records lost with a previous database, or
	// a box created out of band) — locate it across the connected spokes.
	if !matched {
		hosting, err := s.spokeHostingBox(ctx, idOrName)
		if err != nil {
			return err
		}
		mgr = hosting
	}
	// mgr is nil when the record is terminated, or when no session names the box
	// and no connected spoke reports it: either way the box is already gone
	// everywhere, which is the desired end state. Skip the destroy and fall
	// through to forget any record so the UI clears without error.
	if mgr != nil {
		if err := mgr.Destroy(ctx, idOrName); err != nil {
			// Removal is idempotent: if the box's container is already gone on its
			// spoke (e.g. an operator removed it out of band), the desired end state —
			// the box no longer exists — already holds. Treat that as success and fall
			// through to forget the session so the box disappears from the UI without a
			// spurious error. Any other failure is real and surfaced.
			if !docker.IsNotFound(err) {
				return err
			}
			s.logger().Info("box already gone on its spoke; treating destroy as success", "box", idOrName)
		}
	}
	s.mu.Lock()
	var dropped []string
	var torn []tornBox
	for tok, sess := range s.byToken {
		if boxMatchesSession(sess, idOrName) {
			delete(s.byToken, tok)
			dropped = append(dropped, tok)
			// A terminated record's destroy hooks already ran when the sync pass
			// tombstoned it; re-running them would replay cleanup that happened.
			if !sess.terminated() {
				torn = append(torn, tornBox{boxID: sess.BoxID, state: sess.HookState})
			}
		}
	}
	s.mu.Unlock()
	for _, tok := range dropped {
		// The token is a secret (it's the auth URL), so it is never logged.
		if err := s.store.Delete(tok); err != nil {
			s.logger().Warn("failed to delete session from store", "err", err)
		}
	}
	for _, tb := range torn {
		s.runDestroyHooks(hooks.BoxInfo{BoxID: tb.boxID}, tb.state)
		s.deleteProxiesForBox(tb.boxID)
	}
	// Defence in depth: also clear proxies for the identifier the caller passed, in
	// case it is a box ID with no matching session (so the torn loop above missed
	// it). A non-box-ID (container ID) matches no proxies, so this is a no-op then.
	// This guarantees a destroyed box leaves no proxy a later same-id box can reuse.
	s.deleteProxiesForBox(idOrName)
	return nil
}

// spokeHostingBox returns the manager of the spoke whose box list contains
// idOrName (matched by box ID or container-ID prefix), or nil when no spoke
// reports such a box. It mirrors how the admin box list is assembled (see
// listBoxes), so a destroy can locate a box that has no tracked session. A spoke
// that fails to list is skipped (it cannot be the host to destroy on).
//
// @arg ctx Context for the per-spoke list requests.
// @arg idOrName The box ID or container ID to locate.
// @return boxManager The hosting spoke's manager, or nil when no spoke reports the box.
// @error error Always nil (per-spoke list failures are logged and skipped).
//
// @testcase TestDestroySessionlessBoxFindsSpoke locates a sessionless remote box's spoke.
func (s *Server) spokeHostingBox(ctx context.Context, idOrName string) (boxManager, error) {
	for name, bm := range s.allSpokes() {
		boxes, err := bm.List(ctx)
		if err != nil {
			s.logger().Warn("listing spoke failed while locating box to destroy", "spoke", name, "err", err)
			continue
		}
		for _, b := range boxes {
			if idMatchesBox(b.BoxID, b.InstanceID, idOrName) {
				return bm, nil
			}
		}
	}
	return nil, nil
}

// tornBox carries the bits of a removed session that the box.destroy hooks need:
// the box ID (for the hook's box context) and the per-hook state to replay.
// It avoids copying the session struct (and its mutex) out from under the lock.
type tornBox struct {
	boxID string
	state map[string]string
}

// ReapLoop periodically destroys orphaned (never-authenticated) boxes, prunes
// their sessions, and runs the sync pass that folds each connected spoke's live
// inventory into the box records (see syncSpokes) — the continuous convergence
// keeping the store honest instead of a one-shot startup reconciliation. It
// blocks until ctx is cancelled.
//
// @arg ctx Context whose cancellation stops the loop.
// @arg every How often to run a reap-and-sync pass.
// @arg log Optional sink for reaper log messages; may be nil.
//
// @testcase TestPruneSessionsAfterReap covers the session pruning ReapLoop relies on.
// @testcase TestSyncMarksVanishedBoxTerminated covers the sync pass ReapLoop runs.
func (s *Server) ReapLoop(ctx context.Context, every time.Duration, log func(string)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Housekeeping: drop expired login sessions/flows (expiry is also
			// enforced at read time, so this just bounds the buckets' growth).
			if err := s.store.PurgeExpiredLogins(time.Now()); err != nil {
				s.logger().Warn("purging expired login sessions", "err", err)
			}
			// Purge objects pinned to spokes that have disappeared (de-enrolled).
			if purged, err := s.PruneDepartedSpokes(); err != nil {
				s.logger().Warn("pruning departed spokes", "err", err)
			} else if len(purged) > 0 && log != nil {
				log(fmt.Sprintf("reaper: purged %d box(es) on departed spoke(s): %s", len(purged), strings.Join(purged, ", ")))
			}
			reaped := s.reapAllSpokes(ctx, log)
			if len(reaped) > 0 {
				s.pruneSessions(reaped)
				if log != nil {
					log(fmt.Sprintf("reaper: destroyed %d orphaned box(es): %s", len(reaped), strings.Join(reaped, ", ")))
				}
			}
			// Converge the records with reality last, so the tick ends with the
			// post-reap state folded in.
			s.syncSpokes(ctx)
		}
	}
}

// reapAllSpokes reaps orphaned boxes on every spoke (local plus each connected
// remote spoke) and returns the combined short IDs of the reaped boxes. A
// spoke's reap error is reported via log (if set) and does not stop the others.
//
// @arg ctx Context for the reap requests.
// @arg log Optional sink for per-spoke reaper errors; may be nil.
// @return []string The combined short IDs of boxes reaped across all spokes.
//
// @testcase TestReapFansOutAcrossSpokes reaps orphans on every spoke.
func (s *Server) reapAllSpokes(ctx context.Context, log func(string)) []string {
	var reaped []string
	for name, bm := range s.allSpokes() {
		ids, err := bm.ReapOrphans(ctx, s.authTTL)
		if err != nil {
			if log != nil {
				log(fmt.Sprintf("reaper: spoke %q: %v", name, err))
			}
			continue
		}
		reaped = append(reaped, ids...)
	}
	return reaped
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
	var torn []tornBox
	for tok, sess := range s.byToken {
		// reaped IDs are short (12 char); ContainerID is the full ID.
		for id := range reaped {
			if strings.HasPrefix(sess.ContainerID, id) {
				delete(s.byToken, tok)
				dropped = append(dropped, tok)
				torn = append(torn, tornBox{boxID: sess.BoxID, state: sess.HookState})
				break
			}
		}
	}
	s.mu.Unlock()
	for _, tok := range dropped {
		// The token is a secret (it's the auth URL), so it is never logged.
		if err := s.store.Delete(tok); err != nil {
			s.logger().Warn("failed to delete session from store", "err", err)
		}
	}
	for _, tb := range torn {
		s.runDestroyHooks(hooks.BoxInfo{BoxID: tb.boxID}, tb.state)
		s.deleteProxiesForBox(tb.boxID)
	}
}

// newToken returns a 256-bit unguessable hex token for an auth page URL.
//
// @arg randSource The reader supplying cryptographic randomness.
// @return string A 64-character hex-encoded random token.
// @error error if the system random source fails.
//
// @testcase TestCreateBoxRegistersSession checks the generated token length.
func newToken(randSource io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := randSource.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
