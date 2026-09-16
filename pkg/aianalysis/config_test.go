package aianalysis

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFromEnvDefaultsDisabled(t *testing.T) {
	for _, name := range []string{
		"OPSCART_AI_ENABLED",
		"OPSCART_AI_PROVIDER",
		"OPSCART_AI_BASE_URL",
		"OPSCART_AI_MODEL",
		"OPSCART_AI_API_KEY",
		"OPSCART_AI_TIMEOUT",
	} {
		t.Setenv(name, "")
	}

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if config.Enabled {
		t.Fatal("LoadConfigFromEnv() enabled AI by default")
	}
	if config.Provider != ProviderOpenAI {
		t.Fatalf("Provider = %q, want %q", config.Provider, ProviderOpenAI)
	}
	if config.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", config.BaseURL, DefaultBaseURL)
	}
	if config.Timeout != DefaultTimeout {
		t.Fatalf("Timeout = %v, want %v", config.Timeout, DefaultTimeout)
	}
	if config.APIKey != "" {
		t.Fatal("LoadConfigFromEnv() produced an API key without configuration")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("OPSCART_AI_ENABLED", "true")
	t.Setenv("OPSCART_AI_PROVIDER", "OPENAI")
	t.Setenv("OPSCART_AI_BASE_URL", "https://llm.example.test/v1")
	t.Setenv("OPSCART_AI_MODEL", "test-model")
	t.Setenv("OPSCART_AI_API_KEY", "synthetic-key")
	t.Setenv("OPSCART_AI_TIMEOUT", "7s")

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if !config.Enabled || config.Provider != ProviderOpenAI || config.BaseURL != "https://llm.example.test/v1" || config.Model != "test-model" || config.APIKey != "synthetic-key" || config.Timeout != 7*time.Second {
		t.Fatalf("LoadConfigFromEnv() = %#v", config)
	}
}

func TestLoadConfigFromEnvRejectsInvalidValues(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		t.Setenv("OPSCART_AI_ENABLED", "sometimes")
		if _, err := LoadConfigFromEnv(); err == nil {
			t.Fatal("LoadConfigFromEnv() error = nil, want invalid boolean error")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Setenv("OPSCART_AI_ENABLED", "true")
		t.Setenv("OPSCART_AI_TIMEOUT", "eventually")
		if _, err := LoadConfigFromEnv(); err == nil {
			t.Fatal("LoadConfigFromEnv() error = nil, want invalid duration error")
		}
	})
}

func TestLoadConfigFromEnvDisabledIgnoresProviderSettings(t *testing.T) {
	t.Setenv("OPSCART_AI_ENABLED", "false")
	t.Setenv("OPSCART_AI_PROVIDER", "unsupported")
	t.Setenv("OPSCART_AI_BASE_URL", "://invalid")
	t.Setenv("OPSCART_AI_MODEL", "ignored-model")
	t.Setenv("OPSCART_AI_API_KEY", "ignored-key")
	t.Setenv("OPSCART_AI_TIMEOUT", "eventually")

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if config.Enabled {
		t.Fatal("LoadConfigFromEnv() enabled AI")
	}
	if config.Provider != ProviderOpenAI || config.BaseURL != DefaultBaseURL || config.Timeout != DefaultTimeout {
		t.Fatalf("LoadConfigFromEnv() = %#v, want disabled defaults", config)
	}
	if config.Model != "" || config.APIKey != "" {
		t.Fatalf("LoadConfigFromEnv() retained disabled provider credentials: %#v", config)
	}
}

func TestNewAIProviderDisabled(t *testing.T) {
	provider, err := NewAIProvider(Config{})
	if err != nil {
		t.Fatalf("NewAIProvider() error = %v", err)
	}
	if provider != nil {
		t.Fatalf("NewAIProvider() = %T, want nil", provider)
	}
}

func TestNewAIProviderConfigurationErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{
			name: "unsupported provider",
			config: Config{
				Enabled:  true,
				Provider: "custom-rag",
			},
			wantErr: "unsupported AI provider",
		},
		{
			name: "missing credential",
			config: Config{
				Enabled:  true,
				Provider: ProviderOpenAI,
			},
			wantErr: "credential is required",
		},
		{
			name: "missing model",
			config: Config{
				Enabled:  true,
				Provider: ProviderOpenAI,
				APIKey:   "synthetic-key",
			},
			wantErr: "model is required",
		},
		{
			name: "invalid timeout",
			config: Config{
				Enabled:  true,
				Provider: ProviderOpenAI,
				Model:    "test-model",
				APIKey:   "synthetic-key",
			},
			wantErr: "timeout must be positive",
		},
		{
			name: "URL credentials",
			config: Config{
				Enabled:  true,
				Provider: ProviderOpenAI,
				BaseURL:  "https://user:password@example.test/v1",
				Model:    "test-model",
				APIKey:   "synthetic-key",
				Timeout:  time.Second,
			},
			wantErr: "base URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewAIProvider(test.config)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("NewAIProvider() error = %v, want containing %q", err, test.wantErr)
			}
			if provider != nil {
				t.Fatalf("NewAIProvider() = %T, want nil", provider)
			}
		})
	}
}
