package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewAzureAPIErrorNeverIncludesResponseBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Ms-Request-Id": []string{"req-123"}},
	}
	err := newAzureAPIError("querying resource group \"rg1\"", resp)
	msg := err.Error()
	if !strings.Contains(msg, "req-123") {
		t.Errorf("error %q missing request ID", msg)
	}
	if !strings.Contains(msg, "authorization failed") {
		t.Errorf("error %q missing safe status label", msg)
	}
	if !strings.Contains(msg, "querying resource group") {
		t.Errorf("error %q missing operation", msg)
	}
	if err.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d", err.StatusCode)
	}
}

func TestAzureRequestIDPrefersXMsRequestID(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"X-Ms-Request-Id":        []string{"server-id"},
		"X-Ms-Client-Request-Id": []string{"client-id"},
	}}
	if got := azureRequestID(resp); got != "server-id" {
		t.Errorf("azureRequestID = %q, want server-id", got)
	}
}

func TestAzureRequestIDFallsBackToClientRequestID(t *testing.T) {
	resp := &http.Response{Header: http.Header{"X-Ms-Client-Request-Id": []string{"client-id"}}}
	if got := azureRequestID(resp); got != "client-id" {
		t.Errorf("azureRequestID = %q, want client-id", got)
	}
}

func TestAzureRequestIDUnknownWhenAbsent(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := azureRequestID(resp); got != "unknown" {
		t.Errorf("azureRequestID = %q, want unknown", got)
	}
}

func TestSafeAzureStatusLabels(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:        "authentication failed",
		http.StatusForbidden:           "authorization failed",
		http.StatusNotFound:            "resource not found",
		http.StatusTooManyRequests:     "throttled",
		http.StatusInternalServerError: "Azure service error",
		http.StatusBadRequest:          "request rejected",
	}
	for code, want := range cases {
		if got := safeAzureStatusLabel(code); got != want {
			t.Errorf("safeAzureStatusLabel(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestQueryErrorNeverLeaksResponseBodyContent(t *testing.T) {
	// End-to-end: the query client must sanitize even a real HTTP response
	// carrying a marker that must never surface in the returned error.
	const secretMarker = "internal-diagnostic-detail-should-never-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-request-id", "test-request-id")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"` + secretMarker + `"}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Errorf("error leaked raw response body content: %v", err)
	}
	var apiErr *AzureAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *AzureAPIError", err)
	}
	if apiErr.RequestID != "test-request-id" {
		t.Errorf("RequestID = %q, want test-request-id", apiErr.RequestID)
	}
}
