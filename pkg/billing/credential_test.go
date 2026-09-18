package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

func TestParseAuthModeValid(t *testing.T) {
	for _, value := range []string{"azure-cli", "Azure-CLI", " azure-cli ", "workload-identity", "WORKLOAD-IDENTITY"} {
		if _, err := ParseAuthMode(value); err != nil {
			t.Errorf("ParseAuthMode(%q): %v", value, err)
		}
	}
}

func TestParseAuthModeRejectsUnknownOrEmpty(t *testing.T) {
	for _, value := range []string{"", "auto", "default", "managed-identity"} {
		if _, err := ParseAuthMode(value); err == nil {
			t.Errorf("ParseAuthMode(%q): expected an error, got nil", value)
		}
	}
}

func TestNewCredentialRejectsUnknownMode(t *testing.T) {
	if _, err := NewCredential(AuthMode("service-principal")); err == nil {
		t.Fatal("expected an error for an unrecognized auth mode, got nil")
	}
}

func TestNewCredentialAzureCLIConstructsWithoutContactingAzure(t *testing.T) {
	// AzureCLICredential's constructor never shells out to `az` — only
	// GetToken does — so this must succeed even where the Azure CLI isn't
	// installed, which is exactly the environment this test runs in.
	cred, err := NewCredential(AuthModeAzureCLI)
	if err != nil {
		t.Fatalf("NewCredential(azure-cli): %v", err)
	}
	if _, ok := cred.(*azidentity.AzureCLICredential); !ok {
		t.Errorf("NewCredential(azure-cli) returned %T, want *azidentity.AzureCLICredential", cred)
	}
}

func unsetEnvForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		old, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s): %v", key, err)
		}
		t.Cleanup(func() {
			if existed {
				os.Setenv(key, old)
			}
		})
	}
}

func TestNewCredentialWorkloadIdentityFailsExplicitlyWithoutFederationEnv(t *testing.T) {
	unsetEnvForTest(t, "AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_FEDERATED_TOKEN_FILE")
	if _, err := NewCredential(AuthModeWorkloadIdentity); err == nil {
		t.Fatal("expected workload-identity credential construction to fail without the webhook's federation env vars, got nil")
	}
}

func TestNewCredentialWorkloadIdentitySucceedsWithFederationEnv(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("AZURE_TENANT_ID", "22222222-2222-2222-2222-222222222222")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))

	cred, err := NewCredential(AuthModeWorkloadIdentity)
	if err != nil {
		t.Fatalf("NewCredential(workload-identity): %v", err)
	}
	if _, ok := cred.(*azidentity.WorkloadIdentityCredential); !ok {
		t.Errorf("NewCredential(workload-identity) returned %T, want *azidentity.WorkloadIdentityCredential", cred)
	}
}

func TestNewCredentialDoesNotFallBackBetweenModes(t *testing.T) {
	// Fully populate workload-identity's federation env, then request
	// azure-cli anyway: the returned credential must still be exactly
	// AzureCLICredential, never a substitution — there is no shared
	// "default chain" left to silently prefer one over the other.
	t.Setenv("AZURE_CLIENT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("AZURE_TENANT_ID", "22222222-2222-2222-2222-222222222222")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))

	cred, err := NewCredential(AuthModeAzureCLI)
	if err != nil {
		t.Fatalf("NewCredential(azure-cli): %v", err)
	}
	if _, ok := cred.(*azidentity.AzureCLICredential); !ok {
		t.Errorf("NewCredential(azure-cli) returned %T even with workload-identity env populated", cred)
	}
}

func TestEffectiveAuthModeNormalizesCase(t *testing.T) {
	cfg := validClusterConfig()
	cfg.AuthMode = "Workload-Identity"
	if got := cfg.EffectiveAuthMode(); got != AuthModeWorkloadIdentity {
		t.Errorf("EffectiveAuthMode() = %q, want %q", got, AuthModeWorkloadIdentity)
	}
}
