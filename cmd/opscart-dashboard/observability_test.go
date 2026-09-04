package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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
	transport := &measuringTransport{base: base, cumulative: cumulative, scan: scan}
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
	for _, path := range []string{"/metrics", "/diagnostics"} {
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
}
