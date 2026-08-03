package store

import "time"

// NullStore is a no-op Store implementation used when persistence is
// disabled.
type NullStore struct{}

var _ Store = (*NullStore)(nil)

func (NullStore) WriteSnapshot(cluster string, scanID string, s SnapshotData) error {
	return nil
}

func (NullStore) UpsertIncidents(cluster string, scanID string, incidents []IncidentData) error {
	return nil
}

func (NullStore) ResolveMissing(cluster string, scanID string) (int, error) {
	return 0, nil
}

func (NullStore) WriteScanHistory(cluster string, scanID string, meta ScanMeta) error {
	return nil
}

func (NullStore) GetMemoryProvenance(cluster string) (*MemoryProvenance, error) {
	return nil, nil
}

func (NullStore) GetOverviewTrend(cluster string) (*OverviewTrend, error) {
	return &OverviewTrend{HasHistory: false}, nil
}

func (NullStore) GetLatestSnapshot(cluster string) (*SnapshotData, error) {
	return nil, nil
}

func (NullStore) GetIncidentHistory(cluster string, fingerprint string) (*IncidentRecord, error) {
	return nil, nil
}

func (NullStore) GetIncidentTimeline(cluster string, fingerprint string) ([]IncidentEvent, error) {
	return nil, nil
}

func (NullStore) BatchGetIncidentHistory(cluster string, fingerprints []string) (map[string]*IncidentRecord, error) {
	return nil, nil
}

func (NullStore) BatchGetReopenCounts(ids []int64) (map[int64]int, error) {
	return nil, nil
}

func (NullStore) QueryIncidents(f IncidentFilter) ([]IncidentSummary, int, error) {
	return nil, 0, nil
}

func (NullStore) GetMemoryScoreboard(cluster string) (*MemoryScoreboard, error) {
	return &MemoryScoreboard{}, nil
}

func (NullStore) GetRecentEvents(cluster string, since time.Time, limit int) ([]RecentEvent, error) {
	return nil, nil
}

func (NullStore) GetChangesSince(cluster string, since time.Time, limit int) ([]RecentEvent, error) {
	return nil, nil
}

func (NullStore) PruneOlderThan(cluster string, cutoff time.Time) (int, error) {
	return 0, nil
}

func (NullStore) Close() error {
	return nil
}
