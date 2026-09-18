package billing

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// AuthMode selects exactly one Azure credential type. There is no "auto"
// mode: azidentity.NewDefaultAzureCredential's chain tries several
// credential types and silently moves on when one fails, which would mean
// an operator's misconfigured Workload Identity federation could silently
// "succeed" against some other ambient identity instead of failing
// visibly. Requiring an explicit mode makes a failure in that mode a
// failure, full stop.
type AuthMode string

const (
	// AuthModeAzureCLI uses the local `az login` session — the local
	// dashboard path.
	AuthModeAzureCLI AuthMode = "azure-cli"
	// AuthModeWorkloadIdentity uses the federated token AKS's Workload
	// Identity webhook injects into the pod — the Helm/in-cluster path.
	AuthModeWorkloadIdentity AuthMode = "workload-identity"
)

// ParseAuthMode validates and normalizes an authMode config value. It never
// defaults to a mode — an empty or unrecognized value is always an error,
// so a configuration that omits authMode fails validation rather than
// silently picking one.
func ParseAuthMode(value string) (AuthMode, error) {
	switch AuthMode(strings.ToLower(strings.TrimSpace(value))) {
	case AuthModeAzureCLI:
		return AuthModeAzureCLI, nil
	case AuthModeWorkloadIdentity:
		return AuthModeWorkloadIdentity, nil
	default:
		return "", fmt.Errorf("authMode %q: use %q or %q", value, AuthModeAzureCLI, AuthModeWorkloadIdentity)
	}
}

// NewCredential constructs exactly the credential type mode names, using
// the maintained azidentity library, with no fallback to any other
// credential type on failure. A caller that gets an error here must treat
// it as fatal for this cluster's billing — never retry with a different
// identity source, since that would defeat the point of an explicit mode.
func NewCredential(mode AuthMode) (azcore.TokenCredential, error) {
	switch mode {
	case AuthModeAzureCLI:
		cred, err := azidentity.NewAzureCLICredential(nil)
		if err != nil {
			return nil, fmt.Errorf("azure-cli credential: %w", err)
		}
		return cred, nil
	case AuthModeWorkloadIdentity:
		cred, err := azidentity.NewWorkloadIdentityCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("workload-identity credential: %w", err)
		}
		return cred, nil
	default:
		return nil, fmt.Errorf("unsupported azure auth mode %q: use %q or %q", mode, AuthModeAzureCLI, AuthModeWorkloadIdentity)
	}
}
