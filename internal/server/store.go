package server

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/clems4ever/llmbox/internal/cluster"
)

// sessionsBucket is the bbolt bucket holding one JSON-encoded persistedSession
// per auth token. loginSessionsBucket and loginFlowsBucket hold the activation
// login state (see LoginStore): a signed-in user's session and the short-lived
// in-flight OIDC handshake state, respectively. joinTokensBucket and spokesBucket
// hold the cluster enrollment state (see cluster.Store): hashed one-time join
// tokens and the per-spoke bearer credentials minted from them.
var (
	sessionsBucket      = []byte("sessions")
	loginSessionsBucket = []byte("login_sessions")
	loginFlowsBucket    = []byte("login_flows")
	joinTokensBucket    = []byte("spoke_join_tokens")
	spokesBucket        = []byte("spokes")
)

// loginSession is a completed activation login, keyed in the store by an opaque
// random session ID (the value of the browser cookie). Its presence means the
// user authenticated and was authorized; CSRF guards the activation POST.
type loginSession struct {
	Email     string    `json:"email"`
	Provider  string    `json:"provider"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`
}

// loginFlow is the short-lived state of an in-flight OIDC handshake, keyed in the
// store by the OAuth state parameter. It is consumed (deleted) on callback.
type loginFlow struct {
	Provider    string    `json:"provider"`
	ReturnToken string    `json:"return_token"`
	Nonce       string    `json:"nonce"`
	Verifier    string    `json:"verifier"`
	ExpiresAt   time.Time `json:"expires_at"`
}

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
	SpokeName    string            `json:"spoke_name,omitempty"`
	Status       string            `json:"status"`
	SessionURL   string            `json:"session_url,omitempty"`
	Err          string            `json:"err,omitempty"`
	ActivatedBy  string            `json:"activated_by,omitempty"`
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

	LoginStore
	cluster.Store
}

// LoginStore persists the activation login state across restarts: completed
// login sessions (keyed by an opaque cookie ID) and the short-lived in-flight
// OIDC handshake state (keyed by the OAuth state parameter). All methods must be
// safe for concurrent use.
type LoginStore interface {
	// SaveLoginFlow stores the in-flight handshake state under the OAuth state key.
	SaveLoginFlow(state string, f loginFlow) error
	// TakeLoginFlow returns and removes the flow for state (one-time use); the bool
	// is false when no flow matches.
	TakeLoginFlow(state string) (loginFlow, bool, error)
	// SaveLoginSession stores a completed login session under its opaque id.
	SaveLoginSession(id string, s loginSession) error
	// LoginSession returns the session for id; the bool is false when none matches.
	LoginSession(id string) (loginSession, bool, error)
	// DeleteLoginSession removes a login session; deleting a missing id is a no-op.
	DeleteLoginSession(id string) error
	// PurgeExpiredLogins drops login sessions and flows that expired before now.
	PurgeExpiredLogins(now time.Time) error
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

// SaveLoginFlow discards the flow.
//
// @arg _ The OAuth state key.
// @arg _ The flow to (not) persist.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) SaveLoginFlow(_ string, _ loginFlow) error { return nil }

// TakeLoginFlow finds nothing.
//
// @arg _ The OAuth state key.
// @return loginFlow The zero flow.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) TakeLoginFlow(_ string) (loginFlow, bool, error) { return loginFlow{}, false, nil }

// SaveLoginSession discards the session.
//
// @arg _ The opaque session id.
// @arg _ The session to (not) persist.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) SaveLoginSession(_ string, _ loginSession) error { return nil }

// LoginSession finds nothing.
//
// @arg _ The opaque session id.
// @return loginSession The zero session.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) LoginSession(_ string) (loginSession, bool, error) {
	return loginSession{}, false, nil
}

// DeleteLoginSession does nothing.
//
// @arg _ The opaque session id.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) DeleteLoginSession(_ string) error { return nil }

// PurgeExpiredLogins does nothing.
//
// @arg _ The cutoff time.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) PurgeExpiredLogins(_ time.Time) error { return nil }

// PutJoinToken discards the token.
//
// @arg _ The token hash key.
// @arg _ The token record to (not) persist.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) PutJoinToken(_ string, _ cluster.JoinTokenRecord) error { return nil }

// TakeJoinToken finds nothing.
//
// @arg _ The token hash key.
// @return cluster.JoinTokenRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) TakeJoinToken(_ string) (cluster.JoinTokenRecord, bool, error) {
	return cluster.JoinTokenRecord{}, false, nil
}

// PutSpoke discards the spoke.
//
// @arg _ The spoke name key.
// @arg _ The spoke record to (not) persist.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) PutSpoke(_ string, _ cluster.SpokeRecord) error { return nil }

// GetSpoke finds nothing.
//
// @arg _ The spoke name key.
// @return cluster.SpokeRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) GetSpoke(_ string) (cluster.SpokeRecord, bool, error) {
	return cluster.SpokeRecord{}, false, nil
}

// ListSpokes returns no spokes.
//
// @return []cluster.SpokeRecord Always nil.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) ListSpokes() ([]cluster.SpokeRecord, error) { return nil, nil }

// DeleteSpoke does nothing.
//
// @arg _ The spoke name key.
// @error error Always nil.
//
// @testcase TestServerWithoutStore checks the server works with a no-op store.
func (noopStore) DeleteSpoke(_ string) error { return nil }

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
		for _, b := range [][]byte{sessionsBucket, loginSessionsBucket, loginFlowsBucket, joinTokensBucket, spokesBucket} {
			if _, berr := tx.CreateBucketIfNotExists(b); berr != nil {
				return berr
			}
		}
		return nil
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

// SaveLoginFlow stores the in-flight OIDC handshake state under the state key.
//
// @arg state The OAuth state parameter the flow is keyed by.
// @arg f The flow to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestLoginStoreFlowRoundTrip saves a flow and takes it back once.
func (b *boltStore) SaveLoginFlow(state string, f loginFlow) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encoding login flow: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(loginFlowsBucket).Put([]byte(state), data)
	})
}

// TakeLoginFlow returns and removes the flow for state in one transaction, so a
// flow can be used at most once.
//
// @arg state The OAuth state parameter to consume.
// @return loginFlow The decoded flow when one matched.
// @return bool True when a flow matched, false otherwise.
// @error error if the transaction or decoding fails.
//
// @testcase TestLoginStoreFlowRoundTrip consumes a flow and finds it gone afterwards.
func (b *boltStore) TakeLoginFlow(state string) (loginFlow, bool, error) {
	var (
		f     loginFlow
		found bool
	)
	err := b.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(loginFlowsBucket)
		v := bkt.Get([]byte(state))
		if v == nil {
			return nil
		}
		if derr := json.Unmarshal(v, &f); derr != nil {
			return fmt.Errorf("decoding login flow: %w", derr)
		}
		found = true
		return bkt.Delete([]byte(state))
	})
	if err != nil {
		return loginFlow{}, false, err
	}
	return f, found, nil
}

// SaveLoginSession stores a completed login session under its opaque id.
//
// @arg id The opaque session id (the browser cookie value).
// @arg s The session to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestLoginStoreSessionRoundTrip saves and reads back a login session.
func (b *boltStore) SaveLoginSession(id string, s loginSession) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encoding login session: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(loginSessionsBucket).Put([]byte(id), data)
	})
}

// LoginSession returns the login session for id.
//
// @arg id The opaque session id to look up.
// @return loginSession The decoded session when one matched.
// @return bool True when a session matched, false otherwise.
// @error error if the read transaction or decoding fails.
//
// @testcase TestLoginStoreSessionRoundTrip reads back a stored login session.
func (b *boltStore) LoginSession(id string) (loginSession, bool, error) {
	var (
		s     loginSession
		found bool
	)
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(loginSessionsBucket).Get([]byte(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &s)
	})
	if err != nil {
		return loginSession{}, false, err
	}
	return s, found, nil
}

// DeleteLoginSession removes a login session; deleting a missing id is a no-op.
//
// @arg id The opaque session id to delete.
// @error error if the write transaction fails.
//
// @testcase TestLoginStoreSessionRoundTrip deletes a session and confirms it is gone.
func (b *boltStore) DeleteLoginSession(id string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(loginSessionsBucket).Delete([]byte(id))
	})
}

// PurgeExpiredLogins drops login sessions and flows whose ExpiresAt is before
// now. Expiry is also enforced at read time, so this is housekeeping that keeps
// the buckets from growing unbounded.
//
// @arg now The cutoff time; entries expiring before it are removed.
// @error error if a transaction or decoding fails.
//
// @testcase TestLoginStorePurgeExpired drops expired entries and keeps live ones.
func (b *boltStore) PurgeExpiredLogins(now time.Time) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		// loginFlow and loginSession both carry an ExpiresAt; decode just that.
		var hdr struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		for _, name := range [][]byte{loginSessionsBucket, loginFlowsBucket} {
			bkt := tx.Bucket(name)
			var stale [][]byte
			if err := bkt.ForEach(func(k, v []byte) error {
				if jerr := json.Unmarshal(v, &hdr); jerr != nil {
					return fmt.Errorf("decoding login entry: %w", jerr)
				}
				if hdr.ExpiresAt.Before(now) {
					stale = append(stale, append([]byte(nil), k...))
				}
				return nil
			}); err != nil {
				return err
			}
			for _, k := range stale {
				if err := bkt.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// PutJoinToken stores a join token record (keyed by the hash of its secret) as
// JSON.
//
// @arg hash The hex SHA-256 of the join token secret, used as the key.
// @arg rec The join token record to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestClusterStoreJoinTokenRoundTrip stores and takes a join token.
func (b *boltStore) PutJoinToken(hash string, rec cluster.JoinTokenRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding join token: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(joinTokensBucket).Put([]byte(hash), data)
	})
}

// TakeJoinToken returns and removes the join token for a hash in one
// transaction, enforcing one-time use.
//
// @arg hash The hex SHA-256 of the presented join token secret.
// @return cluster.JoinTokenRecord The decoded record when one matched.
// @return bool True when a token matched, false otherwise.
// @error error if the transaction or decoding fails.
//
// @testcase TestClusterStoreJoinTokenRoundTrip takes a token once and finds it gone after.
func (b *boltStore) TakeJoinToken(hash string) (cluster.JoinTokenRecord, bool, error) {
	var (
		rec   cluster.JoinTokenRecord
		found bool
	)
	err := b.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(joinTokensBucket)
		v := bkt.Get([]byte(hash))
		if v == nil {
			return nil
		}
		if derr := json.Unmarshal(v, &rec); derr != nil {
			return fmt.Errorf("decoding join token: %w", derr)
		}
		found = true
		return bkt.Delete([]byte(hash))
	})
	if err != nil {
		return cluster.JoinTokenRecord{}, false, err
	}
	return rec, found, nil
}

// PutSpoke stores (creating or replacing) an enrolled spoke as JSON keyed by its
// name.
//
// @arg name The spoke name key.
// @arg rec The spoke record to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestClusterStoreSpokeRoundTrip stores and reads back a spoke.
func (b *boltStore) PutSpoke(name string, rec cluster.SpokeRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding spoke: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(spokesBucket).Put([]byte(name), data)
	})
}

// GetSpoke returns the enrolled spoke for a name.
//
// @arg name The spoke name to look up.
// @return cluster.SpokeRecord The decoded record when one matched.
// @return bool True when a spoke matched, false otherwise.
// @error error if the read transaction or decoding fails.
//
// @testcase TestClusterStoreSpokeRoundTrip reads back a stored spoke.
func (b *boltStore) GetSpoke(name string) (cluster.SpokeRecord, bool, error) {
	var (
		rec   cluster.SpokeRecord
		found bool
	)
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(spokesBucket).Get([]byte(name))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return cluster.SpokeRecord{}, false, err
	}
	return rec, found, nil
}

// ListSpokes returns every enrolled spoke.
//
// @return []cluster.SpokeRecord One entry per enrolled spoke.
// @error error if a read transaction or decoding fails.
//
// @testcase TestClusterStoreSpokeRoundTrip lists the enrolled spokes.
func (b *boltStore) ListSpokes() ([]cluster.SpokeRecord, error) {
	var out []cluster.SpokeRecord
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(spokesBucket).ForEach(func(_, v []byte) error {
			var rec cluster.SpokeRecord
			if derr := json.Unmarshal(v, &rec); derr != nil {
				return fmt.Errorf("decoding spoke: %w", derr)
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteSpoke removes an enrolled spoke; deleting a missing name is a no-op.
//
// @arg name The spoke name to delete.
// @error error if the write transaction fails.
//
// @testcase TestClusterStoreSpokeRoundTrip deletes a spoke and confirms it is gone.
func (b *boltStore) DeleteSpoke(name string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(spokesBucket).Delete([]byte(name))
	})
}
