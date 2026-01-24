package models

import "time"

// ClusterResourceAnalysis represents comprehensive resource usage analysis
type ClusterResourceAnalysis struct {
	Timestamp time.Time `json:"timestamp"`

	// Cluster totals
	TotalCPUCores        float64 `json:"total_cpu_cores"`
	TotalMemoryGB        float64 `json:"total_memory_gb"`
	TotalCPURequested    float64 `json:"total_cpu_requested"`
	TotalMemoryRequested float64 `json:"total_memory_requested"`
	CPUUtilization       float64 `json:"cpu_utilization"`    // Percentage
	MemoryUtilization    float64 `json:"memory_utilization"` // Percentage

	// Namespace breakdown
	Namespaces []NamespaceResourceUsage `json:"namespaces"`

	// Optimization opportunities
	Optimizations []Optimization `json:"optimizations"`
}

// NamespaceResourceUsage represents resource usage for a single namespace
type NamespaceResourceUsage struct {
	Name string `json:"name"`

	// Resource requests
	CPUCoresRequested float64 `json:"cpu_cores_requested"`
	MemoryGBRequested float64 `json:"memory_gb_requested"`
	PodCount          int     `json:"pod_count"`

	// Cluster percentage
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`

	// Waste indicators
	IdlePods         int      `json:"idle_pods"`
	SpotEligiblePods int      `json:"spot_eligible_pods"`
	WasteScore       float64  `json:"waste_score"` // 0-100
	Flags            []string `json:"flags"`       // "IDLE-15d", "SPOT-OK", "OVER-PROV"
}

// WeightedShare returns the weighted average of CPU and Memory percentage as a fraction (0.0-1.0)
func (n NamespaceResourceUsage) WeightedShare() float64 {
	return (n.CPUPercent + n.MemoryPercent) / 2.0 / 100.0
}

// ResourceCapacity represents CPU and memory capacity
type ResourceCapacity struct {
	CPU    float64 `json:"cpu"`    // CPU cores
	Memory float64 `json:"memory"` // GB
}

// Optimization represents an optimization opportunity
type Optimization struct {
	Priority    string `json:"priority"` // "high", "medium", "low"
	Type        string `json:"type"`     // "idle_namespace", "spot_migration", "rightsizing"
	Namespace   string `json:"namespace"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Impact      string `json:"impact"`
}

// CostEstimate represents estimated costs (Phase 2)
type CostEstimate struct {
	TotalClusterCost      float64                `json:"total_cluster_cost"`
	NamespaceCosts        []NamespaceCostInfo    `json:"namespace_costs"`
	OptimizationScenarios []OptimizationScenario `json:"optimization_scenarios"`
	TotalSavingsPotential CostRange              `json:"total_savings_potential"`
	Method                string                 `json:"method"`
	Confidence            string                 `json:"confidence"`
	Assumptions           []string               `json:"assumptions"`
	Disclaimers           []string               `json:"disclaimers"`
}

// NamespaceCostInfo represents cost information for a namespace
type NamespaceCostInfo struct {
	Name          string    `json:"name"`
	EstimatedCost CostRange `json:"estimated_cost"`
	CPUShare      float64   `json:"cpu_share"`
	MemoryShare   float64   `json:"memory_share"`
	WeightedShare float64   `json:"weighted_share"`
}

// CostRange represents a cost estimate range
type CostRange struct {
	Low  float64 `json:"low"`
	Best float64 `json:"best"`
	High float64 `json:"high"`
}

// OptimizationScenario represents a cost optimization opportunity
type OptimizationScenario struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CurrentCost CostRange `json:"current_cost"`
	AfterCost   CostRange `json:"after_cost"`
	Savings     CostRange `json:"savings"`
	Impact      string    `json:"impact"`
	Effort      string    `json:"effort"`   // "Low", "Medium", "High"
	Risk        string    `json:"risk"`     // "Low", "Medium", "High"
	Timeline    string    `json:"timeline"` // "1 day", "1 week", etc.
	Actions     []string  `json:"actions"`  // Step-by-step implementation
}
