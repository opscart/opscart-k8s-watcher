package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestResolveAuthConfigOrder(t *testing.T) {
	t.Run("env vars take priority", func(t *testing.T) {
		t.Setenv("OPSCART_AUTH_USER", "envuser")
		t.Setenv("OPSCART_AUTH_PASS", "envpass")
		t.Setenv("OPSCART_AUTH_SECRET_NAME", "some-secret")
		withSecretFiles(t, "secretuser", "secretpass")

		cfg, err := resolveAuthConfig()
		if err != nil {
			t.Fatalf("resolveAuthConfig: %v", err)
		}
		if cfg.source != authSourceEnv {
			t.Fatalf("source = %q, want %q", cfg.source, authSourceEnv)
		}
		if cfg.username != "envuser" || cfg.password != "envpass" {
			t.Fatalf("got %+v, want env credentials", cfg)
		}
	})

	t.Run("secret used when env not set", func(t *testing.T) {
		t.Setenv("OPSCART_AUTH_SECRET_NAME", "some-secret")
		withSecretFiles(t, "secretuser", "secretpass")

		cfg, err := resolveAuthConfig()
		if err != nil {
			t.Fatalf("resolveAuthConfig: %v", err)
		}
		if cfg.source != authSourceSecret {
			t.Fatalf("source = %q, want %q", cfg.source, authSourceSecret)
		}
		if cfg.username != "secretuser" || cfg.password != "secretpass" {
			t.Fatalf("got %+v, want secret credentials", cfg)
		}
	})

	t.Run("secret name set but files missing errors out", func(t *testing.T) {
		t.Setenv("OPSCART_AUTH_SECRET_NAME", "some-secret")
		original := authSecretDir
		authSecretDir = t.TempDir() // empty dir, no username/password files
		t.Cleanup(func() { authSecretDir = original })

		if _, err := resolveAuthConfig(); err == nil {
			t.Fatal("expected error when secret files are missing, got nil")
		}
	})

	t.Run("generated when nothing configured", func(t *testing.T) {
		cfg, err := resolveAuthConfig()
		if err != nil {
			t.Fatalf("resolveAuthConfig: %v", err)
		}
		if cfg.source != authSourceGenerated {
			t.Fatalf("source = %q, want %q", cfg.source, authSourceGenerated)
		}
		if cfg.username != defaultAuthUsername {
			t.Fatalf("username = %q, want %q", cfg.username, defaultAuthUsername)
		}
		if len(cfg.password) < 16 {
			t.Fatalf("generated password %q is %d chars, want >= 16", cfg.password, len(cfg.password))
		}
	})

	t.Run("partial env vars fall through to generated", func(t *testing.T) {
		t.Setenv("OPSCART_AUTH_USER", "onlyuser")

		cfg, err := resolveAuthConfig()
		if err != nil {
			t.Fatalf("resolveAuthConfig: %v", err)
		}
		if cfg.source != authSourceGenerated {
			t.Fatalf("source = %q, want %q", cfg.source, authSourceGenerated)
		}
	})
}

func TestGeneratePasswordEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		pw, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword: %v", err)
		}
		if len(pw) < 16 {
			t.Fatalf("password %q is %d chars, want >= 16", pw, len(pw))
		}
		if seen[pw] {
			t.Fatalf("generatePassword produced a duplicate: %q", pw)
		}
		seen[pw] = true
	}
}

func TestBasicAuthMiddleware(t *testing.T) {
	cfg := &authConfig{username: "admin", password: "s3cret", source: authSourceGenerated}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := basicAuthMiddleware(cfg, inner)

	t.Run("rejects unauthenticated request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("expected WWW-Authenticate header on 401 response")
		}
	})

	t.Run("rejects wrong credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth("admin", "wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("accepts correct credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth("admin", "s3cret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})
}

// withSecretFiles points authSecretDir at a temp directory populated with
// username/password files, restoring the original value on test cleanup.
func withSecretFiles(t *testing.T, username, password string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "username"), []byte(username), 0o600); err != nil {
		t.Fatalf("write username file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte(password), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	original := authSecretDir
	authSecretDir = dir
	t.Cleanup(func() { authSecretDir = original })
}

func TestMuxHealthzBypassesAuth(t *testing.T) {
	t.Setenv("OPSCART_AUTH_USER", "muxuser")
	t.Setenv("OPSCART_AUTH_PASS", "muxpass")

	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	mux := srv.newMux()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("/healthz WWW-Authenticate = %q, want empty (no auth challenge)", got)
	}
}

func TestMuxOtherRoutesRequireAuth(t *testing.T) {
	t.Setenv("OPSCART_AUTH_USER", "muxuser")
	t.Setenv("OPSCART_AUTH_PASS", "muxpass")

	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	mux := srv.newMux()

	routes := []string{"/", "/costs", "/api/summary", "/warroom", "/settings"}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want %d", route, rec.Code, http.StatusUnauthorized)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("%s: expected WWW-Authenticate header on 401 response", route)
			}
		})
	}
}
