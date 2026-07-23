package store

import (
	"database/sql"
	"fmt"
	"sort"
)

// SaveAllowlistGroup writes (creating or replacing) one group and its domain set.
// The group row and its domains are written in a single transaction so a group is
// never left with a stale domain set: the old domains are cleared and the new set
// inserted wholesale.
//
// @arg g The group to persist; its Domains replace any previously stored set.
// @error error if the write fails.
//
// @testcase TestAllowlistStoreRoundTrip saves a group and reads it back with its domains.
// @testcase TestAllowlistStoreReplacesDomains re-saves a group and sees the domain set replaced.
func (s *sqliteStore) SaveAllowlistGroup(g AllowlistGroup) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin saving allowlist group: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		INSERT INTO allowlist_groups (id, name, description, ttl_seconds, is_global, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, description=excluded.description, ttl_seconds=excluded.ttl_seconds,
			is_global=excluded.is_global, updated_at=excluded.updated_at`,
		g.ID, g.Name, g.Description, g.TTLSeconds, g.IsGlobal,
		encodeTime(g.CreatedAt), encodeTime(g.UpdatedAt)); err != nil {
		return fmt.Errorf("saving allowlist group: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM allowlist_group_domains WHERE group_id = ?`, g.ID); err != nil {
		return fmt.Errorf("clearing allowlist group domains: %w", err)
	}
	for _, d := range g.Domains {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO allowlist_group_domains (group_id, domain) VALUES (?, ?)`,
			g.ID, d); err != nil {
			return fmt.Errorf("saving allowlist group domain: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit saving allowlist group: %w", err)
	}
	return nil
}

// GetAllowlistGroup returns the group for an ID together with its domains.
//
// @arg id The group ID to look up.
// @return AllowlistGroup The decoded group when one matched.
// @return bool True when a group matched, false otherwise.
// @error error if the query or decoding fails.
//
// @testcase TestAllowlistStoreRoundTrip reads back a stored group and misses an unknown id.
func (s *sqliteStore) GetAllowlistGroup(id string) (AllowlistGroup, bool, error) {
	g, ok, err := scanAllowlistGroup(s.db.QueryRow(`
		SELECT id, name, description, ttl_seconds, is_global, created_at, updated_at
		FROM allowlist_groups WHERE id = ?`, id))
	if err != nil {
		return AllowlistGroup{}, false, fmt.Errorf("reading allowlist group: %w", err)
	}
	if !ok {
		return AllowlistGroup{}, false, nil
	}
	if g.Domains, err = s.domainsFor(id); err != nil {
		return AllowlistGroup{}, false, err
	}
	return g, true, nil
}

// ListAllowlistGroups returns every group with its domains, ordered by name.
//
// @return []AllowlistGroup One entry per stored group.
// @error error if the query or scanning fails.
//
// @testcase TestAllowlistStoreRoundTrip lists the stored groups ordered by name.
func (s *sqliteStore) ListAllowlistGroups() ([]AllowlistGroup, error) {
	rows, err := s.db.Query(`
		SELECT id, name, description, ttl_seconds, is_global, created_at, updated_at
		FROM allowlist_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing allowlist groups: %w", err)
	}
	defer rows.Close()
	var out []AllowlistGroup
	for rows.Next() {
		g, _, err := scanAllowlistGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning allowlist group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach domains after the row cursor is closed to avoid nesting queries on
	// the single shared connection.
	for i := range out {
		domains, err := s.domainsFor(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Domains = domains
	}
	return out, nil
}

// DeleteAllowlistGroup removes a group, its domains, and every box assignment
// referencing it; deleting a missing group is a no-op.
//
// @arg id The group ID to delete.
// @error error if the write fails.
//
// @testcase TestAllowlistStoreRoundTrip deletes a group and confirms it and its assignments are gone.
func (s *sqliteStore) DeleteAllowlistGroup(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin deleting allowlist group: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM allowlist_group_domains WHERE group_id = ?`,
		`DELETE FROM allowlist_box_groups WHERE group_id = ?`,
		`DELETE FROM allowlist_groups WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return fmt.Errorf("deleting allowlist group: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deleting allowlist group: %w", err)
	}
	return nil
}

// SetBoxGroups replaces the set of non-global groups assigned to boxID.
//
// @arg boxID The box whose assignment is replaced.
// @arg groupIDs The group IDs to assign; an empty slice clears the assignment.
// @error error if the write fails.
//
// @testcase TestAllowlistBoxGroupsRoundTrip assigns groups to a box, reads them back, and replaces them.
func (s *sqliteStore) SetBoxGroups(boxID string, groupIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin setting box groups: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM allowlist_box_groups WHERE box_id = ?`, boxID); err != nil {
		return fmt.Errorf("clearing box groups: %w", err)
	}
	for _, gid := range groupIDs {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO allowlist_box_groups (box_id, group_id) VALUES (?, ?)`,
			boxID, gid); err != nil {
			return fmt.Errorf("assigning box group: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit setting box groups: %w", err)
	}
	return nil
}

// GetBoxGroups returns the non-global group IDs assigned to boxID, sorted.
//
// @arg boxID The box to look up.
// @return []string The assigned group IDs, sorted; empty when none.
// @error error if the query fails.
//
// @testcase TestAllowlistBoxGroupsRoundTrip reads back a box's assigned groups.
func (s *sqliteStore) GetBoxGroups(boxID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT group_id FROM allowlist_box_groups WHERE box_id = ? ORDER BY group_id`, boxID)
	if err != nil {
		return nil, fmt.Errorf("reading box groups: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			return nil, fmt.Errorf("scanning box group: %w", err)
		}
		out = append(out, gid)
	}
	return out, rows.Err()
}

// ListBoxGroups returns every box-to-groups assignment, keyed by box ID.
//
// @return map[string][]string One sorted group-ID slice per box that has any.
// @error error if the query fails.
//
// @testcase TestAllowlistBoxGroupsRoundTrip lists all box assignments.
func (s *sqliteStore) ListBoxGroups() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT box_id, group_id FROM allowlist_box_groups ORDER BY box_id, group_id`)
	if err != nil {
		return nil, fmt.Errorf("listing box groups: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var boxID, gid string
		if err := rows.Scan(&boxID, &gid); err != nil {
			return nil, fmt.Errorf("scanning box group: %w", err)
		}
		out[boxID] = append(out[boxID], gid)
	}
	return out, rows.Err()
}

// domainsFor returns a group's domains, sorted for stable output.
//
// @arg id The group ID whose domains to read.
// @return []string The group's domains, sorted; nil when none.
// @error error if the query fails.
func (s *sqliteStore) domainsFor(id string) ([]string, error) {
	rows, err := s.db.Query(`SELECT domain FROM allowlist_group_domains WHERE group_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("reading allowlist group domains: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scanning allowlist group domain: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// scanAllowlistGroup scans one group row (without domains), decoding its stored
// timestamps and integer flags. It is shared by GetAllowlistGroup and the per-row
// loop in ListAllowlistGroups.
//
// @arg row A row positioned at a group's seven columns in schema order.
// @return AllowlistGroup The decoded group (Domains unset) when a row was present.
// @return bool True when a row was scanned, false on sql.ErrNoRows.
// @error error if scanning or timestamp decoding fails.
func scanAllowlistGroup(row interface{ Scan(...any) error }) (AllowlistGroup, bool, error) {
	var (
		g                    AllowlistGroup
		createdAt, updatedAt string
	)
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.TTLSeconds, &g.IsGlobal, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return AllowlistGroup{}, false, nil
	}
	if err != nil {
		return AllowlistGroup{}, false, err
	}
	if g.CreatedAt, err = decodeTime(createdAt); err != nil {
		return AllowlistGroup{}, false, err
	}
	if g.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return AllowlistGroup{}, false, err
	}
	return g, true, nil
}
