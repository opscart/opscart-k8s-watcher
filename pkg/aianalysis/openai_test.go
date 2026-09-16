package aianalysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const syntheticAPIKey = "synthetic-provider-key"

func TestOpenAIProviderSendsConfiguredRequestAndMapsResponse(t *testing.T) {
	want := AnalysisResponse{
		Summary: "The restart evidence is consistent with an unstable workload.",
		LikelyCauses: []LikelyCause{{
			Title:     "Repeated process failure",
			Rationale: "The supplied restart count increased.",
		}},
		Recommendations: []Recommendation{{
			Action:    "Inspect the workload configuration.",
			Rationale: "Configuration review is read-only and may explain the restarts.",
		}},
		EvidenceUsed:    []string{"restart count"},
		MissingEvidence: []string{"container termination reason"},
		Confidence:      ConfidenceMedium,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/compatible/v1/responses" {
			t.Errorf("path = %q, want /compatible/v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+syntheticAPIKey {
			t.Errorf("Authorization = %q", got)
		}

		var request openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Model != "configured-model" {
			t.Errorf("model = %q, want configured-model", request.Model)
		}
		if request.MaxOutputTokens != openAIMaxOutputTokens {
			t.Errorf("max_output_tokens = %d, want %d", request.MaxOutputTokens, openAIMaxOutputTokens)
		}
		if request.Store {
			t.Error("store = true, want false")
		}
		for _, constraint := range []string{
			"only the operational evidence supplied",
			"Do not claim or imply direct access",
			"distinguish observed facts from hypotheses",
			"evidence that is missing",
			"only read-only investigation steps",
			"Never claim that an action was executed",
		} {
			if !strings.Contains(request.Instructions, constraint) {
				t.Errorf("instructions missing %q: %q", constraint, request.Instructions)
			}
		}
		if request.Text.Format.Type != "json_schema" || !request.Text.Format.Strict {
			t.Errorf("response format = %#v", request.Text.Format)
		}
		assertControlledInput(t, request.Input)

		structured, err := json.Marshal(want)
		if err != nil {
			t.Errorf("marshal fixture: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		response := map[string]any{
			"status": "completed",
			"output": []any{
				map[string]any{"type": "reasoning"},
				map[string]any{
					"type": "message",
					"content": []any{map[string]any{
						"type": "output_text",
						"text": string(structured),
					}},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	provider := mustProvider(t, server.URL+"/compatible/v1", time.Second)
	got, err := provider.Analyze(context.Background(), syntheticRequest())
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("Analyze() = %#v, want %#v", *got, want)
	}
}

func TestOpenAIProviderTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	provider := mustProvider(t, server.URL, 20*time.Millisecond)
	_, err := provider.Analyze(context.Background(), syntheticRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Analyze() error = %v, want client timeout", err)
	}
}

func TestOpenAIProviderContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider endpoint was called with an already canceled context")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := mustProvider(t, server.URL, time.Second)
	_, err := provider.Analyze(ctx, syntheticRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze() error = %v, want context.Canceled", err)
	}
}

func TestOpenAIProviderContextCancellationWhileReadingBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readStarted := make(chan struct{})
	provider := mustProvider(t, "https://provider.test/v1", time.Second).(*openAIProvider)
	provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &contextReadBody{
				ctx:     request.Context(),
				started: readStarted,
			},
			Header: make(http.Header),
		}, nil
	})

	result := make(chan error, 1)
	go func() {
		_, err := provider.Analyze(ctx, syntheticRequest())
		result <- err
	}()
	<-readStarted
	cancel()

	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze() error = %v, want context.Canceled", err)
	}
	assertErrorDoesNotContain(t, err, syntheticAPIKey, "provider.test")
}

func TestOpenAIProviderTimeoutWhileReadingBody(t *testing.T) {
	readStarted := make(chan struct{})
	provider := mustProvider(t, "https://provider.test/v1", 20*time.Millisecond).(*openAIProvider)
	provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &contextReadBody{
				ctx:     request.Context(),
				started: readStarted,
			},
			Header: make(http.Header),
		}, nil
	})

	result := make(chan error, 1)
	go func() {
		_, err := provider.Analyze(context.Background(), syntheticRequest())
		result <- err
	}()
	<-readStarted

	err := <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Analyze() error = %v, want context.DeadlineExceeded", err)
	}
	assertErrorDoesNotContain(t, err, syntheticAPIKey, "provider.test")
}

func TestOpenAIProviderSanitizesBodyReadErrors(t *testing.T) {
	const bodySentinel = "body-read-content-sentinel"
	provider := mustProvider(t, "https://provider.test/v1", time.Second).(*openAIProvider)
	provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       &failingReadBody{err: errors.New(bodySentinel + " " + request.Header.Get("Authorization"))},
			Header:     make(http.Header),
		}, nil
	})

	_, err := provider.Analyze(context.Background(), syntheticRequest())
	if !errors.Is(err, errInvalidProviderResponse) {
		t.Fatalf("Analyze() error = %v, want invalid provider response", err)
	}
	assertErrorDoesNotContain(t, err, bodySentinel, syntheticAPIKey)
}

func TestOpenAIProviderNon2xxDoesNotExposeAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "credential "+syntheticAPIKey+" rejected", http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := mustProvider(t, server.URL, time.Second)
	_, err := provider.Analyze(context.Background(), syntheticRequest())
	if err == nil || !strings.Contains(err.Error(), "HTTP status 401") {
		t.Fatalf("Analyze() error = %v, want safe HTTP status error", err)
	}
	if strings.Contains(err.Error(), syntheticAPIKey) {
		t.Fatalf("Analyze() error exposed API key: %v", err)
	}
}

func TestOpenAIProviderDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var destinationRequests atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destinationRequests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer destination.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL+"/collect")
				w.WriteHeader(status)
			}))
			defer origin.Close()

			request := syntheticRequest()
			request.Evidence[0].Details = "redirect-evidence-sentinel"
			provider := mustProvider(t, origin.URL, time.Second)
			_, err := provider.Analyze(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP status %d", status)) {
				t.Fatalf("Analyze() error = %v, want safe redirect status error", err)
			}
			if got := destinationRequests.Load(); got != 0 {
				t.Fatalf("redirect destination received %d requests, want 0", got)
			}
			assertErrorDoesNotContain(t, err, syntheticAPIKey, "redirect-evidence-sentinel", destination.URL)
		})
	}
}

func TestOpenAIProviderSanitizesTransportErrors(t *testing.T) {
	tests := []struct {
		name      string
		cause     error
		wantCause error
	}{
		{name: "provider text"},
		{name: "canceled", cause: context.Canceled, wantCause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded, wantCause: context.DeadlineExceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := mustProvider(t, "https://provider-url-sentinel.invalid/v1", time.Second).(*openAIProvider)
			provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				leaked := "transport-content-sentinel " + request.URL.String() + " " + request.Header.Get("Authorization")
				if test.cause != nil {
					return nil, fmt.Errorf("%s: %w", leaked, test.cause)
				}
				return nil, errors.New(leaked)
			})

			_, err := provider.Analyze(context.Background(), syntheticRequest())
			if err == nil {
				t.Fatal("Analyze() error = nil, want transport error")
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("Analyze() error = %v, want errors.Is(%v)", err, test.wantCause)
			}
			assertErrorDoesNotContain(t, err, "transport-content-sentinel", "provider-url-sentinel", syntheticAPIKey)
		})
	}
}

func TestOpenAIProviderRejectsUnsupportedEvidenceBeforeSending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider endpoint received unsupported evidence")
	}))
	defer server.Close()

	request := syntheticRequest()
	request.Evidence[0].Type = EvidenceType("raw_log")
	provider := mustProvider(t, server.URL, time.Second)
	if _, err := provider.Analyze(context.Background(), request); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("Analyze() error = %v, want unsupported evidence error", err)
	}
}

func TestOpenAIProviderRejectsMalformedResponses(t *testing.T) {
	const responseSentinel = "provider-response-secret-sentinel"
	tests := []struct {
		name string
		body string
	}{
		{name: "provider envelope", body: `{"status":"completed","` + responseSentinel},
		{name: "structured output", body: `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{not-json"}]}]}`},
		{name: "unknown structured field", body: `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"summary\":\"summary\",\"likely_causes\":[],\"recommendations\":[],\"evidence_used\":[],\"missing_evidence\":[],\"confidence\":\"low\",\"` + responseSentinel + `\":true}"}]}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			provider := mustProvider(t, server.URL, time.Second)
			_, err := provider.Analyze(context.Background(), syntheticRequest())
			if err == nil {
				t.Fatal("Analyze() error = nil, want malformed response error")
			}
			assertErrorDoesNotContain(t, err, responseSentinel, syntheticAPIKey)
		})
	}
}

func TestOpenAIProviderRequiresCompletedStatus(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"output":[]}`},
		{name: "null", body: `{"status":null,"output":[]}`},
		{name: "incomplete", body: `{"status":"incomplete","output":[]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			provider := mustProvider(t, server.URL, time.Second)
			_, err := provider.Analyze(context.Background(), syntheticRequest())
			if err == nil || err.Error() != "AI provider response was not completed" {
				t.Fatalf("Analyze() error = %v, want incomplete response error", err)
			}
		})
	}
}

func TestOpenAIProviderRejectsResponseOutsideNeutralBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AnalysisResponse)
	}{
		{name: "oversized", mutate: func(response *AnalysisResponse) {
			response.Summary = strings.Repeat("provider-output-sentinel", maxSummaryBytes)
		}},
		{name: "whitespace-only", mutate: func(response *AnalysisResponse) {
			response.Recommendations[0].Action = " \t"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validAnalysisResponse()
			test.mutate(response)
			structured, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"content": []any{map[string]any{"type": "output_text", "text": string(structured)}},
					}},
				})
			}))
			defer server.Close()

			provider := mustProvider(t, server.URL, time.Second)
			_, err = provider.Analyze(context.Background(), syntheticRequest())
			if !errors.Is(err, errInvalidStructuredResponse) {
				t.Fatalf("Analyze() error = %v, want invalid structured response", err)
			}
			assertErrorDoesNotContain(t, err, "provider-output-sentinel", syntheticAPIKey, server.URL)
		})
	}
}

func TestDecodeAnalysisResponsePreservesEmptyArrays(t *testing.T) {
	response, err := decodeAnalysisResponse(validStructuredResponse)
	if err != nil {
		t.Fatalf("decodeAnalysisResponse() error = %v", err)
	}
	if response.LikelyCauses == nil || response.Recommendations == nil || response.EvidenceUsed == nil || response.MissingEvidence == nil {
		t.Fatalf("decodeAnalysisResponse() returned nil arrays: %#v", response)
	}
}

func TestDecodeAnalysisResponseRejectsIncompleteContract(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "malformed", value: `{not-json`},
		{name: "trailing data", value: validStructuredResponse + `{}`},
		{name: "unknown field", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low","unknown":true}`},
		{name: "missing summary", value: `{"likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null summary", value: `{"summary":null,"likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "empty summary", value: `{"summary":"","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "missing causes", value: `{"summary":"summary","recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null causes", value: `{"summary":"summary","likely_causes":null,"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null cause", value: `{"summary":"summary","likely_causes":[null],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "cause missing title", value: `{"summary":"summary","likely_causes":[{"rationale":"why"}],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "cause null title", value: `{"summary":"summary","likely_causes":[{"title":null,"rationale":"why"}],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "cause missing rationale", value: `{"summary":"summary","likely_causes":[{"title":"cause"}],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "cause null rationale", value: `{"summary":"summary","likely_causes":[{"title":"cause","rationale":null}],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "missing recommendations", value: `{"summary":"summary","likely_causes":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null recommendations", value: `{"summary":"summary","likely_causes":[],"recommendations":null,"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null recommendation", value: `{"summary":"summary","likely_causes":[],"recommendations":[null],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "recommendation missing action", value: `{"summary":"summary","likely_causes":[],"recommendations":[{"rationale":"why"}],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "recommendation null action", value: `{"summary":"summary","likely_causes":[],"recommendations":[{"action":null,"rationale":"why"}],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "recommendation missing rationale", value: `{"summary":"summary","likely_causes":[],"recommendations":[{"action":"inspect"}],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "recommendation null rationale", value: `{"summary":"summary","likely_causes":[],"recommendations":[{"action":"inspect","rationale":null}],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "missing evidence used", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"missing_evidence":[],"confidence":"low"}`},
		{name: "null evidence used", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":null,"missing_evidence":[],"confidence":"low"}`},
		{name: "null evidence used item", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[null],"missing_evidence":[],"confidence":"low"}`},
		{name: "missing missing evidence", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"confidence":"low"}`},
		{name: "null missing evidence", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":null,"confidence":"low"}`},
		{name: "null missing evidence item", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[null],"confidence":"low"}`},
		{name: "missing confidence", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[]}`},
		{name: "null confidence", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":null}`},
		{name: "unsupported confidence", value: `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"certain"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := decodeAnalysisResponse(test.value)
			if !errors.Is(err, errInvalidStructuredResponse) {
				t.Fatalf("decodeAnalysisResponse() = %#v, %v; want invalid structured response", response, err)
			}
		})
	}
}

func TestAnalysisRequestHasNoRawSensitiveFields(t *testing.T) {
	forbidden := []string{"secret", "token", "log", "environment", "env", "raw"}
	requestType := reflect.TypeOf(AnalysisRequest{})
	evidenceType := reflect.TypeOf(EvidenceItem{})
	for _, typ := range []reflect.Type{requestType, evidenceType} {
		for i := 0; i < typ.NumField(); i++ {
			field := strings.ToLower(typ.Field(i).Name + " " + typ.Field(i).Tag.Get("json"))
			for _, term := range forbidden {
				if strings.Contains(field, term) {
					t.Fatalf("%s contains forbidden raw-data field %q", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func mustProvider(t *testing.T, baseURL string, timeout time.Duration) AIProvider {
	t.Helper()
	provider, err := NewAIProvider(Config{
		Enabled:  true,
		Provider: ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    "configured-model",
		APIKey:   syntheticAPIKey,
		Timeout:  timeout,
	})
	if err != nil {
		t.Fatalf("NewAIProvider() error = %v", err)
	}
	return provider
}

func syntheticRequest() AnalysisRequest {
	return AnalysisRequest{
		Cluster:       "synthetic-cluster",
		Namespace:     "payments",
		ResourceKind:  "Deployment",
		ResourceName:  "checkout",
		IssueType:     "CrashLoopBackOff",
		Severity:      "critical",
		FirstDetected: time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC),
		ReopenCount:   2,
		Evidence: []EvidenceItem{{
			Type:    EvidenceMetric,
			Summary: "Container restart count increased",
			Details: "restart_count=12",
		}},
	}
}

func assertControlledInput(t *testing.T, input string) {
	t.Helper()
	var decoded AnalysisRequest
	if err := json.Unmarshal([]byte(input), &decoded); err != nil {
		t.Fatalf("decode typed input: %v", err)
	}
	if want := syntheticRequest(); !reflect.DeepEqual(decoded, want) {
		t.Fatalf("provider input = %#v, want %#v", decoded, want)
	}

	var value map[string]any
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	wantKeys := []string{"cluster", "evidence", "first_detected", "issue_type", "namespace", "reopen_count", "resource_kind", "resource_name", "severity"}
	gotKeys := make([]string, 0, len(value))
	for key := range value {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("provider input keys = %v, want %v", gotKeys, wantKeys)
	}

	evidence, ok := value["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("provider evidence = %#v", value["evidence"])
	}
	item, ok := evidence[0].(map[string]any)
	if !ok {
		t.Fatalf("provider evidence item = %#v", evidence[0])
	}
	itemKeys := make([]string, 0, len(item))
	for key := range item {
		itemKeys = append(itemKeys, key)
	}
	sort.Strings(itemKeys)
	if want := []string{"details", "summary", "type"}; !reflect.DeepEqual(itemKeys, want) {
		t.Fatalf("provider evidence keys = %v, want %v", itemKeys, want)
	}
}

func assertErrorDoesNotContain(t *testing.T, err error, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error %q exposed %q", err, value)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type contextReadBody struct {
	ctx     context.Context
	started chan<- struct{}
	once    atomic.Bool
}

func (body *contextReadBody) Read([]byte) (int, error) {
	if body.once.CompareAndSwap(false, true) {
		close(body.started)
	}
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*contextReadBody) Close() error { return nil }

type failingReadBody struct {
	err error
}

func (body *failingReadBody) Read([]byte) (int, error) { return 0, body.err }
func (*failingReadBody) Close() error                  { return nil }

const validStructuredResponse = `{"summary":"summary","likely_causes":[],"recommendations":[],"evidence_used":[],"missing_evidence":[],"confidence":"low"}`
