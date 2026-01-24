package analyzer

// RiskCostConfig contains industry-specific risk parameters
type RiskCostConfig struct {
	Industry           string  `yaml:"industry"`             // "pharma", "fintech", "startup", "generic"
	EngineerHourlyRate float64 `yaml:"engineer_hourly_rate"` // Cost per engineer hour

	// Breach costs by severity (industry-specific)
	PrivilegedContainerCost float64 `yaml:"privileged_container_cost"`
	HostPathCost            float64 `yaml:"host_path_cost"`
	HostPIDCost             float64 `yaml:"host_pid_cost"`
	RunningAsRootCost       float64 `yaml:"running_as_root_cost"`
	HostNetworkCost         float64 `yaml:"host_network_cost"`
	MissingLimitsCost       float64 `yaml:"missing_limits_cost"`
	DefaultSACost           float64 `yaml:"default_sa_cost"`

	// Probability multipliers
	PublicFacingMultiplier    float64 `yaml:"public_facing_multiplier"` // Multiply risk for public clusters
	InternetExposedMultiplier float64 `yaml:"internet_exposed_multiplier"`

	// Risk combinations (compound risks)
	PrivilegedAndHostPath float64 `yaml:"privileged_and_hostpath"` // Multiplier when both present
	RootAndHostNetwork    float64 `yaml:"root_and_hostnetwork"`
}

// DefaultConfig returns sensible defaults for generic industry
func DefaultConfig() *RiskCostConfig {
	return &RiskCostConfig{
		Industry:                  "generic",
		EngineerHourlyRate:        200.0,
		PrivilegedContainerCost:   25000.0,
		HostPathCost:              35000.0,
		HostPIDCost:               20000.0,
		RunningAsRootCost:         8000.0,
		HostNetworkCost:           12000.0,
		MissingLimitsCost:         5000.0,
		DefaultSACost:             6000.0,
		PublicFacingMultiplier:    1.5,
		InternetExposedMultiplier: 2.0,
		PrivilegedAndHostPath:     1.8,
		RootAndHostNetwork:        1.4,
	}
}

// PharmaConfig returns configuration for pharmaceutical industry (HIPAA, high compliance)
func PharmaConfig() *RiskCostConfig {
	return &RiskCostConfig{
		Industry:                  "pharma",
		EngineerHourlyRate:        200.0,
		PrivilegedContainerCost:   50000.0, // 2x - PHI data breach
		HostPathCost:              70000.0, // 2x - Direct data access
		HostPIDCost:               40000.0, // 2x - HIPAA compliance
		RunningAsRootCost:         15000.0, // 1.8x
		HostNetworkCost:           20000.0, // 1.6x
		MissingLimitsCost:         8000.0,  // 1.6x - Availability matters
		DefaultSACost:             10000.0, // 1.6x
		PublicFacingMultiplier:    2.0,     // Higher - patient data
		InternetExposedMultiplier: 3.0,     // Much higher
		PrivilegedAndHostPath:     2.2,     // Severe for PHI
		RootAndHostNetwork:        1.6,
	}
}

// FintechConfig returns configuration for financial services (PCI-DSS, high value)
func FintechConfig() *RiskCostConfig {
	return &RiskCostConfig{
		Industry:                  "fintech",
		EngineerHourlyRate:        250.0,   // Higher engineer costs
		PrivilegedContainerCost:   40000.0, // 1.6x - Payment data
		HostPathCost:              60000.0, // 1.7x
		HostPIDCost:               35000.0, // 1.75x
		RunningAsRootCost:         12000.0, // 1.5x
		HostNetworkCost:           18000.0, // 1.5x
		MissingLimitsCost:         10000.0, // 2x - Uptime SLAs
		DefaultSACost:             9000.0,  // 1.5x
		PublicFacingMultiplier:    1.8,
		InternetExposedMultiplier: 2.5,
		PrivilegedAndHostPath:     2.0,
		RootAndHostNetwork:        1.5,
	}
}

// StartupConfig returns configuration for startups (lower costs, higher risk tolerance)
func StartupConfig() *RiskCostConfig {
	return &RiskCostConfig{
		Industry:                  "startup",
		EngineerHourlyRate:        150.0,   // Lower rates
		PrivilegedContainerCost:   15000.0, // 0.6x - Less at stake
		HostPathCost:              20000.0, // 0.57x
		HostPIDCost:               12000.0, // 0.6x
		RunningAsRootCost:         5000.0,  // 0.62x
		HostNetworkCost:           8000.0,  // 0.66x
		MissingLimitsCost:         3000.0,  // 0.6x
		DefaultSACost:             4000.0,  // 0.66x
		PublicFacingMultiplier:    1.3,     // Lower - less attractive target
		InternetExposedMultiplier: 1.5,
		PrivilegedAndHostPath:     1.5,
		RootAndHostNetwork:        1.3,
	}
}

// GetConfigForIndustry returns appropriate config for industry
func GetConfigForIndustry(industry string) *RiskCostConfig {
	switch industry {
	case "pharma", "pharmaceutical", "healthcare", "medical":
		return PharmaConfig()
	case "fintech", "finance", "banking", "payment":
		return FintechConfig()
	case "startup", "early-stage":
		return StartupConfig()
	default:
		return DefaultConfig()
	}
}
