package models

import "time"

// EmergencyIssue represents a critical problem found in the cluster
type EmergencyIssue struct {
	Severity  string        `json:"severity"` // critical, high, medium, low
	Resource  string        `json:"resource"` // pod, deployment, pvc, etc.
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	Reason    string        `json:"reason"`
	Message   string        `json:"message"`
	Age       time.Duration `json:"age"`
	Restarts  int           `json:"restarts,omitempty"`
	LastEvent string        `json:"last_event,omitempty"`
}

// ClusterSnapshot represents the current state of a cluster
type ClusterSnapshot struct {
	ClusterName  string            `json:"cluster_name"`
	Timestamp    time.Time         `json:"timestamp"`
	Namespaces   []NamespaceInfo   `json:"namespaces"`
	TotalPods    int               `json:"total_pods"`
	HealthyPods  int               `json:"healthy_pods"`
	ProblemPods  int               `json:"problem_pods"`
	Deployments  []DeploymentInfo  `json:"deployments"`
	StatefulSets []StatefulSetInfo `json:"statefulsets"`
	Services     []ServiceInfo     `json:"services"`
	Ingresses    []IngressInfo     `json:"ingresses"`
	PVCs         []PVCInfo         `json:"pvcs"`
}

// NamespaceInfo contains namespace-level information
type NamespaceInfo struct {
	Name          string `json:"name"`
	PodCount      int    `json:"pod_count"`
	HealthyPods   int    `json:"healthy_pods"`
	ProblemPods   int    `json:"problem_pods"`
	HasIngress    bool   `json:"has_ingress"`
	ResourceQuota bool   `json:"resource_quota"`
}

// DeploymentInfo represents a deployment's status
type DeploymentInfo struct {
	Name              string        `json:"name"`
	Namespace         string        `json:"namespace"`
	Replicas          int32         `json:"replicas"`
	ReadyReplicas     int32         `json:"ready_replicas"`
	AvailableReplicas int32         `json:"available_replicas"`
	Healthy           bool          `json:"healthy"`
	Age               time.Duration `json:"age"`
	Image             string        `json:"image"`
}

// StatefulSetInfo represents a statefulset's status
type StatefulSetInfo struct {
	Name                 string        `json:"name"`
	Namespace            string        `json:"namespace"`
	Replicas             int32         `json:"replicas"`
	ReadyReplicas        int32         `json:"ready_replicas"`
	Healthy              bool          `json:"healthy"`
	Age                  time.Duration `json:"age"`
	VolumeClaimTemplates []string      `json:"volume_claim_templates"`
}

// ServiceInfo represents a service
type ServiceInfo struct {
	Name       string  `json:"name"`
	Namespace  string  `json:"namespace"`
	Type       string  `json:"type"`
	ClusterIP  string  `json:"cluster_ip"`
	ExternalIP string  `json:"external_ip,omitempty"`
	Ports      []int32 `json:"ports"`
}

// IngressInfo represents an ingress resource
type IngressInfo struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Hosts      []string `json:"hosts"`
	TLSEnabled bool     `json:"tls_enabled"`
	Backend    string   `json:"backend"`
}

// PVCInfo represents a persistent volume claim
type PVCInfo struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Status       string `json:"status"`
	StorageClass string `json:"storage_class"`
	Size         string `json:"size"`
	VolumeName   string `json:"volume_name,omitempty"`
}

// PodInfo represents detailed pod information
type PodInfo struct {
	Name            string        `json:"name"`
	Namespace       string        `json:"namespace"`
	Phase           string        `json:"phase"`
	Ready           bool          `json:"ready"`
	Restarts        int           `json:"restarts"`
	Age             time.Duration `json:"age"`
	Node            string        `json:"node"`
	IP              string        `json:"ip"`
	Containers      int           `json:"containers"`
	ReadyContainers int           `json:"ready_containers"`
	Reason          string        `json:"reason,omitempty"`
}

// IdleResource represents a resource that appears idle
type IdleResource struct {
	Type            string    `json:"type"`
	Name            string    `json:"name"`
	Namespace       string    `json:"namespace"`
	IdleDays        int       `json:"idle_days"`
	LastActivity    time.Time `json:"last_activity"`
	EstCostPerMonth float64   `json:"est_cost_per_month"`
	Recommendation  string    `json:"recommendation"`
}

// ResourceSearchResult represents a found resource
type ResourceSearchResult struct {
	ClusterName string `json:"cluster"`
	Type        string `json:"type"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Status      string `json:"status"`
}
