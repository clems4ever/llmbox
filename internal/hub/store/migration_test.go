package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// legacySessionsSchema is the sessions table exactly as an older build created
// it, before the box_state/last_seen/name/image/instance_state columns existed.
const legacySessionsSchema = `
CREATE TABLE sessions (
	token         TEXT PRIMARY KEY,
	container_id  TEXT NOT NULL,
	authorize_url TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	hook_state    TEXT,
	box_id        TEXT NOT NULL,
	description   TEXT NOT NULL,
	spoke_name    TEXT NOT NULL,
	status        TEXT NOT NULL,
	session_url   TEXT NOT NULL,
	err           TEXT NOT NULL,
	activated_by  TEXT NOT NULL
);`

// TestSQLiteMigratesLegacySessions checks Open upgrades a database created by an
// older build: the new sessions columns are added in place, an existing row
// decodes with its zero-value new fields (empty box state = running, zero
// last-seen), and the migrated table accepts writes carrying the new fields.
func TestSQLiteMigratesLegacySessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Create a database with the pre-migration schema and one row in it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(legacySessionsSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sessions (token, container_id, authorize_url, created_at, hook_state,
			box_id, description, spoke_name, status, session_url, err, activated_by)
		VALUES ('tok', 'cid', 'https://auth', '2026-01-02T03:04:05Z', NULL,
			'web-box', 'desc', 'edge', 'pending', '', '', '')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// Open runs the migration; the legacy row must load with zero new fields.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrating): %v", err)
	}
	defer st.Close()
	got, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 || got[0].Token != "tok" {
		t.Fatalf("legacy row not loaded: %+v", got)
	}
	if got[0].BoxState != "" || !got[0].LastSeen.IsZero() || got[0].Name != "" || got[0].Image != "" || got[0].InstanceState != "" {
		t.Errorf("legacy row should decode with zero new fields: %+v", got[0])
	}

	// The migrated table must accept the new fields.
	upd := got[0]
	upd.BoxState = BoxStateTerminated
	upd.Name = "n1"
	if err := st.Save(upd); err != nil {
		t.Fatalf("Save after migration: %v", err)
	}
	reread, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after save: %v", err)
	}
	if len(reread) != 1 || reread[0].BoxState != BoxStateTerminated || reread[0].Name != "n1" {
		t.Errorf("migrated write not round-tripped: %+v", reread)
	}
}
