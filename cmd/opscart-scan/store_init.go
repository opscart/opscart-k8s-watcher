package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

var (
	stateless bool
	dbPath    string

	// opStore is the operational memory store, initialized once in main()
	// before any subcommand runs, and shared package-wide (same threading
	// pattern as the flag vars above).
	opStore store.Store
)

// defaultScanDBPath returns ~/.opscart/scan.db, creating the .opscart
// directory (user-only permissions, since it may hold operational metadata
// about the user's clusters) if it doesn't already exist.
func defaultScanDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".opscart")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "scan.db"), nil
}

// initStore resolves the operational memory store from the --stateless and
// --db-path flags, mirroring opscart-dashboard's SQLiteStore-vs-NullStore
// selection (see cmd/opscart-dashboard/main.go:runDashboard). It never
// fails: a bad or unopenable path falls back to NullStore instead of
// crashing the CLI.
func initStore() store.Store {
	if stateless {
		return &store.NullStore{}
	}

	path := dbPath
	if path == "" {
		p, err := defaultScanDBPath()
		if err != nil {
			log.Printf("store: persistence disabled (%v)", err)
			return &store.NullStore{}
		}
		path = p
	}

	sqlDB, err := store.OpenSQLite(path)
	if err != nil {
		log.Printf("store: persistence disabled (%v)", err)
		return &store.NullStore{}
	}
	log.Printf("opscart-scan: operational memory at %s", path)
	return sqlDB
}
