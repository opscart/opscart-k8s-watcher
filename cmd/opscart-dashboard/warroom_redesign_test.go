package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestWarRoomIncidentAgeUsesOperationalMemory(t *testing.T) {
	scan := &clusterScan{wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{{
		Name: "api-abc123", Namespace: "apps", Kind: analyzer.StalePodZombie,
		Status: "CrashLoopBackOff", RestartCount: 9, AgeDays: 1897,
	}}}}
	db := &queryIncidentsStubStore{items: []store.IncidentSummary{{
		Namespace: "apps", Resource: "api-abc123", IssueType: "crash_loop",
		FirstSeen: time.Now().Add(-26 * 24 * time.Hour),
	}}, total: 1}
	body := renderWarRoomPageWithStore(scan, "prod", []string{"prod"}, nil, db)
	if !strings.Contains(body, ">26d<") || strings.Contains(body, ">1897d<") {
		t.Fatalf("War Room did not use incident first_seen")
	}
	if !strings.Contains(body, "api-abc123</a>") {
		t.Fatal("oldest-active summary is not traceable to its finding")
	}
	withoutMemory := renderWarRoomPage(scan, "prod", []string{"prod"})
	if !strings.Contains(withoutMemory, "Active For</span><strong>—</strong>") ||
		!strings.Contains(withoutMemory, `<div class="summary-num">—</div><div class="summary-label">Oldest Active</div>`) {
		t.Fatal("unavailable operational memory did not render em dashes")
	}
}

func TestUtilizationStatusBoundaries(t *testing.T) {
	for value, want := range map[int]string{59: "Nominal", 60: "Elevated", 79: "Elevated", 80: "High", 89: "High", 90: "Critical"} {
		if got := utilizationStatus(value, 0); got != want {
			t.Errorf("utilizationStatus(%d, 0) = %q, want %q", value, got, want)
		}
	}
	if got := utilizationStatus(20, 82); got != "High" {
		t.Errorf("highest CPU/memory utilization not used: got %q", got)
	}
}

func TestWarRoomWorkloadIdentity(t *testing.T) {
	scan := &clusterScan{AllWorkloads: []models.WorkloadRef{
		{Name: "stream-processor", Kind: "Deployment", Namespace: "apps"},
		{Name: "node-agent", Kind: "DaemonSet", Namespace: "ops"},
		{Name: "prometheus", Kind: "StatefulSet", Namespace: "monitoring"},
	}}
	tests := []struct {
		name, pod, namespace, wantIdentity string
	}{
		{"deployment", "stream-processor-66c474d5fd-9zpwq", "apps", "Deployment/stream-processor"},
		{"daemonset", "node-agent-8j6s5", "ops", "DaemonSet/node-agent"},
		{"statefulset ordinal", "prometheus-0", "monitoring", "StatefulSet/prometheus-0"},
		{"bare pod", "storage-provisioner", "kube-system", "Pod/storage-provisioner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := warRoomIssue{Severity: "critical", Type: "crash_loop", Resource: tt.pod, Namespace: tt.namespace}
			enrichWarRoomIdentity(&issue, scan)
			card := renderWarRoomCard(issue, "prod")
			if !strings.Contains(card, tt.wantIdentity) {
				t.Fatalf("card missing identity %q: %s", tt.wantIdentity, card)
			}
			if !strings.Contains(card, "Focus Pod: "+tt.pod) {
				t.Fatalf("card missing Focus Pod subtitle: %s", card)
			}
		})
	}
}

func TestNamespacePostureEvidenceAndIdentity(t *testing.T) {
	scan := &clusterScan{
		netAudit:   &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{Name: "monitoring", RiskLevel: "HIGH", PodCount: 8}}},
		wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "batch", AgeDays: 30, PodCount: 0, Reason: "No pods found. Namespace is 30 days old with zero workloads"}}},
	}
	issues := collectWarRoomIssues(scan, 0)
	body := renderWarRoomPage(scan, "prod", []string{"prod"})
	for _, want := range []string{
		"Namespace/monitoring", "Missing default-deny NetworkPolicy", "8 pods in namespace",
		"kubectl get networkpolicies -n monitoring", "Namespace/batch",
		"No pods found. Namespace is 30 days old with zero workloads",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, forbidden := range []string{"pods exposed", "pod=namespace", "every pod can reach", "critical infrastructure exposed"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("page contains unsupported wording %q", forbidden)
		}
	}
	if len(issues) != 2 {
		t.Fatalf("got %d posture issues, want 2", len(issues))
	}
}

func TestWarRoomSearchAndFilters(t *testing.T) {
	issues := []warRoomIssue{
		{Severity: "critical", Type: "crash_loop", Namespace: "payments", Resource: "checkout-7cddf79d98-jxmtx", WorkloadKind: "Deployment", WorkloadName: "checkout", Classification: "CrashLoopBackOff", Container: "api"},
		{Severity: "high", Type: "unprotected_namespace", Namespace: "monitoring", Resource: "namespace", WorkloadKind: "Namespace", WorkloadName: "monitoring", Classification: "Missing default-deny NetworkPolicy"},
	}
	tests := []struct{ name, q, severity, issueType string }{
		{"workload", "checkout", "", ""},
		{"focus pod", "7cddf79d98-jxmtx", "", ""},
		{"namespace", "payments", "", ""},
		{"classification", "crashloopbackoff", "", ""},
		{"container", "api", "", ""},
		{"severity", "", "critical", ""},
		{"classification filter", "", "", "crash_loop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterWarRoomIssues(issues, tt.q, tt.severity, tt.issueType)
			if len(got) != 1 || got[0].Type != "crash_loop" {
				t.Fatalf("unexpected filtered results: %+v", got)
			}
		})
	}
}

func TestWarRoomFilteringOccursBeforeLimit(t *testing.T) {
	var pods []analyzer.StalePod
	for i := 0; i < 13; i++ {
		pods = append(pods, analyzer.StalePod{Name: fmt.Sprintf("worker-%02d-7cddf79d98-jxmtx", i), Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: int32(100 - i)})
	}
	scan := &clusterScan{wasteAudit: &analyzer.WasteAudit{StalePods: pods}}
	body := renderWarRoomPageWithFilters(scan, "prod", []string{"prod"}, url.Values{"q": {"worker-12"}, "limit": {"12"}})
	if !strings.Contains(body, "worker-12") || !strings.Contains(body, "Showing 1 prioritized incident") {
		t.Fatalf("search was applied after the first 12 results")
	}
}

func TestWarRoomLimitsAndInvalidDefault(t *testing.T) {
	for raw, want := range map[string]int{"12": 12, "25": 25, "50": 50, "": 12, "999": 12, "bad": 12} {
		if got := parseWarRoomLimit(raw); got != want {
			t.Errorf("parseWarRoomLimit(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestWarRoomRankingAndOrdinals(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{
			{Name: "low-restarts", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 2},
			{Name: "high-restarts", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 50},
		}},
		netAudit: &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{Name: "posture", RiskLevel: "HIGH"}}},
	}
	issues := collectWarRoomIssues(scan, 0)
	if issues[0].Resource != "high-restarts" || issues[1].Resource != "low-restarts" || issues[2].Severity != "high" {
		t.Fatalf("ranking changed from severity then restart count: %+v", issues)
	}
	body := renderWarRoomPage(scan, "prod", []string{"prod"})
	if strings.Index(body, "#1") > strings.Index(body, "#2") || !strings.Contains(body, "Ranked by severity · restart count") ||
		!strings.Contains(body, "wr-card c wr-type-crash-loop featured") {
		t.Fatalf("ordinal badges or ranking label are incorrect")
	}
}

func TestWarRoomBriefingDeduplicatesWorkloadsAndOmitsUnavailable(t *testing.T) {
	issues := []warRoomIssue{
		{Severity: "critical", Type: "crash_loop", Namespace: "apps", WorkloadKind: "Deployment", WorkloadName: "api"},
		{Severity: "critical", Type: "privileged_container", Namespace: "apps", WorkloadKind: "Deployment", WorkloadName: "api"},
		{Severity: "high", Type: "unprotected_namespace", Namespace: "apps", WorkloadKind: "Namespace", WorkloadName: "apps"},
	}
	stats := warRoomStatsFor(issues)
	if stats.affectedWorkloads != 1 || stats.namespaceFindings != 1 {
		t.Fatalf("unexpected deduplicated stats: %+v", stats)
	}
	briefing := buildWarRoomBriefing(stats)
	if strings.Contains(briefing, "oldest") || strings.Contains(briefing, "restart count") {
		t.Fatalf("briefing did not omit unavailable facts: %q", briefing)
	}
}

func TestWarRoomBackToOverviewAndResponsiveLayout(t *testing.T) {
	body := renderWarRoomPage(&clusterScan{}, "real/context", []string{"real/context"})
	for _, want := range []string{
		"Back to Overview", `href="/?cluster=real%2Fcontext"`,
		"@media(min-width:1500px){.wr-grid{grid-template-columns:repeat(4,minmax(0,1fr))}}",
		"@media(max-width:1099px)", "grid-template-columns:repeat(2,minmax(0,1fr))",
		"@media(max-width:719px)", ".wr-grid{grid-template-columns:1fr}",
		".wr-grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr))",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, ".wr-card.featured{grid-column") {
		t.Fatal("featured card still spans grid columns")
	}
	if strings.Contains(body, "cluster=current-context") {
		t.Fatal("page leaked synthetic current-context")
	}
	empty := renderWarRoomPage(&clusterScan{}, "", []string{""})
	if strings.Contains(empty, "cluster=current-context") || strings.Contains(empty, `name="cluster" value="current-context"`) {
		t.Fatal("empty context emitted synthetic cluster query")
	}
}

func TestWarRoomDenseEqualCardsAndResetVisibility(t *testing.T) {
	scan := &clusterScan{wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{
		{Name: "api-7cddf79d98-jxmtx", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 8, AgeDays: 2},
		{Name: "worker-7cddf79d98-kbfzw", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 4, AgeDays: 1},
	}}}
	body := renderWarRoomPage(scan, "prod", []string{"prod"})
	for _, want := range []string{
		`class="wr-card c wr-type-crash-loop featured"`,
		`class="wr-card c wr-type-crash-loop"`,
		`class="wr-evidence"`, "Classification", "Active For", "Restarts",
		`<footer class="wr-actions">`, `title="Focus Pod: api-7cddf79d98-jxmtx"`,
		`min-height:260px`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dense card rendering missing %q", want)
		}
	}
	if strings.Contains(body, `<a class="toolbar-reset"`) || strings.Contains(body, `class="toolbar has-reset"`) {
		t.Fatal("unfiltered toolbar rendered or reserved space for Reset")
	}

	filtered := renderWarRoomPageWithFilters(scan, "prod", []string{"prod"}, url.Values{"q": {"api"}})
	if !strings.Contains(filtered, `class="toolbar has-reset"`) ||
		!strings.Contains(filtered, `<a class="toolbar-reset"`) {
		t.Fatal("active filter did not render full-height Reset control")
	}
}

// TestEnrichWarRoomIdentityUsesConfirmedOwnershipOverNamePattern guards the
// naming-collision fix: when scan.PodWorkloads has a confirmed mapping for
// a pod, that mapping is used directly — never the name-pattern fallback,
// which cannot distinguish a real StatefulSet replica from an unrelated
// pod (e.g. a Deployment) that merely shares its naming pattern.
func TestEnrichWarRoomIdentityUsesConfirmedOwnershipOverNamePattern(t *testing.T) {
	// Namespace has an unrelated StatefulSet "worker" AND a Deployment
	// whose pod is literally named "worker-0". A name-pattern check alone
	// (strings.HasPrefix) would misattribute "worker-0" to the StatefulSet.
	scan := &clusterScan{
		AllWorkloads: []models.WorkloadRef{
			{Name: "worker", Kind: "StatefulSet", Namespace: "batch"},
			{Name: "worker-0", Kind: "Deployment", Namespace: "batch"},
		},
		PodWorkloads: map[string]models.WorkloadRef{
			"batch/worker-0": {Name: "worker-0", Kind: "Deployment", Namespace: "batch"},
		},
	}
	issue := warRoomIssue{Severity: "critical", Type: "crash_loop", Resource: "worker-0", Namespace: "batch"}
	enrichWarRoomIdentity(&issue, scan)

	if issue.WorkloadKind != "Deployment" || issue.WorkloadName != "worker-0" {
		t.Fatalf("expected confirmed ownership (Deployment/worker-0), got %s/%s — the StatefulSet naming collision was not prevented",
			issue.WorkloadKind, issue.WorkloadName)
	}
}

// TestEnrichWarRoomIdentityStatefulSetStaysInstanceScopedWithConfirmedOwnership
// confirms that confirmed ownership doesn't break the StatefulSet
// instance-scoping design contract: only Kind is taken from the confirmed
// mapping, WorkloadName stays the pod's own identity (prometheus-0, not
// prometheus), matching existing War Room card behavior.
func TestEnrichWarRoomIdentityStatefulSetStaysInstanceScopedWithConfirmedOwnership(t *testing.T) {
	scan := &clusterScan{
		PodWorkloads: map[string]models.WorkloadRef{
			"monitoring/prometheus-0": {Name: "prometheus", Kind: "StatefulSet", Namespace: "monitoring"},
		},
	}
	issue := warRoomIssue{Severity: "critical", Type: "crash_loop", Resource: "prometheus-0", Namespace: "monitoring"}
	enrichWarRoomIdentity(&issue, scan)

	if issue.WorkloadKind != "StatefulSet" || issue.WorkloadName != "prometheus-0" {
		t.Fatalf("expected StatefulSet/prometheus-0 (instance-scoped), got %s/%s",
			issue.WorkloadKind, issue.WorkloadName)
	}
}
