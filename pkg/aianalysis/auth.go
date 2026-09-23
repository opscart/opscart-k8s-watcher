package aianalysis

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const azureFoundryTokenScope = "https://ai.azure.com/.default"

var errProviderAuthentication = errors.New("AI provider authentication failed")

type requestAuthenticator interface {
	Authorize(context.Context, *http.Request) error
}

type apiKeyAuthenticator struct {
	header string
	prefix string
	key    string
}

func (auth apiKeyAuthenticator) Authorize(_ context.Context, request *http.Request) error {
	request.Header.Set(auth.header, auth.prefix+auth.key)
	return nil
}

type azureTokenAuthenticator struct {
	credential azcore.TokenCredential
}

func (auth azureTokenAuthenticator) Authorize(ctx context.Context, request *http.Request) error {
	token, err := auth.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{azureFoundryTokenScope},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errProviderAuthentication
	}
	if strings.TrimSpace(token.Token) == "" {
		return errProviderAuthentication
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	return nil
}

func newRequestAuthenticator(config Config, provider, authMode string) (requestAuthenticator, error) {
	switch {
	case provider == ProviderOpenAI && authMode == AuthModeAPIKey:
		return apiKeyAuthenticator{header: "Authorization", prefix: "Bearer ", key: strings.TrimSpace(config.APIKey)}, nil
	case provider == ProviderAzureFoundry && authMode == AuthModeAPIKey:
		return apiKeyAuthenticator{header: "api-key", key: strings.TrimSpace(config.APIKey)}, nil
	case provider == ProviderAzureFoundry && authMode == AuthModeWorkloadIdentity:
		credential, err := azidentity.NewWorkloadIdentityCredential(nil)
		if err != nil {
			return nil, errors.New("initialize Azure Workload Identity authentication")
		}
		return azureTokenAuthenticator{credential: credential}, nil
	case provider == ProviderAzureFoundry && authMode == AuthModeAzureCLI:
		credential, err := azidentity.NewAzureCLICredential(nil)
		if err != nil {
			return nil, errors.New("initialize Azure CLI authentication")
		}
		return azureTokenAuthenticator{credential: credential}, nil
	default:
		return nil, errors.New("unsupported AI provider authentication mode")
	}
}
