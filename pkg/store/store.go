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
	GetIncidentTimeline(cluster string, fingerprint string) ([]IncidentEvent, error)
	BatchGetIncidentHistory(cluster string, fingerprints []string) (map[string]*IncidentRecord, error)
	BatchGetReopenCounts(ids []int64) (map[int64]int, error)
	QueryIncidents(f IncidentFilter) (items []IncidentSummary, total int, err error)
	GetMemoryScoreboard(cluster string) (*MemoryScoreboard, error)
	GetRecentEvents(cluster string, since time.Time, limit int) ([]RecentEvent, error)
	GetChangesSince(cluster string, since time.Time, limit int) ([]RecentEvent, error)
	PruneOlderThan(cluster string, cutoff time.Time) (pruned int, err error)
	Close() error
}

// GetEarliestRetainedObservation queries provenance when the selected store
// supports it. Stateless and other stores report no retained history.
func GetEarliestRetainedObservation(s Store, cluster string) (time.Time, error) {
	type provenanceStore interface {
		GetEarliestRetainedObservation(string) (time.Time, error)
	}
	if p, ok := s.(provenanceStore); ok {
		return p.GetEarliestRetainedObservation(cluster)
	}
	return time.Time{}, nil
}

// AtObservationBoundary reports whether an incident was first seen within
// five minutes either side of the earliest retained OMA observation.
func AtObservationBoundary(firstSeen, earliest time.Time) bool {
	if firstSeen.IsZero() || earliest.IsZero() {
		return false
	}
	delta := firstSeen.Sub(earliest)
	return delta >= -5*time.Minute && delta <= 5*time.Minute
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
	Fingerprint  string // "namespace/OwnerKind/owner-name/issue_type"
	Namespace    string
	Resource     string
	IssueType    string
	Severity     string
	DetailsJSON  string // per-type attributes, e.g. {"restarts":932}
	RestartCount int
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
	ID          int64 // 0 when unset (e.g. GetIncidentHistory does not look it up)
	Fingerprint string
	FirstSeen   time.Time
	LastSeen    time.Time
	Status      string // "active" / "resolved"
	DetailsJSON string
}

// IncidentEvent is a single append-only entry in an incident's operational
// timeline (detected, updated, resolved, reopened, ...).
type IncidentEvent struct {
	OccurredAt   time.Time
	EventType    string // DETECTED | UPDATED | RESOLVED | REOPENED
	EventReason  string // RestartMilestone | SeverityChanged | Detected | Resolved | Reopened
	RestartCount int
	Severity     string
	State        string
	Message      string
}

// IncidentFilter narrows a QueryIncidents call. Cluster is required; every
// other field is optional and its zero value means "no filter on this
// dimension".
type IncidentFilter struct {
	Cluster    string
	Text       string // matches resource, namespace, issue_type (LIKE, case-insensitive)
	Namespace  string // exact, empty = all
	IssueType  string // exact, empty = all
	Severity   string // exact, empty = all
	Status     string // "active" | "resolved" | "reopened" | "" (all)
	SinceFirst time.Time
	SortBy     string // "priority" | "severity" | "first_seen" | "last_seen" | "restarts"
	SortDesc   bool
	Page       int // 1-based
	PerPage    int // default 50, cap 200
}

// IncidentSummary is one row of a QueryIncidents result page.
type IncidentSummary struct {
	Fingerprint  string
	Namespace    string
	Resource     string
	IssueType    string
	Severity     string
	Status       string // active | resolved
	ReopenCount  int
	FirstSeen    time.Time
	LastSeen     time.Time
	RestartCount int
	Trend        string // restart-series trend: "accelerating" | "stable" | ""
}

// MemoryScoreboard summarizes a cluster's operational memory: how much
// incident history it has accrued and how it is trending.
type MemoryScoreboard struct {
	TotalSeen             int
	Resolved              int
	Reopened              int // count of DISTINCT incidents with >=1 REOPENED event
	Accelerating          int // count of active incidents whose Trend == "accelerating"
	LongestActiveDays     int
	LongestActiveName     string // resource name of that incident
	MostUnstableNamespace string // namespace with the most incidents (active+resolved)
	MostUnstableCount     int
}

// RecentEvent is one row of a cluster-wide recent-activity feed.
type RecentEvent struct {
	Resource    string
	EventReason string // "Detected" | "Resolved" | "Reopened" | "RestartMilestone" | "SeverityChanged"
	OccurredAt  time.Time
}

// RetentionCutoff computes the cutoff time for PruneOlderThan given a
// retention window in days. days <= 0 disables retention ("keep forever"),
// signaled by enabled=false; callers must not call PruneOlderThan in that
// case.
func RetentionCutoff(days int, now time.Time) (cutoff time.Time, enabled bool) {
	if days <= 0 {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, -days), true
}
