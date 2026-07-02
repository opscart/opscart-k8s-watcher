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
	Phase       string
	Restarts    int32
	AgeDays     int
	StateReason string // CrashLoopBackOff, ImagePullBackOff...
	NotFound    bool   // pod deleted since scan

	// Sections
	Hints      []string             // possible causes
	Commands   []string             // investigation kubectl commands
	Events     []investigationEvent // last 10, this pod only
	ConfigMaps []string
	Secrets    []string
	PVCs       []string

	ScannedAtMs int64
	BackURL     string
}

func investigationHints(issueType string, stateReason string, restarts int32) []string {
	var hints []string
	switch issueType {
	case "crash_loop":
		hints = append(hints,
			"Check application logs with --previous flag — the container is exiting after start",
			"Verify referenced ConfigMaps and Secrets exist and contain expected keys",
			"Check liveness probe configuration — an aggressive probe can kill healthy containers",
			"If restarts climb steadily, the failure is deterministic (config/code), not transient")
		if restarts > 100 {
			hints = append(hints,
				"Restart count is very high — this workload has been failing unattended; check ownership")
		}
	case "image_pull":
		hints = append(hints,
			"Verify the image tag exists in the registry — typos and deleted tags are the most common cause",
			"Check imagePullSecrets are present in the namespace and referenced by the pod spec",
			"If the registry is private, confirm credentials have not expired",
			"Run kubectl describe pod and read the Events section for the exact registry error")
	case "oom_killed":
		hints = append(hints,
			"Memory limit is lower than actual usage — check resources.limits.memory",
			"For JVM workloads, ensure heap settings respect the container limit",
			"Compare requests vs limits — a large gap can hide steady memory growth",
			"If OOM kills are periodic, look for a batch operation or cache growth pattern")
	case "privileged":
		hints = append(hints,
			"Confirm this workload genuinely needs privileged mode — most do not",
			"Consider replacing privileged: true with specific capabilities via securityContext.capabilities.add",
			"If it is a CSI/CNI driver, privileged may be required — document the exception")
	default:
		hints = append(hints,
			"Run kubectl describe on the resource and read the Events section",
			"Check recent changes to this namespace — deployments, config updates, secret rotations")
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
		data.Hints = []string{
			"This pod no longer exists — it may have been deleted or recreated with a new name",
			"Check current pods in the namespace: kubectl get pods -n " + namespace,
			"If owned by a Deployment, a replacement pod likely exists with a different suffix",
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
	data.Hints = investigationHints(issueType, data.StateReason, data.Restarts)

	// Investigation commands
	data.Commands = []string{
		fmt.Sprintf("kubectl logs %s -n %s --previous", podName, namespace),
		fmt.Sprintf("kubectl describe pod %s -n %s", podName, namespace),
		fmt.Sprintf("kubectl get events -n %s --field-selector involvedObject.name=%s", namespace, podName),
	}
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
