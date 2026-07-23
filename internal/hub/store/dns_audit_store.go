package store

import (
	"fmt"
	"strings"
	"time"
)

// defaultDNSAuditLimit caps how many audit rows a query returns when the filter
// names no limit, so the UI's default view stays bounded.
const defaultDNSAuditLimit = 500

// RecordDNSLookup folds one lookup into the aggregate row for its
// (box, domain, verdict), creating it or bumping its hit count and last-seen.
//
// @arg boxID The box that made the lookup.
// @arg domain The queried domain.
// @arg verdict The lookup verdict ("allowed", "blocked", ...).
// @arg at When the lookup happened.
// @error error if the write fails.
//
// @testcase TestDNSAuditStoreRoundTrip records lookups and aggregates repeats.
func (s *sqliteStore) RecordDNSLookup(boxID, domain, verdict string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO dns_audit (box_id, domain, verdict, hits, first_seen, last_seen)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(box_id, domain, verdict) DO UPDATE SET
			hits = hits + 1,
			last_seen = excluded.last_seen`,
		boxID, domain, verdict, encodeTime(at), encodeTime(at))
	if err != nil {
		return fmt.Errorf("recording dns lookup: %w", err)
	}
	return nil
}

// ListDNSAudit returns audit rows matching filter, most-recent first.
//
// @arg filter The query filter (zero fields match anything; Limit 0 uses the default).
// @return []DNSAuditEntry The matching rows, newest last-seen first.
// @error error if the query or scanning fails.
//
// @testcase TestDNSAuditStoreRoundTrip filters by box and verdict and orders by recency.
func (s *sqliteStore) ListDNSAudit(filter DNSAuditFilter) ([]DNSAuditEntry, error) {
	var (
		conds []string
		args  []any
	)
	if filter.BoxID != "" {
		conds = append(conds, "box_id = ?")
		args = append(args, filter.BoxID)
	}
	if filter.Verdict != "" {
		conds = append(conds, "verdict = ?")
		args = append(args, filter.Verdict)
	}
	if filter.Domain != "" {
		conds = append(conds, "domain LIKE ?")
		args = append(args, "%"+filter.Domain+"%")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultDNSAuditLimit
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT box_id, domain, verdict, hits, first_seen, last_seen
		FROM dns_audit `+where+`
		ORDER BY last_seen DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing dns audit: %w", err)
	}
	defer rows.Close()
	var out []DNSAuditEntry
	for rows.Next() {
		var (
			e                   DNSAuditEntry
			firstSeen, lastSeen string
		)
		if err := rows.Scan(&e.BoxID, &e.Domain, &e.Verdict, &e.Hits, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scanning dns audit: %w", err)
		}
		if e.FirstSeen, err = decodeTime(firstSeen); err != nil {
			return nil, err
		}
		if e.LastSeen, err = decodeTime(lastSeen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteDNSAuditForBox drops a box's audit rows.
//
// @arg boxID The box whose audit rows to delete.
// @error error if the write fails.
//
// @testcase TestDNSAuditStoreRoundTrip deletes a box's rows.
func (s *sqliteStore) DeleteDNSAuditForBox(boxID string) error {
	if _, err := s.db.Exec(`DELETE FROM dns_audit WHERE box_id = ?`, boxID); err != nil {
		return fmt.Errorf("deleting dns audit: %w", err)
	}
	return nil
}
