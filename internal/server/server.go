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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
)

// localSpokeName is the registry name of the in-process spoke (the Docker
// manager the server was built with). A box with no explicit spoke runs here, so
// single-host deployments need no remote spoke.
const localSpokeName = "local"

// boxManager is the behaviour Server needs from a spoke's box layer. The local
// implementation is *docker.Manager; a remote spoke is reached over the cluster
// transport. Tests fake it. It is an alias of cluster.BoxManager so the same
// interface is the cluster RPC surface.
type boxManager = cluster.BoxManager

// clusterHub is what Server needs from the cluster hub: resolving connected
// remote spokes by name (for routing) and the HTTP handler spokes connect to.
// The real implementation is *cluster.Hub; tests fake it. nil means clustering
// is disabled (every box uses the local spoke).
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

	// SpokeName is the cluster spoke the box runs on ("local" for the in-process
	// spoke). Set at creation and immutable, so it needs no locking; per-box verbs
	// route to this spoke.
	SpokeName string

	mu          sync.Mutex
	Status      string // "pending" | "ready" | "error"
	SessionURL  string
	Err         string
	ActivatedBy string // identity (email) that submitted the code, when auth is enabled
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
		ContainerID:  s.ContainerID,
		AuthorizeURL: s.AuthorizeURL,
		CreatedAt:    s.CreatedAt,
		HookState:    s.HookState,
		BoxID:        s.BoxID,
		Description:  s.Description,
		SpokeName:    s.SpokeName,
		Status:       s.Status,
		SessionURL:   s.SessionURL,
		Err:          s.Err,
		ActivatedBy:  s.ActivatedBy,
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
		ContainerID:  ps.ContainerID,
		AuthorizeURL: ps.AuthorizeURL,
		CreatedAt:    ps.CreatedAt,
		HookState:    ps.HookState,
		BoxID:        ps.BoxID,
		Description:  ps.Description,
		SpokeName:    ps.SpokeName,
		Status:       ps.Status,
		SessionURL:   ps.SessionURL,
		Err:          ps.Err,
		ActivatedBy:  ps.ActivatedBy,
	}
}

// Server orchestrates boxes and owns the session registry.
type Server struct {
	mgr       boxManager
	hooks     boxHooks // runs lifecycle hooks per box; nil when none configured
	publicURL string   // external base URL, e.g. https://boxes.example.com
	authTTL   time.Duration
	store     Store // persists the registry across restarts

	mu      sync.Mutex
	byToken map[string]*session

	// hub holds the connected remote spokes (nil when clustering is not enabled).
	// The in-process mgr is always the "local" spoke; remote spokes are resolved
	// through the hub by name. Set once at startup via SetHub before serving.
	hub clusterHub

	// auth gates box activation behind provider sign-in (Google, …). nil leaves
	// activation unauthenticated (no provider configured).
	auth *Authenticator

	// spokeImage is the llmbox image named in the admin UI's ready-to-run spoke
	// command; empty falls back to a built-in default. Display-only.
	spokeImage string

	// boxImage is the hub's resolved per-box image (claude_image, or the built-in
	// default when that is unset) stamped onto every creation request that names
	// none. The box image is resolved here on the hub so remote spokes stay
	// config-free and hold no default of their own.
	boxImage string

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
// before the reaper destroys it. store persists the session registry; pass
// noopStore{} to disable persistence. hooks runs lifecycle hooks per box; pass
// nil to disable hook integration. auth gates box activation behind provider
// sign-in; pass nil to leave activation unauthenticated. Call Restore to load any
// saved sessions.
//
// @arg mgr The box manager the server delegates Docker operations to.
// @arg hooks The box lifecycle hook runner; nil disables hook integration.
// @arg publicURL The externally reachable base URL for auth page links.
// @arg authTTL How long a box may stay un-authenticated before being reaped.
// @arg store The session store used to persist the registry; noopStore{} disables it.
// @arg auth The activation authenticator; nil leaves activation unauthenticated.
// @return *Server A ready-to-use Server with an empty in-memory session registry.
//
// @testcase TestCreateBoxRegistersSession builds a Server via New.
// @testcase TestCreateBoxRunsCreateHooks builds a Server with a hook runner.
func New(mgr boxManager, hooks boxHooks, publicURL string, authTTL time.Duration, store Store, auth *Authenticator) *Server {
	return &Server{
		mgr:       mgr,
		hooks:     hooks,
		publicURL: strings.TrimRight(publicURL, "/"),
		authTTL:   authTTL,
		store:     store,
		auth:      auth,
		byToken:   make(map[string]*session),
		log:       slog.Default(),
	}
}

// SetHub attaches the cluster hub holding connected remote spokes. Call it once
// at startup, before serving, when clustering is enabled. Without a hub the
// server runs single-host: every box uses the in-process "local" spoke.
//
// @arg hub The cluster hub resolving remote spokes by name.
//
// @testcase TestCreateBoxRoutesToSpoke routes a box to a remote spoke via the hub.
func (s *Server) SetHub(hub clusterHub) { s.hub = hub }

// SetSpokeImage sets the llmbox image named in the admin UI's ready-to-run spoke
// command. It is display-only and does not affect how spokes run.
//
// @arg image The container image (e.g. ghcr.io/clems4ever/granular-llmbox:0.0.6).
//
// @testcase TestAdminCreateSpokeMintsToken shows the configured image in the command.
func (s *Server) SetSpokeImage(image string) { s.spokeImage = image }

// SetBoxImage sets the hub's resolved per-box image (claude_image, or the
// built-in default when unset) that the server stamps onto a creation request
// naming none. Resolving the image on the hub keeps remote spokes config-free
// and defaultless: the spoke launches exactly the image it is sent.
//
// @arg image The resolved per-box container image (e.g. ghcr.io/clems4ever/granular-llmbox-box:latest); never empty in practice.
//
// @testcase TestCreateBoxDefaultsImageToBoxImage stamps the configured image onto an imageless request.
func (s *Server) SetBoxImage(image string) { s.boxImage = image }

// spoke resolves a spoke name to its box manager. An empty name or "local"
// returns the in-process manager; any other name is looked up among the
// connected remote spokes.
//
// @arg name The spoke name ("" or "local" for the in-process spoke).
// @return boxManager The resolved spoke's box manager.
// @error error if a named remote spoke is not currently connected.
//
// @testcase TestCreateBoxRoutesToSpoke resolves a connected remote spoke.
// @testcase TestCreateBoxUnknownSpoke errors when the named spoke is not connected.
func (s *Server) spoke(name string) (boxManager, error) {
	if name == "" || name == localSpokeName {
		return s.mgr, nil
	}
	if s.hub != nil {
		if bm, ok := s.hub.Spoke(name); ok {
			return bm, nil
		}
	}
	return nil, fmt.Errorf("spoke %q is not connected", name)
}

// allSpokes returns every spoke to fan a cluster-wide operation (list, reap)
// across: the in-process "local" spoke plus each connected remote spoke.
//
// @return map[string]boxManager The local spoke plus all connected remote spokes, keyed by name.
//
// @testcase TestListFansOutAcrossSpokes aggregates boxes from every spoke.
func (s *Server) allSpokes() map[string]boxManager {
	out := map[string]boxManager{localSpokeName: s.mgr}
	if s.hub != nil {
		for name, bm := range s.hub.Spokes() {
			out[name] = bm
		}
	}
	return out
}

// SpokeStatus describes one cluster spoke and its health. The in-process spoke
// is always present and connected; each enrolled remote spoke is reported with
// whether it currently holds a live connection to the hub.
type SpokeStatus struct {
	Name       string    `json:"name" jsonschema:"the spoke's name; 'local' is the in-process spoke"`
	Connected  bool      `json:"connected" jsonschema:"whether the spoke currently has a live connection to the hub"`
	Local      bool      `json:"local,omitempty" jsonschema:"true for the in-process spoke (always connected)"`
	EnrolledAt time.Time `json:"enrolled_at,omitempty" jsonschema:"when the remote spoke enrolled (absent for the local spoke)"`
}

// SpokeStatuses reports every spoke and its health: the in-process "local"
// spoke plus each enrolled remote spoke, marking which are currently connected.
// With clustering disabled it returns just the local spoke.
//
// @arg _ Context (unused; the data is in-memory and in the store).
// @return []SpokeStatus The local spoke followed by each enrolled remote spoke.
// @error error if the enrolled spokes cannot be read from the store.
//
// @testcase TestSpokeStatusesReportsHealth marks enrolled spokes connected or not.
// @testcase TestSpokeStatusesLocalOnly returns just the local spoke without a hub.
func (s *Server) SpokeStatuses(_ context.Context) ([]SpokeStatus, error) {
	out := []SpokeStatus{{Name: localSpokeName, Connected: true, Local: true}}
	if s.hub == nil {
		return out, nil
	}
	connected := s.hub.Spokes()
	enrolled, err := s.store.ListSpokes()
	if err != nil {
		return nil, err
	}
	for _, rec := range enrolled {
		_, isConnected := connected[rec.Name]
		out = append(out, SpokeStatus{Name: rec.Name, Connected: isConnected, EnrolledAt: rec.EnrolledAt})
	}
	return out, nil
}

// Restore loads persisted sessions into the registry and reconciles them with
// the spokes: a session whose box no longer exists on its (reachable) spoke is
// dropped (and deleted from the store) so a stale token can't linger. A session
// whose spoke is not currently connected is kept — the box may still be alive,
// we just can't verify it yet. It returns the number of sessions restored. Call
// it once at startup, before serving.
//
// @arg ctx Context for the spoke listings used to reconcile.
// @return int The number of sessions restored into the registry.
// @error error if the store cannot be read or the local spoke cannot be listed.
//
// @testcase TestRestoreLoadsAndReconciles restores live sessions and drops dead ones.
// @testcase TestRestoreKeepsDisconnectedSpokeSessions keeps a session whose spoke is offline.
func (s *Server) Restore(ctx context.Context) (int, error) {
	saved, err := s.store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("loading sessions: %w", err)
	}

	// List each reachable spoke. The local spoke must succeed; a remote spoke that
	// errors is treated as unreachable (its sessions are kept, not dropped).
	boxesBySpoke := map[string][]docker.Box{}
	for name, bm := range s.allSpokes() {
		boxes, err := bm.List(ctx)
		if err != nil {
			if name == localSpokeName {
				return 0, fmt.Errorf("listing boxes to reconcile sessions: %w", err)
			}
			s.logger().Warn("listing spoke to reconcile sessions failed; keeping its sessions", "spoke", name, "err", err)
			continue
		}
		boxesBySpoke[name] = boxes
	}

	// reachable reports whether we could list the session's spoke; alive reports
	// whether the box is present there. List returns short (12-char) container IDs;
	// a session stores the full one.
	reconcile := func(spokeName, containerID string) (reachable, alive bool) {
		boxes, ok := boxesBySpoke[spokeName]
		if !ok {
			return false, false
		}
		for _, b := range boxes {
			if strings.HasPrefix(containerID, b.ContainerID) {
				return true, true
			}
		}
		return true, false
	}

	s.mu.Lock()
	for _, ps := range saved {
		spokeName := ps.SpokeName
		if spokeName == "" {
			spokeName = localSpokeName
		}
		if reachable, alive := reconcile(spokeName, ps.ContainerID); reachable && !alive {
			if err := s.store.Delete(ps.Token); err != nil {
				s.logger().Warn("failed to delete stale session during restore", "box", ps.BoxID, "err", err)
			}
			continue
		}
		s.byToken[ps.Token] = sessionFromPersisted(ps)
	}
	n := len(s.byToken)
	s.mu.Unlock()
	return n, nil
}

// CreateBox launches a new box and registers an auth session for it. When box
// hooks are configured, it first runs the box.create hooks, injects the files
// they return (e.g. a granular hook's subject token, config, and CLIs), and
// records their opaque state on the session; that state is replayed to the
// box.destroy hooks if box creation later fails so nothing is left dangling. It
// returns the session so callers can build the auth page URL. opts carries the
// optional image, box ID, description, and the spoke to place the box on (empty
// means the local in-process spoke).
//
// @arg ctx Context for the box creation.
// @arg opts The optional image, box ID, description, and target spoke for the box.
// @return *session The registered auth session for the new box.
// @error error if the target spoke is not connected, a box.create hook fails, the box cannot be created, or a session token cannot be generated.
//
// @testcase TestCreateBoxRegistersSession checks the session is registered with box ID/description.
// @testcase TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
// @testcase TestCreateBoxRunsCreateHooks runs the hooks, injects their files, and persists their state.
// @testcase TestCreateBoxRunsDestroyHooksOnCreateFailure replays hook state when box creation fails.
// @testcase TestCreateBoxRoutesToSpoke creates the box on the named remote spoke.
// @testcase TestCreateBoxUnknownSpoke errors when the named spoke is not connected.
// @testcase TestCreateBoxDefaultsImageToBoxImage stamps the hub's box image when the request names none.
// @testcase TestCreateBoxKeepsExplicitImage leaves a request's explicit image untouched.
func (s *Server) CreateBox(ctx context.Context, opts docker.CreateOptions) (*session, error) {
	// Resolve the box image on the hub so remote spokes stay config-free and
	// defaultless: a request that names no image inherits the hub's resolved box
	// image (claude_image, or the built-in default). Spokes reject an imageless
	// create, so this is the only place a default is ever applied.
	if opts.Image == "" {
		opts.Image = s.boxImage
	}
	spokeName := opts.SpokeName
	if spokeName == "" {
		spokeName = localSpokeName
	}
	mgr, err := s.spoke(spokeName)
	if err != nil {
		return nil, err
	}

	box := hooks.BoxInfo{Image: opts.Image, BoxID: opts.BoxID, Description: opts.Description}
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
			opts.Files = append(opts.Files, docker.InjectFile{
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
	tok, err := newToken()
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
	}
	s.mu.Lock()
	s.byToken[tok] = sess
	s.mu.Unlock()
	// Best-effort persist: a disk hiccup shouldn't fail an otherwise-good box.
	if err := s.store.Save(sess.persist()); err != nil {
		s.logger().Warn("failed to persist new session", "box", sess.BoxID, "err", err)
	}
	return sess, nil
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
// (case-insensitive), or nil. Box IDs are unique across boxes, so at most one
// matches.
//
// @arg boxID The box ID to look up.
// @return *session The matching session, or nil if none has that box ID.
//
// @testcase TestGetByBoxID looks a box up by its box ID.
func (s *Server) lookupByBoxID(boxID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byToken {
		if sess.BoxID != "" && strings.EqualFold(sess.BoxID, boxID) {
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
// @error error if the session is unknown, the code is empty, its spoke is not connected, or login fails.
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

// ListBoxes returns all managed boxes across every spoke, each tagged with the
// spoke it runs on. The local spoke must list successfully; a connected remote
// spoke that errors is logged and skipped so one bad spoke doesn't fail the
// whole listing.
//
// @arg ctx Context for the list request.
// @return []docker.Box The boxes managed by this server, tagged with their spoke.
// @error error if the local spoke cannot be listed.
//
// @testcase TestMCPToolsRegisteredAndCreate exercises the server's box wiring.
// @testcase TestListFansOutAcrossSpokes aggregates and tags boxes from every spoke.
func (s *Server) ListBoxes(ctx context.Context) ([]docker.Box, error) {
	out, err := s.mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Spoke = localSpokeName
	}
	if s.hub != nil {
		for name, bm := range s.hub.Spokes() {
			boxes, err := bm.List(ctx)
			if err != nil {
				s.logger().Warn("listing spoke failed; skipping it", "spoke", name, "err", err)
				continue
			}
			for i := range boxes {
				boxes[i].Spoke = name
			}
			out = append(out, boxes...)
		}
	}
	return out, nil
}

// BoxLogs returns the recent console output of the box with the given box ID.
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
func (s *Server) BoxLogs(ctx context.Context, boxID string, tail int) (string, error) {
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		return "", fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", boxID)
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		return "", err
	}
	return mgr.Logs(ctx, sess.ContainerID, tail)
}

// BoxExec runs a shell command inside the box with the given box ID and returns
// its captured output. Like get, logs, and destroy, it is keyed by the box ID
// supplied at create time, so a box created without one is not reachable here. The
// command is run via "/bin/sh -c" so callers can pass an ordinary shell line.
//
// @arg ctx Context for the exec request.
// @arg boxID The box ID of the box to run the command in.
// @arg command The shell command line to run inside the box.
// @return docker.ExecResult The command's stdout, stderr, and exit code.
// @error error if the command is empty, no box has that box ID, its spoke is not connected, or the command cannot be run.
//
// @testcase TestBoxExecByBoxID runs a command in a box looked up by box ID.
func (s *Server) BoxExec(ctx context.Context, boxID, command string) (docker.ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return docker.ExecResult{}, fmt.Errorf("command is required")
	}
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		return docker.ExecResult{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", boxID)
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		return docker.ExecResult{}, err
	}
	return mgr.Exec(ctx, sess.ContainerID, []string{"/bin/sh", "-c", command})
}

// boxMatchesSession reports whether idOrName identifies sess's box — by exact box
// ID (what the admin UI sends) or by container-ID prefix in either direction (so
// a short ID matches the full one, and vice versa). Used to route and clean up a
// destroy regardless of which identifier the caller has.
//
// @arg sess The session to test.
// @arg idOrName The box ID or container ID to match against.
// @return bool Whether the identifier names this session's box.
//
// @testcase TestDestroyBoxByBoxIDRoutesToSpoke routes a box-ID destroy to the box's spoke.
// @testcase TestDestroyRoutesToSpoke routes a container-ID destroy to the box's spoke.
func boxMatchesSession(sess *session, idOrName string) bool {
	if idOrName == "" {
		return false
	}
	if sess.BoxID == idOrName {
		return true
	}
	return sess.ContainerID != "" &&
		(strings.HasPrefix(sess.ContainerID, idOrName) || strings.HasPrefix(idOrName, sess.ContainerID))
}

// DestroyBox destroys a box and forgets any session pointing at it.
//
// @arg ctx Context for the destroy request.
// @arg idOrName The box ID or container ID identifying the box to destroy.
// @error error if the box's spoke is not connected, or the box cannot be destroyed.
//
// @testcase TestDestroyForgetsSession checks the session is forgotten after destroy.
// @testcase TestDestroyRunsDestroyHooks checks the box's hook state is replayed to the destroy hooks.
// @testcase TestDestroyRoutesToSpoke destroys a box on the spoke its session names.
// @testcase TestDestroyBoxByBoxIDRoutesToSpoke destroys a remote box by its box ID.
func (s *Server) DestroyBox(ctx context.Context, idOrName string) error {
	// Route to the spoke the matching session names; default to the local spoke
	// when no session matches (e.g. a raw container ID with no tracked session).
	// idOrName may be a box ID (what the admin UI sends) or a container ID, so
	// match on both — a box-ID-only match previously fell through to the local
	// spoke and failed for boxes running on a remote spoke.
	mgr := s.mgr
	var spokeErr error
	s.mu.Lock()
	for _, sess := range s.byToken {
		if boxMatchesSession(sess, idOrName) {
			mgr, spokeErr = s.spoke(sess.SpokeName)
			break
		}
	}
	s.mu.Unlock()
	if spokeErr != nil {
		return spokeErr
	}
	if err := mgr.Destroy(ctx, idOrName); err != nil {
		return err
	}
	s.mu.Lock()
	var dropped []string
	var torn []tornBox
	for tok, sess := range s.byToken {
		if boxMatchesSession(sess, idOrName) {
			delete(s.byToken, tok)
			dropped = append(dropped, tok)
			torn = append(torn, tornBox{boxID: sess.BoxID, state: sess.HookState})
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
	}
	return nil
}

// tornBox carries the bits of a removed session that the box.destroy hooks need:
// the box ID (for the hook's box context) and the per-hook state to replay.
// It avoids copying the session struct (and its mutex) out from under the lock.
type tornBox struct {
	boxID string
	state map[string]string
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
			// Housekeeping: drop expired login sessions/flows (expiry is also
			// enforced at read time, so this just bounds the buckets' growth).
			if err := s.store.PurgeExpiredLogins(time.Now()); err != nil {
				s.logger().Warn("purging expired login sessions", "err", err)
			}
			reaped := s.reapAllSpokes(ctx, log)
			if len(reaped) > 0 {
				s.pruneSessions(reaped)
				if log != nil {
					log(fmt.Sprintf("reaper: destroyed %d orphaned box(es): %s", len(reaped), strings.Join(reaped, ", ")))
				}
			}
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
