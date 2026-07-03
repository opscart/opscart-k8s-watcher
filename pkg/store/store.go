package store

import "time"

// Store is the operational memory layer. SQLiteStore implements it;
// NullStore is used when persistence is disabled.
type Store interface {
	WriteSnapshot(cluster string, scanID string, s SnapshotData) error
	UpsertIncidents(cluster string, scanID string, incidents []IncidentData) error
	ResolveMissing(cluster string, scanID string) (resolved int, err error)
	WriteScanHistory(cluster string, scanID string, meta ScanMeta) error
	GetOverviewTrend(cluster string) (*OverviewTrend, error)
	GetLatestSnapshot(cluster string) (*SnapshotData, error)
	GetIncidentHistory(cluster string, fingerprint string) (*IncidentRecord, error)
	Close() error
}

type SnapshotData struct {
	ScannedAt      time.Time
	IncidentScore  int
	CriticalCount  int
	WarningCount   int
	SecurityScore  int
	WasteCount     int
	MonthlyCost    float64
	PodCount       int
	NamespaceCount int
	NodeCount      int
}

type IncidentData struct {
	Fingerprint string // "namespace/OwnerKind/owner-name/issue_type"
	Namespace   string
	Resource    string
	IssueType   string
	Severity    string
	DetailsJSON string // per-type attributes, e.g. {"restarts":932}
}

type ScanMeta struct {
	DurationMS int64
	Success    bool
	Error      string
	Version    string
}

type MetricTrend struct {
	Current   int
	Previous  int
	Delta     int
	Direction string // "up" / "down" / "flat"
}

type OverviewTrend struct {
	HasHistory    bool // false when fewer than 2 snapshots exist
	IncidentScore MetricTrend
	CriticalCount MetricTrend
	SecurityScore MetricTrend
	WasteCount    MetricTrend
	CostCurrent   float64
	CostDelta     float64
	ScoreHistory  []int // last 7 incident scores, oldest first
}

type IncidentRecord struct {
	Fingerprint string
	FirstSeen   time.Time
	LastSeen    time.Time
	Status      string // "active" / "resolved"
	DetailsJSON string
}
