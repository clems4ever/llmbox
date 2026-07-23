package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// schema is the full relational schema, created idempotently on Open. Each record
// type maps to its own table with a primary-key column and one column per durable
// field, so the store can query state directly (e.g. purge by expiry) instead of
// scanning opaque blobs. The one map-shaped field (a box's hook state) is held as
// JSON text, the only value without a natural columnar shape.
//
// No usable bearer secret is stored in the clear. The api-key and spoke-credential
// hashes live in secret_hash columns; the remaining server-minted secrets — a
// box's bearer token, an identity session's cookie id, and an OIDC flow's
// state — are keyed by their SHA-256 hash (see store.HashToken), so the primary
// key holds the hash, not the token. The short-lived flow payload an OIDC
// handshake must replay verbatim (nonce, pkce_verifier, and the return target it
// bounces back to) stays reversible by necessity; it is guarded by the file's
// 0600 mode and a 10-minute expiry rather than hashing.
const schema = `
CREATE TABLE IF NOT EXISTS boxes (
	token          TEXT PRIMARY KEY,
	instance_id    TEXT NOT NULL,
	box_id         TEXT NOT NULL,
	spoke          TEXT NOT NULL,
	description    TEXT NOT NULL,
	status         TEXT NOT NULL,
	last_error     TEXT NOT NULL,
	hook_state     TEXT,
	lifecycle      TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	observed_name  TEXT NOT NULL,
	observed_image TEXT NOT NULL,
	observed_state TEXT NOT NULL,
	observed_at    TEXT
);
CREATE TABLE IF NOT EXISTS identity_sessions (
	session_id   TEXT PRIMARY KEY,
	email        TEXT NOT NULL,
	provider     TEXT NOT NULL,
	csrf_token   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	can_admin    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oidc_flows (
	state         TEXT PRIMARY KEY,
	provider      TEXT NOT NULL,
	return_to     TEXT NOT NULL,
	nonce         TEXT NOT NULL,
	pkce_verifier TEXT NOT NULL,
	expires_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spoke_join_tokens (
	secret_hash TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	backend     TEXT NOT NULL DEFAULT '',
	expires_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spokes (
	name        TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL,
	enrolled_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS proxies (
	slug        TEXT PRIMARY KEY,
	box_id      TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	port        INTEGER NOT NULL,
	spoke       TEXT NOT NULL,
	owner       TEXT NOT NULL,
	description TEXT NOT NULL,
	created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	secret_hash TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	expires_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS allowlist_groups (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL,
	ttl_seconds INTEGER NOT NULL,
	is_global   INTEGER NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS allowlist_group_domains (
	group_id TEXT NOT NULL,
	domain   TEXT NOT NULL,
	PRIMARY KEY (group_id, domain)
);
CREATE TABLE IF NOT EXISTS allowlist_box_groups (
	box_id   TEXT NOT NULL,
	group_id TEXT NOT NULL,
	PRIMARY KEY (box_id, group_id)
);
CREATE TABLE IF NOT EXISTS dns_audit (
	box_id     TEXT NOT NULL,
	domain     TEXT NOT NULL,
	verdict    TEXT NOT NULL,
	hits       INTEGER NOT NULL,
	first_seen TEXT NOT NULL,
	last_seen  TEXT NOT NULL,
	PRIMARY KEY (box_id, domain, verdict)
);
`

// sqliteStore is a Store backed by a single SQLite database file via the pure-Go
// modernc.org/sqlite driver (no cgo). A single open connection serializes writes,
// which the low-volume control plane can afford and which sidesteps SQLite's
// writer-lock contention entirely.
type sqliteStore struct {
	db *sql.DB
}

// Open opens (creating if needed) a SQLite-backed Store at path.
//
// @arg path The filesystem path to the SQLite database file.
// @return Store A ready-to-use, SQLite-backed store.
// @error error if the database cannot be opened or initialized.
//
// @testcase TestSQLiteStoreRoundTrip opens a store and round-trips a box.
func Open(path string) (Store, error) { return openSQLite(path) }

// openSQLite opens (creating if needed) a SQLite database at path, applies the
// pragmas for durable concurrent use, ensures every table exists, and migrates
// tables created by earlier builds up to the current schema.
//
// @arg path The filesystem path to the SQLite database file.
// @return *sqliteStore A ready-to-use store backed by the opened database.
// @error error if the database cannot be opened, the schema cannot be created, or the file permissions cannot be restricted.
//
// @testcase TestSQLiteStoreRoundTrip opens a store in a temp dir and round-trips a box.
// @testcase TestOpenRestrictsFilePermissions checks the state file and its WAL sidecars are 0600.
func openSQLite(path string) (*sqliteStore, error) {
	// WAL keeps the file durable across restarts; busy_timeout lets a contended
	// write wait rather than fail immediately; foreign_keys is the modern default.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening store %q: %w", path, err)
	}
	// One connection serializes access so SQLite never returns "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing store %q: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating store %q: %w", path, err)
	}
	// The state file (and its WAL sidecars) holds session data and secret hashes;
	// restrict it to the owner. Running the schema above has created all three
	// files by now. chmod on every open also repairs a file left world-readable by
	// an earlier build. The 0700 state directory is the primary gate; this is
	// defence in depth, so a chmod failure (e.g. a filesystem without POSIX modes)
	// is not fatal.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = db.Close()
			return nil, fmt.Errorf("restricting store permissions on %q: %w", path+suffix, err)
		}
	}
	return &sqliteStore{db: db}, nil
}

// migrate brings a database created by an earlier build up to the current
// schema. The schema's CREATE TABLE IF NOT EXISTS statements only cover new
// databases; columns added later must be retrofitted here. Each step probes the
// live schema and applies only when missing, so migrate is idempotent.
//
// @arg db The opened database to migrate.
// @error error if probing the schema or applying a migration fails.
//
// @testcase TestOpenMigratesJoinTokenBackend opens a pre-backend-column database and finds the column added.
func migrate(db *sql.DB) error {
	// spoke_join_tokens.backend was added after the table shipped; older files
	// lack it ('' means the backend was never recorded — treated as docker).
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spoke_join_tokens') WHERE name = 'backend'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("probing spoke_join_tokens.backend: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE spoke_join_tokens ADD COLUMN backend TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("adding spoke_join_tokens.backend: %w", err)
		}
	}
	// The box-activation handshake was removed when llmbox was reduced to pure box
	// infrastructure; its columns are dropped from databases created by an earlier
	// build so the schema matches what PutBox/GetIdentitySession now read and write.
	if err := dropColumns(db, "boxes", "owner", "authorize_url", "session_url"); err != nil {
		return err
	}
	if err := dropColumns(db, "identity_sessions", "can_activate"); err != nil {
		return err
	}
	if err := dropColumns(db, "oidc_flows", "return_token"); err != nil {
		return err
	}
	return nil
}

// dropColumns removes each named column from table when the live schema still has
// it, so the drop is idempotent and safe on databases already at the new schema.
//
// @arg db The opened database to alter.
// @arg table The table to drop columns from.
// @arg cols The columns to drop when present.
// @error error if probing the schema or dropping a column fails.
//
// @testcase TestOpenDropsActivationColumns opens a pre-reduction database and finds the columns gone.
func dropColumns(db *sql.DB, table string, cols ...string) error {
	for _, col := range cols {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil {
			return fmt.Errorf("probing %s.%s: %w", table, col, err)
		}
		if n == 0 {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, table, col)); err != nil {
			return fmt.Errorf("dropping %s.%s: %w", table, col, err)
		}
	}
	return nil
}

// encodeTime renders a time as a string the same way encoding/json does
// (RFC 3339 with nanoseconds, preserving the offset), so a value survives a
// store round-trip byte-for-byte and compares equal to the original.
//
// @arg t The time to encode.
// @return string The RFC 3339 nanosecond representation.
//
// @testcase TestSQLiteStoreRoundTrip relies on times surviving the round-trip unchanged.
func encodeTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

// decodeTime parses a time previously written by encodeTime, mirroring how
// encoding/json decodes a timestamp so the result matches the stored value.
//
// @arg s The RFC 3339 string to parse.
// @return time.Time The parsed time.
// @error error if s is not a valid RFC 3339 timestamp.
//
// @testcase TestSQLiteStoreRoundTrip reads back stored timestamps unchanged.
func decodeTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding timestamp %q: %w", s, err)
	}
	return t, nil
}

// PutBox writes (creating or replacing) one box keyed by its token.
//
// @arg b The box snapshot to persist.
// @error error if encoding the hook state or the write fails.
//
// @testcase TestSQLiteStoreRoundTrip saves a box and reads it back.
func (s *sqliteStore) PutBox(b Box) error {
	// A nil hook-state map is stored as SQL NULL so it decodes back to nil (not an
	// empty map), keeping the round-trip exact.
	var hookState any
	if b.HookState != nil {
		data, err := json.Marshal(b.HookState)
		if err != nil {
			return fmt.Errorf("encoding hook state: %w", err)
		}
		hookState = string(data)
	}
	// A zero ObservedAt (never observed) is stored as SQL NULL so it decodes back
	// to the zero time, keeping the round-trip exact.
	var observedAt any
	if !b.ObservedAt.IsZero() {
		observedAt = encodeTime(b.ObservedAt)
	}
	_, err := s.db.Exec(`
		INSERT INTO boxes (token, instance_id, box_id, spoke, description,
			status, last_error, hook_state, lifecycle,
			created_at, observed_name, observed_image, observed_state, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET
			instance_id=excluded.instance_id, box_id=excluded.box_id,
			spoke=excluded.spoke, description=excluded.description,
			status=excluded.status, last_error=excluded.last_error,
			hook_state=excluded.hook_state, lifecycle=excluded.lifecycle,
			created_at=excluded.created_at, observed_name=excluded.observed_name,
			observed_image=excluded.observed_image, observed_state=excluded.observed_state,
			observed_at=excluded.observed_at`,
		b.Token, b.InstanceID, b.BoxID, b.Spoke, b.Description,
		b.Status, b.LastError, hookState, string(b.Lifecycle),
		encodeTime(b.CreatedAt), b.ObservedName, b.ObservedImage, b.ObservedState, observedAt)
	if err != nil {
		return fmt.Errorf("saving box: %w", err)
	}
	return nil
}

// DeleteBox removes the box for a token; deleting a missing token is a no-op.
//
// @arg token The token whose box to delete.
// @error error if the write fails.
//
// @testcase TestSQLiteStoreDelete deletes a box and confirms it is gone.
func (s *sqliteStore) DeleteBox(token string) error {
	if _, err := s.db.Exec(`DELETE FROM boxes WHERE token = ?`, token); err != nil {
		return fmt.Errorf("deleting box: %w", err)
	}
	return nil
}

// ListBoxes returns every persisted box.
//
// @return []Box One entry per stored token.
// @error error if the query or row scanning fails.
//
// @testcase TestSQLiteStoreRoundTrip loads the stored boxes back.
func (s *sqliteStore) ListBoxes() ([]Box, error) {
	rows, err := s.db.Query(`
		SELECT token, instance_id, box_id, spoke, description,
			status, last_error, hook_state, lifecycle,
			created_at, observed_name, observed_image, observed_state, observed_at
		FROM boxes`)
	if err != nil {
		return nil, fmt.Errorf("loading boxes: %w", err)
	}
	defer rows.Close()
	var out []Box
	for rows.Next() {
		var (
			b          Box
			lifecycle  string
			createdAt  string
			hookState  sql.NullString
			observedAt sql.NullString
		)
		if err := rows.Scan(&b.Token, &b.InstanceID, &b.BoxID, &b.Spoke, &b.Description,
			&b.Status, &b.LastError, &hookState, &lifecycle,
			&createdAt, &b.ObservedName, &b.ObservedImage, &b.ObservedState, &observedAt); err != nil {
			return nil, fmt.Errorf("scanning box: %w", err)
		}
		b.Lifecycle = Lifecycle(lifecycle)
		if b.CreatedAt, err = decodeTime(createdAt); err != nil {
			return nil, err
		}
		if observedAt.Valid && observedAt.String != "" {
			if b.ObservedAt, err = decodeTime(observedAt.String); err != nil {
				return nil, err
			}
		}
		if hookState.Valid {
			if err := json.Unmarshal([]byte(hookState.String), &b.HookState); err != nil {
				return nil, fmt.Errorf("decoding hook state: %w", err)
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Close releases the underlying database.
//
// @error error if closing the database fails.
//
// @testcase TestSQLiteStoreRoundTrip closes the store when done.
func (s *sqliteStore) Close() error { return s.db.Close() }

// PutOIDCFlow stores the in-flight OIDC handshake state under the state key.
//
// @arg state The OAuth state parameter the flow is keyed by.
// @arg f The flow to persist.
// @error error if the write fails.
//
// @testcase TestIdentityStoreFlowRoundTrip saves a flow and takes it back once.
func (s *sqliteStore) PutOIDCFlow(state string, f OIDCFlow) error {
	_, err := s.db.Exec(`
		INSERT INTO oidc_flows (state, provider, return_to, nonce, pkce_verifier, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(state) DO UPDATE SET
			provider=excluded.provider,
			return_to=excluded.return_to, nonce=excluded.nonce,
			pkce_verifier=excluded.pkce_verifier, expires_at=excluded.expires_at`,
		state, f.Provider, f.ReturnTo, f.Nonce, f.PKCEVerifier, encodeTime(f.ExpiresAt))
	if err != nil {
		return fmt.Errorf("saving oidc flow: %w", err)
	}
	return nil
}

// TakeOIDCFlow returns and removes the flow for state in one transaction, so a
// flow can be used at most once.
//
// @arg state The OAuth state parameter to consume.
// @return OIDCFlow The decoded flow when one matched.
// @return bool True when a flow matched, false otherwise.
// @error error if the transaction or decoding fails.
//
// @testcase TestIdentityStoreFlowRoundTrip consumes a flow and finds it gone afterwards.
func (s *sqliteStore) TakeOIDCFlow(state string) (OIDCFlow, bool, error) {
	// DELETE ... RETURNING fetches and removes the row atomically, enforcing
	// one-time use without a separate read-then-delete race.
	var (
		f         OIDCFlow
		expiresAt string
	)
	err := s.db.QueryRow(`
		DELETE FROM oidc_flows WHERE state = ?
		RETURNING provider, return_to, nonce, pkce_verifier, expires_at`, state).
		Scan(&f.Provider, &f.ReturnTo, &f.Nonce, &f.PKCEVerifier, &expiresAt)
	if err == sql.ErrNoRows {
		return OIDCFlow{}, false, nil
	}
	if err != nil {
		return OIDCFlow{}, false, fmt.Errorf("taking oidc flow: %w", err)
	}
	if f.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return OIDCFlow{}, false, err
	}
	return f, true, nil
}

// PutIdentitySession stores a completed identity session under its opaque id.
//
// @arg id The opaque session id (the browser cookie value).
// @arg sess The session to persist.
// @error error if the write fails.
//
// @testcase TestIdentityStoreSessionRoundTrip saves and reads back an identity session.
func (s *sqliteStore) PutIdentitySession(id string, sess IdentitySession) error {
	_, err := s.db.Exec(`
		INSERT INTO identity_sessions (session_id, email, provider, csrf_token, expires_at, can_admin)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			email=excluded.email, provider=excluded.provider, csrf_token=excluded.csrf_token,
			expires_at=excluded.expires_at, can_admin=excluded.can_admin`,
		id, sess.Email, sess.Provider, sess.CSRFToken, encodeTime(sess.ExpiresAt), sess.CanAdmin)
	if err != nil {
		return fmt.Errorf("saving identity session: %w", err)
	}
	return nil
}

// GetIdentitySession returns the identity session for id.
//
// @arg id The opaque session id to look up.
// @return IdentitySession The decoded session when one matched.
// @return bool True when a session matched, false otherwise.
// @error error if the query or decoding fails.
//
// @testcase TestIdentityStoreSessionRoundTrip reads back a stored identity session.
func (s *sqliteStore) GetIdentitySession(id string) (IdentitySession, bool, error) {
	var (
		sess      IdentitySession
		expiresAt string
	)
	err := s.db.QueryRow(`
		SELECT email, provider, csrf_token, expires_at, can_admin
		FROM identity_sessions WHERE session_id = ?`, id).
		Scan(&sess.Email, &sess.Provider, &sess.CSRFToken, &expiresAt, &sess.CanAdmin)
	if err == sql.ErrNoRows {
		return IdentitySession{}, false, nil
	}
	if err != nil {
		return IdentitySession{}, false, fmt.Errorf("reading identity session: %w", err)
	}
	if sess.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return IdentitySession{}, false, err
	}
	return sess, true, nil
}

// DeleteIdentitySession removes an identity session; deleting a missing id is a no-op.
//
// @arg id The opaque session id to delete.
// @error error if the write fails.
//
// @testcase TestIdentityStoreSessionRoundTrip deletes a session and confirms it is gone.
func (s *sqliteStore) DeleteIdentitySession(id string) error {
	if _, err := s.db.Exec(`DELETE FROM identity_sessions WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("deleting identity session: %w", err)
	}
	return nil
}

// PurgeExpiredIdentities drops identity sessions and flows whose ExpiresAt is
// before now. Expiry is also enforced at read time, so this is housekeeping that
// keeps the tables from growing unbounded.
//
// @arg now The cutoff time; entries expiring before it are removed.
// @error error if either delete fails.
//
// @testcase TestIdentityStorePurgeExpired drops expired entries and keeps live ones.
func (s *sqliteStore) PurgeExpiredIdentities(now time.Time) error {
	cutoff := encodeTime(now)
	if _, err := s.db.Exec(`DELETE FROM identity_sessions WHERE expires_at < ?`, cutoff); err != nil {
		return fmt.Errorf("purging identity sessions: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM oidc_flows WHERE expires_at < ?`, cutoff); err != nil {
		return fmt.Errorf("purging oidc flows: %w", err)
	}
	return nil
}

// PutJoinToken stores a join token record keyed by the hash of its secret.
//
// @arg hash The hex SHA-256 of the join token secret, used as the key.
// @arg rec The join token record to persist.
// @error error if the write fails.
//
// @testcase TestClusterStoreJoinTokenRoundTrip stores and takes a join token.
func (s *sqliteStore) PutJoinToken(hash string, rec cluster.JoinTokenRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO spoke_join_tokens (secret_hash, name, backend, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(secret_hash) DO UPDATE SET name=excluded.name, backend=excluded.backend, expires_at=excluded.expires_at`,
		hash, rec.Name, rec.Backend, encodeTime(rec.ExpiresAt))
	if err != nil {
		return fmt.Errorf("saving join token: %w", err)
	}
	return nil
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
func (s *sqliteStore) TakeJoinToken(hash string) (cluster.JoinTokenRecord, bool, error) {
	var (
		rec       cluster.JoinTokenRecord
		expiresAt string
	)
	err := s.db.QueryRow(`
		DELETE FROM spoke_join_tokens WHERE secret_hash = ? RETURNING name, backend, expires_at`, hash).
		Scan(&rec.Name, &rec.Backend, &expiresAt)
	if err == sql.ErrNoRows {
		return cluster.JoinTokenRecord{}, false, nil
	}
	if err != nil {
		return cluster.JoinTokenRecord{}, false, fmt.Errorf("taking join token: %w", err)
	}
	if rec.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return cluster.JoinTokenRecord{}, false, err
	}
	return rec, true, nil
}

// ListJoinTokens returns every outstanding join token (hash ID, spoke name,
// recorded backend, and expiry).
//
// @return []cluster.JoinTokenInfo One entry per stored join token.
// @error error if the query or scanning fails.
//
// @testcase TestClusterStoreJoinTokenListAndDelete lists and revokes join tokens.
func (s *sqliteStore) ListJoinTokens() ([]cluster.JoinTokenInfo, error) {
	rows, err := s.db.Query(`SELECT secret_hash, name, backend, expires_at FROM spoke_join_tokens`)
	if err != nil {
		return nil, fmt.Errorf("listing join tokens: %w", err)
	}
	defer rows.Close()
	var out []cluster.JoinTokenInfo
	for rows.Next() {
		var (
			info      cluster.JoinTokenInfo
			expiresAt string
		)
		if err := rows.Scan(&info.ID, &info.Name, &info.Backend, &expiresAt); err != nil {
			return nil, fmt.Errorf("scanning join token: %w", err)
		}
		if info.ExpiresAt, err = decodeTime(expiresAt); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteJoinToken removes a join token by its hash ID; deleting a missing one is
// a no-op.
//
// @arg hash The join token hash ID to delete.
// @error error if the write fails.
//
// @testcase TestClusterStoreJoinTokenListAndDelete revokes a join token by ID.
func (s *sqliteStore) DeleteJoinToken(hash string) error {
	if _, err := s.db.Exec(`DELETE FROM spoke_join_tokens WHERE secret_hash = ?`, hash); err != nil {
		return fmt.Errorf("deleting join token: %w", err)
	}
	return nil
}

// PutAPIKey writes (creating or replacing) one API key keyed by its secret hash.
//
// @arg hash The hex SHA-256 of the key's secret (the store key).
// @arg rec The API key record to persist.
// @error error if the write fails.
//
// @testcase TestAPIKeyStoreRoundTrip stores a key this reads back by hash.
func (s *sqliteStore) PutAPIKey(hash string, rec APIKeyRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO api_keys (secret_hash, name, created_at, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(secret_hash) DO UPDATE SET name=excluded.name, created_at=excluded.created_at, expires_at=excluded.expires_at`,
		hash, rec.Name, encodeTime(rec.CreatedAt), encodeTime(rec.ExpiresAt))
	if err != nil {
		return fmt.Errorf("saving api key: %w", err)
	}
	return nil
}

// GetAPIKey returns the API key for a secret hash; the bool is false when none
// matches.
//
// @arg hash The hex SHA-256 of the presented key secret.
// @return APIKeyRecord The decoded record when one matched.
// @return bool True when a key matched, false otherwise.
// @error error if the query or decoding fails.
//
// @testcase TestAPIKeyStoreRoundTrip finds a stored key by hash and misses cleanly on an unknown one.
func (s *sqliteStore) GetAPIKey(hash string) (APIKeyRecord, bool, error) {
	var (
		rec                  APIKeyRecord
		createdAt, expiresAt string
	)
	err := s.db.QueryRow(`SELECT name, created_at, expires_at FROM api_keys WHERE secret_hash = ?`, hash).
		Scan(&rec.Name, &createdAt, &expiresAt)
	if err == sql.ErrNoRows {
		return APIKeyRecord{}, false, nil
	}
	if err != nil {
		return APIKeyRecord{}, false, fmt.Errorf("reading api key: %w", err)
	}
	if rec.CreatedAt, err = decodeTime(createdAt); err != nil {
		return APIKeyRecord{}, false, err
	}
	if rec.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return APIKeyRecord{}, false, err
	}
	return rec, true, nil
}

// ListAPIKeys returns every stored API key (hash ID, name, and validity window).
//
// @return []APIKeyInfo One entry per stored API key.
// @error error if the query or scanning fails.
//
// @testcase TestAPIKeyStoreListAndDelete lists and deletes API keys.
func (s *sqliteStore) ListAPIKeys() ([]APIKeyInfo, error) {
	rows, err := s.db.Query(`SELECT secret_hash, name, created_at, expires_at FROM api_keys`)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKeyInfo
	for rows.Next() {
		var (
			info                 APIKeyInfo
			createdAt, expiresAt string
		)
		if err := rows.Scan(&info.ID, &info.Name, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		if info.CreatedAt, err = decodeTime(createdAt); err != nil {
			return nil, err
		}
		if info.ExpiresAt, err = decodeTime(expiresAt); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteAPIKey removes an API key by its hash ID; deleting a missing one is a
// no-op.
//
// @arg hash The API key hash ID to delete.
// @error error if the write fails.
//
// @testcase TestAPIKeyStoreListAndDelete deletes an API key by ID.
func (s *sqliteStore) DeleteAPIKey(hash string) error {
	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE secret_hash = ?`, hash); err != nil {
		return fmt.Errorf("deleting api key: %w", err)
	}
	return nil
}

// PutSpoke stores (creating or replacing) an enrolled spoke keyed by its name.
//
// @arg name The spoke name key.
// @arg rec The spoke record to persist.
// @error error if the write fails.
//
// @testcase TestClusterStoreSpokeRoundTrip stores and reads back a spoke.
func (s *sqliteStore) PutSpoke(name string, rec cluster.SpokeRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO spokes (name, secret_hash, enrolled_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			secret_hash=excluded.secret_hash, enrolled_at=excluded.enrolled_at`,
		name, rec.CredentialHash, encodeTime(rec.EnrolledAt))
	if err != nil {
		return fmt.Errorf("saving spoke: %w", err)
	}
	return nil
}

// GetSpoke returns the enrolled spoke for a name.
//
// @arg name The spoke name to look up.
// @return cluster.SpokeRecord The decoded record when one matched.
// @return bool True when a spoke matched, false otherwise.
// @error error if the query or decoding fails.
//
// @testcase TestClusterStoreSpokeRoundTrip reads back a stored spoke.
func (s *sqliteStore) GetSpoke(name string) (cluster.SpokeRecord, bool, error) {
	var (
		rec        cluster.SpokeRecord
		enrolledAt string
	)
	err := s.db.QueryRow(`
		SELECT name, secret_hash, enrolled_at FROM spokes WHERE name = ?`, name).
		Scan(&rec.Name, &rec.CredentialHash, &enrolledAt)
	if err == sql.ErrNoRows {
		return cluster.SpokeRecord{}, false, nil
	}
	if err != nil {
		return cluster.SpokeRecord{}, false, fmt.Errorf("reading spoke: %w", err)
	}
	if rec.EnrolledAt, err = decodeTime(enrolledAt); err != nil {
		return cluster.SpokeRecord{}, false, err
	}
	return rec, true, nil
}

// ListSpokes returns every enrolled spoke.
//
// @return []cluster.SpokeRecord One entry per enrolled spoke.
// @error error if the query or scanning fails.
//
// @testcase TestClusterStoreSpokeRoundTrip lists the enrolled spokes.
func (s *sqliteStore) ListSpokes() ([]cluster.SpokeRecord, error) {
	rows, err := s.db.Query(`SELECT name, secret_hash, enrolled_at FROM spokes`)
	if err != nil {
		return nil, fmt.Errorf("listing spokes: %w", err)
	}
	defer rows.Close()
	var out []cluster.SpokeRecord
	for rows.Next() {
		var (
			rec        cluster.SpokeRecord
			enrolledAt string
		)
		if err := rows.Scan(&rec.Name, &rec.CredentialHash, &enrolledAt); err != nil {
			return nil, fmt.Errorf("scanning spoke: %w", err)
		}
		if rec.EnrolledAt, err = decodeTime(enrolledAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteSpoke removes an enrolled spoke; deleting a missing name is a no-op.
//
// @arg name The spoke name to delete.
// @error error if the write fails.
//
// @testcase TestClusterStoreSpokeRoundTrip deletes a spoke and confirms it is gone.
func (s *sqliteStore) DeleteSpoke(name string) error {
	if _, err := s.db.Exec(`DELETE FROM spokes WHERE name = ?`, name); err != nil {
		return fmt.Errorf("deleting spoke: %w", err)
	}
	return nil
}

// SaveProxy stores (creating or replacing) one proxy keyed by its slug.
//
// @arg rec The proxy record to persist.
// @error error if the write fails.
//
// @testcase TestProxyStoreRoundTrip saves a proxy and reads it back.
func (s *sqliteStore) SaveProxy(rec ProxyRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO proxies (slug, box_id, instance_id, port, spoke, owner, description, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			box_id=excluded.box_id, instance_id=excluded.instance_id, port=excluded.port,
			spoke=excluded.spoke, owner=excluded.owner, description=excluded.description,
			created_at=excluded.created_at`,
		rec.Slug, rec.BoxID, rec.InstanceID, rec.Port, rec.Spoke,
		rec.Owner, rec.Description, encodeTime(rec.CreatedAt))
	if err != nil {
		return fmt.Errorf("saving proxy: %w", err)
	}
	return nil
}

// GetProxy returns the proxy for a slug.
//
// @arg slug The proxy slug to look up.
// @return ProxyRecord The decoded record when one matched.
// @return bool True when a proxy matched, false otherwise.
// @error error if the query or decoding fails.
//
// @testcase TestProxyStoreRoundTrip reads back a stored proxy and misses an unknown slug.
func (s *sqliteStore) GetProxy(slug string) (ProxyRecord, bool, error) {
	rec, ok, err := scanProxy(s.db.QueryRow(`
		SELECT slug, box_id, instance_id, port, spoke, owner, description, created_at
		FROM proxies WHERE slug = ?`, slug))
	if err != nil {
		return ProxyRecord{}, false, fmt.Errorf("reading proxy: %w", err)
	}
	return rec, ok, nil
}

// scanProxy scans one proxy row, decoding its stored timestamp. It is shared by
// the single-row GetProxy and the per-row loop in ListProxies.
//
// @arg row A row positioned at a proxy's eight columns in schema order.
// @return ProxyRecord The decoded proxy when a row was present.
// @return bool True when a row was scanned, false on sql.ErrNoRows.
// @error error if scanning or timestamp decoding fails.
//
// @testcase TestProxyStoreRoundTrip reads a proxy back through this scan.
func scanProxy(row interface{ Scan(...any) error }) (ProxyRecord, bool, error) {
	var (
		rec       ProxyRecord
		createdAt string
	)
	err := row.Scan(&rec.Slug, &rec.BoxID, &rec.InstanceID, &rec.Port, &rec.Spoke,
		&rec.Owner, &rec.Description, &createdAt)
	if err == sql.ErrNoRows {
		return ProxyRecord{}, false, nil
	}
	if err != nil {
		return ProxyRecord{}, false, err
	}
	if rec.CreatedAt, err = decodeTime(createdAt); err != nil {
		return ProxyRecord{}, false, err
	}
	return rec, true, nil
}

// ListProxies returns every enabled proxy.
//
// @return []ProxyRecord One entry per stored proxy.
// @error error if the query or scanning fails.
//
// @testcase TestProxyStoreRoundTrip lists the stored proxies.
func (s *sqliteStore) ListProxies() ([]ProxyRecord, error) {
	rows, err := s.db.Query(`
		SELECT slug, box_id, instance_id, port, spoke, owner, description, created_at
		FROM proxies`)
	if err != nil {
		return nil, fmt.Errorf("listing proxies: %w", err)
	}
	defer rows.Close()
	var out []ProxyRecord
	for rows.Next() {
		rec, _, err := scanProxy(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning proxy: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteProxy removes a proxy by its slug; deleting a missing one is a no-op.
//
// @arg slug The proxy slug to delete.
// @error error if the write fails.
//
// @testcase TestProxyStoreRoundTrip deletes a proxy and confirms it is gone.
func (s *sqliteStore) DeleteProxy(slug string) error {
	if _, err := s.db.Exec(`DELETE FROM proxies WHERE slug = ?`, slug); err != nil {
		return fmt.Errorf("deleting proxy: %w", err)
	}
	return nil
}

// PutSetting writes (creating or replacing) the value for a settings key.
//
// @arg key The setting key.
// @arg value The value to store.
// @error error if the write fails.
//
// @testcase TestSettingsStoreRoundTrip stores a setting and reads it back.
func (s *sqliteStore) PutSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("saving setting %q: %w", key, err)
	}
	return nil
}

// GetSetting returns the value for a settings key.
//
// @arg key The setting key to look up.
// @return string The stored value when key is set.
// @return bool True when key is set, false otherwise.
// @error error if the query fails.
//
// @testcase TestSettingsStoreRoundTrip reads back a stored setting and misses an unset key.
func (s *sqliteStore) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading setting %q: %w", key, err)
	}
	return value, true, nil
}
