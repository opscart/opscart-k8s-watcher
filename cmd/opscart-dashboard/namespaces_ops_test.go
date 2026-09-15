package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// namespacesOpsTestScan builds a clusterScan with authoritative evidence
// for two namespaces: "payments" (critical — crash loop + OOM zombie pods,
// unprotected) and "checkout" (degraded, protected). Mirrors the exact
// per-namespace operational signal buildNamespaceOpsRows composes from —
// scan.namespaces (the base/authoritative list), scan.namespacePodCounts,
// scan.report.NamespaceCosts (opportunistic cost enrichment),
// scan.wasteAudit.StalePods, scan.netAudit.Protected/UnprotectedNamespaces,
// and scan.AllWorkloads.
func namespacesOpsTestScan() *clusterScan {
	return &clusterScan{
		namespaceCount: 2,
		namespaces: []*corev1.Namespace{
			testNamespace("payments"),
			testNamespace("checkout"),
		},
		report: &models.CloudCostReport{
			ClusterName: "minikube",
			NamespaceCosts: []models.NamespaceCostInfo{
				{Name: "payments", PodCount: 27, EstimatedCost: models.CostRange{Best: 240}},
				{Name: "checkout", PodCount: 18, EstimatedCost: models.CostRange{Best: 90}},
			},
		},
		wasteAudit: &analyzer.WasteAudit{
			StalePods: []analyzer.StalePod{
				{Name: "api-1", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", Severity: "high", RestartCount: 12},
				{Name: "api-2", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "OOMKilled", Severity: "critical", RestartCount: 6},
				{Name: "worker-1", Namespace: "checkout", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", Severity: "high", RestartCount: 1},
			},
		},
		netAudit: &analyzer.NetworkPolicyAudit{
			ProtectedNamespaces: []analyzer.NamespaceNetworkStatus{
				{Name: "checkout", PodCount: 18, PolicyCount: 2},
			},
			UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{
				{Name: "payments", PodCount: 27, PolicyCount: 0, RiskLevel: "HIGH"},
			},
		},
		AllWorkloads: []models.WorkloadRef{
			{Name: "api", Kind: "Deployment", Namespace: "payments"},
			{Name: "billing", Kind: "Deployment", Namespace: "payments"},
			{Name: "worker", Kind: "Deployment", Namespace: "checkout"},
		},
		nodePodCounts:      map[string]int{"node-a": 27, "node-b": 18},
		namespacePodCounts: map[string]int{"payments": 27, "checkout": 18},
	}
}

func TestNamespacesPageHealthRowsAndActiveIssues(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	for _, want := range []string{
		"payments", "checkout",
		`class="badge badge-danger">Critical</span>`, // payments has an OOMKilled critical issue
		`class="badge badge-warn">Degraded</span>`,   // checkout has only a high-severity crash loop, no critical
		// payments: 2 zombie-pod issues (crash loop + OOM) + 1 HIGH-risk
		// unprotected-namespace issue = 3, the same issue set War Room reports.
		`class="issue-count badge-danger">3<`,
		`class="issue-count badge-warn">1<`, // checkout: 1 active issue
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Namespaces page missing %q\n%s", want, body)
		}
	}
}

func TestNamespacesPageRestartsAndOOMRendering(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	// payments: 12+6=18 restarts, 1 OOMKilled zombie
	if !strings.Contains(body, ">18 / 1<") {
		t.Fatalf("payments restarts/OOM cell missing or wrong:\n%s", body)
	}
	// checkout: 1 restart, 0 OOMKilled
	if !strings.Contains(body, ">1 / 0<") {
		t.Fatalf("checkout restarts/OOM cell missing or wrong:\n%s", body)
	}
}

func TestNamespacesPageGovernanceRendering(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	for _, want := range []string{
		`class="badge badge-danger">Unprotected</span>`,
		`class="badge badge-ok">Protected</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Namespaces page missing governance badge %q\n%s", want, body)
		}
	}
}

func TestNamespacesPageGovernancePartialState(t *testing.T) {
	scan := namespacesOpsTestScan()
	// payments has a policy but an incomplete coverage gap — must render
	// as "Partial", never folded into the zero-policy "Unprotected" bucket.
	scan.netAudit.UnprotectedNamespaces[0].PolicyCount = 1
	scan.netAudit.UnprotectedNamespaces[0].CoverageGapPodCount = 5

	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})
	if !strings.Contains(body, `class="badge badge-warn">Partial</span>`) {
		t.Fatalf("Namespaces page did not render Partial governance state:\n%s", body)
	}
	if strings.Contains(body, `class="badge badge-danger">Unprotected</span>`) {
		t.Fatalf("namespace with a policy present must not render as fully Unprotected:\n%s", body)
	}
}

func TestNamespacesPageWorkloadAndPodCountsPreserved(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	// payments: 2 workloads (api, billing), 27 pods
	if !strings.Contains(body, "<td style=\"text-align:center\">2</td>") {
		t.Fatalf("payments workload count missing:\n%s", body)
	}
	if !strings.Contains(body, "<td style=\"text-align:center\">27</td>") {
		t.Fatalf("payments pod count missing:\n%s", body)
	}
	// checkout: 1 workload, 18 pods
	if !strings.Contains(body, "<td style=\"text-align:center\">1</td>") {
		t.Fatalf("checkout workload count missing:\n%s", body)
	}
	if !strings.Contains(body, "<td style=\"text-align:center\">18</td>") {
		t.Fatalf("checkout pod count missing:\n%s", body)
	}
}

func TestNamespacesPageNoDataLossFromOldColumns(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})
	// The namespace names, pod counts, and cost evidence the old
	// cost-centric table displayed must still be present in some form —
	// this redesign must not silently drop real evidence, only
	// reprioritize it.
	for _, want := range []string{"payments", "checkout", "$240", "$90"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected prior namespace evidence %q to still be present:\n%s", want, body)
		}
	}
}

func TestNamespacesPageCostIsSecondaryNotPrimary(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	healthIdx := strings.Index(body, ">Health<")
	costHeaderIdx := strings.Index(body, "$/mo")
	if healthIdx == -1 || costHeaderIdx == -1 {
		t.Fatalf("expected both Health and $/mo headers present:\nhealthIdx=%d costHeaderIdx=%d", healthIdx, costHeaderIdx)
	}
	if costHeaderIdx < healthIdx {
		t.Fatalf("cost column must not precede Health in column order (cost is secondary context)")
	}
	if !strings.Contains(body, `class="cost-secondary"`) {
		t.Fatalf("cost cell must be visually marked secondary")
	}
	// Cost must never dominate the top summary: no primary "Total Cost/mo" KPI chip.
	if strings.Contains(body, "Total Cost/mo") {
		t.Fatalf("cost must not appear as a primary top-summary KPI")
	}
}

func TestNamespacesPageAttentionCards(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	for _, want := range []string{
		"Namespaces Needing Attention",
		"Namespaces with the highest operational risk based on health, incidents, restart activity, and governance.",
		"Most Active Issues",
		"Highest Restart Activity",
		"Unprotected &amp; Unhealthy",
		`class="attn-metric">3 active issues · Critical</div>`,
		`class="attn-support">2 workloads in namespace</div>`,
		`class="attn-metric">18 restarts · 1 OOM</div>`,
		`class="attn-metric">3 active issues · Unprotected</div>`,
		`class="attn-support">No NetworkPolicy · Critical health</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected attention card %q\n%s", want, body)
		}
	}
	if got := strings.Count(body, `class="attn-support"`); got != 3 {
		t.Fatalf("attention-card supporting detail blocks = %d, want 3\n%s", got, body)
	}
}

func TestNamespacesPagePodCountsComeFromSnapshotPods(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{
			testNamespace("default"),
			testNamespace("payments"),
			testNamespace("kube-system"),
		},
	}
	for i := 0; i < 4; i++ {
		resources.Pods = append(resources.Pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("payments-%d", i), Namespace: "payments"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	for i := 0; i < 7; i++ {
		resources.Pods = append(resources.Pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("system-%d", i), Namespace: "kube-system"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}

	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}
	clusterState := clusterstate.NewClusterState("cluster-a")
	clusterState.Update(resources)
	clusterState.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	scan := buildClusterScan(state, clusterState.Publish())

	if len(scan.report.NamespaceCosts) != 0 {
		t.Fatalf("test fixture invalid: NamespaceCosts = %d, want 0", len(scan.report.NamespaceCosts))
	}
	assertNamespacePodCount(t, renderNamespacesPage(scan, "cluster-a", []string{"cluster-a"}), "payments", 4)
	assertNamespacePodCount(t, renderNamespacesPage(scan, "cluster-a", []string{"cluster-a"}), "kube-system", 7)
	assertNamespacePodCount(t, renderNamespacesPage(scan, "cluster-a", []string{"cluster-a"}), "default", 0)

	// Cost allocation may enrich the cost cell, but it cannot create or
	// overwrite the Pod count sourced from ClusterResources.Pods.
	scan.report.NamespaceCosts = []models.NamespaceCostInfo{
		{Name: "payments", PodCount: 400, EstimatedCost: models.CostRange{Best: 40}},
		{Name: "kube-system", PodCount: 700, EstimatedCost: models.CostRange{Best: 70}},
		{Name: "default", PodCount: 100, EstimatedCost: models.CostRange{Best: 10}},
		{Name: "cost-only", PodCount: 999, EstimatedCost: models.CostRange{Best: 99}},
	}
	body := renderNamespacesPage(scan, "cluster-a", []string{"cluster-a"})
	assertNamespacePodCount(t, body, "payments", 4)
	assertNamespacePodCount(t, body, "kube-system", 7)
	assertNamespacePodCount(t, body, "default", 0)
	if strings.Contains(body, "cost-only") {
		t.Fatalf("cost-only namespace must not create a namespace row:\n%s", body)
	}
}

func assertNamespacePodCount(t *testing.T, body, namespace string, want int) {
	t.Helper()
	marker := fmt.Sprintf(`data-namespace="%s"`, namespace)
	markerIndex := strings.Index(body, marker)
	if markerIndex == -1 {
		t.Fatalf("namespace row %q not found", namespace)
	}
	rowStart := strings.LastIndex(body[:markerIndex], "<tr")
	rowEnd := strings.Index(body[markerIndex:], "</tr>")
	if rowStart == -1 || rowEnd == -1 {
		t.Fatalf("namespace row %q is malformed", namespace)
	}
	row := body[rowStart : markerIndex+rowEnd]
	lines := strings.Split(row, "\n")
	if len(lines) < 5 {
		t.Fatalf("namespace row %q has unexpected markup:\n%s", namespace, row)
	}
	wantCell := fmt.Sprintf(`<td style="text-align:center">%d</td>`, want)
	if lines[4] != wantCell {
		t.Fatalf("namespace %q Pod cell = %q, want %q\n%s", namespace, lines[4], wantCell, row)
	}
}

func TestNamespacesPageBalancedSemanticKPIs(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	for _, want := range []string{
		`grid-template-columns:repeat(6,minmax(0,1fr))`,
		`kpi-chip kpi-total`,
		`kpi-chip kpi-healthy`,
		`kpi-chip kpi-attention`,
		`kpi-chip kpi-unprotected`,
		`kpi-chip kpi-workloads`,
		`kpi-chip kpi-pods`,
		"Total Namespaces",
		"Healthy",
		"Needing Attention",
		"Unprotected",
		"Total Workloads",
		"Total Pods",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected balanced KPI treatment %q\n%s", want, body)
		}
	}
	if got := strings.Count(body, `class="kpi-chip kpi-`); got != 6 {
		t.Fatalf("KPI cards = %d, want 6\n%s", got, body)
	}
}

func TestNamespacesPageTableToolbarUsesRenderedRowState(t *testing.T) {
	body := renderNamespacesPage(namespacesOpsTestScan(), "minikube", []string{"minikube"})

	for _, want := range []string{
		`id="namespace-search"`,
		`id="namespace-state"`,
		`value="all">All namespaces`,
		`value="attention">Needs attention`,
		`value="critical">Critical`,
		`value="degraded">Degraded`,
		`value="healthy">Healthy`,
		`value="unprotected">Unprotected`,
		`data-namespace="payments" data-health="Critical" data-governance="Unprotected"`,
		`data-namespace="checkout" data-health="Degraded" data-governance="Protected"`,
		`search.addEventListener('input',applyFilters)`,
		`state.addEventListener('change',applyFilters)`,
		`&&matchesState(row,selected)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected namespace filter contract %q\n%s", want, body)
		}
	}
}

func TestNamespacesPageAttentionCardsOmittedWhenNoEvidence(t *testing.T) {
	scan := &clusterScan{
		namespaceCount: 1,
		namespaces:     []*corev1.Namespace{testNamespace("quiet-ns")},
		report: &models.CloudCostReport{
			ClusterName: "minikube",
			NamespaceCosts: []models.NamespaceCostInfo{
				{Name: "quiet-ns", PodCount: 3, EstimatedCost: models.CostRange{Best: 10}},
			},
		},
	}
	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})
	if strings.Contains(body, "Namespaces Needing Attention") {
		t.Fatalf("attention section must not render when no namespace has qualifying evidence:\n%s", body)
	}
	if !strings.Contains(body, `class="badge badge-ok">Healthy</span>`) {
		t.Fatalf("namespace with zero active issues must render Healthy:\n%s", body)
	}
}

func TestNamespacesPageEmptyScanRendersWithoutPanic(t *testing.T) {
	body := renderNamespacesPage(nil, "minikube", []string{"minikube"})
	if !strings.Contains(body, "No namespace data available") {
		t.Fatalf("expected empty-state message for nil scan:\n%s", body)
	}
}

// ── Regression coverage: authoritative namespace inventory drives rows,
// not NamespaceCosts (see the reported bug: KPI said 12 namespaces, the
// table said "No namespace data available") ──────────────────────────────

func TestNamespacesPageTwelveNamespacesZeroCostsProduceTwelveRows(t *testing.T) {
	var namespaces []*corev1.Namespace
	for i := 1; i <= 12; i++ {
		namespaces = append(namespaces, testNamespace(fmt.Sprintf("ns-%02d", i)))
	}
	scan := &clusterScan{
		namespaceCount: 12,
		namespaces:     namespaces,
		report:         &models.CloudCostReport{ClusterName: "minikube"}, // zero NamespaceCosts entries
	}

	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})

	if !strings.Contains(body, `<div class="kpi-chip-val">12</div>`) {
		t.Fatalf("Total Namespaces KPI must read 12:\n%s", body)
	}
	if strings.Contains(body, "No namespace data available") {
		t.Fatalf("empty state must not render when the authoritative inventory has 12 namespaces:\n%s", body)
	}
	for _, ns := range namespaces {
		want := fmt.Sprintf(`<span class="ns-name">%s</span>`, ns.Name)
		if !strings.Contains(body, want) {
			t.Fatalf("missing row for namespace %q:\n%s", ns.Name, body)
		}
	}
	if strings.Contains(body, "not listed individually") {
		t.Fatalf("the old row-completeness caveat must no longer exist:\n%s", body)
	}
}

func TestNamespacesPageNamespaceWithoutCostDataStillRenders(t *testing.T) {
	scan := &clusterScan{
		namespaceCount: 2,
		namespaces:     []*corev1.Namespace{testNamespace("has-cost"), testNamespace("no-cost")},
		report: &models.CloudCostReport{
			ClusterName: "minikube",
			NamespaceCosts: []models.NamespaceCostInfo{
				{Name: "has-cost", PodCount: 5, EstimatedCost: models.CostRange{Best: 50}},
			},
		},
	}
	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})

	if !strings.Contains(body, `<span class="ns-name">no-cost</span>`) {
		t.Fatalf("namespace without any NamespaceCosts entry must still render a row:\n%s", body)
	}
	if !strings.Contains(body, `<span class="ns-name">has-cost</span>`) {
		t.Fatalf("namespace with cost data must still render:\n%s", body)
	}

	idx := strings.Index(body, `<span class="ns-name">no-cost</span>`)
	if idx == -1 {
		t.Fatal("no-cost row not found")
	}
	rowSegment := body[idx:]
	if end := strings.Index(rowSegment, "</tr>"); end != -1 {
		rowSegment = rowSegment[:end]
	}
	if !strings.Contains(rowSegment, `<span class="muted">—</span>`) {
		t.Fatalf("no-cost row must render secondary empty markers (—), never a fabricated value:\n%s", rowSegment)
	}
}

func TestNamespacesPageNamespaceCostsOnlyEnrichMatchingRows(t *testing.T) {
	scan := &clusterScan{
		namespaceCount: 1,
		namespaces:     []*corev1.Namespace{testNamespace("real-ns")},
		report: &models.CloudCostReport{
			ClusterName: "minikube",
			NamespaceCosts: []models.NamespaceCostInfo{
				{Name: "real-ns", PodCount: 4, EstimatedCost: models.CostRange{Best: 40}},
				// ghost-ns: stale/bogus cost-allocation data for a namespace
				// that is no longer (or never was) in the authoritative
				// Kubernetes namespace inventory.
				{Name: "ghost-ns", PodCount: 99, EstimatedCost: models.CostRange{Best: 999}},
			},
		},
	}
	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})

	if strings.Contains(body, "ghost-ns") {
		t.Fatalf("a namespace present only in NamespaceCosts must never appear as a row:\n%s", body)
	}
	if !strings.Contains(body, `<span class="ns-name">real-ns</span>`) {
		t.Fatalf("real-ns must render, enriched by its matching cost entry:\n%s", body)
	}
	if !strings.Contains(body, "$40") {
		t.Fatalf("real-ns should be enriched with its matching cost:\n%s", body)
	}
	if !strings.Contains(body, `<div class="kpi-chip-val">1</div>`) {
		t.Fatalf("Total Namespaces KPI must reflect only the authoritative inventory (1), not NamespaceCosts (2):\n%s", body)
	}
}

func TestNamespacesPageRowOrderIsDeterministic(t *testing.T) {
	build := func() *clusterScan {
		return &clusterScan{
			namespaceCount: 3,
			namespaces: []*corev1.Namespace{
				testNamespace("zeta-ns"), testNamespace("alpha-ns"), testNamespace("mid-ns"),
			},
			report: &models.CloudCostReport{ClusterName: "minikube"},
		}
	}

	first := renderNamespacesPage(build(), "minikube", []string{"minikube"})
	second := renderNamespacesPage(build(), "minikube", []string{"minikube"})
	if first != second {
		t.Fatal("row order must be deterministic across renders of identical input")
	}

	// All three are equally Healthy with zero active issues, so the
	// health-rank/issue-count sort falls through to the alphabetical
	// name tiebreak.
	ai := strings.Index(first, `<span class="ns-name">alpha-ns</span>`)
	mi := strings.Index(first, `<span class="ns-name">mid-ns</span>`)
	zi := strings.Index(first, `<span class="ns-name">zeta-ns</span>`)
	if ai == -1 || mi == -1 || zi == -1 {
		t.Fatalf("expected all three namespace rows present: alpha=%d mid=%d zeta=%d\n%s", ai, mi, zi, first)
	}
	if !(ai < mi && mi < zi) {
		t.Fatalf("expected alphabetical order for equal-health namespaces: alpha=%d mid=%d zeta=%d\n%s", ai, mi, zi, first)
	}
}

func TestNamespacesPageEmptyStateReflectsAuthoritativeInventoryOnly(t *testing.T) {
	// Namespaces empty, but NamespaceCosts is not — stale/bogus cost data
	// alone must never fake a non-empty namespace list.
	scan := &clusterScan{
		namespaceCount: 0,
		namespaces:     nil,
		report: &models.CloudCostReport{
			ClusterName: "minikube",
			NamespaceCosts: []models.NamespaceCostInfo{
				{Name: "ghost", PodCount: 1, EstimatedCost: models.CostRange{Best: 5}},
			},
		},
	}
	body := renderNamespacesPage(scan, "minikube", []string{"minikube"})
	if !strings.Contains(body, "No namespace data available") {
		t.Fatalf("empty state must render when the authoritative namespace inventory is empty, even if NamespaceCosts is not:\n%s", body)
	}
	if strings.Contains(body, "ghost") {
		t.Fatalf("a stale NamespaceCosts entry must not leak into the page when the inventory is empty:\n%s", body)
	}

	// Inventory non-empty, NamespaceCosts empty — this was the reported
	// bug and must no longer produce the empty state.
	scan2 := &clusterScan{
		namespaceCount: 1,
		namespaces:     []*corev1.Namespace{testNamespace("real-ns")},
		report:         &models.CloudCostReport{ClusterName: "minikube"},
	}
	body2 := renderNamespacesPage(scan2, "minikube", []string{"minikube"})
	if strings.Contains(body2, "No namespace data available") {
		t.Fatalf("empty state must not render merely because NamespaceCosts is empty:\n%s", body2)
	}
}
