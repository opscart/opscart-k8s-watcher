package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const defaultAuthUsername = "admin"

// authSecretDir is the fixed mount point for the OPSCART_AUTH_SECRET_NAME
// Secret, matching the Helm existingSecret pattern. Overridable in tests.
var authSecretDir = "/etc/opscart/auth"

// authSource identifies where basic-auth credentials came from.
type authSource string

const (
	authSourceEnv       authSource = "env"
	authSourceSecret    authSource = "secret"
	authSourceGenerated authSource = "generated"
)

// authConfig holds the resolved basic-auth credential. There is no field
// or code path anywhere that disables auth — every resolution path below
// produces a usable credential.
type authConfig struct {
	username string
	password string
	source   authSource
}

// resolveAuthConfig determines basic-auth credentials in priority order:
//  1. OPSCART_AUTH_USER / OPSCART_AUTH_PASS env vars, if both set
//  2. OPSCART_AUTH_SECRET_NAME, if set — read from the Secret mounted at
//     authSecretDir (username/password files)
//  3. otherwise, a random generated password with username "admin"
func resolveAuthConfig() (*authConfig, error) {
	if user, pass := os.Getenv("OPSCART_AUTH_USER"), os.Getenv("OPSCART_AUTH_PASS"); user != "" && pass != "" {
		return &authConfig{username: user, password: pass, source: authSourceEnv}, nil
	}

	if secretName := os.Getenv("OPSCART_AUTH_SECRET_NAME"); secretName != "" {
		user, err := readSecretField(filepath.Join(authSecretDir, "username"))
		if err != nil {
			return nil, fmt.Errorf("reading auth secret %q username: %w", secretName, err)
		}
		pass, err := readSecretField(filepath.Join(authSecretDir, "password"))
		if err != nil {
			return nil, fmt.Errorf("reading auth secret %q password: %w", secretName, err)
		}
		return &authConfig{username: user, password: pass, source: authSourceSecret}, nil
	}

	password, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("generating password: %w", err)
	}
	return &authConfig{username: defaultAuthUsername, password: password, source: authSourceGenerated}, nil
}

func readSecretField(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return value, nil
}

// generatePassword returns a random, base32-encoded password. 16 random
// bytes of crypto/rand entropy encode to 26 base32 characters, well above
// the 16-character minimum.
func generatePassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// logAuthConfig logs exactly one line describing the credential source. In
// the auto-generated case, a second line immediately follows with the
// credential itself — the only case where the operator has no other way
// to retrieve it.
func logAuthConfig(cfg *authConfig) {
	switch cfg.source {
	case authSourceEnv:
		log.Printf("auth: basic auth configured (source: env)")
	case authSourceSecret:
		log.Printf("auth: basic auth configured (source: secret)")
	case authSourceGenerated:
		log.Printf("auth: WARNING — using auto-generated password (see above). Configure OPSCART_AUTH_USER/PASS or OPSCART_AUTH_SECRET_NAME for a stable credential.")
		log.Printf("auth: username=%s password=%s", cfg.username, cfg.password)
	}
}

// basicAuthMiddleware wraps h so every request must present valid HTTP
// Basic credentials — no route, including /healthz, is exempt, and there
// is no configuration value that disables this check.
func basicAuthMiddleware(cfg *authConfig, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !credentialsMatch(cfg, user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="opscart", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func credentialsMatch(cfg *authConfig, user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.password)) == 1
	return userOK && passOK
}
