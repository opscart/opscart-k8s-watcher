package models

// SecurityAudit represents a complete security audit of the cluster
type SecurityAudit struct {
	TotalPodsAudited int             `json:"total_pods_audited"`
	SecurityScore    int             `json:"security_score"` // 0-100
	Risks            SecurityRisks   `json:"risks"`
	Issues           []SecurityIssue `json:"issues"`
	PriorityActions  []string        `json:"priority_actions"`
}

// SecurityRisks contains counts of different security risks
type SecurityRisks struct {
	RunningAsRoot         int `json:"running_as_root"`
	PrivilegedContainers  int `json:"privileged_containers"`
	HostNetwork           int `json:"host_network"`
	HostPID               int `json:"host_pid"`
	HostIPC               int `json:"host_ipc"`
	HostPathVolumes       int `json:"host_path_volumes"`       // NEW - Phase 1
	DefaultServiceAccount int `json:"default_service_account"` // NEW - Phase 1
	MissingResourceLimits int `json:"missing_resource_limits"`
	MissingProbes         int `json:"missing_probes"`
	AddedCapabilities     int `json:"added_capabilities"`
	PrivilegeEscalation   int `json:"privilege_escalation"`
	WritableFilesystem    int `json:"writable_filesystem"`
}

// SecurityIssue represents a single security issue
type SecurityIssue struct {
	Type        string `json:"type"`     // "privileged_container", "running_as_root", etc.
	Severity    string `json:"severity"` // "critical", "high", "medium", "low"
	Resource    string `json:"resource"` // "pod", "container"
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Remediation string `json:"remediation"`
}

// EnhancedClusterSnapshot represents detailed cluster state
type EnhancedClusterSnapshot struct {
	ClusterSnapshot // Embed the base snapshot

	// Extended information
	Services        []ServiceDetail     `json:"services"`
	Ingresses       []IngressDetail     `json:"ingresses"`
	PVCDetails      []PVCDetail         `json:"pvc_details"`
	ConfigMaps      []ResourceCount     `json:"configmaps"`
	Secrets         []ResourceCount     `json:"secrets"`
	NetworkPolicies []NetworkPolicyInfo `json:"network_policies"`
}

// ServiceDetail represents detailed service information
type ServiceDetail struct {
	ServiceInfo                   // Embed base service info
	Endpoints   int               `json:"endpoints"`
	Age         string            `json:"age"`
	Selector    map[string]string `json:"selector,omitempty"`
}

// IngressDetail represents detailed ingress information
type IngressDetail struct {
	IngressInfo         // Embed base ingress info
	IngressClass string `json:"ingress_class,omitempty"`
	Age          string `json:"age"`
	Rules        int    `json:"rules"`
}

// PVCDetail represents detailed PVC information
type PVCDetail struct {
	PVCInfo           // Embed base PVC info
	Age        string `json:"age"`
	AccessMode string `json:"access_mode"`
	UsedBy     string `json:"used_by,omitempty"` // Pod using this PVC
}

// ResourceCount represents count of resources in a namespace
type ResourceCount struct {
	Namespace string `json:"namespace"`
	Count     int    `json:"count"`
}

// NetworkPolicyInfo represents network policy information
type NetworkPolicyInfo struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	PodSelector string   `json:"pod_selector"`
	PolicyTypes []string `json:"policy_types"` // Ingress, Egress
}
