package hub

import (
	"github.com/clems4ever/llmbox/internal/hub/store"
)

// The persistence layer lives in the dedicated internal/hub/store package (SQLite
// today, other backends possible later). These aliases keep the names the server
// and its callers already use bound to that package, so persistence can evolve
// without rippling through the server.
type (
	// Store persists the box registry, sign-in (identity) state, and cluster records.
	Store = store.Store
	// IdentitySession is a completed sign-in, used by the admin handlers.
	IdentitySession = store.IdentitySession
	// boxRecord is the on-disk form of a box (its stable identity, provisioning
	// phase, hub lifecycle, and last-observed backend facts).
	boxRecord = store.Box
)

// Box lifecycle states, re-exported from the store package as plain strings for
// the live session's string-typed state (see store.Lifecycle for the model:
// running/terminated are persisted; "unreachable" is computed at read time from
// live spoke connectivity and never stored).
const (
	boxStateRunning    = string(store.LifecycleRunning)
	boxStateTerminated = string(store.LifecycleTerminated)
)

// OpenStore opens (creating if needed) a SQLite-backed Store at path.
//
// @arg path The filesystem path to the store's database file.
// @return Store A ready-to-use, SQLite-backed store.
// @error error if the store cannot be opened or initialized.
//
// @testcase TestCreateBoxPersistsSession opens a store and persists a session through it.
func OpenStore(path string) (Store, error) { return store.Open(path) }
