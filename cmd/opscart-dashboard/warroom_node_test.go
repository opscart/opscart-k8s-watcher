package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

type nodeWarRoomStore struct {
	store.Store
	items []store.IncidentSummary
}

func (s nodeWarRoomStore) QueryIncidents(filter store.IncidentFilter) ([]store.IncidentSummary, int, error) {
	var items []store.IncidentSummary
	for _, item := range s.items {
		if filter.Status == "" || item.Status == filter.Status {
			items = append(items, item)
		}
	}
	return items, len(items), nil
}

func activeNodeIncident(node, condition string) store.IncidentSummary {
	return store.IncidentSummary{
		Fingerprint: "cluster/Node/" + node + "/" + condition,
		Resource:    node, IssueType: condition, Severity: "low", Status: "active",
		FirstSeen: time.Now().Add(-time.Hour),
	}
}

func nodeFinding(node, condition, status string, workloads ...models.CorrelatedWorkload) models.NodeConditionFinding {
	return models.NodeConditionFinding{
		NodeName: node, NodePool: "pool-a", ConditionType: condition,
		ConditionStatus: status, CorrelatedWorkloads: workloads,
	}
}

func TestNodeWarRoomSeverityPolicyAndEligibility(t *testing.T) {
	findings := []models.NodeConditionFinding{
		nodeFinding("ready-false", "Ready", "False"),
		nodeFinding("ready-unknown", "Ready", "Unknown"),
		nodeFinding("disk", "DiskPressure", "True"),
		nodeFinding("memory", "MemoryPressure", "True"),
		nodeFinding("pid", "PIDPressure", "True"),
		nodeFinding("network", "NetworkUnavailable", "True"),
	}
	items := make([]store.IncidentSummary, 0, len(findings))
	for _, finding := range findings {
		items = append(items, activeNodeIncident(finding.NodeName, finding.ConditionType))
	}
	issues := collectActiveNodeWarRoomIssues(&clusterScan{nodeHealth: findings}, nodeWarRoomStore{items: items}, "prod")
	if len(issues) != len(findings) {
		t.Fatalf("got %d node issues, want %d", len(issues), len(findings))
	}
	for _, issue := range issues {
		want := "high"
		if issue.Type == "Ready" {
			want = "critical"
		}
		if issue.Severity != want {
			t.Errorf("%s severity = %q, want %q", issue.Type, issue.Severity, want)
		}
		if issue.Severity == "low" {
			t.Errorf("%s used persisted placeholder severity", issue.Type)
		}
		if issue.Type == "Ready" && issue.ConditionStatus == "" {
			t.Errorf("Ready issue for %s lost its False/Unknown status", issue.Resource)
		}
	}
}

func TestNodeWorkloadCountDoesNotChangeSeverity(t *testing.T) {
	one := nodeFinding("node-one", "DiskPressure", "True", models.CorrelatedWorkload{Namespace: "a", Name: "api", PodCount: 1})
	many := nodeFinding("node-many", "DiskPressure", "True")
	for i := 0; i < 20; i++ {
		many.CorrelatedWorkloads = append(many.CorrelatedWorkloads, models.CorrelatedWorkload{Namespace: "n", Name: string(rune('a' + i)), PodCount: 1})
	}
	store := nodeWarRoomStore{items: []store.IncidentSummary{
		activeNodeIncident("node-one", "DiskPressure"), activeNodeIncident("node-many", "DiskPressure"),
	}}
	issues := collectActiveNodeWarRoomIssues(&clusterScan{nodeHealth: []models.NodeConditionFinding{one, many}}, store, "prod")
	for _, issue := range issues {
		if issue.Severity != "high" {
			t.Fatalf("%s severity = %q, want high", issue.Resource, issue.Severity)
		}
	}
}

func TestNodeWarRoomRankingUsesExistingSeverityAndTieBreaks(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{{Name: "api-pod", Namespace: "app", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 7}}},
		nodeHealth: []models.NodeConditionFinding{
			nodeFinding("node-ready", "Ready", "False"), nodeFinding("node-disk", "DiskPressure", "True"),
		},
	}
	db := nodeWarRoomStore{items: []store.IncidentSummary{
		activeNodeIncident("node-ready", "Ready"), activeNodeIncident("node-disk", "DiskPressure"),
	}}
	issues := collectWarRoomIssuesWithStore(scan, 0, db, "prod")
	if got := []string{issues[0].Resource, issues[1].Resource, issues[2].Resource}; !reflect.DeepEqual(got, []string{"api-pod", "node-ready", "node-disk"}) {
		t.Fatalf("ranking = %v, want critical workload, critical Ready, high DiskPressure", got)
	}
	if issues[0].Severity != "critical" || issues[1].Severity != "critical" || issues[2].Severity != "high" {
		t.Fatalf("unexpected severity ordering: %#v", issues)
	}
}

func TestResolvedNodeExcludedAndConditionsRemainIndependent(t *testing.T) {
	resolved := activeNodeIncident("node-a", "PIDPressure")
	resolved.Status = "resolved"
	db := nodeWarRoomStore{items: []store.IncidentSummary{
		activeNodeIncident("node-a", "DiskPressure"), activeNodeIncident("node-a", "MemoryPressure"), resolved,
	}}
	issues := collectActiveNodeWarRoomIssues(&clusterScan{}, db, "prod")
	if len(issues) != 2 || issues[0].Type != "DiskPressure" || issues[1].Type != "MemoryPressure" {
		t.Fatalf("node conditions = %#v, want independent active DiskPressure and MemoryPressure", issues)
	}
}

func TestNodeWarRoomCompactContextAndInvestigationLink(t *testing.T) {
	finding := nodeFinding("worker-21", "DiskPressure", "True",
		models.CorrelatedWorkload{Namespace: "payments", Kind: "Deployment", Name: "api", PodCount: 3},
		models.CorrelatedWorkload{Namespace: "payments", Kind: "StatefulSet", Name: "db", PodCount: 2},
	)
	issue := collectActiveNodeWarRoomIssues(&clusterScan{nodeHealth: []models.NodeConditionFinding{finding}}, nodeWarRoomStore{items: []store.IncidentSummary{activeNodeIncident("worker-21", "DiskPressure")}}, "prod")[0]
	if issue.Message != "2 colocated workloads · 5 pods" {
		t.Fatalf("message = %q", issue.Message)
	}
	html := renderWarRoomCard(issue, "prod")
	for _, want := range []string{"DiskPressure on worker-21", "Node pool: pool-a", "2 colocated workloads · 5 pods", "/investigate?", "node=worker-21", "type=DiskPressure", "View node health"} {
		if !strings.Contains(html, want) {
			t.Errorf("card missing %q: %s", want, html)
		}
	}
	lower := strings.ToLower(html)
	for _, forbidden := range []string{"affected", "impacted", "caused by"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("node card contains forbidden wording/link %q: %s", forbidden, html)
		}
	}
}

func TestNodeTopIssuesAreDeterministicAndUseInvestigation(t *testing.T) {
	input := []warRoomIssue{
		{Severity: "high", Type: "MemoryPressure", Resource: "node-b", IsNode: true, Message: "0 colocated workloads · 0 pods"},
		{Severity: "high", Type: "DiskPressure", Resource: "node-a", IsNode: true, Message: "1 colocated workload · 2 pods"},
	}
	first := buildTopIssues(nil, input, "prod")
	second := buildTopIssues(nil, append([]warRoomIssue(nil), input...), "prod")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Top 5 ordering changed across identical input")
	}
	for _, issue := range first {
		if !strings.HasPrefix(issue.URL, "/investigate?") || !strings.Contains(issue.URL, "node=") {
			t.Errorf("node Top 5 URL = %q", issue.URL)
		}
	}
}

func TestNodeCollectionOrderingIndependentOfIncidentOrder(t *testing.T) {
	items := []store.IncidentSummary{
		activeNodeIncident("node-b", "DiskPressure"),
		activeNodeIncident("node-a", "MemoryPressure"),
		activeNodeIncident("node-a", "DiskPressure"),
	}
	reversed := []store.IncidentSummary{items[2], items[1], items[0]}
	first := collectActiveNodeWarRoomIssues(&clusterScan{}, nodeWarRoomStore{items: items}, "prod")
	second := collectActiveNodeWarRoomIssues(&clusterScan{}, nodeWarRoomStore{items: reversed}, "prod")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("node ordering depends on incident query order:\nfirst  %#v\nsecond %#v", first, second)
	}
}

func TestWorkloadOnlyWarRoomCollectionUnchanged(t *testing.T) {
	scan := &clusterScan{wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{{Name: "api-pod", Namespace: "app", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 3}}}}
	want := collectWarRoomIssues(scan, 0)
	got := collectWarRoomIssuesWithStore(scan, 0, nodeWarRoomStore{}, "prod")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workload-only issues changed:\ngot  %#v\nwant %#v", got, want)
	}
}
