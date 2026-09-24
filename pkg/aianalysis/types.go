// Package aianalysis defines the provider-neutral contract for AI-assisted
// operational analysis.
package aianalysis

import (
	"context"
	"fmt"
	"time"
)

// AIProvider analyzes explicitly supplied operational evidence.
type AIProvider interface {
	Analyze(ctx context.Context, req AnalysisRequest) (*AnalysisResponse, error)
}

// EvidenceType identifies the controlled category of an evidence item.
type EvidenceType string

const (
	EvidenceObservation   EvidenceType = "observation"
	EvidenceMetric        EvidenceType = "metric"
	EvidenceEvent         EvidenceType = "event"
	EvidenceConfiguration EvidenceType = "configuration"
	EvidenceLifecycle     EvidenceType = "lifecycle"
	// EvidenceLogSignals identifies a controlled, locally derived diagnostic
	// signal item: fixed category names and integer counts classified from
	// bounded previous-container logs. It never carries raw log lines,
	// excerpts, stack traces, or any other log-derived free-form string —
	// see cmd/opscart-dashboard/ai_log_signals.go, the sole producer.
	EvidenceLogSignals EvidenceType = "log_signals"
)

// EvidenceItem is the sanitization boundary for provider transmission.
// Callers must explicitly construct concise, sanitized evidence; raw Kubernetes
// objects, logs, Secret values, tokens, and environment variables are not part
// of this contract and must never be copied into these fields.
type EvidenceItem struct {
	Type    EvidenceType `json:"type"`
	Summary string       `json:"summary"`
	Details string       `json:"details"`
}

// AnalysisRequest contains only the issue identity and evidence eligible for
// transmission to an AI provider.
type AnalysisRequest struct {
	Cluster       string         `json:"cluster"`
	Namespace     string         `json:"namespace"`
	ResourceKind  string         `json:"resource_kind"`
	ResourceName  string         `json:"resource_name"`
	IssueType     string         `json:"issue_type"`
	Severity      string         `json:"severity"`
	FirstDetected time.Time      `json:"first_detected"`
	ReopenCount   int            `json:"reopen_count"`
	Evidence      []EvidenceItem `json:"evidence"`
}

// Confidence is the provider's confidence in its evidence-bound analysis.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// LikelyCause distinguishes a hypothesis from the evidence supporting it.
type LikelyCause struct {
	Title     string `json:"title"`
	Rationale string `json:"rationale"`
}

// Recommendation is a read-only operator action with its rationale.
type Recommendation struct {
	Action    string `json:"action"`
	Rationale string `json:"rationale"`
}

// AnalysisResponse is independent of any provider wire format or UI rendering.
type AnalysisResponse struct {
	Summary         string           `json:"summary"`
	LikelyCauses    []LikelyCause    `json:"likely_causes"`
	Recommendations []Recommendation `json:"recommendations"`
	EvidenceUsed    []string         `json:"evidence_used"`
	MissingEvidence []string         `json:"missing_evidence"`
	Confidence      Confidence       `json:"confidence"`
}

func validateRequest(req AnalysisRequest) error {
	if req.Cluster == "" {
		return fmt.Errorf("cluster is required")
	}
	if req.IssueType == "" {
		return fmt.Errorf("issue type is required")
	}
	if req.ReopenCount < 0 {
		return fmt.Errorf("reopen count cannot be negative")
	}
	for i, item := range req.Evidence {
		if !validEvidenceType(item.Type) {
			return fmt.Errorf("evidence item %d has unsupported type", i)
		}
		if item.Summary == "" {
			return fmt.Errorf("evidence item %d summary is required", i)
		}
	}
	return nil
}

func validEvidenceType(value EvidenceType) bool {
	switch value {
	case EvidenceObservation, EvidenceMetric, EvidenceEvent, EvidenceConfiguration, EvidenceLifecycle, EvidenceLogSignals:
		return true
	default:
		return false
	}
}
