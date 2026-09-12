package store

import (
	"database/sql"
	"time"
)

// resolveAfter is how long an active incident must be continuously absent
// from trustworthy scan evidence, starting from its first observed absence,
// before it resolves. Preserves the historical ~3-minute effective behavior
// (resolveAfter ≈ the old 3-scan debounce at a 60s scan interval) as a
// wall-clock duration instead of a scan count, per docs/08 §3.
const resolveAfter = 3 * time.Minute

// ResolveMissing advances the absence lifecycle for every active incident
// not refreshed by the current scan.
//
// The first missing evaluation for an incident records absent_since as its
// absence start and leaves it active — it does not resolve on that same
// call, regardless of how stale last_seen already was. A later missing
// evaluation resolves the incident once resolveAfter has elapsed since
// absent_since. UpsertIncidents clears absent_since the instant the same
// evidence reappears (see its ON CONFLICT clause), so a reappearance before
// resolveAfter elapses restarts the clock with no extra bookkeeping here,
// and repeated ResolveMissing calls can never advance it on their own.
func (s *SQLiteStore) ResolveMissing(cluster string, scanID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	candidates, err := absentActiveIncidents(tx, cluster, scanID)
	if err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	resolved := 0
	for _, c := range candidates {
		if !c.absentSince.Valid {
			if err := markAbsent(tx, c.id, now); err != nil {
				return 0, err
			}
			continue
		}
		if !canResolve(c.absentSince.Int64, now) {
			continue
		}
		if err := markResolved(tx, c, scanID, now); err != nil {
			return 0, err
		}
		resolved++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return resolved, nil
}

// canResolve reports whether an absence that started at absentSinceUnix has
// lasted long enough, as of nowUnix, to resolve.
func canResolve(absentSinceUnix, nowUnix int64) bool {
	return time.Duration(nowUnix-absentSinceUnix)*time.Second >= resolveAfter
}

// absentIncident is one active incident not refreshed by the current scan.
// absentSince is NULL until the first missing evaluation records it.
type absentIncident struct {
	id          int64
	restart     int
	severity    string
	absentSince sql.NullInt64
}

// absentActiveIncidents returns every active incident whose evidence was not
// reconfirmed by scanID.
func absentActiveIncidents(tx *sql.Tx, cluster, scanID string) ([]absentIncident, error) {
	rows, err := tx.Query(
		`SELECT id, current_restart_count, severity, absent_since FROM incidents
		 WHERE cluster=? AND status='active' AND last_scan_id != ?`,
		cluster, scanID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []absentIncident
	for rows.Next() {
		var c absentIncident
		if err := rows.Scan(&c.id, &c.restart, &c.severity, &c.absentSince); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// markAbsent records nowUnix as the incident's absence start.
func markAbsent(tx *sql.Tx, id int64, nowUnix int64) error {
	_, err := tx.Exec(`UPDATE incidents SET absent_since=? WHERE id=?`, nowUnix, id)
	return err
}

// markResolved flips one incident to resolved and records the RESOLVED event.
func markResolved(tx *sql.Tx, c absentIncident, scanID string, nowUnix int64) error {
	if _, err := tx.Exec(`UPDATE incidents SET status='resolved' WHERE id=?`, c.id); err != nil {
		return err
	}
	return insertIncidentEvent(tx, c.id, scanID, nowUnix, "RESOLVED", "Resolved",
		c.restart, c.severity, "resolved", "Incident resolved")
}
