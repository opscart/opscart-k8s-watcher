package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

var _ Store = (*SQLiteStore)(nil)

// schemaVersion is stored in PRAGMA user_version. Bump it whenever the
// schema changes, and teach migrateSchema how to reach it from the
// previous version.
const schemaVersion = 4

// restartMilestones are the restart-count thresholds that generate a
// RestartMilestone timeline event when crossed.
var restartMilestones = []int{10, 50, 100, 500, 1000, 2500, 5000, 10000}

// resolveThreshold is the number of consecutive scans an active incident
// must be absent from before it is marked resolved. CrashLoopBackOff pods
// briefly report Running between crashes, so a single missed scan is not
// enough signal — debouncing over resolveThreshold scans avoids flapping
// resolved/reopened churn in the timeline.
const resolveThreshold = 3

// flapAbsorptionWindow is the gap after a RESOLVED event within which a
// reappearance is treated as a continuation of the same active period
// rather than a genuine recurrence. Some issue types (e.g. probe_failure)
// have underlying conditions that flap scan-to-scan, so the incident can
// satisfy neither the crash-loop nor probe-failure detection criteria for
// a scan or two, get resolved by the debounce above, and then immediately
// reappear — producing thousands of alternating RESOLVED/REOPENED events
// for what is really one continuous problem.
const flapAbsorptionWindow = 15 * time.Minute

const schema = `
PRAGMA auto_vacuum = INCREMENTAL;

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
CREATE INDEX IF NOT EXISTS idx_incidents_cluster_status ON incidents(cluster, status);
CREATE INDEX IF NOT EXISTS idx_incidents_cluster_ns ON incidents(cluster, namespace);
CREATE INDEX IF NOT EXISTS idx_incidents_cluster_first ON incidents(cluster, first_seen);

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
	db      *sql.DB
	closeMu sync.Mutex
	closed  bool
}

// OpenSQLite opens (and if necessary migrates) the SQLite database at path.
func OpenSQLite(path string) (*SQLiteStore, error) {
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		store, err := openSQLite(path)
		if err == nil {
			return store, nil
		}
		lastErr = err
		if !isRetryableSQLiteOpenError(err) || attempt == attempts {
			break
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	return nil, fmt.Errorf("open SQLite database %q: %w", path, lastErr)
}

func openSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("create database handle: %w", err)
	}
	closeOnError := func(err error) (*SQLiteStore, error) {
		_ = db.Close()
		return nil, err
	}
	// SQLite allows only a single writer; serialize all access through
	// one connection to avoid "database is locked" errors.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return closeOnError(fmt.Errorf("ping database: %w", err))
	}

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return closeOnError(fmt.Errorf("configure PRAGMA journal_mode=WAL: %w", err))
	}
	if !strings.EqualFold(journalMode, "wal") {
		return closeOnError(fmt.Errorf("verify PRAGMA journal_mode=WAL: database reported %q", journalMode))
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return closeOnError(fmt.Errorf("configure PRAGMA synchronous=NORMAL: %w", err))
	}
	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		return closeOnError(fmt.Errorf("verify PRAGMA synchronous=NORMAL: %w", err))
	}
	if synchronous != 1 {
		return closeOnError(fmt.Errorf("verify PRAGMA synchronous=NORMAL: database reported %d", synchronous))
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return closeOnError(fmt.Errorf("verify database schema version: %w", err))
	}

	switch {
	case version == 0:
		if _, err := db.Exec(schema); err != nil {
			return closeOnError(fmt.Errorf("create database schema: %w", err))
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return closeOnError(fmt.Errorf("record database schema version %d: %w", schemaVersion, err))
		}
	case version < schemaVersion:
		if err := migrateSchema(db); err != nil {
			return closeOnError(fmt.Errorf("migrate database schema from version %d to %d: %w", version, schemaVersion, err))
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return closeOnError(fmt.Errorf("record migrated database schema version %d: %w", schemaVersion, err))
		}
	case version > schemaVersion:
		return closeOnError(fmt.Errorf("verify database schema version: database version %d is newer than supported version %d", version, schemaVersion))
	}

	var verifiedVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&verifiedVersion); err != nil {
		return closeOnError(fmt.Errorf("verify database initialization: read schema version: %w", err))
	}
	if verifiedVersion != schemaVersion {
		return closeOnError(fmt.Errorf("verify database initialization: schema version is %d, want %d", verifiedVersion, schemaVersion))
	}

	return &SQLiteStore{db: db}, nil
}

func isRetryableSQLiteOpenError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6, 14: // SQLITE_BUSY, SQLITE_LOCKED, SQLITE_CANTOPEN
		return true
	default:
		return false
	}
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

	// Query-path indexes for the incidents registry (QueryIncidents). Added
	// unconditionally and idempotently regardless of the version being
	// migrated from — cheap to check, safe to re-run.
	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_incidents_cluster_status ON incidents(cluster, status);
		CREATE INDEX IF NOT EXISTS idx_incidents_cluster_ns ON incidents(cluster, namespace);
		CREATE INDEX IF NOT EXISTS idx_incidents_cluster_first ON incidents(cluster, first_seen);
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
	incidents = focusIncidentsByFingerprint(incidents)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lookupStmt, err := tx.Prepare(
		`SELECT id, resource, current_restart_count, severity, status FROM incidents WHERE cluster=? AND fingerprint=?`,
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
			resource = excluded.resource,
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
		var prevResource, prevSeverity, prevStatus string
		lookupErr := lookupStmt.QueryRow(cluster, inc.Fingerprint).Scan(&existingID, &prevResource, &prevRestart, &prevSeverity, &prevStatus)
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
			var absorbed bool
			absorbed, err = absorbRecentResolution(tx, incidentID, now)
			if err != nil {
				return err
			}
			if !absorbed {
				err = insertIncidentEvent(tx, incidentID, scanID, now, "REOPENED", "Reopened",
					inc.RestartCount, inc.Severity, "active", "Incident reoccurred")
			} else {
				err = emitDriftEvents(tx, incidentID, scanID, now, prevSeverity, prevRestart, prevResource == inc.Resource, inc)
			}
		default:
			err = emitDriftEvents(tx, incidentID, scanID, now, prevSeverity, prevRestart, prevResource == inc.Resource, inc)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// focusIncidentsByFingerprint selects exactly one coherent observation for
// each workload-scoped fingerprint. The highest restart count wins; resource
// provides a stable tie-breaker so selection does not depend on scan order.
func focusIncidentsByFingerprint(incidents []IncidentData) []IncidentData {
	focused := make(map[string]IncidentData, len(incidents))
	for _, inc := range incidents {
		current, exists := focused[inc.Fingerprint]
		if !exists ||
			inc.RestartCount > current.RestartCount ||
			(inc.RestartCount == current.RestartCount && inc.Resource < current.Resource) {
			focused[inc.Fingerprint] = inc
		}
	}

	fingerprints := make([]string, 0, len(focused))
	for fingerprint := range focused {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)

	result := make([]IncidentData, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		result = append(result, focused[fingerprint])
	}
	return result
}

// absorbRecentResolution checks whether the incident's most recent RESOLVED
// event occurred within flapAbsorptionWindow of now. If so, that RESOLVED
// event was premature — it deletes it and returns true so the caller skips
// emitting a REOPENED event, treating the reappearance as a continuation of
// the same active period rather than a new one. Returns false (with no
// changes) if the incident has no RESOLVED event or the gap is too large,
// meaning the caller should treat this as a genuine recurrence.
func absorbRecentResolution(tx *sql.Tx, incidentID int64, now int64) (bool, error) {
	var eventID, occurredAt int64
	err := tx.QueryRow(
		`SELECT id, occurred_at FROM incident_events
		 WHERE incident_id=? AND event_type='RESOLVED'
		 ORDER BY occurred_at DESC LIMIT 1`,
		incidentID,
	).Scan(&eventID, &occurredAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if time.Duration(now-occurredAt)*time.Second >= flapAbsorptionWindow {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM incident_events WHERE id=?`, eventID); err != nil {
		return false, err
	}
	return true, nil
}

// emitDriftEvents records SeverityChanged/RestartMilestone events for an
// incident that is already (or was just flap-absorbed back to) active,
// comparing against its previously stored severity/restart count.
func emitDriftEvents(tx *sql.Tx, incidentID int64, scanID string, now int64, prevSeverity string, prevRestart int, sameResource bool, inc IncidentData) error {
	if prevSeverity != inc.Severity {
		if err := insertIncidentEvent(tx, incidentID, scanID, now, "UPDATED", "SeverityChanged",
			inc.RestartCount, inc.Severity, "active",
			fmt.Sprintf("Severity changed %s → %s", prevSeverity, inc.Severity)); err != nil {
			return err
		}
	}
	if sameResource {
		if milestone := highestMilestoneCrossed(prevRestart, inc.RestartCount); milestone > 0 {
			var lastMilestone int
			_ = tx.QueryRow(
				`SELECT COALESCE(MAX(restart_count), 0) FROM incident_events
			 WHERE incident_id=? AND event_reason='RestartMilestone'`,
				incidentID,
			).Scan(&lastMilestone)
			if lastMilestone < milestone {
				if err := insertIncidentEvent(tx, incidentID, scanID, now, "UPDATED", "RestartMilestone",
					inc.RestartCount, inc.Severity, "active",
					fmt.Sprintf("Restart count exceeded %d", milestone)); err != nil {
					return err
				}
			}
		}
	}
	return nil
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

func (s *SQLiteStore) recentSnapshots(cluster string, since time.Time, limit int) ([]snapshotRow, error) {
	rows, err := s.db.Query(
		`SELECT scanned_at, incident_score, critical_count, warning_count,
		        security_score, waste_count, monthly_cost, pod_count,
		        namespace_count, node_count
		 FROM cluster_snapshots
		 WHERE cluster = ? AND scanned_at >= ?
		 ORDER BY scanned_at DESC
		 LIMIT ?`,
		cluster, since.Unix(), limit,
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
	rows, err := s.recentSnapshots(cluster, time.Now().Add(-7*24*time.Hour), 20000)
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
	rows, err := s.recentSnapshots(cluster, time.Time{}, 1)
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

// BatchGetIncidentHistory returns operational-memory history for every
// fingerprint in fingerprints, keyed by fingerprint. One query for the
// whole set — never one query per fingerprint. Fingerprints with no
// matching incident are simply absent from the returned map.
func (s *SQLiteStore) BatchGetIncidentHistory(cluster string, fingerprints []string) (map[string]*IncidentRecord, error) {
	out := make(map[string]*IncidentRecord, len(fingerprints))
	if len(fingerprints) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(fingerprints))
	args := make([]any, 0, len(fingerprints)+1)
	args = append(args, cluster)
	for i, fp := range fingerprints {
		placeholders[i] = "?"
		args = append(args, fp)
	}

	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT id, fingerprint, first_seen, last_seen, status, details_json
		 FROM incidents
		 WHERE cluster = ? AND fingerprint IN (%s)`,
		strings.Join(placeholders, ","),
	), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, firstSeen, lastSeen int64
		var fingerprint string
		var status, detailsJSON sql.NullString
		if err := rows.Scan(&id, &fingerprint, &firstSeen, &lastSeen, &status, &detailsJSON); err != nil {
			return nil, err
		}
		out[fingerprint] = &IncidentRecord{
			ID:          id,
			Fingerprint: fingerprint,
			FirstSeen:   time.Unix(firstSeen, 0),
			LastSeen:    time.Unix(lastSeen, 0),
			Status:      status.String,
			DetailsJSON: detailsJSON.String,
		}
	}
	return out, rows.Err()
}

// BatchGetReopenCounts exports batchReopenCounts (already used internally
// by QueryIncidents) for callers outside this package that need reopen
// counts for a batch of incident ids without a per-id query.
func (s *SQLiteStore) BatchGetReopenCounts(ids []int64) (map[int64]int, error) {
	return s.batchReopenCounts(ids)
}

// inClause builds a "?,?,?" placeholder string and matching args slice for
// an IN(...) clause over ids. Callers must guard len(ids) == 0 themselves
// (an empty IN() is invalid SQL).
func inClause(ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

// orderByClause maps IncidentFilter.SortBy to a trusted (non-user-supplied)
// ORDER BY expression. Every branch is a fixed string from this switch, so
// interpolating it into the query below is not a SQL-injection risk. A
// secondary "id" sort keeps pagination deterministic when the primary key
// has ties.
func orderByClause(sortBy string, desc bool) string {
	col := "last_seen"
	switch sortBy {
	case "priority", "":
		return `CASE status WHEN 'active' THEN 0 ELSE 1 END ASC,
			CASE severity
				WHEN 'critical' THEN 0
				WHEN 'high' THEN 1
				WHEN 'medium' THEN 2
				WHEN 'low' THEN 3
				ELSE 4
			END ASC,
			first_seen DESC, fingerprint ASC`
	case "severity":
		return `CASE severity
			WHEN 'critical' THEN 0
			WHEN 'high' THEN 1
			WHEN 'medium' THEN 2
			WHEN 'low' THEN 3
			ELSE 4
		END ASC, first_seen DESC, fingerprint ASC`
	case "first_seen":
		col = "first_seen"
	case "restarts":
		col = "current_restart_count"
	case "last_seen":
		col = "last_seen"
	}
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	return col + " " + dir + ", fingerprint ASC"
}

// trendEvent is one DETECTED/RestartMilestone event, used by deriveTrend.
type trendEvent struct {
	occurredAt   int64
	restartCount int
}

// batchReopenCounts returns, for each incident id, the number of REOPENED
// events in its journal. One query for the whole page — never per-row.
func (s *SQLiteStore) batchReopenCounts(ids []int64) (map[int64]int, error) {
	counts := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return counts, nil
	}
	placeholders, args := inClause(ids)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT incident_id, COUNT(*) FROM incident_events
		 WHERE incident_id IN (%s) AND event_type = 'REOPENED'
		 GROUP BY incident_id`,
		placeholders,
	), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// batchTrendEvents returns, for each incident id, its two most recent
// DETECTED/RestartMilestone events (newest first) — the only journal
// entries that carry both a restart count and a timestamp without scanning
// every event. One windowed query for the whole page.
func (s *SQLiteStore) batchTrendEvents(ids []int64) (map[int64][]trendEvent, error) {
	out := make(map[int64][]trendEvent, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := inClause(ids)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT incident_id, occurred_at, restart_count FROM (
			SELECT incident_id, occurred_at, restart_count,
				ROW_NUMBER() OVER (
					PARTITION BY incident_id
					ORDER BY occurred_at DESC, id DESC
				) AS rn
			FROM incident_events
			WHERE incident_id IN (%s)
			  AND (event_type = 'DETECTED'
				OR (event_type = 'UPDATED' AND event_reason = 'RestartMilestone'))
		) WHERE rn <= 2
		ORDER BY incident_id, occurred_at DESC
	`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e trendEvent
		if err := rows.Scan(&id, &e.occurredAt, &e.restartCount); err != nil {
			return nil, err
		}
		out[id] = append(out[id], e)
	}
	return out, rows.Err()
}

// restartTrendApplies identifies findings backed by a pod restart series.
// Namespace posture, security/configuration, and unattached-resource findings
// have no meaningful restart trajectory and must not be labeled "stable".
func RestartTrendApplies(issueType string) bool {
	switch issueType {
	case "crash_loop", "oomkilled", "oom_killed", "probe_failure":
		return true
	default:
		return false
	}
}

// deriveTrend classifies an active, restart-applicable incident's trajectory as
// "accelerating" or "stable" from its two most recent milestone-relevant
// events (DETECTED and RestartMilestone) — the cheapest points in the
// journal that carry both a restart count and a timestamp.
//
// Heuristic: lifetimeRate is current restart count divided by active age in
// days (with a one-day minimum). recentRate is the restart delta per day
// between the two newest DETECTED/RestartMilestone journal points (with a
// one-hour minimum interval). A recent rate greater than 125% of lifetime rate
// is "accelerating". Otherwise it is "stable"; fewer than two points or a
// latest point older than 24 hours are also "stable" because no acceleration
// can be established from the available restart series.
//
// This intentionally approximates a 48h window with whatever two events
// happen to exist rather than sampling restart_count on a fixed schedule,
// so it stays a single indexed query with no per-row journal scan.
// "recovering" is reserved for a future trend dimension (e.g. severity
// de-escalation): restart counts are monotonically non-decreasing within an
// incident's active lifetime, so this heuristic never emits it.
func deriveTrend(events []trendEvent, currentRestart int, firstSeenUnix int64) string {
	now := time.Now().Unix()

	daysSinceFirst := float64(now-firstSeenUnix) / 86400
	if daysSinceFirst < 1 {
		daysSinceFirst = 1
	}
	lifetimeRate := float64(currentRestart) / daysSinceFirst

	if len(events) < 2 {
		return "stable"
	}
	// events[0] is the most recent (see batchTrendEvents' DESC ordering).
	latest, prev := events[0], events[1]

	if now-latest.occurredAt > 24*3600 {
		return "stable"
	}

	elapsedHours := float64(latest.occurredAt-prev.occurredAt) / 3600
	if elapsedHours < 1 {
		elapsedHours = 1
	}
	recentRate := float64(latest.restartCount-prev.restartCount) / (elapsedHours / 24)

	if recentRate > lifetimeRate*1.25 {
		return "accelerating"
	}
	return "stable"
}

// QueryIncidents implements the incidents registry: a filtered, sorted,
// paginated view over the incidents table plus derived per-row fields
// (ReopenCount, Trend) computed with two batched follow-up queries rather
// than per-row lookups.
func (s *SQLiteStore) QueryIncidents(f IncidentFilter) ([]IncidentSummary, int, error) {
	where := []string{"cluster = ?"}
	args := []any{f.Cluster}

	if f.Text != "" {
		like := "%" + f.Text + "%"
		where = append(where, "(resource LIKE ? OR namespace LIKE ? OR issue_type LIKE ?)")
		args = append(args, like, like, like)
	}
	if f.Namespace != "" {
		where = append(where, "namespace = ?")
		args = append(args, f.Namespace)
	}
	if f.IssueType != "" {
		where = append(where, "issue_type = ?")
		args = append(args, f.IssueType)
	}
	if f.Severity != "" {
		where = append(where, "severity = ?")
		args = append(args, f.Severity)
	}
	if !f.SinceFirst.IsZero() {
		where = append(where, "first_seen >= ?")
		args = append(args, f.SinceFirst.Unix())
	}
	switch f.Status {
	case "active":
		where = append(where, "status = 'active'")
	case "resolved":
		where = append(where, "status = 'resolved'")
	case "reopened":
		where = append(where, `status = 'active' AND id IN (
			SELECT incident_id FROM incident_events WHERE event_type = 'REOPENED'
		)`)
	}
	whereClause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM incidents "+whereClause, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 200 {
		perPage = 200
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	rowArgs := append(append([]any{}, args...), perPage, offset)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT id, fingerprint, namespace, resource, issue_type, severity,
			status, first_seen, last_seen, current_restart_count
		 FROM incidents %s ORDER BY %s LIMIT ? OFFSET ?`,
		whereClause, orderByClause(f.SortBy, f.SortDesc),
	), rowArgs...)
	if err != nil {
		return nil, 0, err
	}

	type rawRow struct {
		id                     int64
		fingerprint, namespace string
		resource, issueType    string
		severity, status       string
		firstSeen, lastSeen    int64
		restartCount           int
	}
	var raws []rawRow
	ids := make([]int64, 0, perPage)
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(
			&r.id, &r.fingerprint, &r.namespace, &r.resource, &r.issueType,
			&r.severity, &r.status, &r.firstSeen, &r.lastSeen, &r.restartCount,
		); err != nil {
			rows.Close()
			return nil, 0, err
		}
		raws = append(raws, r)
		ids = append(ids, r.id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()

	reopenCounts, err := s.batchReopenCounts(ids)
	if err != nil {
		return nil, 0, err
	}
	trendInputs, err := s.batchTrendEvents(ids)
	if err != nil {
		return nil, 0, err
	}

	items := make([]IncidentSummary, 0, len(raws))
	for _, r := range raws {
		summary := IncidentSummary{
			Fingerprint:  r.fingerprint,
			Namespace:    r.namespace,
			Resource:     r.resource,
			IssueType:    r.issueType,
			Severity:     r.severity,
			Status:       r.status,
			ReopenCount:  reopenCounts[r.id],
			FirstSeen:    time.Unix(r.firstSeen, 0),
			LastSeen:     time.Unix(r.lastSeen, 0),
			RestartCount: r.restartCount,
		}
		if r.status == "active" && RestartTrendApplies(r.issueType) {
			summary.Trend = deriveTrend(trendInputs[r.id], r.restartCount, r.firstSeen)
		}
		items = append(items, summary)
	}

	return items, total, nil
}

// countAccelerating counts active incidents whose Trend (computed the same
// way QueryIncidents derives it) is "accelerating".
func (s *SQLiteStore) countAccelerating(cluster string) (int, error) {
	rows, err := s.db.Query(
		`SELECT id, current_restart_count, first_seen FROM incidents
		 WHERE cluster = ? AND status = 'active'
		   AND issue_type IN ('crash_loop','oomkilled','oom_killed','probe_failure')`,
		cluster,
	)
	if err != nil {
		return 0, err
	}
	type activeRow struct {
		id           int64
		restartCount int
		firstSeen    int64
	}
	var actives []activeRow
	for rows.Next() {
		var r activeRow
		if err := rows.Scan(&r.id, &r.restartCount, &r.firstSeen); err != nil {
			rows.Close()
			return 0, err
		}
		actives = append(actives, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	if len(actives) == 0 {
		return 0, nil
	}

	ids := make([]int64, len(actives))
	for i, r := range actives {
		ids[i] = r.id
	}
	trendInputs, err := s.batchTrendEvents(ids)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, r := range actives {
		if deriveTrend(trendInputs[r.id], r.restartCount, r.firstSeen) == "accelerating" {
			count++
		}
	}
	return count, nil
}

// GetMemoryScoreboard summarizes a cluster's accrued incident history:
// totals, reopen/acceleration counts, the longest-running active incident,
// and the namespace with the most incident history.
func (s *SQLiteStore) GetMemoryScoreboard(cluster string) (*MemoryScoreboard, error) {
	sb := &MemoryScoreboard{}

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM incidents WHERE cluster = ?`, cluster,
	).Scan(&sb.TotalSeen); err != nil {
		return nil, err
	}

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM incidents WHERE cluster = ? AND status = 'resolved'`, cluster,
	).Scan(&sb.Resolved); err != nil {
		return nil, err
	}

	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT e.incident_id) FROM incident_events e
		 JOIN incidents i ON i.id = e.incident_id
		 WHERE i.cluster = ? AND e.event_type = 'REOPENED'`, cluster,
	).Scan(&sb.Reopened); err != nil {
		return nil, err
	}

	accelerating, err := s.countAccelerating(cluster)
	if err != nil {
		return nil, err
	}
	sb.Accelerating = accelerating

	var longestName sql.NullString
	var longestFirstSeen sql.NullInt64
	err = s.db.QueryRow(
		`SELECT resource, first_seen FROM incidents
		 WHERE cluster = ? AND status = 'active'
		 ORDER BY first_seen ASC LIMIT 1`, cluster,
	).Scan(&longestName, &longestFirstSeen)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if longestFirstSeen.Valid {
		sb.LongestActiveName = longestName.String
		sb.LongestActiveDays = int(time.Since(time.Unix(longestFirstSeen.Int64, 0)).Hours() / 24)
	}

	var unstableNS sql.NullString
	var unstableCount sql.NullInt64
	err = s.db.QueryRow(
		`SELECT namespace, COUNT(*) AS c FROM incidents WHERE cluster = ?
		 GROUP BY namespace ORDER BY c DESC LIMIT 1`, cluster,
	).Scan(&unstableNS, &unstableCount)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if unstableNS.Valid {
		sb.MostUnstableNamespace = unstableNS.String
		sb.MostUnstableCount = int(unstableCount.Int64)
	}

	return sb, nil
}

// GetRecentEvents returns the most recent incident_events for cluster that
// occurred at or after since, newest first, joined against their parent
// incident's resource name. Unlike GetChangesSince, this is intentionally
// unfiltered by event_reason — every event type, not just
// meaningfulEventReasons.
func (s *SQLiteStore) GetRecentEvents(cluster string, since time.Time, limit int) ([]RecentEvent, error) {
	rows, err := s.db.Query(
		`SELECT incidents.resource, incident_events.event_reason, incident_events.occurred_at
		 FROM incident_events
		 JOIN incidents ON incident_events.incident_id = incidents.id
		 WHERE incidents.cluster = ? AND incident_events.occurred_at >= ?
		 ORDER BY incident_events.occurred_at DESC LIMIT ?`,
		cluster, since.Unix(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecentEvent
	for rows.Next() {
		var resource string
		var eventReason sql.NullString
		var occurredAt int64
		if err := rows.Scan(&resource, &eventReason, &occurredAt); err != nil {
			return nil, err
		}
		out = append(out, RecentEvent{
			Resource:    resource,
			EventReason: eventReason.String,
			OccurredAt:  time.Unix(occurredAt, 0),
		})
	}
	return out, rows.Err()
}

// meaningfulEventReasons are the event_reason values worth surfacing in a
// "what changed" diff — every reason insertIncidentEvent ever writes (see
// UpsertIncidents/ResolveMissing/emitDriftEvents). There is currently no
// internal/housekeeping event_reason to exclude, but the allowlist is kept
// explicit so a future addition doesn't leak into this feed silently.
var meaningfulEventReasons = []string{"Detected", "Resolved", "Reopened", "RestartMilestone", "SeverityChanged"}

// GetChangesSince returns incident_events for cluster that occurred at or
// after since, newest first, limited to meaningfulEventReasons — a
// cursor-bounded diff of "what changed" rather than GetRecentEvents'
// unfiltered feed.
func (s *SQLiteStore) GetChangesSince(cluster string, since time.Time, limit int) ([]RecentEvent, error) {
	reasonPlaceholders := make([]string, len(meaningfulEventReasons))
	args := make([]any, 0, len(meaningfulEventReasons)+2)
	args = append(args, cluster)
	for i, reason := range meaningfulEventReasons {
		reasonPlaceholders[i] = "?"
		args = append(args, reason)
	}
	args = append(args, since.Unix(), limit)

	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT incidents.resource, incident_events.event_reason, incident_events.occurred_at
		 FROM incident_events
		 JOIN incidents ON incident_events.incident_id = incidents.id
		 WHERE incidents.cluster = ?
		   AND incident_events.event_reason IN (%s)
		   AND incident_events.occurred_at >= ?
		 ORDER BY incident_events.occurred_at DESC LIMIT ?`,
		strings.Join(reasonPlaceholders, ","),
	), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecentEvent
	for rows.Next() {
		var resource string
		var eventReason sql.NullString
		var occurredAt int64
		if err := rows.Scan(&resource, &eventReason, &occurredAt); err != nil {
			return nil, err
		}
		out = append(out, RecentEvent{
			Resource:    resource,
			EventReason: eventReason.String,
			OccurredAt:  time.Unix(occurredAt, 0),
		})
	}
	return out, rows.Err()
}

// PruneOlderThan removes data older than cutoff for cluster, in one
// transaction:
//   - Resolved incidents (and their full event history) are deleted once
//     last_seen falls before cutoff.
//   - Active incidents are never deleted regardless of age — only their
//     event journal is trimmed, and even then the DETECTED event is kept
//     forever so an incident's first-seen provenance always survives.
//   - cluster_snapshots and scan_history rows older than cutoff are deleted
//     outright — they carry no active/resolved notion of their own.
//
// The returned count is the number of resolved incidents deleted.
func (s *SQLiteStore) PruneOlderThan(cluster string, cutoff time.Time) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	cutoffUnix := cutoff.Unix()

	rows, err := tx.Query(
		`SELECT id FROM incidents WHERE cluster = ? AND status = 'resolved' AND last_seen < ?`,
		cluster, cutoffUnix,
	)
	if err != nil {
		return 0, err
	}
	var resolvedIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		resolvedIDs = append(resolvedIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	if len(resolvedIDs) > 0 {
		placeholders, idArgs := inClause(resolvedIDs)
		if _, err := tx.Exec(
			fmt.Sprintf(`DELETE FROM incident_events WHERE incident_id IN (%s)`, placeholders),
			idArgs...,
		); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			fmt.Sprintf(`DELETE FROM incidents WHERE id IN (%s)`, placeholders),
			idArgs...,
		); err != nil {
			return 0, err
		}
	}

	if _, err := tx.Exec(
		`DELETE FROM incident_events
		 WHERE occurred_at < ?
		   AND event_type != 'DETECTED'
		   AND incident_id IN (SELECT id FROM incidents WHERE cluster = ? AND status = 'active')`,
		cutoffUnix, cluster,
	); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(
		`DELETE FROM cluster_snapshots WHERE cluster = ? AND scanned_at < ?`,
		cluster, cutoffUnix,
	); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(
		`DELETE FROM scan_history WHERE cluster = ? AND scanned_at < ?`,
		cluster, cutoffUnix,
	); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	pruned := len(resolvedIDs)
	if pruned > 0 {
		// Reclaim the pages just freed above. PRAGMA incremental_vacuum
		// (not VACUUM) because: VACUUM rewrites the entire database file
		// and cannot run inside a transaction — on this store's
		// single-writer connection (SetMaxOpenConns(1) in OpenSQLite) it
		// would stall every other DB operation for its duration, which is
		// unacceptable on a per-scan-cycle cadence. incremental_vacuum only
		// touches pages already marked free by this prune, runs standalone
		// with no explicit transaction, and is checkpointed into the WAL
		// like any other write. It requires auto_vacuum=INCREMENTAL, set at
		// schema creation for new databases; databases created before this
		// schema version keep auto_vacuum=NONE (retrofitting it requires a
		// full VACUUM, which we avoid for the same reason), so this call is
		// simply a no-op for them — freed pages are still reused for new
		// rows either way, making this a disk-usage optimization rather
		// than a correctness concern. Best-effort: errors are ignored.
		_, _ = s.db.Exec("PRAGMA incremental_vacuum")
	}

	return pruned, nil
}

func (s *SQLiteStore) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	// All application workers and HTTP handlers must be stopped before Close.
	// A truncating checkpoint moves committed WAL frames into the main database
	// and removes the WAL dependency before the connection is released.
	var checkpointErr error
	var busy, logFrames, checkpointedFrames int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		checkpointErr = fmt.Errorf("checkpoint SQLite WAL: %w", err)
	} else if busy != 0 {
		checkpointErr = fmt.Errorf(
			"checkpoint SQLite WAL: busy=%d log_frames=%d checkpointed_frames=%d",
			busy, logFrames, checkpointedFrames,
		)
	}
	if err := s.db.Close(); err != nil {
		return errors.Join(checkpointErr, fmt.Errorf("close SQLite database: %w", err))
	}
	return checkpointErr
}
