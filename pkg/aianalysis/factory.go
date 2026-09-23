package aianalysis

import (
	"fmt"
	"net/url"
	"strings"
)

// NewAIProvider creates the configured provider without making an external
// connection. A disabled configuration intentionally returns a nil provider.
func NewAIProvider(config Config) (AIProvider, error) {
	if !config.Enabled {
		return nil, nil
	}

	provider := strings.ToLower(strings.TrimSpace(config.Provider))
	if provider != ProviderOpenAI && provider != ProviderAzureFoundry {
		return nil, fmt.Errorf("unsupported AI provider %q", provider)
	}
	authMode := strings.ToLower(strings.TrimSpace(config.AuthMode))
	if authMode == "" && provider == ProviderOpenAI {
		authMode = AuthModeAPIKey
	}
	if authMode == "" {
		return nil, fmt.Errorf("AI provider authentication mode is required")
	}
	if provider == ProviderOpenAI && authMode != AuthModeAPIKey {
		return nil, fmt.Errorf("unsupported authentication mode %q for AI provider %q", authMode, provider)
	}
	if provider == ProviderAzureFoundry && authMode != AuthModeAPIKey && authMode != AuthModeWorkloadIdentity && authMode != AuthModeAzureCLI {
		return nil, fmt.Errorf("unsupported authentication mode %q for AI provider %q", authMode, provider)
	}
	if authMode == AuthModeAPIKey && strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("AI provider credential is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("AI provider model is required")
	}
	if config.Timeout <= 0 {
		return nil, fmt.Errorf("AI provider timeout must be positive")
	}

	baseURL, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("AI provider base URL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if provider == ProviderAzureFoundry {
		if err := validateAzureFoundryBaseURL(baseURL); err != nil {
			return nil, err
		}
	}

	authenticator, err := newRequestAuthenticator(config, provider, authMode)
	if err != nil {
		return nil, err
	}

	return newOpenAIProvider(config, baseURL, authenticator), nil
}

func validateAzureFoundryBaseURL(baseURL *url.URL) error {
	host := strings.ToLower(baseURL.Hostname())
	if baseURL.Scheme != "https" || baseURL.Port() != "" ||
		(!strings.HasSuffix(host, ".openai.azure.com") && !strings.HasSuffix(host, ".services.ai.azure.com")) ||
		strings.TrimRight(baseURL.EscapedPath(), "/") != "/openai/v1" {
		return fmt.Errorf("Azure Foundry base URL must be an HTTPS Azure OpenAI endpoint ending in /openai/v1")
	}
	return nil
}
