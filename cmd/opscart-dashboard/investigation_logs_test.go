package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func activeCrashLoopScan(podName, namespace string) *clusterScan {
	return &clusterScan{
		wasteAudit: &analyzer.WasteAudit{
			StalePods: []analyzer.StalePod{
				{
					Name:         podName,
					Namespace:    namespace,
					Kind:         analyzer.StalePodZombie,
					Status:       "CrashLoopBackOff",
					RestartCount: 4,
				},
			},
		},
	}
}

func multiContainerInvestigationPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fraud-detection-abc123",
			Namespace:         "payments",
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "example/app:1.0"},
				{Name: "istio-proxy", Image: "example/proxy:1.0"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 4,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
				{Name: "istio-proxy", RestartCount: 0, Ready: true},
			},
		},
	}
}

func newContainerLogTestServer(t *testing.T) *server {
	t.Helper()
	srv := newTestServer()
	srv.logsEnabled = true
	clientset := fake.NewSimpleClientset(multiContainerInvestigationPod())
	srv.kubeClientFor = func(string) (kubernetes.Interface, error) {
		return clientset, nil
	}
	state := srv.getState(bogusClusterCtx)
	state.scan = activeCrashLoopScan("fraud-detection-abc123", "payments")
	return srv
}

func TestHandleInvestigationLogsSelectsOneContainerAndSource(t *testing.T) {
	tests := []struct {
		name              string
		container         string
		previous          bool
		expectedSource    string
		expectedLogOutput string
	}{
		{
			name:              "previous app container",
			container:         "app",
			previous:          true,
			expectedSource:    "previous",
			expectedLogOutput: "2026-08-12T10:00:00Z app failed to connect\n",
		},
		{
			name:              "current istio sidecar",
			container:         "istio-proxy",
			previous:          false,
			expectedSource:    "current",
			expectedLogOutput: "2026-08-12T10:00:00Z proxy ready\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContainerLogTestServer(t)
			srv.podLogReader = func(_ context.Context, _ kubernetes.Interface, namespace, podName string, options *corev1.PodLogOptions) ([]byte, error) {
				if namespace != "payments" || podName != "fraud-detection-abc123" {
					t.Fatalf("unexpected log target %s/%s", namespace, podName)
				}
				if options.Container != tc.container || options.Previous != tc.previous {
					t.Fatalf("unexpected options: container=%q previous=%v", options.Container, options.Previous)
				}
				if options.TailLines == nil || *options.TailLines != investigationLogTailLines {
					t.Fatalf("expected %d tail lines", investigationLogTailLines)
				}
				if options.LimitBytes == nil || *options.LimitBytes != investigationLogMaxBytes {
					t.Fatalf("expected %d byte limit", investigationLogMaxBytes)
				}
				if !options.Timestamps {
					t.Fatal("expected timestamps to be enabled")
				}
				return []byte(tc.expectedLogOutput), nil
			}

			url := "/api/investigation/logs?cluster=" + bogusClusterCtx +
				"&ns=payments&pod=fraud-detection-abc123&type=crash_loop&container=" + tc.container +
				"&previous=" + strconv.FormatBool(tc.previous)
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()
			srv.handleInvestigationLogs(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("expected Cache-Control no-store, got %q", rec.Header().Get("Cache-Control"))
			}
			var payload struct {
				Container string `json:"container"`
				Source    string `json:"source"`
				Logs      string `json:"logs"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload.Container != tc.container || payload.Source != tc.expectedSource || payload.Logs != tc.expectedLogOutput {
				t.Fatalf("unexpected response: %#v", payload)
			}
		})
	}
}

func TestHandleInvestigationLogsRejectsUnavailablePreviousLogs(t *testing.T) {
	srv := newContainerLogTestServer(t)
	readerCalled := false
	srv.podLogReader = func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) ([]byte, error) {
		readerCalled = true
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/investigation/logs?cluster="+bogusClusterCtx+"&ns=payments&pod=fraud-detection-abc123&type=crash_loop&container=istio-proxy&previous=true", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationLogs(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}
	if readerCalled {
		t.Fatal("log reader must not run when previous logs are unavailable")
	}
}

func TestHandleInvestigationLogsWhenDisabled(t *testing.T) {
	t.Setenv("OPSCART_LOGS_ENABLED", "false")
	srv := newTestServer()
	clientCalled := false
	srv.kubeClientFor = func(string) (kubernetes.Interface, error) {
		clientCalled = true
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/investigation/logs?cluster="+bogusClusterCtx+"&ns=payments&pod=fraud-detection-abc123&type=crash_loop&container=app&previous=false", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationLogs(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d: %s", rec.Code, rec.Body.String())
	}
	if clientCalled {
		t.Fatal("Kubernetes client must not be created while log preview is disabled")
	}
}

func TestHandleInvestigationLogsRejectsNonIssuePod(t *testing.T) {
	srv := newContainerLogTestServer(t)
	clientCalled := false
	srv.kubeClientFor = func(string) (kubernetes.Interface, error) {
		clientCalled = true
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/investigation/logs?cluster="+bogusClusterCtx+"&ns=payments&pod=healthy-api&type=crash_loop&container=app&previous=false", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationLogs(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d: %s", rec.Code, rec.Body.String())
	}
	if clientCalled {
		t.Fatal("Kubernetes client must not be created for a pod outside the active issue list")
	}
}

func TestHandleInvestigationPageRendersMultiContainerLogControls(t *testing.T) {
	srv := newContainerLogTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster="+bogusClusterCtx+"&ns=payments&pod=fraud-detection-abc123&type=crash_loop", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{"Container Logs", "app · 4 restarts", "istio-proxy · 0 restarts", "Live preview · not stored"} {
		if !strings.Contains(body, expected) {
			t.Errorf("expected rendered page to contain %q", expected)
		}
	}
	if strings.Contains(body, ">Download<") {
		t.Error("log preview must not render a download control")
	}
}

func TestLogPreviewEnabledFromEnvDefaultsOn(t *testing.T) {
	t.Setenv("OPSCART_LOGS_ENABLED", "")
	if !logPreviewEnabledFromEnv() {
		t.Fatal("expected log preview to be enabled by default")
	}

	t.Setenv("OPSCART_LOGS_ENABLED", "false")
	if logPreviewEnabledFromEnv() {
		t.Fatal("expected log preview to be disabled for false")
	}
}
