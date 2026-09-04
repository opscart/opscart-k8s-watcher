package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dashboardScanInterval = 60 * time.Second

type apiMetricKey struct {
	Operation string
	Resource  string
	Result    string
}

type apiOperationKey struct {
	Operation string
	Resource  string
}

type apiCounters struct {
	mu       sync.RWMutex
	requests map[apiMetricKey]uint64
	objects  map[string]uint64
}

func newAPICounters() *apiCounters {
	return &apiCounters{requests: make(map[apiMetricKey]uint64), objects: make(map[string]uint64)}
}

func (c *apiCounters) recordRequest(operation, resource, result string) {
	c.mu.Lock()
	c.requests[apiMetricKey{operation, resource, result}]++
	c.mu.Unlock()
}

func (c *apiCounters) recordObjects(resource string, count uint64) {
	if count == 0 {
		return
	}
	c.mu.Lock()
	c.objects[resource] += count
	c.mu.Unlock()
}

type apiCounterSnapshot struct {
	Requests map[apiMetricKey]uint64
	Objects  map[string]uint64
}

func (c *apiCounters) snapshot() apiCounterSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := apiCounterSnapshot{Requests: make(map[apiMetricKey]uint64, len(c.requests)), Objects: make(map[string]uint64, len(c.objects))}
	for k, v := range c.requests {
		s.Requests[k] = v
	}
	for k, v := range c.objects {
		s.Objects[k] = v
	}
	return s
}

func (s apiCounterSnapshot) totals() (requests, errors uint64) {
	for key, count := range s.Requests {
		requests += count
		if key.Result == "error" || strings.HasPrefix(key.Result, "4") || strings.HasPrefix(key.Result, "5") {
			errors += count
		}
	}
	return
}

var processAPICounters = newAPICounters()

type measuringTransport struct {
	base       http.RoundTripper
	cumulative *apiCounters
	scan       *apiCounters
}

func (t *measuringTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	operation, resource := classifyKubernetesRequest(req.Method, req.URL)
	resp, err := t.base.RoundTrip(req)
	result := "error"
	if err == nil && resp != nil {
		result = strconv.Itoa(resp.StatusCode)
	}
	t.cumulative.recordRequest(operation, resource, result)
	if t.scan != nil {
		t.scan.recordRequest(operation, resource, result)
	}
	if err == nil && resp != nil && resp.Body != nil && operation == "LIST" {
		resp.Body = &countingResponseBody{ReadCloser: resp.Body, resource: resource, counters: []*apiCounters{t.cumulative, t.scan}}
	}
	return resp, err
}

type countingResponseBody struct {
	io.ReadCloser
	resource string
	counters []*apiCounters
	buf      bytes.Buffer
	once     sync.Once
}

func (b *countingResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.buf.Write(p[:n])
	}
	if err == io.EOF {
		b.count()
	}
	return n, err
}

func (b *countingResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.count()
	return err
}

func (b *countingResponseBody) count() {
	b.once.Do(func() {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(b.buf.Bytes(), &list) != nil {
			return
		}
		for _, counters := range b.counters {
			if counters != nil {
				counters.recordObjects(b.resource, uint64(len(list.Items)))
			}
		}
	})
}

func classifyKubernetesRequest(method string, u *url.URL) (operation, resource string) {
	segments := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	resourceIndex := -1
	if len(segments) >= 3 && segments[0] == "api" {
		resourceIndex = 2
	} else if len(segments) >= 4 && segments[0] == "apis" {
		resourceIndex = 3
	}
	resource = "unknown"
	if resourceIndex >= 0 && resourceIndex < len(segments) {
		if segments[resourceIndex] == "watch" && resourceIndex+1 < len(segments) {
			resourceIndex++
		}
		if segments[resourceIndex] == "namespaces" && resourceIndex+2 < len(segments) {
			resourceIndex += 2
		}
		resource = segments[resourceIndex]
	}

	if resource == "pods" && resourceIndex+2 < len(segments) && segments[resourceIndex+2] == "log" {
		return "POD_LOG_GET", resource
	}
	if strings.EqualFold(u.Query().Get("watch"), "true") || (resourceIndex > 0 && segments[resourceIndex-1] == "watch") {
		return "WATCH", resource
	}
	if method != http.MethodGet {
		return strings.ToUpper(method), resource
	}
	if resourceIndex == len(segments)-1 {
		return "LIST", resource
	}
	return "GET", resource
}

type scanObservation struct {
	CompletedAt time.Time
	Duration    time.Duration
	API         apiCounterSnapshot
}

type diagnosticOperation struct {
	Operation string
	Resource  string
	Count     uint64
}

type diagnosticsPageData struct {
	Cluster       string
	CompletedAt   string
	Duration      string
	Interval      string
	Requests      uint64
	Errors        uint64
	Pods          uint64
	Nodes         uint64
	Namespaces    uint64
	Events        uint64
	TopOperations []diagnosticOperation
}

func (srv *server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	state := srv.getState(srv.activeCtx(r))
	state.mu.RLock()
	obs := state.observation
	state.mu.RUnlock()
	requests, errors := obs.API.totals()
	data := diagnosticsPageData{Cluster: displayName(state.ctx), CompletedAt: "No completed scan", Interval: dashboardScanInterval.String(), Requests: requests, Errors: errors,
		Pods: obs.API.Objects["pods"], Nodes: obs.API.Objects["nodes"], Namespaces: obs.API.Objects["namespaces"], Events: obs.API.Objects["events"]}
	if !obs.CompletedAt.IsZero() {
		data.CompletedAt = obs.CompletedAt.Format(time.RFC3339)
		data.Duration = obs.Duration.Round(time.Millisecond).String()
	}
	byOperation := make(map[apiOperationKey]uint64)
	for key, count := range obs.API.Requests {
		byOperation[apiOperationKey{key.Operation, key.Resource}] += count
	}
	for key, count := range byOperation {
		data.TopOperations = append(data.TopOperations, diagnosticOperation{key.Operation, key.Resource, count})
	}
	sort.Slice(data.TopOperations, func(i, j int) bool { return data.TopOperations[i].Count > data.TopOperations[j].Count })
	if len(data.TopOperations) > 10 {
		data.TopOperations = data.TopOperations[:10]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := getDiagnosticsTmpl().Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

var getDiagnosticsTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("diagnostics.html").ParseFS(templateFS, "templates/base.html", "templates/diagnostics.html"))
})

func (srv *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writePrometheusMetrics(w, processAPICounters.snapshot(), srv.observationSnapshots())
}

func (srv *server) observationSnapshots() map[string]scanObservation {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	result := make(map[string]scanObservation, len(srv.states))
	for cluster, state := range srv.states {
		state.mu.RLock()
		result[displayName(cluster)] = state.observation
		state.mu.RUnlock()
	}
	return result
}

func writePrometheusMetrics(w io.Writer, cumulative apiCounterSnapshot, observations map[string]scanObservation) {
	fmt.Fprintln(w, "# HELP opscart_kubernetes_api_requests_total Kubernetes API requests made by this process.")
	fmt.Fprintln(w, "# TYPE opscart_kubernetes_api_requests_total counter")
	keys := make([]apiMetricKey, 0, len(cumulative.Requests))
	for key := range cumulative.Requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Operation != keys[j].Operation {
			return keys[i].Operation < keys[j].Operation
		}
		if keys[i].Resource != keys[j].Resource {
			return keys[i].Resource < keys[j].Resource
		}
		return keys[i].Result < keys[j].Result
	})
	for _, key := range keys {
		fmt.Fprintf(w, "opscart_kubernetes_api_requests_total{operation=%q,resource=%q,status=%q} %d\n", key.Operation, key.Resource, key.Result, cumulative.Requests[key])
	}
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_duration_seconds Wall-clock duration of the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_duration_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_timestamp_seconds Unix timestamp of the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_timestamp_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_requests Kubernetes API requests made by the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_requests gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_errors Kubernetes API transport errors and HTTP 4xx/5xx responses in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_errors gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_objects_examined Kubernetes objects returned in LIST responses to the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_objects_examined gauge")
	clusters := make([]string, 0, len(observations))
	for cluster := range observations {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	for _, cluster := range clusters {
		obs := observations[cluster]
		requests, errors := obs.API.totals()
		fmt.Fprintf(w, "opscart_scanner_last_scan_duration_seconds{cluster=%q} %g\n", cluster, obs.Duration.Seconds())
		timestamp := int64(0)
		if !obs.CompletedAt.IsZero() {
			timestamp = obs.CompletedAt.Unix()
		}
		fmt.Fprintf(w, "opscart_scanner_last_scan_timestamp_seconds{cluster=%q} %d\n", cluster, timestamp)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_requests{cluster=%q} %d\n", cluster, requests)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_errors{cluster=%q} %d\n", cluster, errors)
		for _, resource := range []string{"pods", "nodes", "namespaces", "events"} {
			fmt.Fprintf(w, "opscart_scanner_last_scan_objects_examined{cluster=%q,resource=%q} %d\n", cluster, resource, obs.API.Objects[resource])
		}
	}
}
