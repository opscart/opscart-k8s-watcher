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
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// aiFormRequest builds a same-origin, form-encoded POST request without
// dispatching it — used by the concurrent-refine tests below, which need
// to control each request's own context.
func aiFormRequest(target string, form url.Values, origin string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return request
}

// waitUntil polls cond (bounded, no fixed sleep) until it reports true or
// the deadline elapses, returning whether it became true in time — used
// only to observe an already-blocked goroutine's effect (e.g. "the
// provider was not called a second time"), never to await business logic
// that has its own explicit synchronization.
func waitUntil(deadline time.Duration, cond func() bool) bool {
	limit := time.Now().Add(deadline)
	for {
		if cond() {
			return true
		}
		if time.Now().After(limit) {
			return cond()
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHandleAIRefineConcurrentIdenticalRequestsWaitAndShareOneResult is the
// required deterministic test: two concurrent, identical refinements
// (same selector, same stable refined-evidence hash) must share exactly
// one provider call, and the follower must WAIT for and receive the
// leader's own result rather than being rejected outright.
func TestHandleAIRefineConcurrentIdenticalRequestsWaitAndShareOneResult(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	release := make(chan struct{})
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), release: release, started: make(chan struct{})}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)
	form := refineForm(selector, previewID)

	// 2. Start refinement request A.
	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		srv.handleAIRefine(recA, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
		close(doneA)
	}()

	// 3. Confirm the provider entered.
	<-provider.started

	// 4. Start an identical concurrent refinement request B.
	recB := httptest.NewRecorder()
	doneB := make(chan struct{})
	go func() {
		srv.handleAIRefine(recB, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
		close(doneB)
	}()

	// 5. Confirm B is waiting (neither request has completed) and the
	// provider call count remains 1.
	stillWaiting := waitUntil(150*time.Millisecond, func() bool {
		select {
		case <-doneA:
			return true
		case <-doneB:
			return true
		default:
			return false
		}
	})
	if stillWaiting {
		t.Fatal("expected both A and B to still be waiting on the blocked provider call")
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls while both requests are in flight = %d, want 1", got)
	}

	// 6. Release the provider.
	close(release)
	<-doneA
	<-doneB

	// 7. Both responses must succeed.
	if recA.Code != http.StatusOK || recB.Code != http.StatusOK {
		t.Fatalf("A status=%d body=%s; B status=%d body=%s", recA.Code, recA.Body.String(), recB.Code, recB.Body.String())
	}
	// 8. Both responses must contain the same result.
	if recA.Body.String() != recB.Body.String() {
		t.Fatalf("waiter received a different result than the leader: A=%s B=%s", recA.Body.String(), recB.Body.String())
	}
	// 9. The provider must have been called exactly once.
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 for two concurrent identical refinements", got)
	}
}

// TestHandleAIRefineDifferentEvidenceHashesProduceSeparateCalls proves two
// concurrent refinements that do NOT share a stable evidence hash (here:
// two different selectors) never join the same in-flight call.
func TestHandleAIRefineDifferentEvidenceHashesProduceSeparateCalls(t *testing.T) {
	scanA, dbA, podA := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	release := make(chan struct{})
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), release: release, started: make(chan struct{})}
	srv, _, _ := newAILogSignalsTestServer("prod", scanA, dbA, provider, podA)
	selectorA := collectWarRoomAISelections(scanA, "prod", dbA)[0].Selector
	previewA := requestPreview(t, srv, "prod", selectorA)

	// A second, independently-fixtured selector in the same cluster/scan
	// (a second namespace/pod) with its own preview, so its refined
	// evidence hash is guaranteed to differ from selectorA's.
	state := srv.getState("prod")
	state.mu.Lock()
	combined := *scanA
	combined.wasteAudit = &analyzer.WasteAudit{ScannedAt: scanA.wasteAudit.ScannedAt, StalePods: append(
		append([]analyzer.StalePod(nil), scanA.wasteAudit.StalePods...),
		analyzer.StalePod{Name: "payments-1", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", Severity: "critical", RestartCount: 4, AgeDays: 3},
	)}
	combined.PodWorkloads = map[string]models.WorkloadRef{
		"payments/payments-0": {Kind: "StatefulSet", Name: "payments-0", Namespace: "payments"},
		"payments/payments-1": {Kind: "StatefulSet", Name: "payments-1", Namespace: "payments"},
	}
	podB := podA.DeepCopy()
	podB.Name = "payments-1"
	combined.aiPodEvidence = buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{podA, podB}, nil, true, true, scanA.report.Timestamp)
	clientset := fake.NewSimpleClientset(podA, podB)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return clientset, nil }
	dbB := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{"prod": append(
		append([]store.IncidentSummary(nil), dbA.incidents["prod"]...),
		store.IncidentSummary{
			Fingerprint: store.WorkloadFingerprintForPod("payments", "payments-1", store.IssueCrashLoop),
			Namespace:   "payments", Resource: "payments-1", IssueType: store.IssueCrashLoop, Severity: "critical", Status: "active",
			FirstSeen: scanA.report.Timestamp.Add(-2 * time.Hour), LastSeen: scanA.report.Timestamp,
		},
	)}}
	state.scan = &combined
	state.db = dbB
	state.mu.Unlock()
	srv.db = dbB

	selectorB := ""
	for _, selection := range collectWarRoomAISelections(&combined, "prod", dbB) {
		if selection.Issue.Resource == "payments-1" {
			selectorB = selection.Selector
		}
	}
	if selectorB == "" {
		t.Fatal("expected a second selectable issue for payments-1")
	}
	previewB := requestPreview(t, srv, "prod", selectorB)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, form := range []url.Values{refineForm(selectorA, previewA), refineForm(selectorB, previewB)} {
		wg.Add(1)
		go func(i int, form url.Values) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			srv.handleAIRefine(recorder, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
			codes[i] = recorder.Code
		}(i, form)
	}
	waitUntil(150*time.Millisecond, func() bool { return provider.callCount() >= 2 })
	close(release)
	wg.Wait()

	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 for two different selectors/evidence hashes", provider.callCount())
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, code)
		}
	}
}

// TestHandleAIRefineCanceledWaiterDoesNotAffectSharedRequest proves a
// waiter whose own request context is canceled simply stops watching —
// the shared provider call, and any other waiter still depending on it,
// are completely unaffected.
func TestHandleAIRefineCanceledWaiterDoesNotAffectSharedRequest(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	release := make(chan struct{})
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse(), release: release, started: make(chan struct{})}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)
	form := refineForm(selector, previewID)

	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		srv.handleAIRefine(recA, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
		close(doneA)
	}()
	<-provider.started

	ctx, cancel := context.WithCancel(context.Background())
	reqB := aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com").WithContext(ctx)
	recB := httptest.NewRecorder()
	doneB := make(chan struct{})
	go func() {
		srv.handleAIRefine(recB, reqB)
		close(doneB)
	}()

	// Cancel B before the shared provider call ever completes.
	cancel()
	<-doneB
	if recB.Body.Len() != 0 {
		t.Fatalf("expected the canceled waiter to write no response body, got status %d body=%s", recB.Code, recB.Body.String())
	}

	// Explicitly confirm the shared provider call is still active after B
	// canceled: neither A nor the provider have finished, and the provider
	// has not been called again on B's behalf.
	stillRunning := waitUntil(150*time.Millisecond, func() bool {
		select {
		case <-doneA:
			return true
		default:
			return false
		}
	})
	if stillRunning {
		t.Fatal("the shared provider call must remain active after only one waiter canceled")
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls after B canceled = %d, want 1 (B's cancellation must not trigger a new call)", provider.callCount())
	}

	// Release the provider and confirm the OTHER waiter (A) still receives
	// the result the shared call produces.
	close(release)
	<-doneA
	if recA.Code != http.StatusOK {
		t.Fatalf("leader status = %d, body=%s (a canceled waiter must not corrupt the shared request)", recA.Code, recA.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}
}

// TestHandleAIRefineFailureWakesAllWaiters proves a provider failure is
// published to every waiter as the same safe, sanitized result — never a
// raw provider error/body — and still counts as exactly one provider call.
func TestHandleAIRefineFailureWakesAllWaiters(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	release := make(chan struct{})
	provider := &fakeWarRoomAIProvider{err: errors.New("provider-secret-body"), release: release, started: make(chan struct{})}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)
	form := refineForm(selector, previewID)

	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		srv.handleAIRefine(recA, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
		close(doneA)
	}()
	<-provider.started

	recB := httptest.NewRecorder()
	doneB := make(chan struct{})
	go func() {
		srv.handleAIRefine(recB, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com"))
		close(doneB)
	}()
	waitUntil(150*time.Millisecond, func() bool { return false })
	close(release)
	<-doneA
	<-doneB

	if recA.Code != http.StatusBadGateway || recB.Code != http.StatusBadGateway {
		t.Fatalf("A status=%d B status=%d, want both 502", recA.Code, recB.Code)
	}
	if recA.Body.String() != recB.Body.String() {
		t.Fatalf("both waiters must see the same safe failure result: A=%s B=%s", recA.Body.String(), recB.Body.String())
	}
	if strings.Contains(recA.Body.String(), "provider-secret-body") || strings.Contains(recB.Body.String(), "provider-secret-body") {
		t.Fatalf("a provider failure must never leak the raw provider error/body: A=%s B=%s", recA.Body.String(), recB.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}
	if _, ok := srv.aiRuntime.cache.latest("prod", selector); ok {
		t.Fatal("a failed refinement must not be cached")
	}
	if srv.aiRuntime.cache.isInFlight("prod", selector) {
		t.Fatal("a failed refinement must not leave server-side in-flight state")
	}

	// A subsequent refine attempt must be able to proceed rather than being
	// rejected as still in progress.
	provider.err = nil
	provider.response = testWarRoomAIResponse()
	srv.aiOperationCooldown = newAIOperationCooldown(0, srv.aiRuntime.cache.now)
	retryPreviewID := requestPreview(t, srv, "prod", selector)
	retryRecorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, retryPreviewID), "http://example.com")
	if retryRecorder.Code != http.StatusOK {
		t.Fatalf("retry after failed refinement status = %d, want 200; body=%s", retryRecorder.Code, retryRecorder.Body.String())
	}
}

// ctxAwareProvider blocks until the context it is given is canceled or
// expires, then reports what it observed on observed. started is closed
// (once) the moment Analyze begins, so a test can deterministically know
// the shared provider call is actually in flight before acting further.
type ctxAwareProvider struct {
	once     sync.Once
	started  chan struct{}
	observed chan error
}

func (p *ctxAwareProvider) Analyze(ctx context.Context, _ aianalysis.AnalysisRequest) (*aianalysis.AnalysisResponse, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	err := ctx.Err()
	p.observed <- err
	return nil, err
}

// TestHandleAIRefineSharedOperationHasBoundedTimeout is required test A:
// the shared refinement provider call must be bounded by the server's
// configured AI timeout, never run unboundedly, and its failure must
// surface as the existing safe gateway-timeout response with no
// provider/internal error text leaked — and the in-flight entry must be
// cleared afterward so a later request can proceed.
func TestHandleAIRefineSharedOperationHasBoundedTimeout(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &ctxAwareProvider{started: make(chan struct{}), observed: make(chan error, 1)}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	srv.aiTimeout = 20 * time.Millisecond
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")

	select {
	case err := <-provider.observed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("provider observed %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the shared provider call never observed the server AI timeout")
	}

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusGatewayTimeout, recorder.Body.String())
	}
	for _, leak := range []string{"context deadline exceeded", "ctxAwareProvider"} {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("response leaked internal/provider error text %q: %s", leak, recorder.Body.String())
		}
	}
	if srv.aiRuntime.cache.isInFlight("prod", selector) {
		t.Fatal("in-flight entry was not removed after the shared operation timed out")
	}
}

// TestHandleAIRefineAllWaitersDisconnectStillBoundedAndCleansUp is
// required test C: if every HTTP request watching a shared refinement
// disconnects, the underlying provider call must still be bounded by the
// server AI timeout (never left to run forever) and must still clear its
// in-flight entry once it exits.
func TestHandleAIRefineAllWaitersDisconnectStillBoundedAndCleansUp(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &ctxAwareProvider{started: make(chan struct{}), observed: make(chan error, 1)}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	srv.aiTimeout = 30 * time.Millisecond
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)
	form := refineForm(selector, previewID)

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	recA, recB := httptest.NewRecorder(), httptest.NewRecorder()
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() {
		srv.handleAIRefine(recA, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com").WithContext(ctxA))
		close(doneA)
	}()
	<-provider.started
	go func() {
		srv.handleAIRefine(recB, aiFormRequest("/api/investigation/ai/refine?cluster=prod", form, "http://example.com").WithContext(ctxB))
		close(doneB)
	}()

	cancelA()
	cancelB()
	<-doneA
	<-doneB
	if recA.Body.Len() != 0 || recB.Body.Len() != 0 {
		t.Fatalf("expected no response body written to any disconnected waiter: A=%q B=%q", recA.Body.String(), recB.Body.String())
	}

	// The shared operation must still be bounded by the server timeout and
	// exit on its own, even though nobody is watching it anymore.
	select {
	case err := <-provider.observed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("provider observed %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the shared operation never completed after all waiters disconnected")
	}
	if !waitUntil(500*time.Millisecond, func() bool { return !srv.aiRuntime.cache.isInFlight("prod", selector) }) {
		t.Fatal("in-flight entry was not removed after all waiters disconnected")
	}
}
