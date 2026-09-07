package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestClassifyKubernetesRequest(t *testing.T) {
	tests := []struct{ name, method, rawURL, operation, resource string }{
		{"core list", "GET", "/api/v1/nodes", "LIST", "nodes"},
		{"core namespaced get", "GET", "/api/v1/namespaces/x/pods/pod-a", "GET", "pods"},
		{"grouped list", "GET", "/apis/apps/v1/namespaces/x/deployments", "LIST", "deployments"},
		{"autoscaling list", "GET", "/apis/autoscaling/v2/namespaces/x/horizontalpodautoscalers", "LIST", "horizontalpodautoscalers"},
		{"networking list", "GET", "/apis/networking.k8s.io/v1/namespaces/x/networkpolicies", "LIST", "networkpolicies"},
		{"watch query", "GET", "/api/v1/pods?watch=true", "WATCH", "pods"},
		{"watch path", "GET", "/api/v1/watch/pods", "WATCH", "pods"},
		{"pod logs", "GET", "/api/v1/namespaces/x/pods/pod-a/log", "POD_LOG_GET", "pods"},
		{"write", "POST", "/apis/apps/v1/namespaces/x/deployments", "POST", "deployments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatal(err)
			}
			op, resource := classifyKubernetesRequest(tt.method, u)
			if op != tt.operation || resource != tt.resource {
				t.Fatalf("got %s/%s, want %s/%s", op, resource, tt.operation, tt.resource)
			}
		})
	}
}

func TestAPICountersConcurrent(t *testing.T) {
	counters := newAPICounters()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				counters.recordRequest("LIST", "pods", "200")
				counters.recordObjects("pods", 1)
			}
		}()
	}
	wg.Wait()
	snapshot := counters.snapshot()
	if got := snapshot.Requests[apiMetricKey{"LIST", "pods", "200"}]; got != 2000 {
		t.Fatalf("requests = %d, want 2000", got)
	}
	if got := snapshot.Objects["pods"]; got != 2000 {
		t.Fatalf("objects = %d, want 2000", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMeasuringTransportSeparatesScanAndCumulative(t *testing.T) {
	cumulative, scan := newAPICounters(), newAPICounters()
	body := `{"kind":"PodList","items":[{},{}]}`
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	transport := &measuringTransport{base: base, cumulative: cumulative, local: scan}
	req := httptest.NewRequest("GET", "https://cluster/api/v1/pods", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	interactive := &measuringTransport{base: base, cumulative: cumulative}
	resp, err = interactive.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if got, _ := cumulative.snapshot().totals(); got != 2 {
		t.Fatalf("cumulative requests = %d, want 2", got)
	}
	if got, _ := scan.snapshot().totals(); got != 1 {
		t.Fatalf("scan requests = %d, want 1", got)
	}
	if got := cumulative.snapshot().Objects["pods"]; got != 4 {
		t.Fatalf("cumulative pods = %d, want 4", got)
	}
	if got := scan.snapshot().Objects["pods"]; got != 2 {
		t.Fatalf("scan pods = %d, want 2", got)
	}
}

func TestMeasuringTransportRecordsErrors(t *testing.T) {
	counters := newAPICounters()
	transport := &measuringTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }), cumulative: counters}
	_, _ = transport.RoundTrip(httptest.NewRequest("GET", "https://cluster/api/v1/nodes/node-a", nil))
	if got := counters.snapshot().Requests[apiMetricKey{"GET", "nodes", "error"}]; got != 1 {
		t.Fatalf("errors = %d, want 1", got)
	}
}

func TestWritePrometheusMetrics(t *testing.T) {
	cumulative := newAPICounters()
	cumulative.recordRequest("LIST", "pods", "200")
	local := newAPICounters()
	local.recordRequest("LIST", "events", "500")
	local.recordObjects("events", 3)
	var output strings.Builder
	writePrometheusMetrics(&output, cumulative.snapshot(), map[string]scanObservation{"cluster-a": {CompletedAt: time.Unix(123, 0), Duration: 1500 * time.Millisecond, API: local.snapshot()}})
	for _, want := range []string{
		`opscart_kubernetes_api_requests_total{operation="LIST",resource="pods",status="200"} 1`,
		`opscart_scanner_last_scan_duration_seconds{cluster="cluster-a"} 1.5`,
		`opscart_scanner_last_scan_timestamp_seconds{cluster="cluster-a"} 123`,
		`opscart_scanner_last_scan_api_requests{cluster="cluster-a"} 1`,
		`opscart_scanner_last_scan_api_errors{cluster="cluster-a"} 1`,
		`opscart_scanner_last_scan_objects_examined{cluster="cluster-a",resource="events"} 3`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q\n%s", want, output.String())
		}
	}
}

func TestDiagnosticsAndMetricsRequireAuthentication(t *testing.T) {
	t.Setenv("OPSCART_AUTH_USER", "tester")
	t.Setenv("OPSCART_AUTH_PASS", "secret")
	srv := newServer([]string{"cluster-a"}, nil, 90, false)
	handler := srv.newMux()
	for _, path := range []string{"/metrics", "/settings/diagnostics"} {
		t.Run(path, func(t *testing.T) {
			unauthorized := httptest.NewRecorder()
			handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", path, nil))
			if unauthorized.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized status = %d", unauthorized.Code)
			}
			authorizedReq := httptest.NewRequest("GET", path, nil)
			authorizedReq.SetBasicAuth("tester", "secret")
			authorized := httptest.NewRecorder()
			handler.ServeHTTP(authorized, authorizedReq)
			if authorized.Code != http.StatusOK {
				t.Fatalf("authorized status = %d: %s", authorized.Code, authorized.Body.String())
			}
		})
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/diagnostics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized legacy diagnostics status = %d", unauthorized.Code)
	}
	authorizedReq := httptest.NewRequest("GET", "/diagnostics?cluster=cluster-a", nil)
	authorizedReq.SetBasicAuth("tester", "secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedReq)
	if authorized.Code != http.StatusPermanentRedirect {
		t.Fatalf("legacy diagnostics status = %d, want %d", authorized.Code, http.StatusPermanentRedirect)
	}
	if got := authorized.Header().Get("Location"); got != "/settings/diagnostics?cluster=cluster-a" {
		t.Fatalf("legacy diagnostics redirect = %q", got)
	}
}

func TestInvestigationObservationIsRequestScopedAndReplaced(t *testing.T) {
	srv := newTestServer()
	srv.logsEnabled = false
	state := srv.getState(bogusClusterCtx)
	scanCounters := newAPICounters()
	scanCounters.recordRequest("LIST", "nodes", "200")
	state.observation = scanObservation{CompletedAt: time.Now(), API: scanCounters.snapshot()}

	requestNumber := 0
	srv.kubeClientFor = func(_ string, local *apiCounters) (kubernetes.Interface, error) {
		requestNumber++
		if local == nil {
			t.Fatal("Pod Investigation HTML request received no local counters")
		}
		if requestNumber == 1 {
			local.recordRequest("GET", "pods", "200")
			local.recordRequest("GET", "replicasets", "200")
			local.recordRequest("LIST", "events", "500")
		} else {
			local.recordRequest("LIST", "services", "200")
		}
		return fake.NewSimpleClientset(investigationOwnedPod("payments", fmt.Sprintf("api-%d", requestNumber), "StatefulSet", "api")), nil
	}

	requestInvestigation := func(pod string) {
		req := httptest.NewRequest(http.MethodGet, "/investigate?cluster="+bogusClusterCtx+"&ns=payments&pod="+pod+"&type=crash_loop", nil)
		rec := httptest.NewRecorder()
		srv.handleInvestigationPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("investigation status = %d: %s", rec.Code, rec.Body.String())
		}
	}

	requestInvestigation("api-1")
	first := srv.investigationSnapshot()
	if got, errors := first.API.totals(); got != 3 || errors != 1 {
		t.Fatalf("first totals = %d requests/%d errors, want 3/1", got, errors)
	}
	operations := diagnosticOperations(first.API)
	for _, want := range []diagnosticOperation{{"GET", "pods", 1}, {"GET", "replicasets", 1}, {"LIST", "events", 1}} {
		found := false
		for _, got := range operations {
			found = found || got == want
		}
		if !found {
			t.Errorf("operation breakdown missing %+v: %+v", want, operations)
		}
	}
	if got, _ := state.observation.API.totals(); got != 1 {
		t.Fatalf("Investigation changed scan requests to %d", got)
	}

	requestInvestigation("api-2")
	second := srv.investigationSnapshot()
	if second.Pod != "api-2" {
		t.Fatalf("latest target pod = %q, want api-2", second.Pod)
	}
	if got, errors := second.API.totals(); got != 1 || errors != 0 {
		t.Fatalf("replacement totals = %d requests/%d errors, want 1/0", got, errors)
	}
	if got := diagnosticOperations(second.API); len(got) != 1 || got[0] != (diagnosticOperation{"LIST", "services", 1}) {
		t.Fatalf("replacement operations = %+v", got)
	}
}

func TestNonInvestigationTrafficDoesNotReplaceObservation(t *testing.T) {
	srv := newTestServer()
	local := newAPICounters()
	local.recordRequest("GET", "pods", "200")
	srv.completeInvestigation("payments", "api", time.Now().Add(-time.Second), local)
	want := srv.investigationSnapshot()

	scan := newAPICounters()
	scan.recordRequest("LIST", "nodes", "200")
	state := srv.getState(bogusClusterCtx)
	state.observation = scanObservation{CompletedAt: time.Now(), API: scan.snapshot()}
	rec := httptest.NewRecorder()
	srv.handleSettingsPage(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d", rec.Code)
	}
	got := srv.investigationSnapshot()
	if got.CompletedAt != want.CompletedAt || got.Pod != want.Pod {
		t.Fatalf("non-Investigation traffic replaced observation: got %+v, want %+v", got, want)
	}
}

func TestSettingsDiscoversDiagnosticsAndDiagnosticsHasEmptyInvestigationState(t *testing.T) {
	srv := newTestServer()
	settings := httptest.NewRecorder()
	srv.handleSettingsPage(settings, httptest.NewRequest(http.MethodGet, "/settings?cluster="+bogusClusterCtx, nil))
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `/settings/diagnostics?cluster=`) {
		t.Fatalf("Settings does not link to cluster-aware Diagnostics: status=%d body=%s", settings.Code, settings.Body.String())
	}

	diagnostics := httptest.NewRecorder()
	srv.handleDiagnostics(diagnostics, httptest.NewRequest(http.MethodGet, "/settings/diagnostics?cluster="+bogusClusterCtx, nil))
	for _, want := range []string{"Diagnostics", "Scanner Diagnostics", "Investigation Diagnostics", "No Pod Investigation request has completed yet", "<aside class=\"sidebar\">"} {
		if !strings.Contains(diagnostics.Body.String(), want) {
			t.Errorf("Diagnostics page missing %q", want)
		}
	}
}

func TestConcurrentTransportTrafficCannotEnterInvestigationCounters(t *testing.T) {
	cumulative := newAPICounters()
	investigation := newAPICounters()
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[]}`)), Header: make(http.Header)}, nil
	})
	measured := &measuringTransport{base: base, cumulative: cumulative, local: investigation}
	unrelated := &measuringTransport{base: base, cumulative: cumulative}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, _ := measured.RoundTrip(httptest.NewRequest("GET", "https://cluster/api/v1/namespaces/x/pods/p", nil))
			_ = resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			resp, _ := unrelated.RoundTrip(httptest.NewRequest("GET", "https://cluster/api/v1/events", nil))
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	if got, _ := investigation.snapshot().totals(); got != 50 {
		t.Fatalf("Investigation requests = %d, want 50", got)
	}
	if got := investigation.snapshot().Requests[apiMetricKey{"LIST", "events", "200"}]; got != 0 {
		t.Fatalf("unrelated Event requests attributed to Investigation: %d", got)
	}
	if got, _ := cumulative.snapshot().totals(); got != 100 {
		t.Fatalf("cumulative requests = %d, want 100", got)
	}
}
