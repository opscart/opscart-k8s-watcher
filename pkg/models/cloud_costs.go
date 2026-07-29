package models

import "time"

// CloudCostReport is the top-level structure for comprehensive cloud cost analysis
type CloudCostReport struct {
	Timestamp             time.Time `json:"timestamp"`
	ClusterName           string    `json:"cluster_name"`
	Region                string    `json:"region"`
	Provider              string    `json:"provider"` // "azure", "aws", "gcp"
	DetectedProvider      string    `json:"detected_provider"`
	EffectiveProvider     string    `json:"effective_provider"`
	ProviderDetectionMode string    `json:"provider_detection_mode"`
	ProviderWarning       string    `json:"provider_warning,omitempty"`
	PricingCoverage       string    `json:"pricing_coverage"`
	PricingWarnings       []string  `json:"pricing_warnings,omitempty"`
	Currency              string    `json:"currency"`
	ScopeExclusions       []string  `json:"scope_exclusions,omitempty"`
	LastPriceRefresh      time.Time `json:"last_price_refresh,omitempty"`

	// Infrastructure costs (computed from actual node/VM pricing)
	NodePoolCosts []NodePoolCost `json:"node_pool_costs"`
	TotalNodeCost float64        `json:"total_node_cost"` // sum of all node pools $/month

	// Namespace-level allocation (split by real infra cost)
	NamespaceCosts []NamespaceCostInfo `json:"namespace_costs"`

	// Summary
	TotalMonthlyCost      float64                `json:"total_monthly_cost"`
	TotalAnnualCost       float64                `json:"total_annual_cost"`
	CostBreakdown         CostBreakdown          `json:"cost_breakdown"`
	OptimizationScenarios []OptimizationScenario `json:"optimization_scenarios"`
	TotalSavingsPotential CostRange              `json:"total_savings_potential"`

	// Metadata
	PricingSource string   `json:"pricing_source"` // "azure-retail-api", "embedded-catalog", "user-provided"
	Assumptions   []string `json:"assumptions"`
	Disclaimers   []string `json:"disclaimers"`
}

// NodePoolCost represents cost analysis for a single AKS node pool
type NodePoolCost struct {
	Name             string `json:"name"`
	VMSize           string `json:"vm_size"` // e.g., "Standard_D4s_v3"
	NodeCount        int    `json:"node_count"`
	MaxNodeCount     int    `json:"max_node_count"` // autoscaler max (if applicable)
	Priority         string `json:"priority"`       // "Regular", "Spot"
	OS               string `json:"os"`             // "Linux", "Windows"
	Mode             string `json:"mode"`           // "System", "User"
	Provider         string `json:"provider"`
	Region           string `json:"region"`
	PricingAvailable bool   `json:"pricing_available"`
	PricingWarning   string `json:"pricing_warning,omitempty"`

	// Per-node specs
	CPUCoresPerNode float64 `json:"cpu_cores_per_node"`
	MemoryGBPerNode float64 `json:"memory_gb_per_node"`

	// Costs
	PricePerNodeHour  float64 `json:"price_per_node_hour"`
	PricePerNodeMonth float64 `json:"price_per_node_month"`
	TotalMonthly      float64 `json:"total_monthly"` // nodeCount * pricePerNodeMonth
	SpotDiscount      float64 `json:"spot_discount"` // 0.0 – 1.0 (e.g. 0.7 = 70% off)

	// Utilization
	TotalCPUCapacity     float64 `json:"total_cpu_capacity"`
	TotalMemoryCapacity  float64 `json:"total_memory_capacity"`
	CPURequested         float64 `json:"cpu_requested"`
	MemoryRequested      float64 `json:"memory_requested"`
	CPUUtilizationPct    float64 `json:"cpu_utilization_pct"`
	MemoryUtilizationPct float64 `json:"memory_utilization_pct"`

	// Reserved Instance potential
	RISavings    float64 `json:"ri_savings"`     // monthly savings with 1-year RI vs PAYG
	RISavings3yr float64 `json:"ri_savings_3yr"` // monthly savings with 3-year RI vs PAYG
}

// CostBreakdown shows where money is spent
type CostBreakdown struct {
	Compute      float64 `json:"compute"`       // Node VMs
	ManagedDisks float64 `json:"managed_disks"` // PVCs / OS disks
	Networking   float64 `json:"networking"`    // Load balancers, IPs, egress
	Other        float64 `json:"other"`
}

// NodeInfo represents a single Kubernetes node with cloud metadata
type NodeInfo struct {
	Name           string  `json:"name"`
	NodePool       string  `json:"node_pool"`
	VMSize         string  `json:"vm_size"`
	Region         string  `json:"region"`
	Zone           string  `json:"zone"`
	OS             string  `json:"os"`
	Priority       string  `json:"priority"` // "Regular" or "Spot"
	CPUCapacity    float64 `json:"cpu_capacity"`
	MemGBCapacity  float64 `json:"mem_gb_capacity"`
	CPURequested   float64 `json:"cpu_requested"`
	MemGBRequested float64 `json:"mem_gb_requested"`
	Provider       string  `json:"provider"`
}

// VMPricing holds pricing info for a VM SKU
type VMPricing struct {
	SKU             string  `json:"sku"`
	Family          string  `json:"family"`
	CPUCores        int     `json:"cpu_cores"`
	MemoryGB        float64 `json:"memory_gb"`
	PayAsYouGoHour  float64 `json:"pay_as_you_go_hour"`  // $/hour on-demand
	PayAsYouGoMonth float64 `json:"pay_as_you_go_month"` // $/month on-demand (730 hrs)
	SpotHour        float64 `json:"spot_hour"`           // $/hour spot
	OneYearRI       float64 `json:"one_year_ri_month"`   // $/month 1-year reserved
	ThreeYearRI     float64 `json:"three_year_ri_month"` // $/month 3-year reserved
	Region          string  `json:"region"`
}
