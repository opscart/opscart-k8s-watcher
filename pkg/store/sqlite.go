package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var _ Store = (*SQLiteStore)(nil)

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
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint  TEXT    NOT NULL,
    cluster      TEXT    NOT NULL,
    namespace    TEXT    NOT NULL,
    resource     TEXT    NOT NULL,
    issue_type   TEXT    NOT NULL,
    severity     TEXT    NOT NULL,
    first_seen   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL,
    details_json TEXT,
    status       TEXT    NOT NULL DEFAULT 'active',
    last_scan_id TEXT
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
		if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
			db.Close()
			return nil, err
		}
	case version > 1:
		db.Close()
		return nil, fmt.Errorf("database schema is newer than this version of opscart")
	}

	return &SQLiteStore{db: db}, nil
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

	stmt, err := tx.Prepare(`
		INSERT INTO incidents (
			fingerprint, cluster, namespace, resource, issue_type,
			severity, first_seen, last_seen, details_json, status, last_scan_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)
		ON CONFLICT(cluster, fingerprint) DO UPDATE SET
			last_seen = excluded.last_seen,
			details_json = excluded.details_json,
			severity = excluded.severity,
			status = 'active',
			last_scan_id = excluded.last_scan_id
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	for _, inc := range incidents {
		if _, err := stmt.Exec(
			inc.Fingerprint, cluster, inc.Namespace, inc.Resource, inc.IssueType,
			inc.Severity, now, now, inc.DetailsJSON, scanID,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) ResolveMissing(cluster string, scanID string) (int, error) {
	res, err := s.db.Exec(
		`UPDATE incidents SET status='resolved'
		 WHERE cluster=? AND status='active' AND last_scan_id != ?`,
		cluster, scanID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
