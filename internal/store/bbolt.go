package store

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/clems4ever/llmbox/internal/cluster"
)

// sessionsBucket is the bbolt bucket holding one JSON-encoded PersistedSession
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
	proxiesBucket       = []byte("proxies")
)

// boltStore is a Store backed by a single bbolt database file.
type boltStore struct {
	db *bolt.DB
}

// Open opens (creating if needed) a bbolt-backed Store at path.
//
// @arg path The filesystem path to the bbolt database file.
// @return Store A ready-to-use, bbolt-backed store.
// @error error if the database cannot be opened or initialized.
//
// @testcase TestBoltStoreRoundTrip opens a store and round-trips a session.
func Open(path string) (Store, error) { return openBolt(path) }

// openBolt opens (creating if needed) a bbolt database at path and ensures every
// bucket exists.
//
// @arg path The filesystem path to the bbolt database file.
// @return *boltStore A ready-to-use store backed by the opened database.
// @error error if the database cannot be opened or a bucket cannot be created.
//
// @testcase TestBoltStoreRoundTrip opens a store in a temp dir and round-trips a session.
func openBolt(path string) (*boltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening session store %q: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{sessionsBucket, loginSessionsBucket, loginFlowsBucket, joinTokensBucket, spokesBucket, proxiesBucket} {
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
func (b *boltStore) Save(ps PersistedSession) error {
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
// @return []PersistedSession One entry per stored token.
// @error error if a read transaction or decoding fails.
//
// @testcase TestBoltStoreRoundTrip loads the stored sessions back.
func (b *boltStore) LoadAll() ([]PersistedSession, error) {
	var out []PersistedSession
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).ForEach(func(_, v []byte) error {
			var ps PersistedSession
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
func (b *boltStore) SaveLoginFlow(state string, f LoginFlow) error {
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
// @return LoginFlow The decoded flow when one matched.
// @return bool True when a flow matched, false otherwise.
// @error error if the transaction or decoding fails.
//
// @testcase TestLoginStoreFlowRoundTrip consumes a flow and finds it gone afterwards.
func (b *boltStore) TakeLoginFlow(state string) (LoginFlow, bool, error) {
	var (
		f     LoginFlow
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
		return LoginFlow{}, false, err
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
func (b *boltStore) SaveLoginSession(id string, s LoginSession) error {
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
// @return LoginSession The decoded session when one matched.
// @return bool True when a session matched, false otherwise.
// @error error if the read transaction or decoding fails.
//
// @testcase TestLoginStoreSessionRoundTrip reads back a stored login session.
func (b *boltStore) LoginSession(id string) (LoginSession, bool, error) {
	var (
		s     LoginSession
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
		return LoginSession{}, false, err
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
		// LoginFlow and LoginSession both carry an ExpiresAt; decode just that.
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

// ListJoinTokens returns every outstanding join token (hash ID, spoke name, and
// expiry).
//
// @return []cluster.JoinTokenInfo One entry per stored join token.
// @error error if a read transaction or decoding fails.
//
// @testcase TestClusterStoreJoinTokenListAndDelete lists and revokes join tokens.
func (b *boltStore) ListJoinTokens() ([]cluster.JoinTokenInfo, error) {
	var out []cluster.JoinTokenInfo
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(joinTokensBucket).ForEach(func(k, v []byte) error {
			var rec cluster.JoinTokenRecord
			if derr := json.Unmarshal(v, &rec); derr != nil {
				return fmt.Errorf("decoding join token: %w", derr)
			}
			out = append(out, cluster.JoinTokenInfo{ID: string(k), Name: rec.Name, ExpiresAt: rec.ExpiresAt})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteJoinToken removes a join token by its hash ID; deleting a missing one is
// a no-op.
//
// @arg hash The join token hash ID to delete.
// @error error if the write transaction fails.
//
// @testcase TestClusterStoreJoinTokenListAndDelete revokes a join token by ID.
func (b *boltStore) DeleteJoinToken(hash string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(joinTokensBucket).Delete([]byte(hash))
	})
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

// SaveProxy stores (creating or replacing) one proxy as JSON keyed by its slug.
//
// @arg rec The proxy record to persist.
// @error error if encoding or the write transaction fails.
//
// @testcase TestProxyStoreRoundTrip saves a proxy and reads it back.
func (b *boltStore) SaveProxy(rec ProxyRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding proxy: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(proxiesBucket).Put([]byte(rec.Slug), data)
	})
}

// GetProxy returns the proxy for a slug.
//
// @arg slug The proxy slug to look up.
// @return ProxyRecord The decoded record when one matched.
// @return bool True when a proxy matched, false otherwise.
// @error error if the read transaction or decoding fails.
//
// @testcase TestProxyStoreRoundTrip reads back a stored proxy and misses an unknown slug.
func (b *boltStore) GetProxy(slug string) (ProxyRecord, bool, error) {
	var (
		rec   ProxyRecord
		found bool
	)
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(proxiesBucket).Get([]byte(slug))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return ProxyRecord{}, false, err
	}
	return rec, found, nil
}

// ListProxies returns every enabled proxy.
//
// @return []ProxyRecord One entry per stored proxy.
// @error error if a read transaction or decoding fails.
//
// @testcase TestProxyStoreRoundTrip lists the stored proxies.
func (b *boltStore) ListProxies() ([]ProxyRecord, error) {
	var out []ProxyRecord
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(proxiesBucket).ForEach(func(_, v []byte) error {
			var rec ProxyRecord
			if derr := json.Unmarshal(v, &rec); derr != nil {
				return fmt.Errorf("decoding proxy: %w", derr)
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

// DeleteProxy removes a proxy by its slug; deleting a missing one is a no-op.
//
// @arg slug The proxy slug to delete.
// @error error if the write transaction fails.
//
// @testcase TestProxyStoreRoundTrip deletes a proxy and confirms it is gone.
func (b *boltStore) DeleteProxy(slug string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(proxiesBucket).Delete([]byte(slug))
	})
}
