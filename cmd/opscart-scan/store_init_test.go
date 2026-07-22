package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// withCleanFlags resets the package-level stateless/dbPath flag vars after
// the test, since they're shared global state parsed by cobra at runtime.
func withCleanFlags(t *testing.T) {
	t.Helper()
	origStateless, origDBPath := stateless, dbPath
	t.Cleanup(func() {
		stateless, dbPath = origStateless, origDBPath
	})
}

func TestDefaultScanDBPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := defaultScanDBPath()
	if err != nil {
		t.Fatalf("defaultScanDBPath: %v", err)
	}

	wantDir := filepath.Join(home, ".opscart")
	if got := filepath.Dir(path); got != wantDir {
		t.Fatalf("expected path under %s, got %s", wantDir, path)
	}
	if filepath.Base(path) != "scan.db" {
		t.Fatalf("expected file name scan.db, got %s", filepath.Base(path))
	}

	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf(".opscart dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", wantDir)
	}
}

func TestInitStore_Stateless(t *testing.T) {
	withCleanFlags(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	stateless = true
	dbPath = ""

	s := initStore()
	if _, ok := s.(*store.NullStore); !ok {
		t.Fatalf("expected *store.NullStore, got %T", s)
	}

	if err := s.WriteSnapshot("test-cluster", "scan-1", store.SnapshotData{}); err != nil {
		t.Fatalf("NullStore.WriteSnapshot returned error: %v", err)
	}
	snap, err := s.GetLatestSnapshot("test-cluster")
	if err != nil || snap != nil {
		t.Fatalf("expected no snapshot to be persisted in stateless mode, got %v, err=%v", snap, err)
	}

	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("expected no files created under HOME in stateless mode, found %v", entries)
	}
}

func TestInitStore_DBPathOverride(t *testing.T) {
	withCleanFlags(t)

	custom := filepath.Join(t.TempDir(), "custom.db")
	stateless = false
	dbPath = custom

	s := initStore()
	defer s.Close()

	if _, ok := s.(*store.SQLiteStore); !ok {
		t.Fatalf("expected *store.SQLiteStore, got %T", s)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("expected db file at override path %s: %v", custom, err)
	}
}

func TestInitStore_OpenFailureFallsBackToNullStore(t *testing.T) {
	withCleanFlags(t)

	// A directory can't be opened as a sqlite db file, forcing OpenSQLite
	// to fail so we can exercise the fallback path.
	stateless = false
	dbPath = t.TempDir()

	s := initStore()
	if _, ok := s.(*store.NullStore); !ok {
		t.Fatalf("expected fallback to *store.NullStore on open failure, got %T", s)
	}
}
