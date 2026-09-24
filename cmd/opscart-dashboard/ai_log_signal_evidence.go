package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

// logSignalsEvidenceItem renders only fixed, actionable categories and
// counts. The stable form buckets volatile counts for evidence hashing.
func logSignalsEvidenceItem(preview logSignalsPreview) (aianalysis.EvidenceItem, string) {
	fixed := fmt.Sprintf("source=%s; container_role=%s; lines_requested=%d; byte_limit=%d",
		preview.Source, preview.ContainerRole, preview.LinesRequested, preview.ByteLimit)
	exact := fixed + fmt.Sprintf("; bytes_received=%d; raw_lines_included=%d", preview.BytesReceived, preview.RawLinesIncluded)
	stable := fixed + fmt.Sprintf("; bytes_received_bucket=%s; raw_lines_included=%d",
		logSignalCountBucket(preview.BytesReceived), preview.RawLinesIncluded)
	for _, signal := range preview.Signals {
		if !logSignalIsActionable(signal.Category) {
			continue
		}
		exact += fmt.Sprintf("; %s=%d", signal.Category, signal.Count)
		stable += fmt.Sprintf("; %s=%s", signal.Category, logSignalCountBucket(signal.Count))
	}
	return aianalysis.EvidenceItem{
		Type:    aianalysis.EvidenceLogSignals,
		Summary: "Locally derived diagnostic signals (previous container logs)",
		Details: exact,
	}, stable
}

// appendLogSignalsEvidence extends sanitized base evidence without reading logs.
func appendLogSignalsEvidence(capture warRoomAICapture, preview logSignalsPreview) (warRoomAICapture, error) {
	item, stableDetails := logSignalsEvidenceItem(preview)
	request := capture.Request
	request.Evidence = append(cloneEvidenceItems(capture.Request.Evidence), item)
	if _, err := validateAndEncodeWarRoomAIRequest(request); err != nil {
		return warRoomAICapture{}, err
	}
	stableItem := item
	stableItem.Details = stableDetails
	stableRequest := request
	stableRequest.Evidence = append(cloneEvidenceItems(capture.StableEvidence), stableItem)
	stableEncoded, err := validateAndEncodeWarRoomAIRequest(stableRequest)
	if err != nil {
		return warRoomAICapture{}, err
	}
	digest := sha256.Sum256(stableEncoded)
	refined := capture
	refined.Request = request
	refined.Hash = hex.EncodeToString(digest[:])
	refined.StableEvidence = stableRequest.Evidence
	return refined, nil
}
