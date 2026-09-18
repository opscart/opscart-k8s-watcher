package billing

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// configPathEnvVar names the environment variable the dashboard reads at
// startup for the billing configuration file path. Unset means billing is
// disabled for every cluster — the documented default.
const configPathEnvVar = "OPSCART_AZURE_BILLING_CONFIG"

// LoadFromEnv reads configPathEnvVar and loads that file, or returns a nil,
// nil Config when the variable is unset — the fully-disabled default. It
// never contacts Azure.
func LoadFromEnv() (*Config, error) {
	path := strings.TrimSpace(os.Getenv(configPathEnvVar))
	if path == "" {
		return nil, nil
	}
	return Load(path)
}

// DefaultRefreshInterval matches the task's documented default operating
// cadence: billing data changes slowly, and a 6-hour cadence keeps this
// entirely off the Kubernetes scan and page-render paths.
const DefaultRefreshInterval = 6 * time.Hour

// MinRefreshInterval is the smallest refreshInterval Validate accepts.
// Even at this floor, two queries per refresh stay far under Cost
// Management's per-tenant QPU quotas (12 QPU/10s, 60 QPU/min, 600 QPU/hour:
// https://learn.microsoft.com/azure/cost-management-billing/costs/manage-automation#qpu-quotas),
// while still catching an operator's typo (e.g. "1s") long before it
// becomes a production incident.
const MinRefreshInterval = 15 * time.Minute

// DefaultReportingPeriodMode reports the current calendar month to date, in
// UTC, matching the Azure Portal Cost Analysis default view so a dashboard
// figure and a portal figure are comparing the same window unless a custom
// period is explicitly configured.
const DefaultReportingPeriodMode = "month-to-date"

const customReportingPeriodMode = "custom"

// aksResourceIDPattern matches
// /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{name}
// case-insensitively on the provider/resource-type segments, since ARM
// resource IDs are case-insensitive there.
var aksResourceIDPattern = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourceGroups/([^/]+)/providers/Microsoft\.ContainerService/managedClusters/([^/]+)$`)

// allowedManagementHosts lists every Azure Resource Manager host this
// package will ever attach a bearer token to. A configured
// managementEndpoint must resolve to one of these — never an arbitrary
// operator-supplied host, since that host would receive the same
// ARM-scoped token used for the real Azure Cost Management API.
//
// Azure Commercial only, for now: armTokenScope (query_client.go) is
// hardcoded to https://management.azure.com/.default, and credential
// construction (credential.go) does not configure a sovereign-cloud
// authority. Accepting management.usgovcloudapi.net or
// management.chinacloudapi.cn here without also wiring the matching token
// audience and AAD authority together would silently produce a token that
// doesn't match the endpoint it's sent to — every sovereign-cloud call
// would just fail authentication, not work partially. Add those hosts back
// only alongside that wiring, not on their own.
var allowedManagementHosts = map[string]bool{
	"management.azure.com": true, // Azure Public/Commercial
}

// validateManagementEndpoint rejects anything but a plain https URL to one
// of allowedManagementHosts: no userinfo/credentials embedded in the URL,
// no query string, no fragment, and no path beyond the host itself.
func validateManagementEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("managementEndpoint %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("managementEndpoint %q: must use https", raw)
	}
	if u.User != nil {
		return fmt.Errorf("managementEndpoint %q: must not include userinfo", raw)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("managementEndpoint %q: must not include a query string", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("managementEndpoint %q: must not include a fragment", raw)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return fmt.Errorf("managementEndpoint %q: must not include a path", raw)
	}
	host := strings.ToLower(u.Hostname())
	if !allowedManagementHosts[host] {
		return fmt.Errorf("managementEndpoint %q: host %q is not a supported Azure Resource Manager endpoint", raw, host)
	}
	return nil
}

// PeriodConfig describes the reporting window. Mode "month-to-date" (the
// default) recomputes the window relative to now on every refresh. Mode
// "custom" fixes Start/End for portal reconciliation, both inclusive
// calendar dates in the cluster's billing UI sense — Resolve converts them
// to the Cost Management API's half-open UTC boundaries explicitly.
type PeriodConfig struct {
	Mode  string `yaml:"mode,omitempty"`
	Start string `yaml:"start,omitempty"`
	End   string `yaml:"end,omitempty"`
}

// ClusterConfig is one dashboard cluster's Azure billing mapping. Each
// configured cluster gets its own ClusterConfig, its own Provider, and its
// own Runtime — see runtime.go — so a mistake or outage in one cluster's
// billing configuration can never affect another's.
type ClusterConfig struct {
	// ClusterContext must exactly match a --cluster/--clusters entry.
	ClusterContext string `yaml:"cluster"`
	// Enabled gates this cluster's billing runtime. Absent/false means
	// disabled — the documented default for the whole feature.
	Enabled bool `yaml:"enabled"`

	// AuthMode selects exactly one Azure credential type — "azure-cli" or
	// "workload-identity" — with no default and no fallback (see
	// credential.go). Required.
	AuthMode string `yaml:"authMode"`

	SubscriptionID string `yaml:"subscriptionId"`
	// AKSResourceID is the full ARM resource ID of the AKS cluster. Its
	// resource group is the "cluster resource group" scope.
	AKSResourceID string `yaml:"aksResourceId"`
	// NodeResourceGroup is the AKS-managed resource group holding node
	// VMs/VMSS, disks, load balancers, and public IPs for this cluster's
	// node pools. If empty, it is resolved from the AKS resource's
	// properties.nodeResourceGroup at runtime (see aks_identity.go) and
	// cached for the life of the Runtime.
	NodeResourceGroup string `yaml:"nodeResourceGroup,omitempty"`

	// CostBasis selects ActualCost (default) or AmortizedCost. See
	// docs — toggling this in the Azure Portal reconciliation reference
	// produced no visible change in the rounded total, so ActualCost is the
	// safe, simpler default.
	CostBasis string `yaml:"costBasis,omitempty"`

	Period PeriodConfig `yaml:"period,omitempty"`

	// RefreshInterval defaults to DefaultRefreshInterval when zero.
	RefreshInterval time.Duration `yaml:"refreshInterval,omitempty"`

	// ManagementEndpoint overrides the ARM endpoint (default
	// https://management.azure.com). Must still resolve to
	// allowedManagementHosts, which currently only lists Azure Commercial —
	// there is no supported way to point this at a sovereign cloud yet (see
	// that var's doc comment). Never required for normal use.
	ManagementEndpoint string `yaml:"managementEndpoint,omitempty"`
}

// Config is the top-level billing configuration file: a list of per-cluster
// mappings, keyed by cluster context at lookup time.
type Config struct {
	Clusters []ClusterConfig `yaml:"clusters"`
}

// Load reads and validates a billing configuration file. It never contacts
// Azure. A missing path is not an error at this layer — callers (dashboard
// startup) treat "no config" as "billing disabled for every cluster."
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading azure billing config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing azure billing config %q: %w", path, err)
	}
	seen := make(map[string]bool, len(cfg.Clusters))
	for i := range cfg.Clusters {
		c := &cfg.Clusters[i]
		if !c.Enabled {
			continue
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("azure billing config %q: cluster %q: %w", path, c.ClusterContext, err)
		}
		if seen[c.ClusterContext] {
			return nil, fmt.Errorf("azure billing config %q: duplicate cluster %q", path, c.ClusterContext)
		}
		seen[c.ClusterContext] = true
	}
	return &cfg, nil
}

// ClusterByContext returns the enabled, valid configuration for clusterCtx,
// if one exists. It never returns a disabled entry as ok — the caller does
// not need to re-check Enabled.
func (c *Config) ClusterByContext(clusterCtx string) (ClusterConfig, bool) {
	if c == nil {
		return ClusterConfig{}, false
	}
	for _, cc := range c.Clusters {
		if cc.ClusterContext == clusterCtx && cc.Enabled {
			return cc, true
		}
	}
	return ClusterConfig{}, false
}

// Validate checks structural correctness only — it never calls Azure.
// AKSResourceID's subscription and resource group are cross-checked against
// SubscriptionID/NodeResourceGroup for internal consistency, catching a
// copy-paste mismatch before any API call is attempted.
func (c ClusterConfig) Validate() error {
	if strings.TrimSpace(c.ClusterContext) == "" {
		return fmt.Errorf("cluster is required")
	}
	if _, err := ParseAuthMode(c.AuthMode); err != nil {
		return err
	}
	if strings.TrimSpace(c.SubscriptionID) == "" {
		return fmt.Errorf("subscriptionId is required")
	}
	identity, err := ParseAKSResourceID(c.AKSResourceID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(identity.SubscriptionID, c.SubscriptionID) {
		return fmt.Errorf("aksResourceId subscription %q does not match subscriptionId %q", identity.SubscriptionID, c.SubscriptionID)
	}
	if c.NodeResourceGroup != "" && strings.EqualFold(c.NodeResourceGroup, identity.ResourceGroup) {
		return fmt.Errorf("nodeResourceGroup %q must not equal the cluster resource group", c.NodeResourceGroup)
	}
	switch strings.ToLower(strings.TrimSpace(c.CostBasis)) {
	case "", "actualcost":
	case "amortizedcost":
	default:
		return fmt.Errorf("costBasis %q: use ActualCost or AmortizedCost", c.CostBasis)
	}
	switch strings.ToLower(strings.TrimSpace(c.Period.Mode)) {
	case "", DefaultReportingPeriodMode:
		if c.Period.Start != "" || c.Period.End != "" {
			return fmt.Errorf("period.start/period.end require period.mode: custom")
		}
	case customReportingPeriodMode:
		if _, err := parseInclusiveDate(c.Period.Start); err != nil {
			return fmt.Errorf("period.start: %w", err)
		}
		if _, err := parseInclusiveDate(c.Period.End); err != nil {
			return fmt.Errorf("period.end: %w", err)
		}
	default:
		return fmt.Errorf("period.mode %q: use %q or %q", c.Period.Mode, DefaultReportingPeriodMode, customReportingPeriodMode)
	}
	if c.RefreshInterval < 0 {
		return fmt.Errorf("refreshInterval must not be negative")
	}
	if c.RefreshInterval > 0 && c.RefreshInterval < MinRefreshInterval {
		return fmt.Errorf("refreshInterval %s is below the minimum %s", c.RefreshInterval, MinRefreshInterval)
	}
	if c.ManagementEndpoint != "" {
		if err := validateManagementEndpoint(c.ManagementEndpoint); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveAuthMode returns the configured, already-validated auth mode.
// Call only after Validate has succeeded — this accessor is not itself a
// validation point, so an invalid/empty AuthMode silently returns "" here
// instead of erroring; NewCredential("") then fails explicitly.
func (c ClusterConfig) EffectiveAuthMode() AuthMode {
	mode, _ := ParseAuthMode(c.AuthMode)
	return mode
}

// EffectiveCostBasis returns the configured cost basis, defaulting to
// ActualCost.
func (c ClusterConfig) EffectiveCostBasis() CostBasis {
	if strings.EqualFold(strings.TrimSpace(c.CostBasis), string(CostBasisAmortizedCost)) {
		return CostBasisAmortizedCost
	}
	return CostBasisActualCost
}

// EffectiveRefreshInterval returns the configured refresh interval,
// defaulting to DefaultRefreshInterval when unset.
func (c ClusterConfig) EffectiveRefreshInterval() time.Duration {
	if c.RefreshInterval <= 0 {
		return DefaultRefreshInterval
	}
	return c.RefreshInterval
}

// EffectiveManagementEndpoint returns the configured ARM endpoint, defaulting
// to the public Azure Commercial cloud.
func (c ClusterConfig) EffectiveManagementEndpoint() string {
	if c.ManagementEndpoint != "" {
		return strings.TrimRight(c.ManagementEndpoint, "/")
	}
	return defaultManagementEndpoint
}

// ResolvePeriod converts the configured period into explicit UTC [start, end]
// boundaries relative to now, making the inclusive-UI-date vs API-date
// boundary conversion explicit in one place: a configured inclusive calendar
// end date becomes 23:59:59 UTC of that same day — the last instant of the
// last included day, matching Azure Cost Management's own documented
// timePeriod examples — not the exclusive start of the following day.
func (c ClusterConfig) ResolvePeriod(now time.Time) (start, end time.Time, err error) {
	now = now.UTC()
	switch strings.ToLower(strings.TrimSpace(c.Period.Mode)) {
	case customReportingPeriodMode:
		start, err = parseInclusiveDate(c.Period.Start)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("period.start: %w", err)
		}
		endDate, err := parseInclusiveDate(c.Period.End)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("period.end: %w", err)
		}
		end = endOfDay(endDate)
		if end.Before(start) {
			return time.Time{}, time.Time{}, fmt.Errorf("period.end %q must not be before period.start %q", c.Period.End, c.Period.Start)
		}
		return start, end, nil
	default:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = now
		return start, end, nil
	}
}

func endOfDay(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, time.UTC)
}

func parseInclusiveDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("date is required (YYYY-MM-DD)")
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD", value)
	}
	return t.UTC(), nil
}

// AKSIdentity is the parsed form of an AKS resource ID.
type AKSIdentity struct {
	SubscriptionID string
	ResourceGroup  string
	ClusterName    string
	ResourceID     string
}

// ParseAKSResourceID validates and decomposes an AKS managedClusters ARM
// resource ID. It never invents a resource group or subscription — an
// unparseable ID is always a validation error, never a fallback.
func ParseAKSResourceID(id string) (AKSIdentity, error) {
	m := aksResourceIDPattern.FindStringSubmatch(strings.TrimSpace(id))
	if m == nil {
		return AKSIdentity{}, fmt.Errorf("aksResourceId %q is not a valid AKS managedClusters resource ID (expected /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{name})", id)
	}
	return AKSIdentity{
		SubscriptionID: m[1],
		ResourceGroup:  m[2],
		ClusterName:    m[3],
		ResourceID:     id,
	}, nil
}
