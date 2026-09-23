package aianalysis

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type fakeTokenCredential struct {
	getToken func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)
}

func (credential fakeTokenCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return credential.getToken(ctx, options)
}

func TestAPIKeyAuthenticatorsUseProviderSpecificHeaders(t *testing.T) {
	tests := []struct {
		name        string
		auth        requestAuthenticator
		wantHeader  string
		wantValue   string
		otherHeader string
	}{
		{
			name:        "OpenAI bearer key",
			auth:        apiKeyAuthenticator{header: "Authorization", prefix: "Bearer ", key: "synthetic-key"},
			wantHeader:  "Authorization",
			wantValue:   "Bearer synthetic-key",
			otherHeader: "api-key",
		},
		{
			name:        "Azure API key",
			auth:        apiKeyAuthenticator{header: "api-key", key: "synthetic-key"},
			wantHeader:  "api-key",
			wantValue:   "synthetic-key",
			otherHeader: "Authorization",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			if err := test.auth.Authorize(context.Background(), request); err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if got := request.Header.Get(test.wantHeader); got != test.wantValue {
				t.Fatalf("%s = %q, want %q", test.wantHeader, got, test.wantValue)
			}
			if got := request.Header.Get(test.otherHeader); got != "" {
				t.Fatalf("%s = %q, want empty", test.otherHeader, got)
			}
		})
	}
}

func TestAzureTokenAuthenticatorUsesFixedScopeAndBearerHeader(t *testing.T) {
	credential := fakeTokenCredential{getToken: func(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
		if len(options.Scopes) != 1 || options.Scopes[0] != azureFoundryTokenScope {
			t.Fatalf("Scopes = %#v, want %q", options.Scopes, azureFoundryTokenScope)
		}
		return azcore.AccessToken{Token: "synthetic-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	}}
	request, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if err := (azureTokenAuthenticator{credential: credential}).Authorize(context.Background(), request); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer synthetic-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("api-key"); got != "" {
		t.Fatalf("api-key = %q, want empty", got)
	}
}

func TestAzureTokenAuthenticatorSanitizesCredentialError(t *testing.T) {
	const sensitive = "credential-output-containing-sensitive-detail"
	credential := fakeTokenCredential{getToken: func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, errors.New(sensitive)
	}}
	request, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	err = (azureTokenAuthenticator{credential: credential}).Authorize(context.Background(), request)
	if !errors.Is(err, errProviderAuthentication) {
		t.Fatalf("Authorize() error = %v, want authentication failure", err)
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("Authorize() leaked credential error: %v", err)
	}
}

func TestAzureTokenAuthenticatorPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	credential := fakeTokenCredential{getToken: func(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, ctx.Err()
	}}
	request, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if err := (azureTokenAuthenticator{credential: credential}).Authorize(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Authorize() error = %v, want context cancellation", err)
	}
}

func TestWorkloadIdentityConstructionErrorIsSanitized(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "")
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	_, err := newRequestAuthenticator(Config{}, ProviderAzureFoundry, AuthModeWorkloadIdentity)
	if err == nil {
		t.Fatal("newRequestAuthenticator() error = nil")
	}
	if got, want := err.Error(), "initialize Azure Workload Identity authentication"; got != want {
		t.Fatalf("newRequestAuthenticator() error = %q, want %q", got, want)
	}
}
