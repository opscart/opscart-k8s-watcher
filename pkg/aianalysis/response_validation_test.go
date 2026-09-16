package aianalysis

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateResponseAcceptsBoundedResponseAndEmptyArrays(t *testing.T) {
	response := validAnalysisResponse()
	response.Summary = strings.Repeat("s", maxSummaryBytes)
	response.LikelyCauses[0].Title = strings.Repeat("t", maxCauseTitleBytes)
	response.LikelyCauses[0].Rationale = strings.Repeat("r", maxCauseRationaleBytes)
	response.Recommendations[0].Action = strings.Repeat("a", maxRecommendationActionBytes)
	response.Recommendations[0].Rationale = strings.Repeat("r", maxRecommendationRationaleBytes)
	response.EvidenceUsed[0] = strings.Repeat("e", maxEvidenceReferenceBytes)
	response.MissingEvidence[0] = strings.Repeat("m", maxEvidenceReferenceBytes)
	if err := ValidateResponse(response); err != nil {
		t.Fatalf("ValidateResponse() error = %v", err)
	}

	response.LikelyCauses = []LikelyCause{}
	response.Recommendations = []Recommendation{}
	response.EvidenceUsed = []string{}
	response.MissingEvidence = []string{}
	if err := ValidateResponse(response); err != nil {
		t.Fatalf("ValidateResponse() rejected legitimate empty arrays: %v", err)
	}
}

func TestValidateResponseRejectsInvalidOrOversizedContentSafely(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AnalysisResponse)
	}{
		{name: "zero response", mutate: func(response *AnalysisResponse) { *response = AnalysisResponse{} }},
		{name: "blank summary", mutate: func(response *AnalysisResponse) { response.Summary = " \t\n" }},
		{name: "long summary", mutate: func(response *AnalysisResponse) { response.Summary = strings.Repeat("s", maxSummaryBytes+1) }},
		{name: "nil causes", mutate: func(response *AnalysisResponse) { response.LikelyCauses = nil }},
		{name: "too many causes", mutate: func(response *AnalysisResponse) {
			response.LikelyCauses = make([]LikelyCause, maxLikelyCauses+1)
			for i := range response.LikelyCauses {
				response.LikelyCauses[i] = LikelyCause{Title: "cause", Rationale: "rationale"}
			}
		}},
		{name: "blank cause title", mutate: func(response *AnalysisResponse) { response.LikelyCauses[0].Title = " " }},
		{name: "long cause title", mutate: func(response *AnalysisResponse) {
			response.LikelyCauses[0].Title = strings.Repeat("t", maxCauseTitleBytes+1)
		}},
		{name: "blank cause rationale", mutate: func(response *AnalysisResponse) { response.LikelyCauses[0].Rationale = " " }},
		{name: "long cause rationale", mutate: func(response *AnalysisResponse) {
			response.LikelyCauses[0].Rationale = strings.Repeat("r", maxCauseRationaleBytes+1)
		}},
		{name: "nil recommendations", mutate: func(response *AnalysisResponse) { response.Recommendations = nil }},
		{name: "too many recommendations", mutate: func(response *AnalysisResponse) {
			response.Recommendations = make([]Recommendation, maxRecommendations+1)
			for i := range response.Recommendations {
				response.Recommendations[i] = Recommendation{Action: "inspect", Rationale: "rationale"}
			}
		}},
		{name: "blank recommendation action", mutate: func(response *AnalysisResponse) { response.Recommendations[0].Action = " " }},
		{name: "long recommendation action", mutate: func(response *AnalysisResponse) {
			response.Recommendations[0].Action = strings.Repeat("a", maxRecommendationActionBytes+1)
		}},
		{name: "blank recommendation rationale", mutate: func(response *AnalysisResponse) { response.Recommendations[0].Rationale = " " }},
		{name: "long recommendation rationale", mutate: func(response *AnalysisResponse) {
			response.Recommendations[0].Rationale = strings.Repeat("r", maxRecommendationRationaleBytes+1)
		}},
		{name: "nil evidence used", mutate: func(response *AnalysisResponse) { response.EvidenceUsed = nil }},
		{name: "too much evidence used", mutate: func(response *AnalysisResponse) {
			response.EvidenceUsed = make([]string, maxEvidenceUsedItems+1)
			for i := range response.EvidenceUsed {
				response.EvidenceUsed[i] = "evidence"
			}
		}},
		{name: "blank evidence used", mutate: func(response *AnalysisResponse) { response.EvidenceUsed[0] = " " }},
		{name: "long evidence used", mutate: func(response *AnalysisResponse) {
			response.EvidenceUsed[0] = strings.Repeat("e", maxEvidenceReferenceBytes+1)
		}},
		{name: "nil missing evidence", mutate: func(response *AnalysisResponse) { response.MissingEvidence = nil }},
		{name: "too much missing evidence", mutate: func(response *AnalysisResponse) {
			response.MissingEvidence = make([]string, maxMissingEvidenceItems+1)
			for i := range response.MissingEvidence {
				response.MissingEvidence[i] = "missing evidence"
			}
		}},
		{name: "blank missing evidence", mutate: func(response *AnalysisResponse) { response.MissingEvidence[0] = " " }},
		{name: "long missing evidence", mutate: func(response *AnalysisResponse) {
			response.MissingEvidence[0] = strings.Repeat("m", maxEvidenceReferenceBytes+1)
		}},
		{name: "invalid confidence", mutate: func(response *AnalysisResponse) { response.Confidence = Confidence("certain") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validAnalysisResponse()
			test.mutate(response)
			err := ValidateResponse(response)
			if !errors.Is(err, ErrInvalidResponse) || err.Error() != ErrInvalidResponse.Error() {
				t.Fatalf("ValidateResponse() error = %v, want stable invalid response error", err)
			}
		})
	}

	if err := ValidateResponse(nil); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("ValidateResponse(nil) error = %v, want ErrInvalidResponse", err)
	}
}

func validAnalysisResponse() *AnalysisResponse {
	return &AnalysisResponse{
		Summary: "Evidence-bound summary.",
		LikelyCauses: []LikelyCause{{
			Title:     "Likely cause",
			Rationale: "Supplied evidence supports this hypothesis.",
		}},
		Recommendations: []Recommendation{{
			Action:    "Inspect the workload configuration.",
			Rationale: "This is a read-only operator step.",
		}},
		EvidenceUsed:    []string{"restart count"},
		MissingEvidence: []string{"termination reason"},
		Confidence:      ConfidenceMedium,
	}
}
