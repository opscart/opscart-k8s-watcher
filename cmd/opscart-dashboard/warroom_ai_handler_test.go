package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

type fakeWarRoomAIProvider struct {
	mu       sync.Mutex
	calls    int
	requests []aianalysis.AnalysisRequest
	response *aianalysis.AnalysisResponse
	err      error
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (provider *fakeWarRoomAIProvider) Analyze(ctx context.Context, request aianalysis.AnalysisRequest) (*aianalysis.AnalysisResponse, error) {
	provider.mu.Lock()
	provider.calls++
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if provider.started != nil {
		provider.once.Do(func() { close(provider.started) })
	}
	if provider.release != nil {
		select {
		case <-provider.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if provider.err != nil {
		return nil, provider.err
	}
	response := cloneAnalysisResponse(*provider.response)
	return &response, nil
}

func (provider *fakeWarRoomAIProvider) callCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func (provider *fakeWarRoomAIProvider) lastRequest() aianalysis.AnalysisRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.requests[len(provider.requests)-1]
}

func testWarRoomAIResponse() *aianalysis.AnalysisResponse {
	return &aianalysis.AnalysisResponse{
		Summary:         "Restart failure remains active.",
		LikelyCauses:    []aianalysis.LikelyCause{{Title: "Startup failure", Rationale: "The supplied restart count is high."}},
		Recommendations: []aianalysis.Recommendation{{Action: "Inspect configuration", Rationale: "Validate inputs without changing the cluster."}},
		EvidenceUsed:    []string{"restart_count"}, MissingEvidence: []string{"previous container termination reason"},
		Confidence: aianalysis.ConfidenceMedium,
	}
}

func newWarRoomAITestServer(clusters []string, scans map[string]*clusterScan, db warRoomAITestStore, provider aianalysis.AIProvider) *server {
	states := make(map[string]*dashboardState, len(scans))
	for cluster, scan := range scans {
		states[cluster] = &dashboardState{ctx: cluster, scan: scan, db: db}
	}
	srv := &server{
		clusterList: clusters, states: states, db: db,
		auth:       &authConfig{username: "operator", password: "test-password", source: authSourceEnv},
		aiProvider: provider,
	}
	if provider != nil {
		srv.aiRuntime = newWarRoomAIRuntime("openai", "synthetic-model")
	}
	return srv
}

func warRoomAIPost(t *testing.T, handler http.Handler, target, selector, origin string, authenticate bool) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"issue": {selector}}
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if authenticate {
		request.SetBasicAuth("operator", "test-password")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestWarRoomAIIsDisabledAndManualOnly(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	disabled := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, nil)

	request := httptest.NewRequest(http.MethodGet, "/warroom?cluster=prod", nil)
	request.SetBasicAuth("operator", "test-password")
	recorder := httptest.NewRecorder()
	disabled.newMux().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("disabled page status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `class="ai-chip"`) {
		t.Fatal("AI link rendered while AI was disabled")
	}

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	enabled := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	request = httptest.NewRequest(http.MethodGet, "/warroom?cluster=prod", nil)
	request.SetBasicAuth("operator", "test-password")
	recorder = httptest.NewRecorder()
	enabled.newMux().ServeHTTP(recorder, request)
	if provider.callCount() != 0 {
		t.Fatal("War Room GET invoked the provider")
	}
	// The War Room page never embeds a generate control itself — generation
	// only happens from the Investigation page's AI Analysis tab.
	if strings.Contains(recorder.Body.String(), `id="ai-generate"`) {
		t.Fatal("War Room page rendered a generation control")
	}
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	unauthorized := warRoomAIPost(t, enabled.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", false)
	if unauthorized.Code != http.StatusUnauthorized || provider.callCount() != 0 {
		t.Fatalf("unauthenticated generation status=%d calls=%d", unauthorized.Code, provider.callCount())
	}
}

func TestWarRoomAISelectionIsServerResolvedAndClusterIsolated(t *testing.T) {
	prodScan, prodDB := warRoomAICrashFixture("prod", 17)
	stagingScan, stagingDB := warRoomAICrashFixture("staging", 3)
	db := prodDB
	db.incidents["staging"] = stagingDB.incidents["staging"]
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod", "staging"}, map[string]*clusterScan{"prod": prodScan, "staging": stagingScan}, db, provider)
	selector := collectWarRoomAISelections(prodScan, "prod", db)[0].Selector

	recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if recorder.Code != http.StatusOK || provider.callCount() != 1 {
		t.Fatalf("generation status=%d calls=%d body=%s", recorder.Code, provider.callCount(), recorder.Body.String())
	}
	request := provider.lastRequest()
	if request.Cluster != "prod" || request.Evidence[1].Details != "restart_count=17; resource_age_days=3" {
		t.Fatalf("provider received anything other than server-resolved prod evidence: %+v", request)
	}

	recorder = warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=staging", selector, "http://example.com", true)
	if recorder.Code != http.StatusNotFound || provider.callCount() != 1 {
		t.Fatalf("cross-cluster selector status=%d calls=%d", recorder.Code, provider.callCount())
	}
	statesBefore := len(srv.states)
	recorder = warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=attacker", selector, "http://example.com", true)
	if recorder.Code != http.StatusBadRequest || len(srv.states) != statesBefore {
		t.Fatalf("unknown cluster status=%d states=%d want=%d", recorder.Code, len(srv.states), statesBefore)
	}
}

func TestWarRoomAIDefaultClusterUsesNonemptyProviderIdentity(t *testing.T) {
	scan, db := warRoomAICrashFixture("", 5)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{""}, map[string]*clusterScan{"": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "", db)[0].Selector
	recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis", selector, "http://example.com", true)
	if recorder.Code != http.StatusOK || provider.callCount() != 1 {
		t.Fatalf("default-cluster generation status=%d calls=%d body=%s", recorder.Code, provider.callCount(), recorder.Body.String())
	}
	if got := provider.lastRequest().Cluster; got != "current-context" {
		t.Fatalf("provider cluster identity = %q, want current-context", got)
	}
}

func TestWarRoomAIRejectsCrossOriginAndBrowserEvidence(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "https://evil.example", true)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", recorder.Code)
	}
	recorder = warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "https://example.com", true)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-scheme origin status = %d", recorder.Code)
	}
	recorder = warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "", true)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing origin status = %d", recorder.Code)
	}
	form := url.Values{"issue": {selector}, "evidence": {"browser-secret"}}
	request := httptest.NewRequest(http.MethodPost, "/api/warroom/ai-analysis?cluster=prod", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.SetBasicAuth("operator", "test-password")
	recorder = httptest.NewRecorder()
	srv.newMux().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || provider.callCount() != 0 {
		t.Fatalf("browser evidence status=%d calls=%d", recorder.Code, provider.callCount())
	}
}

func TestWarRoomAISameOriginValidation(t *testing.T) {
	tests := []struct {
		name           string
		target         string
		origin         string
		forwardedHost  string
		secFetchSite   string
		forwardedProto []string
		want           bool
	}{
		{name: "direct HTTP", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://dashboard.example", want: true},
		{name: "direct HTTPS", target: "https://dashboard.example/api/warroom/ai-analysis", origin: "https://dashboard.example", want: true},
		{name: "forwarded HTTP", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://dashboard.example", forwardedProto: []string{"http"}, want: true},
		{name: "TLS terminating proxy", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "https://dashboard.example", forwardedProto: []string{"https"}, want: true},
		{name: "host mismatch", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://other.example", want: false},
		{name: "forwarded host is not trusted", target: "http://internal.example/api/warroom/ai-analysis", origin: "https://public.example", forwardedHost: "public.example", forwardedProto: []string{"https"}, want: false},
		{name: "cross-site fetch", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://dashboard.example", secFetchSite: "cross-site", want: false},
		{name: "missing origin", target: "http://dashboard.example/api/warroom/ai-analysis", want: false},
		{name: "empty forwarded proto", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://dashboard.example", forwardedProto: []string{""}, want: false},
		{name: "unsupported forwarded proto", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "http://dashboard.example", forwardedProto: []string{"ftp"}, want: false},
		{name: "whitespace forwarded proto", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "https://dashboard.example", forwardedProto: []string{" https "}, want: false},
		{name: "comma-separated forwarded proto", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "https://dashboard.example", forwardedProto: []string{"https,http"}, want: false},
		{name: "repeated forwarded proto", target: "http://dashboard.example/api/warroom/ai-analysis", origin: "https://dashboard.example", forwardedProto: []string{"https", "https"}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.target, nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.forwardedHost != "" {
				request.Header.Set("X-Forwarded-Host", test.forwardedHost)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			for _, value := range test.forwardedProto {
				request.Header.Add("X-Forwarded-Proto", value)
			}
			if got := sameOriginWarRoomAIRequest(request); got != test.want {
				t.Fatalf("sameOriginWarRoomAIRequest() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWarRoomAICacheReuseAndExplicitRegeneration(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	handler := srv.newMux()

	first := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	second := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || provider.callCount() != 1 || !strings.Contains(second.Body.String(), `"status":"cached"`) {
		t.Fatalf("cache reuse first=%d second=%d calls=%d body=%s", first.Code, second.Code, provider.callCount(), second.Body.String())
	}
	regenerated := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod&regenerate=1", selector, "http://example.com", true)
	if regenerated.Code != http.StatusOK || provider.callCount() != 2 {
		t.Fatalf("regeneration status=%d calls=%d body=%s", regenerated.Code, provider.callCount(), regenerated.Body.String())
	}
}

func TestWarRoomAIStaleEvidenceRequiresExplicitRegeneration(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	handler := srv.newMux()
	if got := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true); got.Code != http.StatusOK {
		t.Fatalf("initial generation status=%d body=%s", got.Code, got.Body.String())
	}
	updated, _ := warRoomAICrashFixture("prod", 8)
	updated.report.Timestamp = updated.report.Timestamp.Add(time.Minute)
	updated.wasteAudit.ScannedAt = updated.wasteAudit.ScannedAt.Add(time.Minute)
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = updated
	srv.states["prod"].mu.Unlock()

	stale := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if stale.Code != http.StatusOK || !strings.Contains(stale.Body.String(), `"status":"stale"`) || provider.callCount() != 1 {
		t.Fatalf("stale request status=%d calls=%d body=%s", stale.Code, provider.callCount(), stale.Body.String())
	}
	page := srv.buildInvestigationAIPageData(updated, "prod", selector, "")
	if page.Status != "STALE" || page.EvidenceCaptured != formatWarRoomAITime(scan.report.Timestamp) {
		t.Fatalf("stale result metadata = status %q captured %q, want original evidence time %q", page.Status, page.EvidenceCaptured, formatWarRoomAITime(scan.report.Timestamp))
	}
	regenerated := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod&regenerate=1", selector, "http://example.com", true)
	if regenerated.Code != http.StatusOK || provider.callCount() != 2 || !strings.Contains(provider.lastRequest().Evidence[1].Details, "restart_count=8") {
		t.Fatalf("latest evidence was not regenerated: status=%d calls=%d request=%+v", regenerated.Code, provider.callCount(), provider.lastRequest())
	}
}

func TestWarRoomAIDuplicateGenerationAndCancellation(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), started: make(chan struct{}), release: make(chan struct{})}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	handler := srv.newMux()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	}()
	<-provider.started
	duplicate := warRoomAIPost(t, handler, "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if duplicate.Code != http.StatusConflict || provider.callCount() != 1 {
		t.Fatalf("duplicate status=%d calls=%d", duplicate.Code, provider.callCount())
	}
	close(provider.release)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first generation status=%d body=%s", first.Code, first.Body.String())
	}

	cancelProvider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), started: make(chan struct{}), release: make(chan struct{})}
	cancelServer := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, cancelProvider)
	form := url.Values{"issue": {selector}}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/warroom/ai-analysis?cluster=prod", strings.NewReader(form.Encode())).WithContext(ctx)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.SetBasicAuth("operator", "test-password")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		cancelServer.newMux().ServeHTTP(recorder, request)
		close(done)
	}()
	<-cancelProvider.started
	cancel()
	<-done
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled generation status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, ok := cancelServer.aiRuntime.cache.latest("prod", selector); ok {
		t.Fatal("canceled result was cached")
	}
}

func TestWarRoomAILateResponseIsMarkedStale(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), started: make(chan struct{}), release: make(chan struct{})}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	}()
	<-provider.started
	updated, _ := warRoomAICrashFixture("prod", 9)
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = updated
	srv.states["prod"].mu.Unlock()
	close(provider.release)
	recorder := <-done
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"stale"`) {
		t.Fatalf("late response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWarRoomAIProviderErrorsAreSafe(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), err: errors.New("provider-secret-and-body")}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "provider-secret-and-body") {
		t.Fatalf("unsafe provider error response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWarRoomAIRejectsInvalidProviderResponsesWithoutCaching(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*aianalysis.AnalysisResponse)
	}{
		{name: "oversized", mutate: func(response *aianalysis.AnalysisResponse) {
			response.Summary = strings.Repeat("oversized-provider-output", 128)
		}},
		{name: "invalid", mutate: func(response *aianalysis.AnalysisResponse) {
			response.LikelyCauses[0].Title = " \t"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scan, db := warRoomAICrashFixture("prod", 7)
			response := testWarRoomAIResponse()
			test.mutate(response)
			provider := &fakeWarRoomAIProvider{response: response}
			srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
			selection := collectWarRoomAISelections(scan, "prod", db)[0]

			recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selection.Selector, "http://example.com", true)
			if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "provider-output") {
				t.Fatalf("invalid provider response status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if _, ok := srv.aiRuntime.cache.latest("prod", selection.Selector); ok {
				t.Fatal("invalid provider response was cached")
			}
			page := srv.buildInvestigationAIPageData(scan, "prod", selection.Selector, "")
			if page.Result != nil {
				t.Fatal("invalid provider response was available for rendering")
			}
		})
	}
}
