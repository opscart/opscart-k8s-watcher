package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var _ Store = (*SQLiteStore)(nil)

// schemaVersion is stored in PRAGMA user_version. Bump it whenever the
// schema changes, and teach migrateSchema how to reach it from the
// previous version.
const schemaVersion = 3

// restartMilestones are the restart-count thresholds that generate a
// RestartMilestone timeline event when crossed.
var restartMilestones = []int{10, 50, 100, 500, 1000, 2500, 5000, 10000}

// resolveThreshold is the number of consecutive scans an active incident
// must be absent from before it is marked resolved. CrashLoopBackOff pods
// briefly report Running between crashes, so a single missed scan is not
// enough signal — debouncing over resolveThreshold scans avoids flapping
// resolved/reopened churn in the timeline.
const resolveThreshold = 3

const schema = `
CREATE TABLE IF NOT EXISTS cluster_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id         TEXT    NOT NULL,
    cluster         TEXT    NOT NULL,
    scanned_at      INTEGER NOT NULL,
    incident_score  INTEGER NOT NULL,
    critical_count  INTEGER NOT NULL,
    warning_count   INTEGER NOT NULL,
    security_score  INTEGER NOT NULL,
    waste_count     INTEGER NOT NULL,
    monthly_cost    REAL    NOT NULL,
    pod_count       INTEGER,
    namespace_count INTEGER,
    node_count      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cs ON cluster_snapshots(cluster, scanned_at DESC);

CREATE TABLE IF NOT EXISTS incidents (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint           TEXT    NOT NULL,
    cluster               TEXT    NOT NULL,
    namespace             TEXT    NOT NULL,
    resource              TEXT    NOT NULL,
    issue_type            TEXT    NOT NULL,
    severity              TEXT    NOT NULL,
    first_seen            INTEGER NOT NULL,
    last_seen             INTEGER NOT NULL,
    details_json          TEXT,
    status                TEXT    NOT NULL DEFAULT 'active',
    last_scan_id          TEXT,
    current_restart_count INTEGER NOT NULL DEFAULT 0,
    missing_scans         INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_inc_fp ON incidents(cluster, fingerprint);

CREATE TABLE IF NOT EXISTS scan_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id     TEXT    NOT NULL,
    cluster     TEXT    NOT NULL,
    scanned_at  INTEGER NOT NULL,
    duration_ms INTEGER,
    success     INTEGER NOT NULL DEFAULT 1,
    error       TEXT,
    version     TEXT
);

CREATE TABLE IF NOT EXISTS incident_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_id   INTEGER NOT NULL,
    scan_id       TEXT    NOT NULL,
    occurred_at   INTEGER NOT NULL,
    event_type    TEXT    NOT NULL,
    event_reason  TEXT,
    restart_count INTEGER,
    severity      TEXT,
    state         TEXT,
    message       TEXT
);
CREATE INDEX IF NOT EXISTS idx_incident_events_incident ON incident_events(incident_id, occurred_at);
`

// SQLiteStore is a Store implementation backed by a local SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (and if necessary migrates) the SQLite database at path.
func OpenSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite allows only a single writer; serialize all access through
	// one connection to avoid "database is locked" errors.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}

	switch {
	case version == 0:
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, err
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	case version < schemaVersion:
		if err := migrateSchema(db); err != nil {
			db.Close()
			return nil, err
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	case version > schemaVersion:
		db.Close()
		return nil, fmt.Errorf("database schema is newer than this version of opscart")
	}

	return &SQLiteStore{db: db}, nil
}

// migrateSchema brings a database created by an older version of opscart
// up to schemaVersion. It is idempotent: safe to run on a database that
// already has some or all of the newer schema in place.
func migrateSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := migrateIncidentsTable(tx); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS incident_events (
		    id            INTEGER PRIMARY KEY AUTOINCREMENT,
		    incident_id   INTEGER NOT NULL,
		    scan_id       TEXT    NOT NULL,
		    occurred_at   INTEGER NOT NULL,
		    event_type    TEXT    NOT NULL,
		    event_reason  TEXT,
		    restart_count INTEGER,
		    severity      TEXT,
		    state         TEXT,
		    message       TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_incident_events_incident ON incident_events(incident_id, occurred_at);
	`); err != nil {
		return err
	}

	return tx.Commit()
}

// migrateIncidentsTable ensures the incidents table has an integer primary
// key and a current_restart_count column. SQLite cannot add a PRIMARY KEY
// via ALTER TABLE, so if id is missing the table is rebuilt; otherwise
// missing columns are added in place.
func migrateIncidentsTable(tx *sql.Tx) error {
	rows, err := tx.Query(`PRAGMA table_info(incidents)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	if !cols["id"] {
		if _, err := tx.Exec(`
			CREATE TABLE incidents_new (
			    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			    fingerprint           TEXT    NOT NULL,
			    cluster               TEXT    NOT NULL,
			    namespace             TEXT    NOT NULL,
			    resource              TEXT    NOT NULL,
			    issue_type            TEXT    NOT NULL,
			    severity              TEXT    NOT NULL,
			    first_seen            INTEGER NOT NULL,
			    last_seen             INTEGER NOT NULL,
			    details_json          TEXT,
			    status                TEXT    NOT NULL DEFAULT 'active',
			    last_scan_id          TEXT,
			    current_restart_count INTEGER NOT NULL DEFAULT 0,
			    missing_scans         INTEGER NOT NULL DEFAULT 0
			);
			INSERT INTO incidents_new (
			    fingerprint, cluster, namespace, resource, issue_type,
			    severity, first_seen, last_seen, details_json, status, last_scan_id
			)
			SELECT fingerprint, cluster, namespace, resource, issue_type,
			    severity, first_seen, last_seen, details_json, status, last_scan_id
			FROM incidents;
			DROP TABLE incidents;
			ALTER TABLE incidents_new RENAME TO incidents;
			CREATE UNIQUE INDEX IF NOT EXISTS idx_inc_fp ON incidents(cluster, fingerprint);
		`); err != nil {
			return err
		}
		return nil
	}

	if !cols["current_restart_count"] {
		if _, err := tx.Exec(`ALTER TABLE incidents ADD COLUMN current_restart_count INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}

	if !cols["missing_scans"] {
		if _, err := tx.Exec(`ALTER TABLE incidents ADD COLUMN missing_scans INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}

	return nil
}

// highestMilestoneCrossed returns the highest restart milestone strictly
// greater than prev and less than or equal to curr, or 0 if none crossed.
func highestMilestoneCrossed(prev, curr int) int {
	highest := 0
	for _, m := range restartMilestones {
		if prev < m && curr >= m {
			highest = m
		}
	}
	return highest
}

func insertIncidentEvent(tx *sql.Tx, incidentID int64, scanID string, occurredAt int64, eventType, eventReason string, restartCount int, severity, state, message string) error {
	_, err := tx.Exec(
		`INSERT INTO incident_events (
			incident_id, scan_id, occurred_at, event_type, event_reason,
			restart_count, severity, state, message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		incidentID, scanID, occurredAt, eventType, eventReason, restartCount, severity, state, message,
	)
	return err
}

func (s *SQLiteStore) WriteSnapshot(cluster string, scanID string, d SnapshotData) error {
	_, err := s.db.Exec(
		`INSERT INTO cluster_snapshots (
			scan_id, cluster, scanned_at, incident_score, critical_count,
			warning_count, security_score, waste_count, monthly_cost,
			pod_count, namespace_count, node_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		scanID, cluster, d.ScannedAt.Unix(), d.IncidentScore, d.CriticalCount,
		d.WarningCount, d.SecurityScore, d.WasteCount, d.MonthlyCost,
		d.PodCount, d.NamespaceCount, d.NodeCount,
	)
	return err
}

func (s *SQLiteStore) UpsertIncidents(cluster string, scanID string, incidents []IncidentData) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lookupStmt, err := tx.Prepare(
		`SELECT id, current_restart_count, severity, status FROM incidents WHERE cluster=? AND fingerprint=?`,
	)
	if err != nil {
		return err
	}
	defer lookupStmt.Close()

	upsertStmt, err := tx.Prepare(`
		INSERT INTO incidents (
			fingerprint, cluster, namespace, resource, issue_type,
			severity, first_seen, last_seen, details_json, status, last_scan_id,
			current_restart_count, missing_scans
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, 0)
		ON CONFLICT(cluster, fingerprint) DO UPDATE SET
			last_seen = excluded.last_seen,
			details_json = excluded.details_json,
			severity = excluded.severity,
			status = 'active',
			last_scan_id = excluded.last_scan_id,
			current_restart_count = excluded.current_restart_count,
			missing_scans = 0
	`)
	if err != nil {
		return err
	}
	defer upsertStmt.Close()

	now := time.Now().Unix()
	for _, inc := range incidents {
		var existingID int64
		var prevRestart int
		var prevSeverity, prevStatus string
		lookupErr := lookupStmt.QueryRow(cluster, inc.Fingerprint).Scan(&existingID, &prevRestart, &prevSeverity, &prevStatus)
		if lookupErr != nil && lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		isNew := lookupErr == sql.ErrNoRows

		res, err := upsertStmt.Exec(
			inc.Fingerprint, cluster, inc.Namespace, inc.Resource, inc.IssueType,
			inc.Severity, now, now, inc.DetailsJSON, scanID, inc.RestartCount,
		)
		if err != nil {
			return err
		}

		incidentID := existingID
		if isNew {
			incidentID, err = res.LastInsertId()
			if err != nil {
				return err
			}
		}

		switch {
		case isNew:
			err = insertIncidentEvent(tx, incidentID, scanID, now, "DETECTED", "Detected",
				inc.RestartCount, inc.Severity, "active",
				fmt.Sprintf("%s first detected", inc.IssueType))
		case prevStatus == "resolved":
			err = insertIncidentEvent(tx, incidentID, scanID, now, "REOPENED", "Reopened",
				inc.RestartCount, inc.Severity, "active", "Incident reoccurred")
		default:
			if prevSeverity != inc.Severity {
				if err = insertIncidentEvent(tx, incidentID, scanID, now, "UPDATED", "SeverityChanged",
					inc.RestartCount, inc.Severity, "active",
					fmt.Sprintf("Severity changed %s → %s", prevSeverity, inc.Severity)); err != nil {
					return err
				}
			}
			if milestone := highestMilestoneCrossed(prevRestart, inc.RestartCount); milestone > 0 {
				err = insertIncidentEvent(tx, incidentID, scanID, now, "UPDATED", "RestartMilestone",
					inc.RestartCount, inc.Severity, "active",
					fmt.Sprintf("Restart count exceeded %d", milestone))
			}
		}
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ResolveMissing debounces resolution over resolveThreshold consecutive
// scans: an active incident absent from the current scan has its
// missing_scans counter incremented, and only flips to status='resolved'
// (emitting one RESOLVED event) once that counter reaches resolveThreshold.
// UpsertIncidents resets missing_scans to 0 as soon as the incident
// reappears, so a resolution requires resolveThreshold *consecutive* misses.
func (s *SQLiteStore) ResolveMissing(cluster string, scanID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT id, current_restart_count, severity, missing_scans FROM incidents
		 WHERE cluster=? AND status='active' AND last_scan_id != ?`,
		cluster, scanID,
	)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id           int64
		restart      int
		severity     string
		missingScans int
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.restart, &c.severity, &c.missingScans); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	now := time.Now().Unix()
	resolved := 0
	for _, c := range candidates {
		missing := c.missingScans + 1
		if missing >= resolveThreshold {
			if _, err := tx.Exec(
				`UPDATE incidents SET status='resolved', missing_scans=? WHERE id=?`,
				missing, c.id,
			); err != nil {
				return 0, err
			}
			if err := insertIncidentEvent(tx, c.id, scanID, now, "RESOLVED", "Resolved",
				c.restart, c.severity, "resolved", "Incident resolved"); err != nil {
				return 0, err
			}
			resolved++
			continue
		}
		if _, err := tx.Exec(
			`UPDATE incidents SET missing_scans=? WHERE id=?`,
			missing, c.id,
		); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return resolved, nil
}

func (s *SQLiteStore) WriteScanHistory(cluster string, scanID string, meta ScanMeta) error {
	success := 0
	if meta.Success {
		success = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO scan_history (
			scan_id, cluster, scanned_at, duration_ms, success, error, version
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		scanID, cluster, time.Now().Unix(), meta.DurationMS, success, meta.Error, meta.Version,
	)
	return err
}

type snapshotRow struct {
	ScannedAt      int64
	IncidentScore  int
	CriticalCount  int
	WarningCount   int
	SecurityScore  int
	WasteCount     int
	MonthlyCost    float64
	PodCount       sql.NullInt64
	NamespaceCount sql.NullInt64
	NodeCount      sql.NullInt64
}

func (s *SQLiteStore) recentSnapshots(cluster string, limit int) ([]snapshotRow, error) {
	rows, err := s.db.Query(
		`SELECT scanned_at, incident_score, critical_count, warning_count,
			security_score, waste_count, monthly_cost, pod_count,
			namespace_count, node_count
		 FROM cluster_snapshots
		 WHERE cluster = ?
		 ORDER BY scanned_at DESC
		 LIMIT ?`,
		cluster, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []snapshotRow
	for rows.Next() {
		var r snapshotRow
		if err := rows.Scan(
			&r.ScannedAt, &r.IncidentScore, &r.CriticalCount, &r.WarningCount,
			&r.SecurityScore, &r.WasteCount, &r.MonthlyCost, &r.PodCount,
			&r.NamespaceCount, &r.NodeCount,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func trend(current, previous int) MetricTrend {
	delta := current - previous
	direction := "flat"
	if delta > 0 {
		direction = "up"
	} else if delta < 0 {
		direction = "down"
	}
	return MetricTrend{
		Current:   current,
		Previous:  previous,
		Delta:     delta,
		Direction: direction,
	}
}

func (s *SQLiteStore) GetOverviewTrend(cluster string) (*OverviewTrend, error) {
	rows, err := s.recentSnapshots(cluster, 7)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &OverviewTrend{HasHistory: false}, nil
	}

	scoreHistory := make([]int, len(rows))
	for i, r := range rows {
		scoreHistory[len(rows)-1-i] = r.IncidentScore
	}

	current := rows[0]

	if len(rows) == 1 {
		return &OverviewTrend{
			HasHistory:    false,
			IncidentScore: MetricTrend{Current: current.IncidentScore},
			CriticalCount: MetricTrend{Current: current.CriticalCount},
			SecurityScore: MetricTrend{Current: current.SecurityScore},
			WasteCount:    MetricTrend{Current: current.WasteCount},
			CostCurrent:   current.MonthlyCost,
			ScoreHistory:  scoreHistory,
		}, nil
	}

	currentTime := time.Unix(current.ScannedAt, 0)
	previous := rows[len(rows)-1]
	for _, r := range rows[1:] {
		if currentTime.Sub(time.Unix(r.ScannedAt, 0)) >= 24*time.Hour {
			previous = r
			break
		}
	}

	return &OverviewTrend{
		HasHistory:    true,
		IncidentScore: trend(current.IncidentScore, previous.IncidentScore),
		CriticalCount: trend(current.CriticalCount, previous.CriticalCount),
		SecurityScore: trend(current.SecurityScore, previous.SecurityScore),
		WasteCount:    trend(current.WasteCount, previous.WasteCount),
		CostCurrent:   current.MonthlyCost,
		CostDelta:     current.MonthlyCost - previous.MonthlyCost,
		ScoreHistory:  scoreHistory,
	}, nil
}

func (s *SQLiteStore) GetLatestSnapshot(cluster string) (*SnapshotData, error) {
	rows, err := s.recentSnapshots(cluster, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &SnapshotData{
		ScannedAt:      time.Unix(r.ScannedAt, 0),
		IncidentScore:  r.IncidentScore,
		CriticalCount:  r.CriticalCount,
		WarningCount:   r.WarningCount,
		SecurityScore:  r.SecurityScore,
		WasteCount:     r.WasteCount,
		MonthlyCost:    r.MonthlyCost,
		PodCount:       int(r.PodCount.Int64),
		NamespaceCount: int(r.NamespaceCount.Int64),
		NodeCount:      int(r.NodeCount.Int64),
	}, nil
}

func (s *SQLiteStore) GetIncidentHistory(cluster string, fingerprint string) (*IncidentRecord, error) {
	var firstSeen, lastSeen int64
	var status, detailsJSON sql.NullString

	err := s.db.QueryRow(
		`SELECT first_seen, last_seen, status, details_json
		 FROM incidents
		 WHERE cluster = ? AND fingerprint = ?`,
		cluster, fingerprint,
	).Scan(&firstSeen, &lastSeen, &status, &detailsJSON)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &IncidentRecord{
		Fingerprint: fingerprint,
		FirstSeen:   time.Unix(firstSeen, 0),
		LastSeen:    time.Unix(lastSeen, 0),
		Status:      status.String,
		DetailsJSON: detailsJSON.String,
	}, nil
}

func (s *SQLiteStore) GetIncidentTimeline(cluster string, fingerprint string) ([]IncidentEvent, error) {
	rows, err := s.db.Query(`
		SELECT e.occurred_at, e.event_type, e.event_reason, e.restart_count,
			e.severity, e.state, e.message
		FROM incident_events e
		JOIN incidents i ON i.id = e.incident_id
		WHERE i.cluster = ? AND i.fingerprint = ?
		ORDER BY e.occurred_at ASC, e.id ASC
	`, cluster, fingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IncidentEvent
	for rows.Next() {
		var occurredAt int64
		var eventType string
		var eventReason, severity, state, message sql.NullString
		var restartCount sql.NullInt64
		if err := rows.Scan(&occurredAt, &eventType, &eventReason, &restartCount, &severity, &state, &message); err != nil {
			return nil, err
		}
		out = append(out, IncidentEvent{
			OccurredAt:   time.Unix(occurredAt, 0),
			EventType:    eventType,
			EventReason:  eventReason.String,
			RestartCount: int(restartCount.Int64),
			Severity:     severity.String,
			State:        state.String,
			Message:      message.String,
		})
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
