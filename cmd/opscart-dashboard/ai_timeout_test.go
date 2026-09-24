package main

import (
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

// TestConfigureAITimeout proves configureAITimeout stores a positive
// timeout, rejects a non-positive one with an error (never a process
// exit), and leaves newServer's safe default in place otherwise — a
// server/configuration helper must not terminate the process itself; that
// decision belongs to its caller (main.go). This is server-configuration
// behavior, not refinement-handler behavior — see ai_refine.go for how
// aiTimeout is actually used.
func TestConfigureAITimeout(t *testing.T) {
	srv := newTestServer()
	if srv.aiTimeout != aianalysis.DefaultTimeout {
		t.Fatalf("newServer's default aiTimeout = %s, want %s (unit-test servers must stay bounded even without startup configuration)", srv.aiTimeout, aianalysis.DefaultTimeout)
	}

	if err := srv.configureAITimeout(5 * time.Second); err != nil {
		t.Fatalf("configureAITimeout(5s) returned an error: %v", err)
	}
	if srv.aiTimeout != 5*time.Second {
		t.Fatalf("aiTimeout = %s, want 5s", srv.aiTimeout)
	}

	for _, invalid := range []time.Duration{0, -1 * time.Second} {
		before := srv.aiTimeout
		err := srv.configureAITimeout(invalid)
		if err == nil {
			t.Fatalf("configureAITimeout(%s) = nil error, want an error", invalid)
		}
		if srv.aiTimeout != before {
			t.Fatalf("configureAITimeout(%s) changed aiTimeout from %s to %s despite returning an error", invalid, before, srv.aiTimeout)
		}
	}
}
