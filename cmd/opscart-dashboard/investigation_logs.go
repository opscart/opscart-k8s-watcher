package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	investigationLogTailLines int64 = 200
	investigationLogMaxBytes  int64 = 256 * 1024
)

type kubeClientFactory func(string) (kubernetes.Interface, error)

type podLogReaderFunc func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) ([]byte, error)

type investigationLogContainer struct {
	Name              string
	Restarts          int32
	PreviousAvailable bool
	Selected          bool
}

func logPreviewEnabledFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv("OPSCART_LOGS_ENABLED"))
	if raw == "" {
		return true
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("logs: invalid OPSCART_LOGS_ENABLED value %q; log preview disabled", raw)
		return false
	}
	return enabled
}

func readPodLogs(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, options *corev1.PodLogOptions) ([]byte, error) {
	return clientset.CoreV1().Pods(namespace).GetLogs(podName, options).DoRaw(ctx)
}

func (srv *server) configuredCluster(ctx string) bool {
	for _, configured := range srv.clusterList {
		if configured == ctx {
			return true
		}
	}
	return false
}

func isActiveInvestigationTarget(scan *clusterScan, namespace, podName, issueType string) bool {
	if scan == nil {
		return false
	}
	for _, issue := range collectWarRoomIssues(scan, 0) {
		issuePod := strings.SplitN(issue.Resource, "/", 2)[0]
		if issue.Namespace == namespace && issuePod == podName && issue.Type == issueType {
			return true
		}
	}
	return false
}

func validInvestigationLogType(issueType string) bool {
	switch issueType {
	case "crash_loop", "image_pull_backoff", "oomkilled", "privileged_container", "probe_failure":
		return true
	default:
		return false
	}
}

func writeInvestigationLogError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// handleInvestigationLogs returns a bounded live preview for one container
// belonging to the active issue being investigated. Log bodies are returned
// directly to the browser and are never sent to the incident store.
func (srv *server) handleInvestigationLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeInvestigationLogError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !srv.logsEnabled {
		writeInvestigationLogError(w, http.StatusNotFound, "container log preview is disabled")
		return
	}

	ctxName := srv.activeCtx(r)
	namespace := r.URL.Query().Get("ns")
	podName := r.URL.Query().Get("pod")
	containerName := r.URL.Query().Get("container")
	issueType := r.URL.Query().Get("type")
	previousValue := r.URL.Query().Get("previous")
	previous := previousValue == "true"

	if !srv.configuredCluster(ctxName) {
		writeInvestigationLogError(w, http.StatusBadRequest, "invalid cluster")
		return
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 ||
		len(validation.IsDNS1123Subdomain(podName)) > 0 ||
		len(validation.IsDNS1123Label(containerName)) > 0 ||
		(previousValue != "" && previousValue != "true" && previousValue != "false") ||
		!validInvestigationLogType(issueType) {
		writeInvestigationLogError(w, http.StatusBadRequest, "invalid log request")
		return
	}

	state := srv.getState(ctxName)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()
	if !isActiveInvestigationTarget(scan, namespace, podName, issueType) {
		writeInvestigationLogError(w, http.StatusNotFound, "active investigation target not found")
		return
	}

	clientset, err := srv.kubeClientFor(ctxName)
	if err != nil {
		log.Printf("investigation logs: kube client: %v", err)
		writeInvestigationLogError(w, http.StatusBadGateway, "cluster connection failed")
		return
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	pod, err := clientset.CoreV1().Pods(namespace).Get(requestCtx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeInvestigationLogError(w, http.StatusNotFound, "pod not found")
			return
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			writeInvestigationLogError(w, http.StatusGatewayTimeout, "pod lookup timed out")
			return
		}
		log.Printf("investigation logs: getting pod %s/%s: %v", namespace, podName, err)
		writeInvestigationLogError(w, http.StatusBadGateway, "pod lookup failed")
		return
	}

	containerFound := false
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			containerFound = true
			break
		}
	}
	if !containerFound {
		writeInvestigationLogError(w, http.StatusNotFound, "container not found")
		return
	}
	if previous {
		previousAvailable := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == containerName && status.RestartCount > 0 {
				previousAvailable = true
				break
			}
		}
		if !previousAvailable {
			writeInvestigationLogError(w, http.StatusConflict, "previous logs are unavailable for this container")
			return
		}
	}

	options := &corev1.PodLogOptions{
		Container:  containerName,
		Previous:   previous,
		TailLines:  int64Pointer(investigationLogTailLines),
		LimitBytes: int64Pointer(investigationLogMaxBytes),
		Timestamps: true,
	}
	logBytes, err := srv.podLogReader(requestCtx, clientset, namespace, podName, options)
	if err != nil {
		if requestCtx.Err() == context.DeadlineExceeded {
			writeInvestigationLogError(w, http.StatusGatewayTimeout, "log request timed out")
			return
		}
		log.Printf("investigation logs: reading %s/%s container %s: %v", namespace, podName, containerName, err)
		writeInvestigationLogError(w, http.StatusBadGateway, "logs could not be read")
		return
	}

	source := "current"
	if previous {
		source = "previous"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pod":        podName,
		"container":  containerName,
		"source":     source,
		"tail_lines": investigationLogTailLines,
		"logs":       string(logBytes),
	})
}

func populateInvestigationLogContainers(data *investigationPageData, pod *corev1.Pod) {
	restartsByName := make(map[string]int32, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		restartsByName[status.Name] = status.RestartCount
	}

	selectedName := data.ContainerName
	selectedExists := false
	for _, container := range pod.Spec.Containers {
		if container.Name == selectedName {
			selectedExists = true
			break
		}
	}
	if !selectedExists && len(pod.Spec.Containers) > 0 {
		selectedName = pod.Spec.Containers[0].Name
	}
	data.ContainerName = selectedName
	data.DefaultLogSource = "current"
	data.LogContainers = make([]investigationLogContainer, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		restarts := restartsByName[container.Name]
		selected := container.Name == selectedName
		data.LogContainers = append(data.LogContainers, investigationLogContainer{
			Name:              container.Name,
			Restarts:          restarts,
			PreviousAvailable: restarts > 0,
			Selected:          selected,
		})
		if selected && restarts > 0 {
			data.DefaultLogSource = "previous"
		}
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
