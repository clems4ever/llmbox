package server

import (
	"github.com/clems4ever/llmbox/internal/store"
)

// The persistence layer lives in the dedicated internal/store package (bbolt
// today, other backends possible later). These aliases keep the names the server
// and its callers already use bound to that package, so persistence can evolve
// without rippling through the server.
type (
	// Store persists the auth-session registry, login state, and cluster records.
	Store = store.Store
	// LoginSession is a completed activation login, used by the admin handlers.
	LoginSession = store.LoginSession
	// persistedSession is the on-disk form of a box's auth session.
	persistedSession = store.PersistedSession
)

// OpenStore opens (creating if needed) a bbolt-backed Store at path.
//
// @arg path The filesystem path to the store's database file.
// @return Store A ready-to-use, bbolt-backed store.
// @error error if the store cannot be opened or initialized.
//
// @testcase TestCreateBoxPersistsSession opens a store and persists a session through it.
func OpenStore(path string) (Store, error) { return store.Open(path) }
