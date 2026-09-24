package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// ── target resolution tests ─────────────────────────────────────────────

// aiLogSignalsCrashFixture builds a crash_loop-style scan/store fixture
// with a real aiPodEvidence snapshot, used across this file's and
// ai_refine_test.go's server-side resolution, preview-handler, and refine
// tests.
func aiLogSignalsCrashFixture(cluster string, appRestarts int32, appCrashLooping, includeIstioProxy bool, istioRestarts int32) (*clusterScan, warRoomAITestStore, *corev1.Pod) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	const namespace = "payments"
	const podName = "payments-0"

	appStatus := corev1.ContainerStatus{Name: "app", RestartCount: appRestarts, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	if appCrashLooping {
		appStatus.Ready = false
		appStatus.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	}
	containers := []corev1.Container{{Name: "app"}}
	statuses := []corev1.ContainerStatus{appStatus}
	if includeIstioProxy {
		containers = append(containers, corev1.Container{Name: istioProxyContainerName})
		istioStatus := corev1.ContainerStatus{Name: istioProxyContainerName, RestartCount: istioRestarts, Ready: true}
		if istioRestarts > 0 {
			istioStatus.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			istioStatus.Ready = false
		}
		statuses = append(statuses, istioStatus)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Spec:       corev1.PodSpec{Containers: containers},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}

	fingerprint := store.WorkloadFingerprintForPod(namespace, podName, store.IssueCrashLoop)
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName(cluster)},
		wasteAudit: &analyzer.WasteAudit{ScannedAt: now, StalePods: []analyzer.StalePod{{
			Name: podName, Namespace: namespace, Kind: analyzer.StalePodZombie,
			Status: "CrashLoopBackOff", Severity: "critical", RestartCount: appRestarts, AgeDays: 3,
		}}},
		PodWorkloads: map[string]models.WorkloadRef{namespace + "/" + podName: {Kind: "StatefulSet", Name: podName, Namespace: namespace}},
	}
	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, nil, true, true, now)

	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		cluster: {{
			Fingerprint: fingerprint, Namespace: namespace, Resource: podName,
			IssueType: store.IssueCrashLoop, Severity: "critical", Status: "active",
			FirstSeen: now.Add(-2 * time.Hour), LastSeen: now, ReopenCount: 0,
		}},
	}}
	return scan, db, pod
}

func TestResolveAILogSignalsTargetServerSideEnforcement(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	selection, container, err := resolveAILogSignalsTarget(scan, "prod", db, selector)
	if err != nil {
		t.Fatalf("resolveAILogSignalsTarget: %v", err)
	}
	if container != "app" || selection.Issue.Namespace != "payments" || selection.Issue.Resource != "payments-0" {
		t.Fatalf("unexpected resolution: container=%q namespace=%q pod=%q", container, selection.Issue.Namespace, selection.Issue.Resource)
	}
}

func TestResolveAILogSignalsTargetRejectsIstioProxy(t *testing.T) {
	// istio-proxy itself is the one crash-looping container — it must never
	// be selected as the log-signals target, even though it is the only
	// container matching the crash_loop container-selection heuristic.
	scan, db, _ := aiLogSignalsCrashFixture("prod", 0, false, true, 6)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	_, _, err := resolveAILogSignalsTarget(scan, "prod", db, selector)
	if err != errAILogSignalsIstioProxyRejected {
		t.Fatalf("err = %v, want errAILogSignalsIstioProxyRejected", err)
	}
}

func TestResolveAILogSignalsTargetRequiresRestart(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 0, true, false, 0)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	_, _, err := resolveAILogSignalsTarget(scan, "prod", db, selector)
	if err != errAILogSignalsNoPreviousLogs {
		t.Fatalf("err = %v, want errAILogSignalsNoPreviousLogs", err)
	}
}

func TestResolveAILogSignalsTargetRejectsUnsupportedIssueType(t *testing.T) {
	scan := &clusterScan{
		netAudit: &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{
			{Name: "test-ns", PodCount: 3},
		}},
	}
	db := warRoomAITestStore{}
	selections := collectWarRoomAISelections(scan, "prod", db)
	if len(selections) != 1 {
		t.Fatalf("expected one selectable namespace finding, got %d", len(selections))
	}
	_, _, err := resolveAILogSignalsTarget(scan, "prod", db, selections[0].Selector)
	if err != errAILogSignalsUnsupportedIssue {
		t.Fatalf("err = %v, want errAILogSignalsUnsupportedIssue", err)
	}
}

// ── preview handler tests ───────────────────────────────────────────────

type countingLogReader struct {
	calls int32
	body  []byte
	err   error
}

func (r *countingLogReader) read(_ context.Context, _ kubernetes.Interface, _, _ string, _ *corev1.PodLogOptions) ([]byte, error) {
	atomic.AddInt32(&r.calls, 1)
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte(nil), r.body...), nil
}

func (r *countingLogReader) callCount() int { return int(atomic.LoadInt32(&r.calls)) }

func countPodGetActions(clientset kubernetes.Interface) int {
	fc, ok := clientset.(*fake.Clientset)
	if !ok {
		return -1
	}
	count := 0
	for _, action := range fc.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "pods" {
			count++
		}
	}
	return count
}

func aiFormPost(t *testing.T, handler http.HandlerFunc, target string, form url.Values, origin string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

func newAILogSignalsTestServer(cluster string, scan *clusterScan, db warRoomAITestStore, provider aianalysis.AIProvider, pods ...*corev1.Pod) (*server, *countingLogReader, *fake.Clientset) {
	srv := newWarRoomAITestServer([]string{cluster}, map[string]*clusterScan{cluster: scan}, db, provider)
	srv.logsEnabled = true
	objs := make([]runtime.Object, len(pods))
	for i, pod := range pods {
		objs[i] = pod
	}
	clientset := fake.NewSimpleClientset(objs...)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return clientset, nil }
	reader := &countingLogReader{body: []byte("2026-09-15T00:00:00Z connection refused by upstream\n")}
	srv.podLogReader = reader.read
	return srv, reader, clientset
}

func decodePreviewResponse(t *testing.T, recorder *httptest.ResponseRecorder) aiLogSignalsPreviewResponse {
	t.Helper()
	var response aiLogSignalsPreviewResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode preview response: %v (%s)", err, recorder.Body.String())
	}
	return response
}

func TestHandleAILogSignalsPreviewHonorsCallBudgetAndReturnsSafePreview(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Namespace: "payments"}}
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, clientset := newAILogSignalsTestServer("prod", scan, db, provider, pod, otherPod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if got := countPodGetActions(clientset); got != 1 {
		t.Fatalf("Pods.Get calls = %d, want 1 (no iteration over replicas)", got)
	}
	if reader.callCount() != 1 {
		t.Fatalf("Pods.GetLogs calls = %d, want 1", reader.callCount())
	}
	if provider.callCount() != 0 {
		t.Fatal("preview must never call the AI provider")
	}

	response := decodePreviewResponse(t, recorder)
	if response.PreviewID == "" {
		t.Fatal("missing opaque preview_id")
	}
	if !response.CanRefine || srv.aiLogSignalsPreviewCache.size() != 1 {
		t.Fatal("actionable preview must have exactly one cached ID")
	}
	if response.Source != aiLogSignalsSourcePreviousContainer || response.LinesRequested != investigationLogTailLines ||
		response.ByteLimit != investigationLogMaxBytes || response.RawLinesIncluded != 0 {
		t.Fatalf("unexpected preview shape: %+v", response)
	}
	if len(response.Signals) != 1 || response.Signals[0].Category != "connection_refused" || response.Signals[0].Count != 1 {
		t.Fatalf("unexpected signals: %+v", response.Signals)
	}

	// Rejecting istio-proxy proves nothing about what the remaining
	// container actually runs — the preview must claim only that it is the
	// resolved target container, never a specific role such as
	// "application", which this repository has no reliable way to confirm.
	if response.ContainerRole != "target_container" {
		t.Fatalf("container_role = %q, want %q", response.ContainerRole, "target_container")
	}
	if strings.Contains(recorder.Body.String(), "application") {
		t.Fatalf("preview response must never claim an application-container role: %s", recorder.Body.String())
	}
}

func TestHandleAILogSignalsPreviewUsesFrozenFetchBounds(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 2, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	srv.logsEnabled = true
	clientset := fake.NewSimpleClientset(pod)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return clientset, nil }

	var captured *corev1.PodLogOptions
	srv.podLogReader = func(_ context.Context, _ kubernetes.Interface, namespace, podName string, options *corev1.PodLogOptions) ([]byte, error) {
		captured = options
		if namespace != "payments" || podName != "payments-0" {
			t.Fatalf("unexpected target %s/%s", namespace, podName)
		}
		return []byte("panic: boom\n"), nil
	}

	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if captured == nil {
		t.Fatal("log reader was not invoked")
	}
	if !captured.Previous {
		t.Fatal("expected Previous: true")
	}
	if captured.TailLines == nil || *captured.TailLines != investigationLogTailLines {
		t.Fatalf("TailLines = %v, want %d", captured.TailLines, investigationLogTailLines)
	}
	if captured.LimitBytes == nil || *captured.LimitBytes != investigationLogMaxBytes {
		t.Fatalf("LimitBytes = %v, want %d", captured.LimitBytes, investigationLogMaxBytes)
	}
	if !captured.Timestamps {
		t.Fatal("expected Timestamps: true")
	}
	if captured.Container != "app" {
		t.Fatalf("Container = %q, want app", captured.Container)
	}
}

func TestHandleAILogSignalsPreviewRejectsArbitraryContainerInput(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	// The browser must never be able to smuggle a pod/container selection —
	// only the opaque "issue" selector is accepted; anything else is
	// rejected outright.
	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}, "container": {"istio-proxy"}, "pod": {"someone-elses-pod"}}, "http://example.com")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleAILogSignalsPreviewNoSignalsStillReturnsValidResult(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("application started\nready to serve traffic\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodePreviewResponse(t, recorder)
	if len(response.Signals) != 0 {
		t.Fatalf("expected zero signals, got %v", response.Signals)
	}
	if response.PreviewID != "" || response.CanRefine || srv.aiLogSignalsPreviewCache.size() != 0 {
		t.Fatal("zero-signal preview must not have an ID or cache entry")
	}
	if provider.callCount() != 0 {
		t.Fatal("no-signals preview must never call the AI provider")
	}
}

func TestHandleAILogSignalsPreviewRejectsCrossOrigin(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://attacker.example.com")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func TestHandleAILogSignalsPreviewCooldownRejectsRapidRepeat(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	first := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first preview status = %d, body=%s", first.Code, first.Body.String())
	}
	second := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second preview status = %d, want 429", second.Code)
	}
}
