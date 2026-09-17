package billing

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// DefaultCredential returns the maintained Azure identity library's default
// credential chain. It transparently covers both required authentication
// paths without separate code: WorkloadIdentityCredential activates from the
// environment variables the AKS/Helm Azure Workload Identity webhook
// injects into the pod, and AzureCLICredential activates from a local `az
// login` session when running the dashboard on a developer machine. Neither
// path stores or logs a credential — azidentity acquires tokens directly
// from Azure AD / the workload identity federation endpoint.
func DefaultCredential() (azcore.TokenCredential, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("acquiring Azure credential: %w", err)
	}
	return cred, nil
}
