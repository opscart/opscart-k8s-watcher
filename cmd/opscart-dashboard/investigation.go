package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
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
	PodName   string
	Namespace string
	IssueType string // crash_loop, image_pull, oom_killed, privileged
	Severity  string
	OwnerKind string // Deployment / StatefulSet / Job / (none)
	OwnerName string

	// Status
	Phase         string
	Restarts      int32
	AgeDays       int
	StateReason   string // CrashLoopBackOff, ImagePullBackOff...
	NotFound      bool   // pod deleted since scan
	FirstDetected string

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

type investigationHint struct {
	Confidence string // "high" / "medium" / "low"
	Title      string
	Reason     string
	Command    string // optional — if this hint has a specific command
}

func investigationHints(issueType string, stateReason string, restarts int32, pod *corev1.Pod) []investigationHint {
	var hints []investigationHint

	switch issueType {
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
				Reason:     fmt.Sprintf("Restart count of %d indicates the failure reproduces consistently on every start.", restarts),
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

	case "image_pull":
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

	case "oom_killed":
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

	case "privileged":
		hints = append(hints, investigationHint{
			Confidence: "high",
			Title:      "Confirm privileged mode is actually required",
			Reason:     "Most workloads do not need privileged: true — verify with the owning team.",
		})
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Replace with specific capabilities",
			Reason:     "Use securityContext.capabilities.add for specific needs instead of full privileged access.",
		})

	default:
		hints = append(hints, investigationHint{
			Confidence: "medium",
			Title:      "Inspect pod events",
			Reason:     "Events contain the most recent failure reason from the Kubernetes control plane.",
			Command:    fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", pod.Namespace, pod.Name),
		})
	}
	return hints
}

// resolveOwner walks Pod → ReplicaSet → Deployment (or returns the direct owner).
func resolveOwner(clientset *kubernetes.Clientset, pod *corev1.Pod) (kind, name string) {
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
func podEvents(clientset *kubernetes.Clientset, namespace, podName string, n int) []investigationEvent {
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

func (srv *server) handleInvestigationPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	podName := r.URL.Query().Get("pod")
	namespace := r.URL.Query().Get("ns")
	issueType := r.URL.Query().Get("type")

	if podName == "" || namespace == "" {
		http.Error(w, "missing pod or ns query parameter", http.StatusBadRequest)
		return
	}

	// Sidebar needs critical count from the cached scan
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	q := "?cluster=" + url.QueryEscape(ctx)
	data := investigationPageData{
		DashHref:      "/" + q,
		WrHref:        "/warroom" + q,
		CostsHref:     "/costs" + q,
		InfraHref:     "/infrastructure" + q,
		WasteHref:     "/waste" + q,
		SecurityHref:  "/security" + q,
		IncidentsHref: "/incidents" + q,
		ActivePage:    "incidents",
		ClusterName:   displayName(ctx),
		CriticalCount: countCriticalIssues(scan),
		PodName:       podName,
		Namespace:     namespace,
		IssueType:     issueType,
		BackURL:       "/warroom" + q,
		ScannedAtMs:   time.Now().UnixMilli(),
	}

	// Live cluster data — fresh client, not scan cache
	clientset, err := kubeClient(ctx)
	if err != nil {
		log.Printf("investigation: kube client: %v", err)
		http.Error(w, "cluster connection failed", http.StatusBadGateway)
		return
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
	data.Severity = "critical"
	for _, cs := range pod.Status.ContainerStatuses {
		data.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && data.StateReason == "" {
			data.StateReason = cs.State.Waiting.Reason
		}
	}

	// Owner, events, related resources, hints
	data.OwnerKind, data.OwnerName = resolveOwner(clientset, pod)
	data.Events = podEvents(clientset, namespace, podName, 10)
	data.ConfigMaps, data.Secrets, data.PVCs = referencedResources(pod)
	data.Hints = investigationHints(issueType, data.StateReason, data.Restarts, pod)

	// ── NEW: First detected (from incidents table) ────────────────────────────
	ownerNameForFP := data.OwnerName
	if ownerNameForFP == "" {
		ownerNameForFP = store.OwnerNameFromPod(podName)
	}
	fp := store.Fingerprint(namespace, "Workload", ownerNameForFP, issueType)
	if rec, err := srv.db.GetIncidentHistory(ctx, fp); err == nil && rec != nil {
		data.FirstDetected = firstDetectedLabel(rec.FirstSeen)
	}
	// ── END NEW

	// Investigation commands
	data.OperationalSummary = buildOperationalSummary(&data)
	if data.OwnerKind == "Deployment" {
		data.Commands = append(data.Commands,
			fmt.Sprintf("kubectl rollout history deployment/%s -n %s", data.OwnerName, namespace))
	}

	renderInvestigation(w, data)
}

var getInvestigationTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("investigation.html").
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/investigation.html"),
	)
})

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

	var parts []string

	// Restart pattern
	if data.Restarts > 0 {
		rate := "stable"
		if data.AgeDays > 0 && int(data.Restarts)/data.AgeDays > 200 {
			rate = "accelerating"
		}
		failureType := "deterministic configuration or application"
		if rate == "accelerating" {
			failureType = "worsening or resource-related"
		}
		parts = append(parts, fmt.Sprintf(
			"This workload has restarted %d times over %d day(s). The restart rate appears %s, suggesting a %s failure.",
			data.Restarts, max(data.AgeDays, 1), rate, failureType))
	}

	// Config references
	if len(data.Secrets) == 0 && len(data.ConfigMaps) == 0 {
		parts = append(parts,
			"No referenced ConfigMaps or Secrets were detected in the pod spec — missing configuration is unlikely to be the root cause.")
	} else {
		parts = append(parts, fmt.Sprintf(
			"The pod references %d ConfigMap(s) and %d Secret(s) — verify these exist and contain the expected keys.",
			len(data.ConfigMaps), len(data.Secrets)))
	}

	// Issue-specific recommendation
	switch data.IssueType {
	case "crash_loop":
		parts = append(parts,
			"Investigation should begin with previous container logs. Estimated time: 5–10 minutes.")
	case "image_pull":
		parts = append(parts,
			"Investigation should begin with registry credentials and image tag verification. Estimated time: 2–5 minutes.")
	case "oom_killed":
		parts = append(parts,
			"Investigation should begin with memory limit configuration. Estimated time: 5 minutes.")
	case "privileged":
		parts = append(parts,
			"Review whether privileged mode is genuinely required — most workloads can use specific capabilities instead.")
	}

	return strings.Join(parts, " ")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// firstDetectedLabel converts a first_seen timestamp to a human label.
// Returns "today", "1 day ago", "N days ago", or "" for zero time.
func firstDetectedLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	days := int(time.Since(t).Hours() / 24)
	switch days {
	case 0:
		return "today"
	case 1:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", days)
	}
}
