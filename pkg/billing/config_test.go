package billing

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validAKSResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/example-aks-rg/providers/Microsoft.ContainerService/managedClusters/example-aks"
const validSubscriptionID = "11111111-1111-1111-1111-111111111111"

func validClusterConfig() ClusterConfig {
	return ClusterConfig{
		ClusterContext: "prod",
		Enabled:        true,
		AuthMode:       string(AuthModeAzureCLI),
		SubscriptionID: validSubscriptionID,
		AKSResourceID:  validAKSResourceID,
	}
}

func TestParseAKSResourceID(t *testing.T) {
	identity, err := ParseAKSResourceID(validAKSResourceID)
	if err != nil {
		t.Fatalf("ParseAKSResourceID: %v", err)
	}
	if identity.SubscriptionID != validSubscriptionID {
		t.Errorf("SubscriptionID = %q", identity.SubscriptionID)
	}
	if identity.ResourceGroup != "example-aks-rg" {
		t.Errorf("ResourceGroup = %q", identity.ResourceGroup)
	}
	if identity.ClusterName != "example-aks" {
		t.Errorf("ClusterName = %q", identity.ClusterName)
	}
}

func TestParseAKSResourceIDRejectsMalformed(t *testing.T) {
	for _, id := range []string{
		"",
		"/subscriptions/x/resourceGroups/y",
		"/subscriptions/x/resourceGroups/y/providers/Microsoft.Compute/virtualMachines/z",
	} {
		if _, err := ParseAKSResourceID(id); err == nil {
			t.Errorf("ParseAKSResourceID(%q): expected error, got nil", id)
		}
	}
}

func TestClusterConfigValidateAcceptsMinimalValid(t *testing.T) {
	if err := validClusterConfig().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestClusterConfigValidateRejectsMissingAuthMode(t *testing.T) {
	cfg := validClusterConfig()
	cfg.AuthMode = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a missing authMode error, got nil")
	}
}

func TestClusterConfigValidateRejectsInvalidAuthMode(t *testing.T) {
	cfg := validClusterConfig()
	cfg.AuthMode = "auto"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an invalid authMode error, got nil")
	}
}

func TestClusterConfigValidateRejectsSubscriptionMismatch(t *testing.T) {
	cfg := validClusterConfig()
	cfg.SubscriptionID = "22222222-2222-2222-2222-222222222222"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected subscription mismatch error, got nil")
	}
}

func TestClusterConfigValidateRejectsNodeResourceGroupEqualsClusterResourceGroup(t *testing.T) {
	cfg := validClusterConfig()
	cfg.NodeResourceGroup = "example-aks-rg"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected node resource group collision error, got nil")
	}
}

func TestClusterConfigValidateRejectsInvalidCostBasis(t *testing.T) {
	cfg := validClusterConfig()
	cfg.CostBasis = "TotalCost"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid cost basis error, got nil")
	}
}

func TestClusterConfigValidateRejectsCustomPeriodMissingDates(t *testing.T) {
	cfg := validClusterConfig()
	cfg.Period = PeriodConfig{Mode: "custom"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing custom period dates error, got nil")
	}
}

func TestClusterConfigValidateRejectsDatesWithoutCustomMode(t *testing.T) {
	cfg := validClusterConfig()
	cfg.Period = PeriodConfig{Start: "2026-08-17", End: "2026-09-15"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for dates supplied without period.mode: custom")
	}
}

func TestResolvePeriodMonthToDateDefault(t *testing.T) {
	cfg := validClusterConfig()
	now := time.Date(2026, 9, 17, 15, 30, 0, 0, time.UTC)
	start, end, err := cfg.ResolvePeriod(now)
	if err != nil {
		t.Fatalf("ResolvePeriod: %v", err)
	}
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	if !end.Equal(now) {
		t.Errorf("end = %v, want %v", end, now)
	}
}

func TestResolvePeriodCustomInclusiveEndDate(t *testing.T) {
	cfg := validClusterConfig()
	cfg.Period = PeriodConfig{Mode: "custom", Start: "2026-08-17", End: "2026-09-15"}
	start, end, err := cfg.ResolvePeriod(time.Now())
	if err != nil {
		t.Fatalf("ResolvePeriod: %v", err)
	}
	if want := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	// The inclusive UI end date 2026-09-15 must resolve to the last instant
	// of that day, not midnight (which would silently drop that day's
	// charges) and not the following day (ambiguous against the API's own
	// documented examples).
	want := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)
	if !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

func TestResolvePeriodCustomRejectsEndBeforeStart(t *testing.T) {
	cfg := validClusterConfig()
	cfg.Period = PeriodConfig{Mode: "custom", Start: "2026-09-15", End: "2026-08-17"}
	if _, _, err := cfg.ResolvePeriod(time.Now()); err == nil {
		t.Fatal("expected error for end before start, got nil")
	}
}

func TestEffectiveDefaults(t *testing.T) {
	cfg := validClusterConfig()
	if got := cfg.EffectiveCostBasis(); got != CostBasisActualCost {
		t.Errorf("EffectiveCostBasis default = %q, want ActualCost", got)
	}
	if got := cfg.EffectiveRefreshInterval(); got != DefaultRefreshInterval {
		t.Errorf("EffectiveRefreshInterval default = %v, want %v", got, DefaultRefreshInterval)
	}
	if got := cfg.EffectiveManagementEndpoint(); got != defaultManagementEndpoint {
		t.Errorf("EffectiveManagementEndpoint default = %q", got)
	}

	cfg.CostBasis = "AmortizedCost"
	if got := cfg.EffectiveCostBasis(); got != CostBasisAmortizedCost {
		t.Errorf("EffectiveCostBasis = %q, want AmortizedCost", got)
	}
}

func TestLoadRejectsInvalidClusterConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billing.yaml")
	content := `
clusters:
  - cluster: prod
    enabled: true
    authMode: azure-cli
    subscriptionId: "22222222-2222-2222-2222-222222222222"
    aksResourceId: "` + validAKSResourceID + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to reject a subscription mismatch, got nil")
	}
}

func TestLoadRejectsMissingAuthMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billing.yaml")
	content := `
clusters:
  - cluster: prod
    enabled: true
    subscriptionId: "` + validSubscriptionID + `"
    aksResourceId: "` + validAKSResourceID + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to reject a missing authMode, got nil")
	}
}

func TestLoadAndClusterByContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billing.yaml")
	content := `
clusters:
  - cluster: prod
    enabled: true
    authMode: azure-cli
    subscriptionId: "` + validSubscriptionID + `"
    aksResourceId: "` + validAKSResourceID + `"
    refreshInterval: 1h
  - cluster: staging
    enabled: false
    subscriptionId: "` + validSubscriptionID + `"
    aksResourceId: "` + validAKSResourceID + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prod, ok := cfg.ClusterByContext("prod")
	if !ok {
		t.Fatal("expected prod to be enabled and found")
	}
	if prod.EffectiveRefreshInterval() != time.Hour {
		t.Errorf("refreshInterval = %v, want 1h", prod.EffectiveRefreshInterval())
	}
	if _, ok := cfg.ClusterByContext("staging"); ok {
		t.Error("disabled cluster staging must not be returned as configured")
	}
	if _, ok := cfg.ClusterByContext("nonexistent"); ok {
		t.Error("unconfigured cluster must not be returned as configured")
	}
}

func TestLoadFromEnvUnsetIsDisabled(t *testing.T) {
	t.Setenv(configPathEnvVar, "")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil Config when %s is unset, got %+v", configPathEnvVar, cfg)
	}
}

func TestValidateManagementEndpointAcceptsAzureCommercial(t *testing.T) {
	if err := validateManagementEndpoint("https://management.azure.com"); err != nil {
		t.Errorf("validateManagementEndpoint(https://management.azure.com): %v", err)
	}
}

// TestValidateManagementEndpointRejectsSovereignCloudsForNow documents a
// deliberate current limitation, not an oversight: armTokenScope
// (query_client.go) is hardcoded to Azure Commercial's token audience, and
// credential construction does not configure a sovereign-cloud AAD
// authority. Accepting these hosts without that wiring would produce a
// token whose audience never matches the endpoint — every sovereign-cloud
// call would fail authentication. See allowedManagementHosts' doc comment.
func TestValidateManagementEndpointRejectsSovereignCloudsForNow(t *testing.T) {
	for _, host := range []string{"management.usgovcloudapi.net", "management.chinacloudapi.cn"} {
		if err := validateManagementEndpoint("https://" + host); err == nil {
			t.Errorf("validateManagementEndpoint(https://%s): expected rejection (sovereign clouds not yet supported), got nil", host)
		}
	}
}

func TestValidateManagementEndpointRejectsUnknownHost(t *testing.T) {
	if err := validateManagementEndpoint("https://management.evil.example.com"); err == nil {
		t.Fatal("expected an error for an unsupported host, got nil")
	}
}

func TestValidateManagementEndpointRejectsHTTP(t *testing.T) {
	if err := validateManagementEndpoint("http://management.azure.com"); err == nil {
		t.Fatal("expected an error for a plain-http endpoint, got nil")
	}
}

func TestValidateManagementEndpointRejectsUserinfo(t *testing.T) {
	if err := validateManagementEndpoint("https://user:pass@management.azure.com"); err == nil {
		t.Fatal("expected an error for an endpoint carrying userinfo, got nil")
	}
}

func TestValidateManagementEndpointRejectsQueryString(t *testing.T) {
	if err := validateManagementEndpoint("https://management.azure.com?x=1"); err == nil {
		t.Fatal("expected an error for an endpoint carrying a query string, got nil")
	}
}

func TestValidateManagementEndpointRejectsFragment(t *testing.T) {
	if err := validateManagementEndpoint("https://management.azure.com#x"); err == nil {
		t.Fatal("expected an error for an endpoint carrying a fragment, got nil")
	}
}

func TestValidateManagementEndpointRejectsPath(t *testing.T) {
	if err := validateManagementEndpoint("https://management.azure.com/some/path"); err == nil {
		t.Fatal("expected an error for an endpoint carrying a path, got nil")
	}
}

func TestClusterConfigValidateRejectsUnsupportedManagementEndpoint(t *testing.T) {
	cfg := validClusterConfig()
	cfg.ManagementEndpoint = "https://attacker.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for an unsupported managementEndpoint, got nil")
	}
}

func TestClusterConfigValidateAcceptsEmptyManagementEndpoint(t *testing.T) {
	cfg := validClusterConfig()
	cfg.ManagementEndpoint = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with default (empty) managementEndpoint: %v", err)
	}
}

func TestClusterConfigValidateRejectsRefreshIntervalBelowMinimum(t *testing.T) {
	cfg := validClusterConfig()
	cfg.RefreshInterval = time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a refreshInterval below MinRefreshInterval, got nil")
	}
}

func TestClusterConfigValidateAcceptsRefreshIntervalAtMinimum(t *testing.T) {
	cfg := validClusterConfig()
	cfg.RefreshInterval = MinRefreshInterval
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with refreshInterval == MinRefreshInterval: %v", err)
	}
}

func TestClusterConfigValidateAcceptsZeroRefreshInterval(t *testing.T) {
	// Zero means "use DefaultRefreshInterval" (config.go), not "below the
	// floor" — only an explicit too-small positive value is rejected.
	cfg := validClusterConfig()
	cfg.RefreshInterval = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with zero refreshInterval: %v", err)
	}
}
