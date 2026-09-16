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
	if provider != ProviderOpenAI {
		return nil, fmt.Errorf("unsupported AI provider %q", provider)
	}
	if strings.TrimSpace(config.APIKey) == "" {
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

	return newOpenAIProvider(config, baseURL), nil
}
