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
	maxResponseBytes     = 1 << 20
	analysisInstructions = `Analyze only the operational evidence supplied in the input.
Do not claim or imply direct access to the Kubernetes cluster, logs, credentials, or any other data source.
Clearly distinguish observed facts from hypotheses. Identify material evidence that is missing.
Recommend only read-only investigation steps or actions for a human operator to evaluate.
Never claim that an action was executed or that cluster state was changed.`
)

type openAIProvider struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

type openAIRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions"`
	Input        string           `json:"input"`
	Store        bool             `json:"store"`
	Text         openAITextConfig `json:"text"`
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
		Model:        provider.model,
		Instructions: analysisInstructions,
		Input:        string(input),
		Store:        false,
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
	switch *wire.Confidence {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
	default:
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
