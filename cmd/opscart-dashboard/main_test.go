package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name            string
		dbPersistent    bool
		wantPersistence string
	}{
		{name: "SQLiteStore", dbPersistent: true, wantPersistence: "persistent"},
		{name: "NullStore", dbPersistent: false, wantPersistence: "ephemeral"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var db store.Store
			if tt.dbPersistent {
				sqlDB, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
				if err != nil {
					t.Fatalf("OpenSQLite: %v", err)
				}
				defer sqlDB.Close()
				db = sqlDB
			} else {
				db = &store.NullStore{}
			}

			srv := newServer([]string{"test-ctx"}, db, 90, tt.dbPersistent)

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			srv.handleHealth(rec, req)

			resp := rec.Result()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want %q", ct, "application/json")
			}

			var body struct {
				Status      string `json:"status"`
				Persistence string `json:"persistence"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != "ok" {
				t.Errorf("status field = %q, want %q", body.Status, "ok")
			}
			if body.Persistence != tt.wantPersistence {
				t.Errorf("persistence = %q, want %q", body.Persistence, tt.wantPersistence)
			}
		})
	}
}
