package analyzer

import (
	"fmt"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CostPoolKey is the provider-neutral identity of a priced compute pool.
// All fields participate in identity; missing metadata must remain unresolved.
type CostPoolKey struct {
	Provider     string
	PoolName     string
	InstanceType string
	CapacityType string
	Region       string
	OS           string
	Architecture string
}

// PodCostEligibility describes whether a Pod can participate in dollar allocation.
type PodCostEligibility string

const (
	PodCostEligible   PodCostEligibility = "eligible"
	PodCostExcluded   PodCostEligibility = "excluded"
	PodCostUnresolved PodCostEligibility = "unresolved"
)

// PodCostReason preserves why a Pod was included, excluded, or unresolved.
type PodCostReason string

const (
	PodCostReasonBoundRunning        PodCostReason = "bound_running"
	PodCostReasonBoundPending        PodCostReason = "bound_pending"
	PodCostReasonUnknownPhaseBound   PodCostReason = "unknown_phase_bound"
	PodCostReasonUnassignedRunning   PodCostReason = "unassigned_running"
	PodCostReasonUnassignedPending   PodCostReason = "unassigned_pending"
	PodCostReasonSucceeded           PodCostReason = "succeeded"
	PodCostReasonFailed              PodCostReason = "failed"
	PodCostReasonDeleting            PodCostReason = "deleting"
	PodCostReasonUnknownPhaseUnbound PodCostReason = "unknown_phase_unbound"
	PodCostReasonNodeMissing         PodCostReason = "node_missing_from_snapshot"
)

// OwnerResolutionStatus captures how workload ownership was resolved.
type OwnerResolutionStatus string

const (
	OwnerResolutionResolved             OwnerResolutionStatus = "resolved"
	OwnerResolutionBarePod              OwnerResolutionStatus = "bare_pod"
	OwnerResolutionAmbiguous            OwnerResolutionStatus = "ambiguous_controller_owner"
	OwnerResolutionUnresolvedReplicaSet OwnerResolutionStatus = "unresolved_replicaset"
	OwnerResolutionUnresolvedJob        OwnerResolutionStatus = "unresolved_job"
)

// OwnerResolution preserves ownership evidence without guessing.
type OwnerResolution struct {
	Status         OwnerResolutionStatus
	ControllerKind string
	ControllerName string
	ControllerUID  types.UID
	Warning        string
}

// PodCostInput is the canonical Kubernetes-side input for cost allocation.
type PodCostInput struct {
	Namespace string
	PodName   string
	NodeName  string

	Phase             corev1.PodPhase
	DeletionTimestamp *metav1.Time

	Eligibility        PodCostEligibility
	EligibilityReason  PodCostReason
	EligibilityWarning string

	WorkloadKind string
	WorkloadName string
	WorkloadUID  types.UID

	ParentWorkloadKind string
	ParentWorkloadName string
	ParentWorkloadUID  types.UID

	OwnerResolution OwnerResolution

	CPURequestMilli    int64
	MemoryRequestBytes int64

	PoolKey *CostPoolKey
}

// ControllerIndexes are pure, cluster-wide indexes used for deterministic
// owner resolution. Building these indexes performs no Kubernetes API calls.
type ControllerIndexes struct {
	ReplicaSets map[string]appsv1.ReplicaSet
	Jobs        map[string]batchv1.Job
}

// NewControllerIndexes builds cluster-wide ReplicaSet and Job indexes.
func NewControllerIndexes(replicaSets []appsv1.ReplicaSet, jobs []batchv1.Job) ControllerIndexes {
	idx := ControllerIndexes{
		ReplicaSets: make(map[string]appsv1.ReplicaSet, len(replicaSets)),
		Jobs:        make(map[string]batchv1.Job, len(jobs)),
	}
	for _, rs := range replicaSets {
		idx.ReplicaSets[namespacedKey(rs.Namespace, rs.Name)] = rs
	}
	for _, job := range jobs {
		idx.Jobs[namespacedKey(job.Namespace, job.Name)] = job
	}
	return idx
}

func namespacedKey(namespace, name string) string {
	return namespace + "\x00" + name
}

// EffectivePodRequests returns Kubernetes-effective CPU/memory requests.
// CPU is returned in millicores and memory in bytes.
func EffectivePodRequests(pod corev1.Pod) (cpuMilli int64, memoryBytes int64) {
	appCPU, appMem := int64(0), int64(0)
	for _, c := range pod.Spec.Containers {
		cpu, mem := containerRequests(c)
		appCPU += cpu
		appMem += mem
	}

	restartableCPU, restartableMem := int64(0), int64(0)
	peakInitCPU, peakInitMem := int64(0), int64(0)

	for _, c := range pod.Spec.InitContainers {
		cpu, mem := containerRequests(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			restartableCPU += cpu
			restartableMem += mem
			appCPU += cpu
			appMem += mem
			if restartableCPU > peakInitCPU {
				peakInitCPU = restartableCPU
			}
			if restartableMem > peakInitMem {
				peakInitMem = restartableMem
			}
			continue
		}

		stageCPU := restartableCPU + cpu
		stageMem := restartableMem + mem
		if stageCPU > peakInitCPU {
			peakInitCPU = stageCPU
		}
		if stageMem > peakInitMem {
			peakInitMem = stageMem
		}
	}

	cpuMilli = maxInt64(appCPU, peakInitCPU)
	memoryBytes = maxInt64(appMem, peakInitMem)

	if pod.Spec.Overhead != nil {
		if q, ok := pod.Spec.Overhead[corev1.ResourceCPU]; ok {
			cpuMilli += q.MilliValue()
		}
		if q, ok := pod.Spec.Overhead[corev1.ResourceMemory]; ok {
			memoryBytes += q.Value()
		}
	}
	return cpuMilli, memoryBytes
}

func containerRequests(c corev1.Container) (int64, int64) {
	var cpuMilli, memoryBytes int64
	if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		cpuMilli = q.MilliValue()
	}
	if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		memoryBytes = q.Value()
	}
	return cpuMilli, memoryBytes
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ClassifyPodCostEligibility classifies a Pod using an already-observed node snapshot.
func ClassifyPodCostEligibility(pod corev1.Pod, knownNodes map[string]struct{}) (PodCostEligibility, PodCostReason, string) {
	if pod.DeletionTimestamp != nil {
		return PodCostExcluded, PodCostReasonDeleting, ""
	}

	nodeName := strings.TrimSpace(pod.Spec.NodeName)
	if nodeName != "" {
		if _, ok := knownNodes[nodeName]; !ok {
			return PodCostUnresolved, PodCostReasonNodeMissing, fmt.Sprintf("pod is bound to node %q, which is absent from the node snapshot", nodeName)
		}
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		if nodeName == "" {
			return PodCostUnresolved, PodCostReasonUnassignedRunning, "running pod has no node assignment"
		}
		return PodCostEligible, PodCostReasonBoundRunning, ""
	case corev1.PodPending:
		if nodeName == "" {
			return PodCostUnresolved, PodCostReasonUnassignedPending, "pending pod has no node assignment"
		}
		return PodCostEligible, PodCostReasonBoundPending, ""
	case corev1.PodSucceeded:
		return PodCostExcluded, PodCostReasonSucceeded, ""
	case corev1.PodFailed:
		return PodCostExcluded, PodCostReasonFailed, ""
	default:
		if nodeName == "" {
			return PodCostUnresolved, PodCostReasonUnknownPhaseUnbound, "pod phase is unknown and pod has no node assignment"
		}
		return PodCostEligible, PodCostReasonUnknownPhaseBound, fmt.Sprintf("pod phase %q is not a recognized allocation phase", pod.Status.Phase)
	}
}

// BuildNodeCostPoolKey resolves a canonical CostPoolKey only from observed node
// metadata. It performs no pricing lookup and never substitutes missing values.
func BuildNodeCostPoolKey(node corev1.Node) (*CostPoolKey, []string) {
	labels := node.Labels
	key := CostPoolKey{
		Provider:     string(DetectNodeProvider(node)),
		PoolName:     strings.TrimSpace(kube.NodePoolName(node)),
		InstanceType: firstNonEmpty(labels["node.kubernetes.io/instance-type"], labels["beta.kubernetes.io/instance-type"]),
		CapacityType: firstNonEmpty(labels["kubernetes.azure.com/scalesetpriority"], labels["eks.amazonaws.com/capacityType"]),
		Region:       firstNonEmpty(labels["topology.kubernetes.io/region"], labels["failure-domain.beta.kubernetes.io/region"]),
		OS:           firstNonEmpty(labels["kubernetes.io/os"], node.Status.NodeInfo.OperatingSystem),
		Architecture: firstNonEmpty(labels["kubernetes.io/arch"], node.Status.NodeInfo.Architecture),
	}

	var reasons []string
	if key.Provider == "" || key.Provider == string(CloudProviderUnknown) {
		reasons = append(reasons, "provider")
	}
	if key.PoolName == "" {
		reasons = append(reasons, "pool name")
	}
	if key.InstanceType == "" {
		reasons = append(reasons, "instance type")
	}
	if key.CapacityType == "" {
		reasons = append(reasons, "capacity type")
	}
	if key.Region == "" {
		reasons = append(reasons, "region")
	}
	if key.OS == "" {
		reasons = append(reasons, "os")
	}
	if key.Architecture == "" {
		reasons = append(reasons, "architecture")
	}
	if len(reasons) > 0 {
		return nil, reasons
	}
	return &key, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}

// BuildNodePoolKeyIndex maps observed node names to canonical pool identities.
func BuildNodePoolKeyIndex(nodes []corev1.Node) (map[string]*CostPoolKey, map[string][]string) {
	keys := make(map[string]*CostPoolKey, len(nodes))
	unresolved := make(map[string][]string)
	for _, node := range nodes {
		key, reasons := BuildNodeCostPoolKey(node)
		if key == nil {
			unresolved[node.Name] = append([]string(nil), reasons...)
			continue
		}
		keys[node.Name] = key
	}
	return keys, unresolved
}

// ResolvePodWorkload resolves a Pod to exactly one leaf workload, with an
// optional CronJob parent subtotal for Jobs.
func ResolvePodWorkload(pod corev1.Pod, indexes ControllerIndexes) (workloadKind, workloadName string, workloadUID types.UID, parentKind, parentName string, parentUID types.UID, resolution OwnerResolution) {
	controllerOwners := controllerOwnerReferences(pod.OwnerReferences)
	if len(controllerOwners) == 0 {
		return "Pod", pod.Name, pod.UID, "", "", "", OwnerResolution{Status: OwnerResolutionBarePod}
	}
	if len(controllerOwners) > 1 {
		return "UnknownOwner", pod.Name, pod.UID, "", "", "", OwnerResolution{
			Status:  OwnerResolutionAmbiguous,
			Warning: "pod has multiple controller owner references",
		}
	}

	owner := controllerOwners[0]
	resolution = OwnerResolution{
		Status:         OwnerResolutionResolved,
		ControllerKind: owner.Kind,
		ControllerName: owner.Name,
		ControllerUID:  owner.UID,
	}

	switch owner.Kind {
	case "ReplicaSet":
		rs, ok := indexes.ReplicaSets[namespacedKey(pod.Namespace, owner.Name)]
		if !ok {
			resolution.Status = OwnerResolutionUnresolvedReplicaSet
			resolution.Warning = "replicaset controller not present in cluster-wide index"
			return "ReplicaSet", owner.Name, owner.UID, "", "", "", resolution
		}
		rsOwners := controllerOwnerReferences(rs.OwnerReferences)
		if len(rsOwners) == 1 && rsOwners[0].Kind == "Deployment" {
			dep := rsOwners[0]
			return "Deployment", dep.Name, dep.UID, "", "", "", resolution
		}
		if len(rsOwners) != 0 {
			resolution.Status = OwnerResolutionUnresolvedReplicaSet
			resolution.Warning = "replicaset controller could not be resolved uniquely to a Deployment"
		}
		return "ReplicaSet", rs.Name, rs.UID, "", "", "", resolution

	case "StatefulSet", "DaemonSet":
		return owner.Kind, owner.Name, owner.UID, "", "", "", resolution

	case "Job":
		job, ok := indexes.Jobs[namespacedKey(pod.Namespace, owner.Name)]
		if !ok {
			resolution.Status = OwnerResolutionUnresolvedJob
			resolution.Warning = "job controller not present in cluster-wide index; CronJob parent cannot be resolved"
			return "Job", owner.Name, owner.UID, "", "", "", resolution
		}
		jobOwners := controllerOwnerReferences(job.OwnerReferences)
		if len(jobOwners) == 1 && jobOwners[0].Kind == "CronJob" {
			parent := jobOwners[0]
			return "Job", job.Name, job.UID, "CronJob", parent.Name, parent.UID, resolution
		}
		return "Job", job.Name, job.UID, "", "", "", resolution

	default:
		return owner.Kind, owner.Name, owner.UID, "", "", "", resolution
	}
}

func controllerOwnerReferences(refs []metav1.OwnerReference) []metav1.OwnerReference {
	owners := make([]metav1.OwnerReference, 0, 1)
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			owners = append(owners, ref)
		}
	}
	return owners
}

// BuildPodCostInput builds one canonical PodCostInput from already-observed
// Kubernetes snapshots. It performs no Kubernetes or cloud API calls.
func BuildPodCostInput(pod corev1.Pod, knownNodes map[string]struct{}, nodePoolKeys map[string]*CostPoolKey, indexes ControllerIndexes) PodCostInput {
	cpuMilli, memoryBytes := EffectivePodRequests(pod)
	eligibility, reason, warning := ClassifyPodCostEligibility(pod, knownNodes)
	workloadKind, workloadName, workloadUID, parentKind, parentName, parentUID, ownerResolution := ResolvePodWorkload(pod, indexes)

	input := PodCostInput{
		Namespace:          pod.Namespace,
		PodName:            pod.Name,
		NodeName:           pod.Spec.NodeName,
		Phase:              pod.Status.Phase,
		DeletionTimestamp:  pod.DeletionTimestamp,
		Eligibility:        eligibility,
		EligibilityReason:  reason,
		EligibilityWarning: warning,
		WorkloadKind:       workloadKind,
		WorkloadName:       workloadName,
		WorkloadUID:        workloadUID,
		ParentWorkloadKind: parentKind,
		ParentWorkloadName: parentName,
		ParentWorkloadUID:  parentUID,
		OwnerResolution:    ownerResolution,
		CPURequestMilli:    cpuMilli,
		MemoryRequestBytes: memoryBytes,
	}

	if eligibility == PodCostEligible && pod.Spec.NodeName != "" {
		if key, ok := nodePoolKeys[pod.Spec.NodeName]; ok {
			copied := *key
			input.PoolKey = &copied
		} else {
			input.Eligibility = PodCostUnresolved
			input.EligibilityReason = PodCostReasonNodeMissing
			if input.EligibilityWarning == "" {
				input.EligibilityWarning = fmt.Sprintf("node %q has no resolved cost pool identity", pod.Spec.NodeName)
			}
		}
	}
	return input
}
