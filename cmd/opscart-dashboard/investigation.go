package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ── Investigation page ────────────────────────────────────────────────────────

type investigationEvent struct {
	Type    string // Normal / Warning
	Reason  string
	Message string
	Age     string
	Count   int32
}

type investigationPageData struct {
	// Sidebar fields
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ActivePage    string
	ClusterName   string
	CriticalCount int
	Clusters      []sidebarCluster

	// Pod identity
	PodName          string
	Namespace        string
	IssueType        string // crash_loop, image_pull_backoff, oomkilled, privileged_container, probe_failure...
	Severity         string
	OwnerKind        string // Deployment / StatefulSet / Job / (none)
	OwnerName        string
	ContainerName    string // set when the finding is container-scoped (e.g. privileged_container); empty otherwis
	LogContainers    []investigationLogContainer
	LogsAvailable    bool
	DefaultLogSource string
	WorkloadLabel    string
	TrackingLabel    string
	PodAge           string
	DescribeCommand  string

	// Status
	Phase         string
	Restarts      int32
	AgeDays       int
	StateReason   string // CrashLoopBackOff, ImagePullBackOff...
	NotFound      bool   // pod deleted since scan
	FirstDetected string

	// Namespace-scoped findings (unprotected_namespace, idle_namespace) — no pod involved
	NamespaceScoped              bool
	NamespacePodCount            int    // observed pods in the namespace
	NamespacePolicyCount         int    // observed NetworkPolicy objects
	NamespaceCoverageGapPodCount int    // observed pods without full directional coverage
	NamespaceFinding             string // the specific finding description

	// Node-scoped condition incidents.
	NodeScoped             bool
	NodeName               string
	NodePool               string
	IncidentStatus         string
	CurrentNodeObservation bool
	NodeConditionStatus    string
	NodeReason             string
	NodeMessage            string
	NodeLastTransition     string
	RetainedNodeEvidence   bool
	PlacementWorkloads     int
	PlacementPods          int
	PlacementNamespaces    []nodePlacementNamespace

	// Blast radius
	BlastSiblings      []blastRadiusPod     // sibling pods under same owner
	BlastServices      []blastRadiusService // services routing to this deployment
	BlastSharedConf    []blastSharedDep     // other deployments sharing same CMs/Secrets
	BlastHealthy       int                  // count of healthy siblings
	BlastTotal         int                  // total replicas
	BlastIngresses     []blastIngressRule   // ingress rules routing to affected services
	BlastNamespacePods []blastNamespacePod  // other workloads in namespace
	BlastNsHealthy     int                  // namespace-wide healthy pod count
	BlastNsTotal       int                  // namespace-wide total pod count
	CustomerImpact     string
	Timeline           []store.IncidentEvent
	// Sections
	Hints              []investigationHint  // possible causes
	Commands           []string             // investigation kubectl commands
	Events             []investigationEvent // last 10, this pod only
	ConfigMaps         []string
	Secrets            []string
	PVCs               []string
	OperationalSummary string

	ScannedAtMs int64
	BackURL     string
}

type nodePlacementNamespace struct {
	Name      string
	Workloads int
	Pods      int
	Items     []models.CorrelatedWorkload
}

type nodeInvestigationDetails struct {
	NodeName            string                      `json:"node_name"`
	NodePool            string                      `json:"node_pool"`
	ConditionType       string                      `json:"condition_type"`
	ConditionStatus     string                      `json:"condition_status"`
	Reason              string                      `json:"reason"`
	Message             string                      `json:"message"`
	LastTransitionTime  time.Time                   `json:"last_transition_time"`
	CorrelatedWorkloads []models.CorrelatedWorkload `json:"correlated_workloads"`
}

type blastIngressRule struct {
	Host     string
	Path     string
	External bool // true = customer-facing
}

type blastNamespacePod struct {
	WorkloadName string
	Healthy      int
	Total        int
}

type investigationHint struct {
	Confidence string // "high" / "medium" / "low"
	Step       int
	Title      string
	Reason     string
	Command    string // optional — if this hint has a specific command
}

type blastRadiusPod struct {
	Name    string
	Phase   string
	Healthy bool
}

type blastRadiusService struct {
	Name      string
	Namespace string
	Type      string // ClusterIP / LoadBalancer / NodePort
	Ports     string // "80/TCP, 443/TCP"
}

type blastSharedDep struct {
	DeploymentName string
	SharedItems    []string // names of the shared CMs/Secrets
}

func investigationHints(issueType string, stateReason string, restarts int32, pod *corev1.Pod, namespace string) []investigationHint {
	var hints []investigationHint

	switch issueType {
	case "unprotected_namespace":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Evaluate a default-deny NetworkPolicy",
			Reason:     "Review existing NetworkPolicy selectors and intended ingress and egress controls before considering a default-deny policy.",
			Command:    fmt.Sprintf("kubectl get networkpolicies -n %s", namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Review pods and intended network access",
			Reason:     "Confirm the pod count and whether any of them handle sensitive data or traffic.",
			Command:    fmt.Sprintf("kubectl get pods -n %s", namespace),
		})

	case "idle_namespace":
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Confirm this namespace is genuinely unused",
			Reason:     "Idle namespaces consuming resources may be forgotten test environments or decommissioned projects.",
			Command:    fmt.Sprintf("kubectl get all -n %s", namespace),
		})

	case "crash_loop":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Check previous container logs",
			Reason:     "Container is exiting after start — previous logs capture the failure before restart.",
			Command:    fmt.Sprintf("kubectl logs %s -n %s --previous", pod.Name, pod.Namespace),
		})
		if restarts > 100 {
			hints = append(hints, investigationHint{
				Confidence: "high",
				Title:      "Deterministic failure — not transient",
				Reason: fmt.Sprintf(
					"The pod has restarted %d times. Review previous container logs and restart timing to identify the recurring condition.",
					restarts,
				),
			})
		}
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Verify ConfigMaps and Secrets exist",
			Reason:     "Missing or misconfigured references cause immediate container exit with no log output.",
			Command:    fmt.Sprintf("kubectl describe pod %s -n %s", pod.Name, pod.Namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Check liveness probe configuration",
			Reason:     "An aggressive probe threshold can kill a healthy container before it finishes starting.",
		})
		hints = append(hints, investigationHint{
			Confidence: "low",
			Title:      "Check for OOMKill in events",
			Reason:     "If memory limit is too low, container is killed before writing logs.",
			Command:    fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", pod.Namespace, pod.Name),
		})

	case "image_pull_backoff":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Verify image tag exists in registry",
			Reason:     "Deleted or mistyped image tags are the most common cause of ImagePullBackOff.",
			Command:    fmt.Sprintf("kubectl describe pod %s -n %s", pod.Name, pod.Namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Check imagePullSecrets in namespace",
			Reason:     "Private registries require a valid pull secret referenced by the pod spec.",
			Command:    fmt.Sprintf("kubectl get secrets -n %s", pod.Namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Verify registry credentials have not expired",
			Reason:     "Credentials rotate on some registries — a working deployment can break without code changes.",
		})

	case "oomkilled":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Memory limit is below actual usage",
			Reason:     "Container was killed by the kernel OOM killer — raise resources.limits.memory.",
			Command:    fmt.Sprintf("kubectl describe pod %s -n %s", pod.Name, pod.Namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Check for JVM heap settings",
			Reason:     "JVM workloads need explicit heap limits that respect the container memory ceiling.",
		})
		hints = append(hints, investigationHint{
			Confidence: "low",
			Title:      "Look for periodic memory growth",
			Reason:     "If OOM kills are periodic rather than immediate, a memory leak or batch job may be the cause.",
		})

	case "privileged_container":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Confirm privileged mode is actually required",
			Reason:     "Most workloads do not need privileged: true — verify with the owning team.",
			Command: fmt.Sprintf(
				"kubectl get pod %s -n %s -o jsonpath='{range .spec.containers[*]}{.name}{\"\\tprivileged=\"}{.securityContext.privileged}{\"\\n\"}{end}'",
				pod.Name,
				pod.Namespace,
			),
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Replace with specific capabilities",
			Reason:     "Use securityContext.capabilities.add for specific needs instead of full privileged access.",
			Command: fmt.Sprintf(
				"kubectl get pod %s -n %s -o jsonpath='{range .spec.containers[*]}{.name}{\"\\tadd=\"}{.securityContext.capabilities.add}{\"\\tdrop=\"}{.securityContext.capabilities.drop}{\"\\n\"}{end}'",
				pod.Name,
				pod.Namespace,
			),
		})
	case "probe_failure":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Check what the startup/liveness probe is actually testing",
			Reason:     "The container is starting and running, but kubelet is killing it for failing its configured probe — the app itself may be healthy.",
			Command:    fmt.Sprintf("kubectl describe pod %s -n %s", pod.Name, pod.Namespace),
		})
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Compare probe timing against real startup time",
			Reason:     "A probe that fires before the app finishes initializing will kill a healthy container. Check initialDelaySeconds, periodSeconds, and failureThreshold against actual startup time.",
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Check previous container logs for the probe endpoint's response",
			Reason:     "If the probe hits an HTTP endpoint, the app's own logs often show why that endpoint returned an error or timed out.",
			Command:    fmt.Sprintf("kubectl logs %s -n %s --previous", pod.Name, pod.Namespace),
		})

	default:
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Inspect pod events",
			Reason:     "Events contain the most recent failure reason from the Kubernetes control plane.",
			Command:    fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", pod.Namespace, pod.Name),
		})
	}
	for i := range hints {
		hints[i].Step = i + 1
	}
	return hints
}

// resolveOwner walks Pod → ReplicaSet → Deployment (or returns the direct owner).
func resolveOwner(clientset kubernetes.Interface, pod *corev1.Pod) (kind, name string) {
	if len(pod.OwnerReferences) == 0 {
		return "", ""
	}
	ref := pod.OwnerReferences[0]
	kind, name = ref.Kind, ref.Name

	// If owned by a ReplicaSet, walk up to the Deployment
	if ref.Kind == "ReplicaSet" {
		rs, err := clientset.AppsV1().ReplicaSets(pod.Namespace).Get(
			context.TODO(), ref.Name, metav1.GetOptions{})
		if err == nil && len(rs.OwnerReferences) > 0 {
			parent := rs.OwnerReferences[0]
			kind, name = parent.Kind, parent.Name
		}
	}
	return kind, name
}

// podEvents returns the last n events for a specific pod, newest first.
func podEvents(clientset kubernetes.Interface, namespace, podName string, n int) []investigationEvent {
	evList, err := clientset.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		log.Printf("investigation: listing events for %s/%s: %v", namespace, podName, err)
		return nil
	}

	events := evList.Items
	sort.Slice(events, func(i, j int) bool {
		return events[i].LastTimestamp.After(events[j].LastTimestamp.Time)
	})
	if len(events) > n {
		events = events[:n]
	}

	var out []investigationEvent
	for _, e := range events {
		age := "unknown"
		if !e.LastTimestamp.IsZero() {
			age = humanAge(time.Since(e.LastTimestamp.Time))
		}
		out = append(out, investigationEvent{
			Type:    e.Type,
			Reason:  e.Reason,
			Message: e.Message,
			Age:     age,
			Count:   e.Count,
		})
	}
	return out
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// referencedNames pulls ConfigMap/Secret/PVC names actually referenced by the pod spec.
func referencedResources(pod *corev1.Pod) (cms, secrets, pvcs []string) {
	seen := map[string]bool{}
	add := func(list *[]string, name string) {
		key := name
		if !seen[key] && name != "" {
			seen[key] = true
			*list = append(*list, name)
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil {
			add(&cms, "cm:"+v.ConfigMap.Name)
		}
		if v.Secret != nil {
			add(&secrets, "sec:"+v.Secret.SecretName)
		}
		if v.PersistentVolumeClaim != nil {
			add(&pvcs, "pvc:"+v.PersistentVolumeClaim.ClaimName)
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef != nil {
				add(&cms, "cm:"+ef.ConfigMapRef.Name)
			}
			if ef.SecretRef != nil {
				add(&secrets, "sec:"+ef.SecretRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil {
				add(&cms, "cm:"+e.ValueFrom.ConfigMapKeyRef.Name)
			}
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(&secrets, "sec:"+e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	// strip prefixes used for dedup
	clean := func(list []string) []string {
		out := make([]string, 0, len(list))
		for _, s := range list {
			out = append(out, s[strings.Index(s, ":")+1:])
		}
		return out
	}
	return clean(cms), clean(secrets), clean(pvcs)
}

// blastRadiusSiblings returns all pods owned by the same workload.
func blastRadiusSiblings(clientset kubernetes.Interface, namespace, ownerKind, ownerName string) (pods []blastRadiusPod, healthy, total int) {
	if ownerName == "" {
		return
	}
	list, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, p := range list.Items {
		kind, name := resolveOwner(clientset, &p)
		if kind != ownerKind || name != ownerName {
			continue
		}
		// Derive kubectl-style status from container state
		status := string(p.Status.Phase)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				status = cs.State.Waiting.Reason
				break
			}
			if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
				status = cs.State.Terminated.Reason
				break
			}
		}
		isHealthy := p.Status.Phase == corev1.PodRunning && func() bool {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil || !cs.Ready {
					return false
				}
			}
			return true
		}()
		if !isHealthy && status == string(corev1.PodRunning) {
			status = "Running (not ready)"
		}
		pods = append(pods, blastRadiusPod{
			Name:    p.Name,
			Phase:   status,
			Healthy: isHealthy,
		})
		total++
		if isHealthy {
			healthy++
		}
	}
	return
}

// blastRadiusServices returns services whose selector matches the pod's labels.
func blastRadiusServices(clientset kubernetes.Interface, namespace string, podLabels map[string]string) []blastRadiusService {
	list, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var results []blastRadiusService
	for _, svc := range list.Items {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		if labelsMatch(svc.Spec.Selector, podLabels) {
			ports := formatPorts(svc.Spec.Ports)
			results = append(results, blastRadiusService{
				Name:      svc.Name,
				Namespace: svc.Namespace,
				Type:      string(svc.Spec.Type),
				Ports:     ports,
			})
		}
	}
	return results
}

// blastRadiusSharedConfig finds other Deployments in the namespace sharing CMs or Secrets with this pod.
func blastRadiusSharedConfig(clientset kubernetes.Interface, namespace, selfName string, cms, secrets []string) []blastSharedDep {
	if len(cms) == 0 && len(secrets) == 0 {
		return nil
	}
	deps, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var results []blastSharedDep
	for _, dep := range deps.Items {
		if dep.Name == selfName {
			continue
		}
		depCMs, depSecrets, _ := referencedResourcesFromSpec(&dep.Spec.Template.Spec)
		var shared []string
		for _, cm := range cms {
			if contains(depCMs, cm) {
				shared = append(shared, "cm:"+cm)
			}
		}
		for _, sec := range secrets {
			if contains(depSecrets, sec) {
				shared = append(shared, "secret:"+sec)
			}
		}
		if len(shared) > 0 {
			results = append(results, blastSharedDep{
				DeploymentName: dep.Name,
				SharedItems:    shared,
			})
		}
	}
	return results
}

// blastIngresses finds Ingress rules that route to any of the given service names.
func blastIngresses(clientset kubernetes.Interface, namespace string, serviceNames []string) []blastIngressRule {
	if len(serviceNames) == 0 {
		return nil
	}
	var list *networkingv1.IngressList
	var err error
	list, err = clientset.NetworkingV1().Ingresses(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil
	}
	svcSet := map[string]bool{}
	for _, s := range serviceNames {
		svcSet[s] = true
	}
	var results []blastIngressRule
	for _, ing := range list.Items {
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil && svcSet[path.Backend.Service.Name] {
					results = append(results, blastIngressRule{
						Host:     rule.Host,
						Path:     path.Path,
						External: rule.Host != "" && !strings.HasSuffix(rule.Host, ".cluster.local"),
					})
				}
			}
		}
	}
	return results
}

// blastNamespaceHealth counts healthy vs total pods per workload in the namespace,
// excluding the pod under investigation.
func blastNamespaceHealth(clientset kubernetes.Interface, namespace, excludeOwnerName string) (workloads []blastNamespacePod, healthy, total int) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return
	}
	type counts struct{ h, t int }
	tally := map[string]*counts{}
	for _, p := range list.Items {
		_, ownerName := resolveOwner(clientset, &p)
		if ownerName == excludeOwnerName {
			continue
		}
		if tally[ownerName] == nil {
			tally[ownerName] = &counts{}
		}
		tally[ownerName].t++
		total++
		isHealthy := func() bool {
			if p.Status.Phase != corev1.PodRunning {
				return false
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil || !cs.Ready {
					return false
				}
			}
			return true
		}()
		if isHealthy {
			tally[ownerName].h++
			healthy++
		}
	}
	// Sort by name for stable output
	names := make([]string, 0, len(tally))
	for k := range tally {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		workloads = append(workloads, blastNamespacePod{
			WorkloadName: name,
			Healthy:      tally[name].h,
			Total:        tally[name].t,
		})
	}
	return
}

// labelsMatch reports whether all selector keys/values exist in target.
func labelsMatch(selector, target map[string]string) bool {
	for k, v := range selector {
		if target[k] != v {
			return false
		}
	}
	return true
}

// formatPorts formats a slice of ServicePort into "80/TCP, 443/TCP".
func formatPorts(ports []corev1.ServicePort) string {
	var parts []string
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
	}
	return strings.Join(parts, ", ")
}

// contains reports whether slice s contains item.
func contains(s []string, item string) bool {
	for _, v := range s {
		if v == item {
			return true
		}
	}
	return false
}

// referencedResourcesFromSpec extracts CM/Secret/PVC names from a PodSpec directly.
func referencedResourcesFromSpec(spec *corev1.PodSpec) (cms, secrets, pvcs []string) {
	seen := map[string]bool{}
	add := func(slice *[]string, name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			*slice = append(*slice, name)
		}
	}
	for _, v := range spec.Volumes {
		if v.ConfigMap != nil {
			add(&cms, v.ConfigMap.Name)
		}
		if v.Secret != nil {
			add(&secrets, v.Secret.SecretName)
		}
		if v.PersistentVolumeClaim != nil {
			add(&pvcs, v.PersistentVolumeClaim.ClaimName)
		}
	}
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				add(&cms, e.ConfigMapRef.Name)
			}
			if e.SecretRef != nil {
				add(&secrets, e.SecretRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				if e.ValueFrom.ConfigMapKeyRef != nil {
					add(&cms, e.ValueFrom.ConfigMapKeyRef.Name)
				}
				if e.ValueFrom.SecretKeyRef != nil {
					add(&secrets, e.ValueFrom.SecretKeyRef.Name)
				}
			}
		}
	}
	return
}

// populateNamespaceFinding fills NamespacePodCount/NamespaceFinding from the same
// audit data War Room already surfaces for these namespace-level issue types
// (see collectWarRoomIssues in warroom.go), rather than re-deriving pod counts.
func populateNamespaceFinding(data *investigationPageData, scan *clusterScan, issueType, namespace string) {
	if scan == nil {
		return
	}
	switch issueType {
	case "unprotected_namespace":
		if scan.netAudit == nil {
			return
		}
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			if ns.Name == namespace {
				data.NamespacePodCount = ns.PodCount
				data.NamespacePolicyCount = ns.PolicyCount
				data.NamespaceCoverageGapPodCount = ns.CoverageGapPodCount
				if ns.PolicyCount == 0 {
					data.NamespaceFinding = "No NetworkPolicy found in this namespace."
				} else {
					data.NamespaceFinding = fmt.Sprintf(
						"%d NetworkPolicy object(s) present, but %d of %d observed pods lack configured ingress and egress coverage",
						ns.PolicyCount, ns.CoverageGapPodCount, ns.PodCount,
					)
				}
				data.Severity = strings.ToLower(ns.RiskLevel)
				return
			}
		}
	case "idle_namespace":
		if scan.wasteAudit == nil {
			return
		}
		for _, ns := range scan.wasteAudit.AbandonedNamespaces {
			if ns.Name == namespace {
				data.NamespacePodCount = ns.PodCount
				data.NamespaceFinding = ns.Reason
				return
			}
		}
	}
}

func nodePlacementNamespaces(workloads []models.CorrelatedWorkload) ([]nodePlacementNamespace, int) {
	var groups []nodePlacementNamespace
	pods := 0
	for _, workload := range workloads {
		pods += workload.PodCount
		if len(groups) == 0 || groups[len(groups)-1].Name != workload.Namespace {
			groups = append(groups, nodePlacementNamespace{Name: workload.Namespace})
		}
		group := &groups[len(groups)-1]
		group.Items = append(group.Items, workload)
		group.Workloads++
		group.Pods += workload.PodCount
	}
	return groups, pods
}

func currentNodeFinding(scan *clusterScan, nodeName, conditionType string) (models.NodeConditionFinding, bool) {
	if scan == nil {
		return models.NodeConditionFinding{}, false
	}
	for _, finding := range scan.nodeHealth {
		if finding.NodeName == nodeName && finding.ConditionType == conditionType {
			return finding, true
		}
	}
	return models.NodeConditionFinding{}, false
}

func populateNodeEvidence(data *investigationPageData, evidence nodeInvestigationDetails, retained bool) {
	data.NodePool = evidence.NodePool
	data.NodeConditionStatus = evidence.ConditionStatus
	data.NodeReason = evidence.Reason
	data.NodeMessage = evidence.Message
	if !evidence.LastTransitionTime.IsZero() {
		data.NodeLastTransition = evidence.LastTransitionTime.Local().Format("Jan 2, 2006 15:04 MST")
	}
	data.PlacementWorkloads = len(evidence.CorrelatedWorkloads)
	data.PlacementNamespaces, data.PlacementPods = nodePlacementNamespaces(evidence.CorrelatedWorkloads)
	data.RetainedNodeEvidence = retained
}

func (srv *server) handleNodeInvestigation(w http.ResponseWriter, ctx, nodeName, issueType string, scan *clusterScan, data investigationPageData) {
	data.NodeScoped = true
	data.NodeName = nodeName
	data.PodName = nodeName // retained as the existing page-title value
	data.WorkloadLabel = "Node/" + nodeName
	data.TrackingLabel = "Node condition scoped"
	data.DescribeCommand = fmt.Sprintf("kubectl describe node %s", nodeName)
	data.Commands = []string{
		data.DescribeCommand,
		fmt.Sprintf("kubectl get pods -A --field-selector spec.nodeName=%s -o wide", nodeName),
	}
	data.Severity, _ = nodeWarRoomSeverity(issueType)
	fp := store.Fingerprint("cluster", "Node", nodeName, issueType)
	var retained nodeInvestigationDetails
	hasIncident := false
	if srv.db != nil {
		if rec, err := srv.db.GetIncidentHistory(ctx, fp); err == nil && rec != nil {
			hasIncident = true
			data.IncidentStatus = rec.Status
			data.FirstDetected = firstDetectedLabel(rec.FirstSeen)
			if json.Unmarshal([]byte(rec.DetailsJSON), &retained) == nil {
				populateNodeEvidence(&data, retained, true)
			}
		}
		if events, err := srv.db.GetIncidentTimeline(ctx, fp); err == nil {
			data.Timeline = events
		}
	}
	if finding, ok := currentNodeFinding(scan, nodeName, issueType); ok {
		data.CurrentNodeObservation = true
		populateNodeEvidence(&data, nodeInvestigationDetails{
			NodeName: finding.NodeName, NodePool: finding.NodePool,
			ConditionType: finding.ConditionType, ConditionStatus: finding.ConditionStatus,
			Reason: finding.Reason, Message: finding.Message, LastTransitionTime: finding.LastTransitionTime,
			CorrelatedWorkloads: finding.CorrelatedWorkloads,
		}, false)
	}
	if !hasIncident && !data.CurrentNodeObservation {
		http.Error(w, "node incident not found", http.StatusNotFound)
		return
	}
	if data.IncidentStatus == "" {
		data.IncidentStatus = "active"
	}
	if data.CurrentNodeObservation {
		data.OperationalSummary = fmt.Sprintf("Kubernetes currently reports %s=%s on Node %s.", issueType, data.NodeConditionStatus, nodeName)
	} else {
		data.OperationalSummary = "No active Kubernetes condition is currently reported for this node."
	}
	renderInvestigation(w, data)
}

func (srv *server) handleInvestigationPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	podName := r.URL.Query().Get("pod")
	nodeName := r.URL.Query().Get("node")
	namespace := r.URL.Query().Get("ns")
	issueType := r.URL.Query().Get("type")

	namespaceScopedTypes := map[string]bool{
		"unprotected_namespace": true,
		"idle_namespace":        true,
	}
	if issueType == "" || (nodeName == "" && (namespace == "" || (podName == "" && !namespaceScopedTypes[issueType]))) {
		http.Error(w, "missing pod or ns query parameter", http.StatusBadRequest)
		return
	}

	// Sidebar needs critical count from the cached scan
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	q := "?cluster=" + url.QueryEscape(ctx)
	from := r.URL.Query().Get("from")
	activePage := "warroom"
	if from == "incidents" {
		activePage = "incidents"
	}
	backURL := "/warroom" + q
	if from == "incidents" {
		backURL = "/incidents" + q
	}
	data := investigationPageData{
		DashHref:      "/" + q,
		WrHref:        "/warroom" + q,
		CostsHref:     "/costs" + q,
		InfraHref:     "/infrastructure" + q,
		WasteHref:     "/waste" + q,
		SecurityHref:  "/security" + q,
		IncidentsHref: "/incidents" + q,
		ActivePage:    activePage,
		ClusterName:   displayName(ctx),
		CriticalCount: countCriticalIssues(scan),
		PodName:       podName,
		Namespace:     namespace,
		IssueType:     issueType,
		WorkloadLabel: "Workload/" + store.OwnerNameFromPod(podName),
		TrackingLabel: "Workload scoped",
		BackURL:       backURL,
		ScannedAtMs:   time.Now().UnixMilli(),
	}
	if nodeName != "" {
		srv.handleNodeInvestigation(w, ctx, nodeName, issueType, scan, data)
		return
	}

	if namespaceScopedTypes[issueType] {
		data.NamespaceScoped = true
		data.WorkloadLabel = "Namespace/" + namespace
		data.TrackingLabel = "Namespace scoped"
		populateNamespaceFinding(&data, scan, issueType, namespace)
		data.Hints = investigationHints(issueType, "", 0, nil, namespace)
		for _, h := range data.Hints {
			if h.Command != "" {
				data.Commands = append(data.Commands, h.Command)
			}
		}
		data.OperationalSummary = buildOperationalSummary(&data)

		fp := store.WorkloadFingerprintForPod(namespace, podName, issueType)
		if rec, err := srv.db.GetIncidentHistory(ctx, fp); err == nil && rec != nil {
			data.FirstDetected = firstDetectedLabel(rec.FirstSeen)
		}
		if events, err := srv.db.GetIncidentTimeline(ctx, fp); err == nil {
			data.Timeline = events
		}
		renderInvestigation(w, data)
		return
	}

	// Live cluster data — fresh client, not scan cache
	clientset, err := srv.kubeClientFor(ctx)
	if err != nil {
		log.Printf("investigation: kube client: %v", err)
		http.Error(w, "cluster connection failed", http.StatusBadGateway)
		return
	}
	if idx := strings.Index(podName, "/"); idx != -1 {
		data.PodName = podName[:idx]
		data.ContainerName = podName[idx+1:]
		podName = podName[:idx]
	}
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		// Pod may have been deleted/recreated since the scan
		data.NotFound = true
		data.Hints = []investigationHint{
			{Confidence: "high", Title: "Pod no longer exists", Reason: "This pod may have been deleted or recreated with a new name since the last scan."},
			{Confidence: "high", Title: "Check current pods in namespace", Reason: "A replacement pod likely exists with a different suffix.", Command: fmt.Sprintf("kubectl get pods -n %s", namespace)},
			{Confidence: "medium", Title: "Check recent events", Reason: "Events may show why the pod was terminated.", Command: fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", namespace)},
		}
		if err != nil {
			// Pod may have been deleted/recreated since the scan
			data.NotFound = true
			data.Hints = []investigationHint{
				{Confidence: "high", Title: "Pod no longer exists", Reason: "This pod may have been deleted or recreated with a new name since the last scan."},
				{Confidence: "high", Title: "Check current pods in namespace", Reason: "A replacement pod likely exists with a different suffix.", Command: fmt.Sprintf("kubectl get pods -n %s", namespace)},
				{Confidence: "medium", Title: "Check recent events", Reason: "Events may show why the pod was terminated.", Command: fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", namespace)},
			}
			for i := range data.Hints {
				data.Hints[i].Step = i + 1
			}
			data.Commands = []string{
				fmt.Sprintf("kubectl get pods -n %s", namespace),
				fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", namespace),
			}
			renderInvestigation(w, data)
			return
		}
		data.Commands = []string{
			fmt.Sprintf("kubectl get pods -n %s", namespace),
			fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", namespace),
		}
		renderInvestigation(w, data)
		return
	}

	// Status
	data.Phase = string(pod.Status.Phase)
	data.AgeDays = int(time.Since(pod.CreationTimestamp.Time).Hours() / 24)
	data.PodAge = humanAge(time.Since(pod.CreationTimestamp.Time))
	data.Severity = "critical"
	for _, cs := range pod.Status.ContainerStatuses {
		data.Restarts += cs.RestartCount
		if state := effectiveContainerState(cs); state != "" && data.StateReason == "" {
			data.StateReason = state
			if data.ContainerName == "" {
				data.ContainerName = cs.Name
			}
		}
	}
	populateInvestigationLogContainers(&data, pod)
	data.LogsAvailable = srv.logsEnabled && len(data.LogContainers) > 0 &&
		isActiveInvestigationTarget(scan, namespace, podName, issueType)

	// Owner, events, related resources, hints
	data.OwnerKind, data.OwnerName = resolveOwner(clientset, pod)
	if data.OwnerKind != "" {
		data.WorkloadLabel = data.OwnerKind + "/" + data.OwnerName
		if data.OwnerKind == "StatefulSet" {
			data.TrackingLabel = "StatefulSet instance scoped"
		} else {
			data.TrackingLabel = data.OwnerKind + " scoped"
		}
	} else {
		data.WorkloadLabel = "Pod/" + podName
		data.TrackingLabel = "Pod scoped"
	}
	data.DescribeCommand = fmt.Sprintf("kubectl describe pod %s -n %s", podName, namespace)
	data.Events = podEvents(clientset, namespace, podName, 10)
	data.ConfigMaps, data.Secrets, data.PVCs = referencedResources(pod)
	data.Hints = investigationHints(issueType, data.StateReason, data.Restarts, pod, namespace)

	// Blast radius
	data.BlastSiblings, data.BlastHealthy, data.BlastTotal = blastRadiusSiblings(clientset, namespace, data.OwnerKind, data.OwnerName)
	data.BlastServices = blastRadiusServices(clientset, namespace, pod.Labels)
	data.BlastSharedConf = blastRadiusSharedConfig(clientset, namespace, data.OwnerName, data.ConfigMaps, data.Secrets)

	svcNames := make([]string, 0, len(data.BlastServices))
	for _, s := range data.BlastServices {
		svcNames = append(svcNames, s.Name)
	}
	data.BlastIngresses = blastIngresses(clientset, namespace, svcNames)
	data.BlastNamespacePods, data.BlastNsHealthy, data.BlastNsTotal = blastNamespaceHealth(clientset, namespace, data.OwnerName)
	data.CustomerImpact = deriveCustomerImpact(data.BlastIngresses, data.BlastServices)

	// ── First detected (from incidents table) ─────────────────────────────────
	// Lookup identity must be SYMMETRIC with the write path (scan persistence),
	// which derives the owner segment from the pod name. data.OwnerName comes
	// from real OwnerReferences and is correct for display/blast radius, but
	// diverges from stored fingerprints for StatefulSets (owner "prometheus"
	// vs stored instance "prometheus-0"), so it must not be used here.
	fp := store.WorkloadFingerprintForPod(namespace, podName, issueType)
	if rec, err := srv.db.GetIncidentHistory(ctx, fp); err == nil && rec != nil {
		data.FirstDetected = firstDetectedLabel(rec.FirstSeen)
	}
	if events, err := srv.db.GetIncidentTimeline(ctx, fp); err == nil {
		data.Timeline = events
	}
	// ── END NEW

	// Investigation commands
	data.OperationalSummary = buildOperationalSummary(&data)
	data.Commands = []string{
		fmt.Sprintf("kubectl logs %s -n %s --previous", podName, namespace),
		fmt.Sprintf("kubectl describe pod %s -n %s", podName, namespace),
		fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s --sort-by='.lastTimestamp'", namespace, podName),
	}
	if data.OwnerKind == "Deployment" {
		data.Commands = append(data.Commands,
			fmt.Sprintf("kubectl rollout history deployment/%s -n %s", data.OwnerName, namespace),
			fmt.Sprintf("kubectl get deployment %s -n %s -o yaml", data.OwnerName, namespace),
		)
	}
	data.Commands = append(data.Commands,
		fmt.Sprintf("kubectl top pod %s -n %s", podName, namespace),
	)
	renderInvestigation(w, data)
}

func effectiveContainerState(status corev1.ContainerStatus) string {
	if status.State.Waiting != nil {
		return status.State.Waiting.Reason
	}
	if status.State.Terminated != nil {
		if status.State.Terminated.Reason != "" {
			return status.State.Terminated.Reason
		}
		return fmt.Sprintf("Terminated (exit code %d)", status.State.Terminated.ExitCode)
	}
	return ""
}

var getInvestigationTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("investigation.html").
			Funcs(template.FuncMap{
				"mul":                   func(a, b int) int { return a * b },
				"issueTypeLabel":        issueTypeLabel,
				"unrepresentedCommands": unrepresentedCommands,
				"div": func(a, b int) int {
					if b == 0 {
						return 0
					}
					return a / b
				},
			}).
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/investigation.html"),
	)
})

func deriveCustomerImpact(ingresses []blastIngressRule, services []blastRadiusService) string {
	// Ingress with external host → likely customer-facing
	for _, ing := range ingresses {
		if ing.External {
			return "possible-external"
		}
	}
	// LoadBalancer service → external IP assigned or pending
	for _, svc := range services {
		if svc.Type == "LoadBalancer" {
			return "possible-external"
		}
	}
	// ClusterIP or NodePort with no ingress → internal
	if len(services) > 0 {
		return "internal"
	}
	return "unknown"
}

func renderInvestigation(w http.ResponseWriter, data investigationPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	var buf strings.Builder
	if err := getInvestigationTmpl().Execute(&buf, data); err != nil {
		log.Printf("investigation template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(buf.String()))
}

func buildOperationalSummary(data *investigationPageData) string {
	if data.NotFound {
		return "This pod no longer exists — it may have been deleted or recreated with a new name since the last scan."
	}

	if data.NamespaceScoped {
		var summary string
		switch data.IssueType {
		case "unprotected_namespace":
			if data.NamespacePolicyCount == 0 {
				summary = fmt.Sprintf("No NetworkPolicy was detected in the %s namespace.", data.Namespace)
			} else {
				summary = fmt.Sprintf(
					"Existing NetworkPolicies do not fully cover %d of %d observed pods in the %s namespace.",
					data.NamespaceCoverageGapPodCount, data.NamespacePodCount, data.Namespace,
				)
			}
		case "idle_namespace":
			summary = fmt.Sprintf("An idle namespace finding is active for the %s namespace.", data.Namespace)
		default:
			summary = fmt.Sprintf("A %s finding is active for the %s namespace.", strings.ToLower(issueTypeLabel(data.IssueType)), data.Namespace)
		}
		summary += fmt.Sprintf(" The namespace currently contains %d pods.", data.NamespacePodCount)
		if data.Severity != "" {
			summary += fmt.Sprintf(" This finding is classified as %s.", strings.ToLower(data.Severity))
		}
		return summary
	}

	subject := data.WorkloadLabel
	if subject == "" {
		subject = "This workload"
	}
	summary := fmt.Sprintf("A %s incident is active for %s.",
		strings.ToLower(issueTypeLabel(data.IssueType)), subject)
	if data.FirstDetected != "" {
		summary += fmt.Sprintf(" Operational memory first detected this incident %s.", relativePart(data.FirstDetected))
	}

	status := data.StateReason
	if status == "" {
		status = data.Phase
	}
	focus := fmt.Sprintf(" The Focus Pod %s", data.PodName)
	if status != "" {
		focus += fmt.Sprintf(" is currently in %s", status)
	}
	if data.PodAge != "" {
		if status != "" {
			focus += fmt.Sprintf(", is %s old", data.PodAge)
		} else {
			focus += fmt.Sprintf(" is %s old", data.PodAge)
		}
	}
	focus += fmt.Sprintf(", and has restarted %d times.", data.Restarts)
	return summary + focus
}

func issueTypeLabel(issueType string) string {
	switch issueType {
	case "probe_failure":
		return "Probe Failure"
	case "crash_loop":
		return "CrashLoopBackOff"
	case "image_pull_backoff":
		return "ImagePullBackOff"
	case "oomkilled":
		return "OOMKilled"
	case "privileged_container":
		return "Privileged Container"
	case "unprotected_namespace":
		return "Unprotected Namespace"
	case "idle_namespace":
		return "Idle Namespace"
	}
	words := strings.Fields(strings.ReplaceAll(issueType, "_", " "))
	for i := range words {
		words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
	}
	return strings.Join(words, " ")
}

func relativePart(label string) string {
	if _, relative, ok := strings.Cut(label, " · "); ok {
		return relative
	}
	return label
}

func unrepresentedCommands(commands []string, hints []investigationHint) []string {
	represented := make(map[string]bool, len(hints))
	for _, hint := range hints {
		if hint.Command != "" {
			represented[hint.Command] = true
		}
	}
	var result []string
	for _, command := range commands {
		if command != "" && !represented[command] {
			result = append(result, command)
		}
	}
	return result
}

// firstDetectedLabel converts a first_seen timestamp to a human label.
// It combines an absolute date with a relative calendar-day duration.
func firstDetectedLabel(t time.Time) string {
	return firstDetectedLabelAt(t, time.Now())
}

func firstDetectedLabelAt(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(now.Location())
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startOfFirstSeen := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
	days := int(startOfToday.Sub(startOfFirstSeen).Hours() / 24)
	var relative string
	switch days {
	case 0:
		relative = "today"
	case 1:
		relative = "1 day"
	default:
		relative = fmt.Sprintf("%d days", days)
	}
	return t.Format("Jan 2") + " · " + relative
}
