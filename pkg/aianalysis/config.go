package aianalysis

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ProviderOpenAI = "openai"

	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultTimeout = 30 * time.Second
)

// Config describes process-level AI provider configuration. APIKey is kept in
// memory only and must never be logged, rendered, or persisted.
type Config struct {
	Enabled  bool
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
	Timeout  time.Duration
}

// LoadConfigFromEnv reads the Phase 1 environment-backed configuration.
func LoadConfigFromEnv() (Config, error) {
	config := Config{
		Provider: ProviderOpenAI,
		BaseURL:  DefaultBaseURL,
		Timeout:  DefaultTimeout,
	}

	if value := strings.TrimSpace(os.Getenv("OPSCART_AI_ENABLED")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("invalid OPSCART_AI_ENABLED: %w", err)
		}
		config.Enabled = enabled
	}
	if !config.Enabled {
		return config, nil
	}
	if value := strings.TrimSpace(os.Getenv("OPSCART_AI_PROVIDER")); value != "" {
		config.Provider = strings.ToLower(value)
	}
	if value := strings.TrimSpace(os.Getenv("OPSCART_AI_BASE_URL")); value != "" {
		config.BaseURL = value
	}
	config.Model = strings.TrimSpace(os.Getenv("OPSCART_AI_MODEL"))
	config.APIKey = strings.TrimSpace(os.Getenv("OPSCART_AI_API_KEY"))
	if value := strings.TrimSpace(os.Getenv("OPSCART_AI_TIMEOUT")); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("invalid OPSCART_AI_TIMEOUT: %w", err)
		}
		config.Timeout = timeout
	}

	return config, nil
}
