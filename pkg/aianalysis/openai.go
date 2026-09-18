package aianalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxResponseBytes = 1 << 20
	// The Phase 2 POC response is a compact structured analysis. This fixed
	// ceiling bounds generation cost without adding configuration surface.
	openAIMaxOutputTokens = 4096

	// analysisInstructions is the model's system prompt. These instructions
	// are intended to reduce specific, observed overclaiming patterns in
	// model output (e.g. treating a container's age as its incident
	// duration); they do not, and cannot, guarantee the model's factual
	// correctness. Any change here (or to the response schema below) must
	// be accompanied by bumping warRoomAIContractVersion
	// (cmd/opscart-dashboard/warroom_ai_cache.go) so a cached result
	// produced under old instructions is never presented as current.
	analysisInstructions = `Analyze only the operational evidence supplied in the input.
Do not claim or imply direct access to the Kubernetes cluster, logs, credentials, or any other data source.

Write a short summary, ideally 2-3 sentences.
Prefer up to 3 distinct hypotheses in likely_causes and up to 3 prioritized read-only checks in recommendations. Do not pad either list to reach that number, and do not invent additional entries beyond what the evidence supports.
State each fact once. Do not restate the same evidence value in the summary, in more than one hypothesis, and again in a check.

Apply these evidence-reading rules:
- A resource's or container's age is not the duration of its current incident; they are different measurements.
- An observed current state (ready, waiting, running) reflects only the moment evidence was captured, not the resource's history before or after that moment.
- A last termination reason of Completed with exit code 0 shows only that the process was not killed or errored. It does not establish that the exit was unprompted, how long the container ran beforehand, or that nothing external (a supervisor, a liveness-probe kill, a preStop hook) caused it.
- A Kubernetes event's reason names what was observed, not why it happened. Do not treat an event reason alone as establishing a cause.
- Readiness (eligibility for Service traffic) and liveness/startup probe behavior are distinct signals; do not conflate a readiness observation with a liveness or startup finding, or vice versa.

Every entry in recommendations must be a read-only inspection, query, or observation step. Never recommend changing configuration, scaling, restarting a workload, or any other mutation, even framed as an example or a fallback.

Clearly distinguish observed facts from hypotheses. When the supplied evidence does not establish a cause, say so plainly in the summary or missing_evidence rather than proposing a hypothesis you cannot support.
Never claim that an action was executed or that cluster state was changed.`
)

type openAIProvider struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

type openAIRequest struct {
	Model           string           `json:"model"`
	Instructions    string           `json:"instructions"`
	Input           string           `json:"input"`
	MaxOutputTokens int              `json:"max_output_tokens"`
	Store           bool             `json:"store"`
	Text            openAITextConfig `json:"text"`
}

type openAITextConfig struct {
	Format openAIResponseFormat `json:"format"`
}

type openAIResponseFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type openAIResponse struct {
	Status string `json:"status"`
	Error  *struct {
		Code string `json:"code"`
	} `json:"error"`
	Output []struct {
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

type analysisResponseWire struct {
	Summary         *string                `json:"summary"`
	LikelyCauses    *[]*likelyCauseWire    `json:"likely_causes"`
	Recommendations *[]*recommendationWire `json:"recommendations"`
	EvidenceUsed    *[]*string             `json:"evidence_used"`
	MissingEvidence *[]*string             `json:"missing_evidence"`
	Confidence      *Confidence            `json:"confidence"`
}

type likelyCauseWire struct {
	Title     *string `json:"title"`
	Rationale *string `json:"rationale"`
}

type recommendationWire struct {
	Action    *string `json:"action"`
	Rationale *string `json:"rationale"`
}

var (
	errProviderRequest           = errors.New("AI provider request failed")
	errInvalidProviderResponse   = errors.New("AI provider returned an invalid response")
	errInvalidStructuredResponse = errors.New("AI provider returned invalid structured output")
)

func newOpenAIProvider(config Config, baseURL *url.URL) AIProvider {
	return &openAIProvider{
		endpoint: strings.TrimRight(baseURL.String(), "/") + "/responses",
		model:    strings.TrimSpace(config.Model),
		apiKey:   strings.TrimSpace(config.APIKey),
		client: &http.Client{
			Timeout: config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (provider *openAIProvider) Analyze(ctx context.Context, req AnalysisRequest) (*AnalysisResponse, error) {
	if err := validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid analysis request: %w", err)
	}

	input, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode analysis evidence: %w", err)
	}
	wireRequest := openAIRequest{
		Model:           provider.model,
		Instructions:    analysisInstructions,
		Input:           string(input),
		MaxOutputTokens: openAIMaxOutputTokens,
		Store:           false,
		Text: openAITextConfig{Format: openAIResponseFormat{
			Type:   "json_schema",
			Name:   "opscart_analysis",
			Schema: analysisSchema(),
			Strict: true,
		}},
	}
	payload, err := json.Marshal(wireRequest)
	if err != nil {
		return nil, fmt.Errorf("encode AI provider request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("create AI provider request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+provider.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")

	httpResponse, err := provider.client.Do(httpRequest)
	if err != nil {
		return nil, safeTransportError(err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("AI provider returned HTTP status %d", httpResponse.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, safeTransportError(err)
		}
		return nil, errInvalidProviderResponse
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("AI provider response exceeded size limit")
	}

	var wireResponse openAIResponse
	if err := json.Unmarshal(body, &wireResponse); err != nil {
		return nil, errInvalidProviderResponse
	}
	if wireResponse.Error != nil {
		return nil, fmt.Errorf("AI provider returned an error")
	}
	if wireResponse.Status != "completed" {
		return nil, fmt.Errorf("AI provider response was not completed")
	}

	structured, refused := responseText(wireResponse)
	if refused {
		return nil, fmt.Errorf("AI provider refused the analysis request")
	}
	if structured == "" {
		return nil, fmt.Errorf("AI provider response did not contain structured output")
	}

	response, err := decodeAnalysisResponse(structured)
	if err != nil {
		return nil, errInvalidStructuredResponse
	}
	return response, nil
}

func safeTransportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("AI provider request canceled: %w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("AI provider request timed out: %w", context.DeadlineExceeded)
	default:
		return errProviderRequest
	}
}

func responseText(response openAIResponse) (string, bool) {
	for _, output := range response.Output {
		for _, content := range output.Content {
			switch content.Type {
			case "output_text":
				if content.Text != "" {
					return content.Text, false
				}
			case "refusal":
				return "", true
			}
		}
	}
	return "", false
}

func decodeAnalysisResponse(value string) (*AnalysisResponse, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var wire analysisResponseWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, errInvalidStructuredResponse
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errInvalidStructuredResponse
	}
	if wire.Summary == nil || *wire.Summary == "" || wire.LikelyCauses == nil || wire.Recommendations == nil || wire.EvidenceUsed == nil || wire.MissingEvidence == nil || wire.Confidence == nil {
		return nil, errInvalidStructuredResponse
	}
	response := &AnalysisResponse{
		Summary:         *wire.Summary,
		LikelyCauses:    make([]LikelyCause, len(*wire.LikelyCauses)),
		Recommendations: make([]Recommendation, len(*wire.Recommendations)),
		EvidenceUsed:    make([]string, len(*wire.EvidenceUsed)),
		MissingEvidence: make([]string, len(*wire.MissingEvidence)),
		Confidence:      *wire.Confidence,
	}
	for i, cause := range *wire.LikelyCauses {
		if cause == nil || cause.Title == nil || cause.Rationale == nil {
			return nil, errInvalidStructuredResponse
		}
		response.LikelyCauses[i] = LikelyCause{Title: *cause.Title, Rationale: *cause.Rationale}
	}
	for i, recommendation := range *wire.Recommendations {
		if recommendation == nil || recommendation.Action == nil || recommendation.Rationale == nil {
			return nil, errInvalidStructuredResponse
		}
		response.Recommendations[i] = Recommendation{Action: *recommendation.Action, Rationale: *recommendation.Rationale}
	}
	for i, evidence := range *wire.EvidenceUsed {
		if evidence == nil {
			return nil, errInvalidStructuredResponse
		}
		response.EvidenceUsed[i] = *evidence
	}
	for i, evidence := range *wire.MissingEvidence {
		if evidence == nil {
			return nil, errInvalidStructuredResponse
		}
		response.MissingEvidence[i] = *evidence
	}
	if err := ValidateResponse(response); err != nil {
		return nil, errInvalidStructuredResponse
	}
	return response, nil
}

func analysisSchema() map[string]any {
	stringArray := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"likely_causes": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"title":     map[string]any{"type": "string"},
						"rationale": map[string]any{"type": "string"},
					},
					"required": []string{"title", "rationale"},
				},
			},
			"recommendations": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"action":    map[string]any{"type": "string"},
						"rationale": map[string]any{"type": "string"},
					},
					"required": []string{"action", "rationale"},
				},
			},
			"evidence_used":    stringArray,
			"missing_evidence": stringArray,
			"confidence": map[string]any{
				"type": "string",
				"enum": []string{string(ConfidenceLow), string(ConfidenceMedium), string(ConfidenceHigh)},
			},
		},
		"required": []string{"summary", "likely_causes", "recommendations", "evidence_used", "missing_evidence", "confidence"},
	}
}
