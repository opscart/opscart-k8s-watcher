package scanner

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nodeIncidentScope = "cluster"
	// fallbackNodeConditionSeverity is used only for a condition type
	// models.NodeConditionSeverity doesn't recognize, so persistence never
	// fails outright on an unexpected/future condition type. It must not be
	// used for any condition the classifier already knows about — those
	// take their real severity below.
	fallbackNodeConditionSeverity = "low"
)

type nodeConditionDetails struct {
	NodeName             string                      `json:"node_name"`
	NodePool             string                      `json:"node_pool,omitempty"`
	ConditionType        string                      `json:"condition_type"`
	ConditionStatus      string                      `json:"condition_status"`
	Reason               string                      `json:"reason,omitempty"`
	Message              string                      `json:"message,omitempty"`
	LastTransitionTime   time.Time                   `json:"last_transition_time"`
	CorrelationSemantics string                      `json:"correlation_semantics"`
	CorrelatedWorkloads  []models.CorrelatedWorkload `json:"correlated_workloads"`
}

// NodeConditionIncidents converts current Node findings to the existing store
// write model. Callers must include these in their scan-wide incident batch;
// lifecycle transitions remain the responsibility of UpsertIncidents and the
// scan's single ResolveMissing call.
func NodeConditionIncidents(findings []models.NodeConditionFinding) []store.IncidentData {
	incidents := make([]store.IncidentData, 0, len(findings))
	for _, finding := range findings {
		details, _ := json.Marshal(nodeConditionDetails{
			NodeName:             finding.NodeName,
			NodePool:             finding.NodePool,
			ConditionType:        finding.ConditionType,
			ConditionStatus:      finding.ConditionStatus,
			Reason:               finding.Reason,
			Message:              finding.Message,
			LastTransitionTime:   finding.LastTransitionTime,
			CorrelationSemantics: "correlated_by_node_placement",
			CorrelatedWorkloads:  finding.CorrelatedWorkloads,
		})
		// Persisted severity must match what the War Room already displays
		// for this same finding (models.NodeConditionSeverity — see
		// nodeWarRoomSeverity in the dashboard). A Ready=False node shown
		// as critical live must not be persisted as "low"; that silently
		// corrupts incident listings, historical counts, and scoring that
		// read the stored value instead of re-deriving it.
		severity, ok := models.NodeConditionSeverity(finding.ConditionType)
		if !ok {
			severity = fallbackNodeConditionSeverity
		}
		incidents = append(incidents, store.IncidentData{
			Fingerprint: store.Fingerprint(nodeIncidentScope, "Node", finding.NodeName, finding.ConditionType),
			Namespace:   "",
			Resource:    finding.NodeName,
			IssueType:   finding.ConditionType,
			Severity:    severity,
			DetailsJSON: string(details),
		})
	}
	return incidents
}

// FindNodeHealthConditions collects the current cluster-wide Node and Pod
// snapshots and returns unhealthy Node conditions with placement correlation.
// It has no persistence or presentation side effects.
func (s *Scanner) FindNodeHealthConditions() ([]models.NodeConditionFinding, error) {
	nodes, err := s.clientset.CoreV1().Nodes().List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	findings := DetectUnhealthyNodeConditions(nodes.Items)
	if len(findings) == 0 {
		return findings, nil
	}
	pods, err := s.clientset.CoreV1().Pods("").List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for node correlation: %w", err)
	}
	return correlateNodeWorkloadsWithOwners(findings, pods.Items, s.jobOwnerIndex("")), nil
}

// DetectUnhealthyNodeConditions returns one finding per unhealthy Kubernetes
// Node condition. It preserves the condition evidence without inferring a
// cause (including for NetworkUnavailable).
func DetectUnhealthyNodeConditions(nodes []corev1.Node) []models.NodeConditionFinding {
	var findings []models.NodeConditionFinding
	for _, node := range nodes {
		for _, condition := range node.Status.Conditions {
			if !isUnhealthyNodeCondition(condition) {
				continue
			}
			findings = append(findings, models.NodeConditionFinding{
				NodeName:           node.Name,
				NodePool:           kube.NodePoolName(node),
				ConditionType:      string(condition.Type),
				ConditionStatus:    string(condition.Status),
				Reason:             condition.Reason,
				Message:            condition.Message,
				LastTransitionTime: condition.LastTransitionTime.Time,
			})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].NodeName != findings[j].NodeName {
			return findings[i].NodeName < findings[j].NodeName
		}
		return findings[i].ConditionType < findings[j].ConditionType
	})
	return findings
}

func isUnhealthyNodeCondition(condition corev1.NodeCondition) bool {
	switch condition.Type {
	case corev1.NodeReady:
		return condition.Status == corev1.ConditionFalse || condition.Status == corev1.ConditionUnknown
	case corev1.NodeDiskPressure, corev1.NodeMemoryPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable:
		return condition.Status == corev1.ConditionTrue
	default:
		return false
	}
}

// CorrelateNodeWorkloads attaches pods to findings solely by current
// spec.nodeName placement. Unscheduled pods are intentionally ignored. Jobs
// supply the existing Job-to-CronJob owner resolution used by pod scanning.
func CorrelateNodeWorkloads(findings []models.NodeConditionFinding, pods []corev1.Pod, jobs []batchv1.Job) []models.NodeConditionFinding {
	return correlateNodeWorkloadsWithOwners(findings, pods, jobOwnersFromJobs(jobs))
}

func correlateNodeWorkloadsWithOwners(findings []models.NodeConditionFinding, pods []corev1.Pod, jobOwners map[string]workloadOwner) []models.NodeConditionFinding {
	result := append([]models.NodeConditionFinding(nil), findings...)
	byNode := make(map[string]map[models.CorrelatedWorkload]int)
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		workload := correlatedWorkloadForPod(pod, jobOwners)
		if byNode[pod.Spec.NodeName] == nil {
			byNode[pod.Spec.NodeName] = make(map[models.CorrelatedWorkload]int)
		}
		byNode[pod.Spec.NodeName][workload]++
	}

	for i := range result {
		counts := byNode[result[i].NodeName]
		workloads := make([]models.CorrelatedWorkload, 0, len(counts))
		for workload, count := range counts {
			workload.PodCount = count
			workloads = append(workloads, workload)
		}
		sort.Slice(workloads, func(i, j int) bool {
			if workloads[i].Namespace != workloads[j].Namespace {
				return workloads[i].Namespace < workloads[j].Namespace
			}
			if workloads[i].Kind != workloads[j].Kind {
				return workloads[i].Kind < workloads[j].Kind
			}
			return workloads[i].Name < workloads[j].Name
		})
		result[i].CorrelatedWorkloads = workloads
	}
	return result
}

func correlatedWorkloadForPod(pod corev1.Pod, jobOwners map[string]workloadOwner) models.CorrelatedWorkload {
	owner := podWorkloadOwner(pod, jobOwners)
	if owner.kind == "ReplicaSet" {
		return models.CorrelatedWorkload{Namespace: pod.Namespace, Kind: "Deployment", Name: store.OwnerNameFromPod(pod.Name)}
	}
	if owner.name != "" {
		return models.CorrelatedWorkload{Namespace: pod.Namespace, Kind: owner.kind, Name: owner.name}
	}
	return models.CorrelatedWorkload{Namespace: pod.Namespace, Kind: "Workload", Name: store.OwnerNameFromPod(pod.Name)}
}
