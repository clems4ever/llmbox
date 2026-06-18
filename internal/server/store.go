package server

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// sessionsBucket is the bbolt bucket holding one JSON-encoded persistedSession
// per auth token.
var sessionsBucket = []byte("sessions")

// persistedSession is the on-disk form of a session. It mirrors the durable
// fields of session (the sync.Mutex and live-only state are not stored as a
// type, just their values) so the registry survives a server restart.
type persistedSession struct {
	Token        string            `json:"token"`
	ContainerID  string            `json:"container_id"`
	AuthorizeURL string            `json:"authorize_url"`
	CreatedAt    time.Time         `json:"created_at"`
	HookState    map[string]string `json:"hook_state,omitempty"`
	BoxID        string            `json:"box_id,omitempty"`
	Description  string            `json:"description,omitempty"`
	Status       string            `json:"status"`
	SessionURL   string            `json:"session_url,omitempty"`
	Err          string            `json:"err,omitempty"`
}

// Store persists the auth-session registry across restarts. All methods must be
// safe for concurrent use. Use OpenStore for a bbolt-backed implementation, or
// noopStore{} to disable persistence.
type Store interface {
	// Save writes (creating or replacing) one session keyed by its token.
	Save(ps persistedSession) error
	// Delete removes the session for a token; deleting a missing token is a no-op.
	Delete(token string) error
	// LoadAll returns every persisted session.
	LoadAll() ([]persistedSession, error)
	// Close releases the underlying store.
	Close() error
}

// noopStore is a Store that persists nothing. It is used in tests that don't
// exercise persistence.
type noopStore struct{}

// Save discards the session.
//
// @arg _ The session to (not) persist.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) Save(_ persistedSession) error { return nil }

// Delete does nothing.
//
// @arg _ The token to (not) delete.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) Delete(_ string) error { return nil }

// LoadAll returns no sessions.
//
// @return []persistedSession Always nil.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) LoadAll() ([]persistedSession, error) { return nil, nil }

// Close does nothing.
//
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) Close() error { return nil }

// boltStore is a Store backed by a single bbolt database file.
type boltStore struct {
	db *bolt.DB
}

// OpenStore opens (creating if needed) a bbolt-backed Store at path.
//
// @arg path The filesystem path to the bbolt database file.
// @return Store A ready-to-use, bbolt-backed session store.
// @error error if the database cannot be opened or initialized.
//
// @testcase TestBoltStoreRoundTrip opens a store and round-trips a session.
func OpenStore(path string) (Store, error) { return openBoltStore(path) }

// openBoltStore opens (creating if needed) a bbolt database at path and ensures
// the sessions bucket exists.
//
// @arg path The filesystem path to the bbolt database file.
// @return *boltStore A ready-to-use store backed by the opened database.
// @error error if the database cannot be opened or the bucket cannot be created.
//
// @testcase TestBoltStoreRoundTrip opens a store in a temp dir and round-trips a session.
func openBoltStore(path string) (*boltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening session store %q: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, berr := tx.CreateBucketIfNotExists(sessionsBucket)
		return berr
	}); err != nil {
		// Close the half-open db before surfacing the real initialization error.
		_ = db.Close()
		return nil, fmt.Errorf("initializing session store %q: %w", path, err)
	}
	return &boltStore{db: db}, nil
}

// Save writes one session as JSON keyed by its token.
//
// @arg ps The session snapshot to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestBoltStoreRoundTrip saves a session and reads it back.
func (b *boltStore) Save(ps persistedSession) error {
	data, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("encoding session: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).Put([]byte(ps.Token), data)
	})
}

// Delete removes the session for a token.
//
// @arg token The token whose session to delete.
// @error error if the write transaction fails.
//
// @testcase TestBoltStoreDelete deletes a session and confirms it is gone.
func (b *boltStore) Delete(token string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).Delete([]byte(token))
	})
}

// LoadAll returns every persisted session.
//
// @return []persistedSession One entry per stored token.
// @error error if a read transaction or decoding fails.
//
// @testcase TestBoltStoreRoundTrip loads the stored sessions back.
func (b *boltStore) LoadAll() ([]persistedSession, error) {
	var out []persistedSession
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).ForEach(func(_, v []byte) error {
			var ps persistedSession
			if derr := json.Unmarshal(v, &ps); derr != nil {
				return fmt.Errorf("decoding session: %w", derr)
			}
			out = append(out, ps)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Close releases the underlying bbolt database.
//
// @error error if closing the database fails.
//
// @testcase TestBoltStoreRoundTrip closes the store when done.
func (b *boltStore) Close() error { return b.db.Close() }
