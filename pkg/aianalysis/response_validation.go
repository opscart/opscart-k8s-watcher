package aianalysis

import (
	"errors"
	"strings"
)

const (
	maxSummaryBytes                 = 1024
	maxLikelyCauses                 = 5
	maxCauseTitleBytes              = 128
	maxCauseRationaleBytes          = 512
	maxRecommendations              = 5
	maxRecommendationActionBytes    = 256
	maxRecommendationRationaleBytes = 512
	maxEvidenceUsedItems            = 10
	maxMissingEvidenceItems         = 10
	maxEvidenceReferenceBytes       = 256
)

// ErrInvalidResponse is returned without provider-controlled details when an
// analysis response does not satisfy the bounded provider-neutral contract.
var ErrInvalidResponse = errors.New("AI analysis response is invalid")

// ValidateResponse enforces the response contract shared by every provider
// and caller before model output is rendered or cached. Limits are UTF-8 byte
// limits so the in-memory POC cache has a deterministic size bound.
func ValidateResponse(response *AnalysisResponse) error {
	if response == nil || !validResponseText(response.Summary, maxSummaryBytes) {
		return ErrInvalidResponse
	}
	if response.LikelyCauses == nil || len(response.LikelyCauses) > maxLikelyCauses {
		return ErrInvalidResponse
	}
	for _, cause := range response.LikelyCauses {
		if !validResponseText(cause.Title, maxCauseTitleBytes) ||
			!validResponseText(cause.Rationale, maxCauseRationaleBytes) {
			return ErrInvalidResponse
		}
	}
	if response.Recommendations == nil || len(response.Recommendations) > maxRecommendations {
		return ErrInvalidResponse
	}
	for _, recommendation := range response.Recommendations {
		if !validResponseText(recommendation.Action, maxRecommendationActionBytes) ||
			!validResponseText(recommendation.Rationale, maxRecommendationRationaleBytes) {
			return ErrInvalidResponse
		}
	}
	if !validResponseStrings(response.EvidenceUsed, maxEvidenceUsedItems) ||
		!validResponseStrings(response.MissingEvidence, maxMissingEvidenceItems) {
		return ErrInvalidResponse
	}
	switch response.Confidence {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
		return nil
	default:
		return ErrInvalidResponse
	}
}

func validResponseStrings(values []string, maxItems int) bool {
	if values == nil || len(values) > maxItems {
		return false
	}
	for _, value := range values {
		if !validResponseText(value, maxEvidenceReferenceBytes) {
			return false
		}
	}
	return true
}

func validResponseText(value string, maxBytes int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maxBytes
}
