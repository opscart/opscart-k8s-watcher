package aianalysis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenAIPromptPreservesEventAndLogSignalProvenance(t *testing.T) {
	request := syntheticRequest()
	request.Evidence = []EvidenceItem{
		{Type: EvidenceEvent, Summary: "Kubernetes Warning event", Details: "reason=BackOff"},
		{Type: EvidenceLogSignals, Summary: "Locally derived diagnostic signals (previous container logs)", Details: "connection_refused=2"},
	}

	provider := mustProvider(t, "https://provider.test/v1", time.Second).(*openAIProvider)
	provider.client.Transport = roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		var wire openAIRequest
		if err := json.NewDecoder(httpRequest.Body).Decode(&wire); err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			"title/summary and type as authoritative provenance boundaries",
			"Kubernetes event evidence",
			"locally derived diagnostic signals from previous-container logs",
			"Keep these sources explicitly distinct",
			"Raw logs were never supplied to or analyzed by the AI provider",
			"operator-side review of previous-container logs or additional locally derived diagnostic signals",
			"never recommend sending raw logs to the AI provider",
		} {
			if !strings.Contains(wire.Instructions, required) {
				t.Errorf("provider prompt missing provenance rule %q", required)
			}
		}
		var supplied AnalysisRequest
		if err := json.Unmarshal([]byte(wire.Input), &supplied); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(supplied.Evidence, request.Evidence) {
			t.Errorf("evidence provenance changed: got %#v", supplied.Evidence)
		}
		encoded, err := json.Marshal(map[string]any{
			"status": "completed",
			"output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": validStructuredResponse}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	})
	if _, err := provider.Analyze(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}
