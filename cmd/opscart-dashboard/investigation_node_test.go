package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

type nodeInvestigationStore struct {
	store.Store
	records   map[string]*store.IncidentRecord
	timelines map[string][]store.IncidentEvent
	summaries []store.IncidentSummary
}

func (s nodeInvestigationStore) QueryIncidents(filter store.IncidentFilter) ([]store.IncidentSummary, int, error) {
	var matches []store.IncidentSummary
	for _, summary := range s.summaries {
		if filter.Status == "" || summary.Status == filter.Status {
			matches = append(matches, summary)
		}
	}
	return matches, len(matches), nil
}

func (s nodeInvestigationStore) GetIncidentHistory(_ string, fingerprint string) (*store.IncidentRecord, error) {
	return s.records[fingerprint], nil
}

func (s nodeInvestigationStore) GetIncidentTimeline(_ string, fingerprint string) ([]store.IncidentEvent, error) {
	return s.timelines[fingerprint], nil
}

func nodeInvestigationDetailsJSON(t *testing.T, node, pool, condition, status string, workloads []models.CorrelatedWorkload) string {
	t.Helper()
	details, err := json.Marshal(nodeInvestigationDetails{
		NodeName: node, NodePool: pool, ConditionType: condition, ConditionStatus: status,
		Reason: "KubeletHasDiskPressure", Message: "kubelet reports disk pressure",
		LastTransitionTime:  time.Date(2026, time.August, 16, 10, 30, 0, 0, time.UTC),
		CorrelatedWorkloads: workloads,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(details)
}

func newNodeInvestigationServer(t *testing.T, status string, current []models.NodeConditionFinding, events []store.IncidentEvent) *server {
	t.Helper()
	const fingerprint = "cluster/Node/worker-21/DiskPressure"
	workloads := []models.CorrelatedWorkload{
		{Namespace: "inventory", Kind: "StatefulSet", Name: "db", PodCount: 2},
		{Namespace: "payments", Kind: "Deployment", Name: "api", PodCount: 3},
	}
	db := nodeInvestigationStore{
		records: map[string]*store.IncidentRecord{fingerprint: {
			Fingerprint: fingerprint, Status: status, FirstSeen: time.Now().Add(-24 * time.Hour),
			DetailsJSON: nodeInvestigationDetailsJSON(t, "worker-21", "userpool", "DiskPressure", "True", workloads),
		}},
		timelines: map[string][]store.IncidentEvent{fingerprint: events},
	}
	srv := newServer([]string{"prod"}, db, 0, true)
	srv.getState("prod").scan = &clusterScan{nodeHealth: current}
	return srv
}

func requestNodeInvestigation(t *testing.T, srv *server, condition string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/investigate?node=worker-21&type="+condition+"&cluster=prod&from=warroom", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("node investigation status = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestActiveNodeInvestigationRendersIdentityObservationAndPlacement(t *testing.T) {
	workloads := []models.CorrelatedWorkload{
		{Namespace: "inventory", Kind: "StatefulSet", Name: "db", PodCount: 2},
		{Namespace: "payments", Kind: "Deployment", Name: "api", PodCount: 3},
	}
	current := []models.NodeConditionFinding{{
		NodeName: "worker-21", NodePool: "userpool", ConditionType: "DiskPressure", ConditionStatus: "True",
		Reason: "KubeletHasDiskPressure", Message: "kubelet reports disk pressure",
		LastTransitionTime:  time.Date(2026, time.August, 16, 10, 30, 0, 0, time.UTC),
		CorrelatedWorkloads: workloads,
	}}
	srv := newNodeInvestigationServer(t, "active", current, []store.IncidentEvent{{
		OccurredAt: time.Now(), EventType: "DETECTED", Message: "DiskPressure first detected",
	}})
	body := requestNodeInvestigation(t, srv, "DiskPressure")
	for _, want := range []string{
		"Node Investigation", "Entity type", "Node name", "worker-21", "Node pool", "userpool",
		"Condition type", "DiskPressure", "Incident status", "active",
		"Current Observation", "DiskPressure · True", "KubeletHasDiskPressure",
		"kubelet reports disk pressure", "Last transition", "Aug 16, 2026",
		"2 workload groups · 5 pods", "inventory", "StatefulSet", "db", "payments", "Deployment", "api",
		"Correlated by node placement — not a claim of causation.", "DETECTED",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("active Node Investigation missing %q", want)
		}
	}
	for _, forbidden := range []string{"affected", "impacted", "caused by", "Focus Pod", "Container Logs"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("active Node Investigation contains forbidden workload/causal text %q", forbidden)
		}
	}
	for _, unsafe := range []string{"kubectl cordon", "kubectl drain", "kubectl delete", "kubectl rollout restart"} {
		if strings.Contains(body, unsafe) {
			t.Errorf("Node Investigation contains mutating command %q", unsafe)
		}
	}
	for _, command := range []string{"kubectl describe node worker-21", "kubectl get pods -A --field-selector spec.nodeName=worker-21 -o wide"} {
		if !strings.Contains(body, command) {
			t.Errorf("Node Investigation missing read-only command %q", command)
		}
	}
}

func TestResolvedNodeInvestigationUsesRetainedEvidenceAndTimeline(t *testing.T) {
	events := []store.IncidentEvent{
		{OccurredAt: time.Now().Add(-time.Hour), EventType: "DETECTED", Message: "DiskPressure first detected"},
		{OccurredAt: time.Now(), EventType: "RESOLVED", Message: "DiskPressure resolved"},
	}
	body := requestNodeInvestigation(t, newNodeInvestigationServer(t, "resolved", nil, events), "DiskPressure")
	for _, want := range []string{
		"Incident status", "resolved", "Last Retained Observation",
		"No active Kubernetes condition is currently reported for this node.",
		"Last reported reason", "KubeletHasDiskPressure", "Last reported message",
		"Last retained placement snapshot", "2 workload groups · 5 pods", "DETECTED", "RESOLVED",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("resolved Node Investigation missing %q", want)
		}
	}
	if strings.Contains(body, "Current placement snapshot") || strings.Contains(body, "0 workload") {
		t.Errorf("resolved Node Investigation fabricated current zero placement: %s", body)
	}
}

func TestNodeInvestigationSupportsReopenedAndConditionSpecificIdentity(t *testing.T) {
	srv := newNodeInvestigationServer(t, "active", nil, []store.IncidentEvent{{EventType: "REOPENED", Message: "DiskPressure reopened"}})
	const memoryFP = "cluster/Node/worker-21/MemoryPressure"
	db := srv.db.(nodeInvestigationStore)
	db.records[memoryFP] = &store.IncidentRecord{Fingerprint: memoryFP, Status: "resolved", DetailsJSON: `{"node_name":"worker-21","condition_type":"MemoryPressure","condition_status":"True"}`}
	db.timelines[memoryFP] = []store.IncidentEvent{{EventType: "RESOLVED", Message: "MemoryPressure resolved"}}
	srv.db = db

	diskBody := requestNodeInvestigation(t, srv, "DiskPressure")
	memoryBody := requestNodeInvestigation(t, srv, "MemoryPressure")
	if !strings.Contains(diskBody, "REOPENED") || strings.Contains(diskBody, "MemoryPressure resolved") {
		t.Fatalf("DiskPressure context did not retain its own reopened timeline")
	}
	if !strings.Contains(memoryBody, "MemoryPressure") || !strings.Contains(memoryBody, "RESOLVED") || strings.Contains(memoryBody, "DiskPressure reopened") {
		t.Fatalf("MemoryPressure context did not retain its own resolved timeline")
	}
}

func TestNodeWarRoomLinkTargetsValidInvestigationRoute(t *testing.T) {
	issue := warRoomIssue{IsNode: true, Resource: "worker-21", Type: "DiskPressure"}
	link := warRoomIssueURL(issue, "prod")
	for _, want := range []string{"/investigate?", "node=worker-21", "type=DiskPressure", "cluster=prod"} {
		if !strings.Contains(link, want) {
			t.Fatalf("Node War Room link %q missing %q", link, want)
		}
	}
	workload := warRoomIssueURL(warRoomIssue{Namespace: "payments", Resource: "api-pod", Type: "crash_loop"}, "prod")
	wantWorkload := investigateURL("payments", "api-pod", "crash_loop", "prod")
	if workload != wantWorkload {
		t.Fatalf("workload Investigation route changed: got %q want %q", workload, wantWorkload)
	}
	srv := newNodeInvestigationServer(t, "active", nil, nil)
	req := httptest.NewRequest(http.MethodGet, link, nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "worker-21") || !strings.Contains(rec.Body.String(), "DiskPressure") {
		t.Fatalf("generated War Room Node link did not open its Investigation: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestIncidentRegistryNodeLinkTargetsNodeInvestigation(t *testing.T) {
	rec := httptest.NewRecorder()
	renderIncidents(rec, incidentsPageData{
		ClusterParam: "prod",
		Incidents: []store.IncidentSummary{{
			Fingerprint: "cluster/Node/worker-21/DiskPressure", Resource: "worker-21",
			IssueType: "DiskPressure", Severity: "low", Status: "active",
		}},
		Page: 1, PerPage: 50, TotalPages: 1, Total: 1,
	})
	body := rec.Body.String()
	for _, want := range []string{"Node/worker-21", "node=worker-21", "type=DiskPressure", "from=incidents"} {
		if !strings.Contains(body, want) {
			t.Errorf("incident registry Node row missing %q", want)
		}
	}
	if strings.Contains(body, "pod=worker-21") {
		t.Errorf("incident registry treated Node as a Pod")
	}
	marker := "window.location='"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("incident registry row has no navigation target")
	}
	start += len(marker)
	end := strings.Index(body[start:], "'")
	link := html.UnescapeString(body[start : start+end])
	srv := newNodeInvestigationServer(t, "active", nil, nil)
	req := httptest.NewRequest(http.MethodGet, link, nil)
	page := httptest.NewRecorder()
	srv.handleInvestigationPage(page, req)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "worker-21") || !strings.Contains(page.Body.String(), "DiskPressure") {
		t.Fatalf("generated registry Node link did not open its Investigation: status=%d body=%s", page.Code, page.Body.String())
	}
}

func TestMultipleNodesRemainDistinctAcrossWarRoomAndInvestigationRoutes(t *testing.T) {
	const condition = "DiskPressure"
	db := nodeInvestigationStore{
		records: map[string]*store.IncidentRecord{}, timelines: map[string][]store.IncidentEvent{},
	}
	var findings []models.NodeConditionFinding
	for _, node := range []string{"worker-a", "worker-b"} {
		fingerprint := "cluster/Node/" + node + "/" + condition
		details := nodeInvestigationDetailsJSON(t, node, "pool-a", condition, "True", nil)
		db.records[fingerprint] = &store.IncidentRecord{Fingerprint: fingerprint, Status: "active", DetailsJSON: details}
		db.summaries = append(db.summaries, store.IncidentSummary{
			Fingerprint: fingerprint, Resource: node, IssueType: condition, Status: "active", Severity: "low",
		})
		findings = append(findings, models.NodeConditionFinding{NodeName: node, NodePool: "pool-a", ConditionType: condition, ConditionStatus: "True"})
	}
	scan := &clusterScan{nodeHealth: findings}
	issues := collectActiveNodeWarRoomIssues(scan, db, "prod")
	if len(issues) != 2 || issues[0].Resource != "worker-a" || issues[1].Resource != "worker-b" {
		t.Fatalf("multi-node War Room representation merged or reordered Nodes: %+v", issues)
	}
	srv := newServer([]string{"prod"}, db, 0, true)
	srv.getState("prod").scan = scan
	for _, issue := range issues {
		link := warRoomIssueURL(issue, "prod")
		req := httptest.NewRequest(http.MethodGet, link, nil)
		rec := httptest.NewRecorder()
		srv.handleInvestigationPage(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), issue.Resource) || !strings.Contains(rec.Body.String(), condition) {
			t.Errorf("%s route failed or opened wrong context: status=%d", issue.Resource, rec.Code)
		}
	}
}
