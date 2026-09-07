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
	mu              sync.RWMutex
	requests        map[apiMetricKey]uint64
	objects         map[string]uint64
	responseBytes   map[apiOperationKey]uint64
	requestDuration map[apiOperationKey]time.Duration
	maxDuration     map[apiOperationKey]time.Duration
}

func newAPICounters() *apiCounters {
	return &apiCounters{
		requests:        make(map[apiMetricKey]uint64),
		objects:         make(map[string]uint64),
		responseBytes:   make(map[apiOperationKey]uint64),
		requestDuration: make(map[apiOperationKey]time.Duration),
		maxDuration:     make(map[apiOperationKey]time.Duration),
	}
}

func (c *apiCounters) recordRequest(operation, resource, result string) {
	c.mu.Lock()
	c.requests[apiMetricKey{operation, resource, result}]++
	c.mu.Unlock()
}

func (c *apiCounters) recordRequestDuration(operation, resource string, duration time.Duration) {
	key := apiOperationKey{operation, resource}
	c.mu.Lock()
	c.requestDuration[key] += duration
	if duration > c.maxDuration[key] {
		c.maxDuration[key] = duration
	}
	c.mu.Unlock()
}

func (c *apiCounters) recordResponseBytes(operation, resource string, count uint64) {
	if count == 0 {
		return
	}
	c.mu.Lock()
	c.responseBytes[apiOperationKey{operation, resource}] += count
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
	Requests        map[apiMetricKey]uint64
	Objects         map[string]uint64
	ResponseBytes   map[apiOperationKey]uint64
	RequestDuration map[apiOperationKey]time.Duration
	MaxDuration     map[apiOperationKey]time.Duration
}

func (c *apiCounters) snapshot() apiCounterSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := apiCounterSnapshot{
		Requests:        make(map[apiMetricKey]uint64, len(c.requests)),
		Objects:         make(map[string]uint64, len(c.objects)),
		ResponseBytes:   make(map[apiOperationKey]uint64, len(c.responseBytes)),
		RequestDuration: make(map[apiOperationKey]time.Duration, len(c.requestDuration)),
		MaxDuration:     make(map[apiOperationKey]time.Duration, len(c.maxDuration)),
	}
	for k, v := range c.requests {
		s.Requests[k] = v
	}
	for k, v := range c.objects {
		s.Objects[k] = v
	}
	for k, v := range c.responseBytes {
		s.ResponseBytes[k] = v
	}
	for k, v := range c.requestDuration {
		s.RequestDuration[k] = v
	}
	for k, v := range c.maxDuration {
		s.MaxDuration[k] = v
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

func (s apiCounterSnapshot) costTotals() (responseBytes uint64, totalDuration, maxDuration time.Duration, throttles uint64) {
	for key, count := range s.Requests {
		if key.Result == "429" {
			throttles += count
		}
	}
	for _, count := range s.ResponseBytes {
		responseBytes += count
	}
	for _, duration := range s.RequestDuration {
		totalDuration += duration
	}
	for _, duration := range s.MaxDuration {
		if duration > maxDuration {
			maxDuration = duration
		}
	}
	return
}

var processAPICounters = newAPICounters()

type measuringTransport struct {
	base       http.RoundTripper
	cumulative *apiCounters
	local      *apiCounters
}

func (t *measuringTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	operation, resource := classifyKubernetesRequest(req.Method, req.URL)
	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	result := "error"
	if err == nil && resp != nil {
		result = strconv.Itoa(resp.StatusCode)
	}
	t.cumulative.recordRequest(operation, resource, result)
	if t.local != nil {
		t.local.recordRequest(operation, resource, result)
	}
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &countingResponseBody{ReadCloser: resp.Body, operation: operation, resource: resource, countObjects: operation == "LIST", started: started, counters: []*apiCounters{t.cumulative, t.local}}
	} else {
		for _, counters := range []*apiCounters{t.cumulative, t.local} {
			if counters != nil {
				counters.recordRequestDuration(operation, resource, time.Since(started))
			}
		}
	}
	return resp, err
}

type countingResponseBody struct {
	io.ReadCloser
	operation    string
	resource     string
	countObjects bool
	started      time.Time
	counters     []*apiCounters
	buf          bytes.Buffer
	bytesRead    uint64
	once         sync.Once
}

func (b *countingResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.bytesRead += uint64(n)
		if b.countObjects {
			_, _ = b.buf.Write(p[:n])
		}
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
		for _, counters := range b.counters {
			if counters != nil {
				counters.recordResponseBytes(b.operation, b.resource, b.bytesRead)
				counters.recordRequestDuration(b.operation, b.resource, time.Since(b.started))
			}
		}
		if !b.countObjects {
			return
		}
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

type investigationObservation struct {
	Namespace   string
	Pod         string
	CompletedAt time.Time
	Duration    time.Duration
	API         apiCounterSnapshot
}

type diagnosticOperation struct {
	Operation string
	Resource  string
	Count     uint64
	Objects   uint64
	Bytes     string
	TotalTime string
	Average   string
}

type diagnosticsPageData struct {
	ActivePage    string
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	NsHref        string
	OptHref       string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ClusterName   string
	CriticalCount int
	Clusters      []sidebarCluster
	Cluster       string
	CompletedAt   string
	Duration      string
	Interval      string
	Requests      uint64
	Errors        uint64
	Throttles     uint64
	ResponseData  string
	TotalAPITime  string
	MaxAPITime    string
	Pods          uint64
	Nodes         uint64
	Namespaces    uint64
	Events        uint64
	TopOperations []diagnosticOperation
	Investigation investigationDiagnostics
}

type investigationDiagnostics struct {
	HasData       bool
	Target        string
	CompletedAt   string
	Duration      string
	Requests      uint64
	Errors        uint64
	TopOperations []diagnosticOperation
}

func (srv *server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	obs := state.observation
	scan := state.scan
	state.mu.RUnlock()
	requests, errors := obs.API.totals()
	responseBytes, totalAPITime, maxAPITime, throttles := obs.API.costTotals()
	q := "?cluster=" + url.QueryEscape(ctx)
	data := diagnosticsPageData{ActivePage: "settings", DashHref: "/" + q, WrHref: "/warroom" + q, CostsHref: "/costs" + q,
		InfraHref: "/infrastructure" + q, NsHref: "/namespaces" + q, OptHref: "/optimizations" + q, WasteHref: "/waste" + q,
		SecurityHref: "/security" + q, IncidentsHref: "/incidents" + q, ClusterName: displayName(ctx), CriticalCount: countCriticalIssues(scan),
		Clusters: convertToSidebarClusters(srv.clusterList, ctx, "/settings/diagnostics"), Cluster: displayName(state.ctx), CompletedAt: "No completed scan", Interval: dashboardScanInterval.String(), Requests: requests, Errors: errors,
		Throttles: throttles, ResponseData: formatByteCount(responseBytes), TotalAPITime: totalAPITime.Round(time.Millisecond).String(), MaxAPITime: maxAPITime.Round(time.Millisecond).String(),
		Pods: obs.API.Objects["pods"], Nodes: obs.API.Objects["nodes"], Namespaces: obs.API.Objects["namespaces"], Events: obs.API.Objects["events"]}
	if !obs.CompletedAt.IsZero() {
		data.CompletedAt = obs.CompletedAt.Format(time.RFC3339)
		data.Duration = obs.Duration.Round(time.Millisecond).String()
	}
	data.TopOperations = diagnosticOperations(obs.API)

	srv.investigationMu.RLock()
	investigation := srv.latestInvestigation
	srv.investigationMu.RUnlock()
	if !investigation.CompletedAt.IsZero() {
		invRequests, invErrors := investigation.API.totals()
		data.Investigation = investigationDiagnostics{HasData: true, Target: investigation.Namespace + "/" + investigation.Pod,
			CompletedAt: investigation.CompletedAt.Format(time.RFC3339), Duration: investigation.Duration.Round(time.Millisecond).String(),
			Requests: invRequests, Errors: invErrors, TopOperations: diagnosticOperations(investigation.API)}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := getDiagnosticsTmpl().Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

var getDiagnosticsTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("diagnostics.html").ParseFS(templateFS, "templates/base.html", "templates/sidebar.html", "templates/diagnostics.html"))
})

func diagnosticOperations(snapshot apiCounterSnapshot) []diagnosticOperation {
	byOperation := make(map[apiOperationKey]uint64)
	for key, count := range snapshot.Requests {
		byOperation[apiOperationKey{key.Operation, key.Resource}] += count
	}
	operations := make([]diagnosticOperation, 0, len(byOperation))
	for key, count := range byOperation {
		average := time.Duration(0)
		if count != 0 {
			average = snapshot.RequestDuration[key] / time.Duration(count)
		}
		objects := uint64(0)
		if key.Operation == "LIST" {
			objects = snapshot.Objects[key.Resource]
		}
		operations = append(operations, diagnosticOperation{
			Operation: key.Operation, Resource: key.Resource, Count: count, Objects: objects,
			Bytes: formatByteCount(snapshot.ResponseBytes[key]), TotalTime: snapshot.RequestDuration[key].Round(time.Millisecond).String(), Average: average.Round(time.Millisecond).String(),
		})
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Count != operations[j].Count {
			return operations[i].Count > operations[j].Count
		}
		if operations[i].Operation != operations[j].Operation {
			return operations[i].Operation < operations[j].Operation
		}
		return operations[i].Resource < operations[j].Resource
	})
	if len(operations) > 10 {
		operations = operations[:10]
	}
	return operations
}

func formatByteCount(count uint64) string {
	const unit = 1024
	if count < unit {
		return fmt.Sprintf("%d B", count)
	}
	value := float64(count)
	units := []string{"KB", "MB", "GB"}
	for _, suffix := range units {
		value /= unit
		if value < unit || suffix == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", count)
}

func (srv *server) completeInvestigation(namespace, pod string, started time.Time, counters *apiCounters) {
	observation := investigationObservation{Namespace: namespace, Pod: pod, CompletedAt: time.Now(), Duration: time.Since(started), API: counters.snapshot()}
	srv.investigationMu.Lock()
	srv.latestInvestigation = observation
	srv.investigationMu.Unlock()
}

func (srv *server) investigationSnapshot() investigationObservation {
	srv.investigationMu.RLock()
	defer srv.investigationMu.RUnlock()
	return srv.latestInvestigation
}

func (srv *server) handleDiagnosticsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/settings/diagnostics"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

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
	fmt.Fprintln(w, "# HELP opscart_kubernetes_api_response_bytes_total Kubernetes API response body bytes read by this process.")
	fmt.Fprintln(w, "# TYPE opscart_kubernetes_api_response_bytes_total counter")
	fmt.Fprintln(w, "# HELP opscart_kubernetes_api_request_duration_seconds_total End-to-end Kubernetes API request time observed by this process.")
	fmt.Fprintln(w, "# TYPE opscart_kubernetes_api_request_duration_seconds_total counter")
	fmt.Fprintln(w, "# HELP opscart_kubernetes_api_request_duration_seconds_max Maximum end-to-end Kubernetes API request time observed by this process.")
	fmt.Fprintln(w, "# TYPE opscart_kubernetes_api_request_duration_seconds_max gauge")
	fmt.Fprintln(w, "# HELP opscart_kubernetes_api_throttles_total Kubernetes API HTTP 429 responses received by this process.")
	fmt.Fprintln(w, "# TYPE opscart_kubernetes_api_throttles_total counter")
	for _, key := range operationKeys(cumulative) {
		fmt.Fprintf(w, "opscart_kubernetes_api_response_bytes_total{operation=%q,resource=%q} %d\n", key.Operation, key.Resource, cumulative.ResponseBytes[key])
		fmt.Fprintf(w, "opscart_kubernetes_api_request_duration_seconds_total{operation=%q,resource=%q} %g\n", key.Operation, key.Resource, cumulative.RequestDuration[key].Seconds())
		fmt.Fprintf(w, "opscart_kubernetes_api_request_duration_seconds_max{operation=%q,resource=%q} %g\n", key.Operation, key.Resource, cumulative.MaxDuration[key].Seconds())
		var throttles uint64
		for metric, count := range cumulative.Requests {
			if metric.Operation == key.Operation && metric.Resource == key.Resource && metric.Result == "429" {
				throttles += count
			}
		}
		fmt.Fprintf(w, "opscart_kubernetes_api_throttles_total{operation=%q,resource=%q} %d\n", key.Operation, key.Resource, throttles)
	}
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_duration_seconds Wall-clock duration of the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_duration_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_timestamp_seconds Unix timestamp of the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_timestamp_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_requests Kubernetes API requests made by the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_requests gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_errors Kubernetes API transport errors and HTTP 4xx/5xx responses in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_errors gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_throttles Kubernetes API HTTP 429 responses in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_throttles gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_response_bytes Kubernetes API response body bytes read in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_response_bytes gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_request_duration_seconds End-to-end Kubernetes API request time in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_request_duration_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_request_max_duration_seconds Maximum end-to-end Kubernetes API request time in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_request_max_duration_seconds gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_operation_response_bytes Kubernetes API response body bytes read by operation and resource in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_operation_response_bytes gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_operation_requests Kubernetes API requests by operation and resource in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_operation_requests gauge")
	fmt.Fprintln(w, "# HELP opscart_scanner_last_scan_api_operation_request_duration_seconds End-to-end Kubernetes API request time by operation and resource in the latest completed full scan.")
	fmt.Fprintln(w, "# TYPE opscart_scanner_last_scan_api_operation_request_duration_seconds gauge")
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
		responseBytes, totalDuration, maxDuration, throttles := obs.API.costTotals()
		fmt.Fprintf(w, "opscart_scanner_last_scan_duration_seconds{cluster=%q} %g\n", cluster, obs.Duration.Seconds())
		timestamp := int64(0)
		if !obs.CompletedAt.IsZero() {
			timestamp = obs.CompletedAt.Unix()
		}
		fmt.Fprintf(w, "opscart_scanner_last_scan_timestamp_seconds{cluster=%q} %d\n", cluster, timestamp)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_requests{cluster=%q} %d\n", cluster, requests)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_errors{cluster=%q} %d\n", cluster, errors)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_throttles{cluster=%q} %d\n", cluster, throttles)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_response_bytes{cluster=%q} %d\n", cluster, responseBytes)
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_request_duration_seconds{cluster=%q} %g\n", cluster, totalDuration.Seconds())
		fmt.Fprintf(w, "opscart_scanner_last_scan_api_request_max_duration_seconds{cluster=%q} %g\n", cluster, maxDuration.Seconds())
		for _, key := range operationKeys(obs.API) {
			var operationRequests uint64
			for metric, count := range obs.API.Requests {
				if metric.Operation == key.Operation && metric.Resource == key.Resource {
					operationRequests += count
				}
			}
			fmt.Fprintf(w, "opscart_scanner_last_scan_api_operation_requests{cluster=%q,operation=%q,resource=%q} %d\n", cluster, key.Operation, key.Resource, operationRequests)
			fmt.Fprintf(w, "opscart_scanner_last_scan_api_operation_response_bytes{cluster=%q,operation=%q,resource=%q} %d\n", cluster, key.Operation, key.Resource, obs.API.ResponseBytes[key])
			fmt.Fprintf(w, "opscart_scanner_last_scan_api_operation_request_duration_seconds{cluster=%q,operation=%q,resource=%q} %g\n", cluster, key.Operation, key.Resource, obs.API.RequestDuration[key].Seconds())
		}
		for _, resource := range []string{"pods", "nodes", "namespaces", "events"} {
			fmt.Fprintf(w, "opscart_scanner_last_scan_objects_examined{cluster=%q,resource=%q} %d\n", cluster, resource, obs.API.Objects[resource])
		}
	}
}

func operationKeys(snapshot apiCounterSnapshot) []apiOperationKey {
	set := make(map[apiOperationKey]struct{})
	for key := range snapshot.ResponseBytes {
		set[key] = struct{}{}
	}
	for key := range snapshot.RequestDuration {
		set[key] = struct{}{}
	}
	keys := make([]apiOperationKey, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Operation != keys[j].Operation {
			return keys[i].Operation < keys[j].Operation
		}
		return keys[i].Resource < keys[j].Resource
	})
	return keys
}
