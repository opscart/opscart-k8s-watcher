package report

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
)

func TestWasteHTMLKeepsWarningsHousekeepingAndCronJobInspection(t *testing.T) {
	audit := &analyzer.WasteAudit{
		ScannedAt:        time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC),
		DetectorWarnings: []analyzer.WasteDetectorWarning{{Category: "Jobs", Error: "forbidden"}},
		StaleJobs:        []analyzer.StaleJob{{Name: "nightly", Namespace: "ops", IsCronJob: true, JobStatus: "NoHistoryLimit", Score: 2}},
		OldReplicaSets:   []analyzer.OldReplicaSet{{Name: "api-old", Namespace: "ops", Score: 1}},
	}
	data := buildWasteHTMLData(audit, "test", 7)
	tmpl, err := template.New("waste").Parse(wasteHTMLTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{
		"Warnings reported", "Check warning: Jobs: forbidden", "ReplicaSet Retention Review",
		"Job / CronJob Retention Review", "kubectl get cronjob nightly -n ops -o yaml",
		"2025-02-03 04:05:06 UTC", "Finding count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	for _, bad := range []string{"safe to delete", "SAFE TO DELETE", "idle storage", "Total Items"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
			t.Errorf("HTML contains unsupported %q", bad)
		}
	}
}

func renderWasteHTMLTest(t *testing.T, a *analyzer.WasteAudit) (WasteHTMLData, string) {
	t.Helper()
	data := buildWasteHTMLData(a, "test", 7)
	tmpl, err := template.New("waste").Parse(wasteHTMLTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	return data, out.String()
}

func TestWasteHTMLPartialEmptyAndHousekeepingOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a        *analyzer.WasteAudit
		findings int
		warning  bool
	}{
		{"missing", nil, 0, false},
		{"empty", &analyzer.WasteAudit{}, 0, false},
		{"warnings only", &analyzer.WasteAudit{DetectorWarnings: []analyzer.WasteDetectorWarning{{Category: "Jobs", Error: "forbidden"}}}, 0, true},
		{"housekeeping only", &analyzer.WasteAudit{OldReplicaSets: []analyzer.OldReplicaSet{{Name: "history-only", OwnerDeployment: "owner"}}}, 1, false},
		{"partial", &analyzer.WasteAudit{OldReplicaSets: []analyzer.OldReplicaSet{{Name: "history-only"}}, DetectorWarnings: []analyzer.WasteDetectorWarning{{Category: "Jobs", Error: "forbidden"}}}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, body := renderWasteHTMLTest(t, tc.a)
			if data.TotalWasteItems != tc.findings || !strings.Contains(body, "Scanned: Unknown") {
				t.Fatal("count or Unknown timestamp missing")
			}
			if tc.warning && !strings.Contains(body, "Check warning: Jobs: forbidden") {
				t.Fatal("warning lost")
			}
			if tc.findings > 0 && (!strings.Contains(body, "history-only") || strings.Contains(body, "<h2>No findings reported</h2>")) {
				t.Fatal("housekeeping hidden")
			}
			for _, bad := range []string{"safe to delete", "cluster looks clean", "never ran", "unused for", "idle storage", "rollout leftovers", "active operational findings"} {
				if strings.Contains(strings.ToLower(body), bad) {
					t.Errorf("unsupported %q", bad)
				}
			}
		})
	}
}

func TestWasteHTMLPreservesMetadataCountsAndReplicaSetLimit(t *testing.T) {
	a := &analyzer.WasteAudit{
		TotalWasteItems:      999,
		AbandonedNamespaces:  []analyzer.AbandonedNamespace{{Name: "ns", PodCount: 3}},
		StalePods:            []analyzer.StalePod{{Name: "bare", Kind: analyzer.StalePodIdle}, {Name: "failed", Kind: analyzer.StalePodZombie, RestartCount: 8}},
		OrphanedPVCs:         []analyzer.OrphanedPVC{{Name: "small", RequestKnown: true, RequestedBytes: 500 << 20, Status: analyzer.PVCBoundNoPod}},
		StaleJobs:            []analyzer.StaleJob{{Name: "cron", Namespace: "ops", IsCronJob: true, JobStatus: "NoHistoryLimit"}, {Name: "cron", Namespace: "ops", IsCronJob: true, JobStatus: "NeverScheduled"}},
		ZeroReplicaWorkloads: []analyzer.ZeroReplicaWorkload{{Name: "zero", Kind: "StatefulSet"}},
		OrphanedServices:     []analyzer.OrphanedService{{Name: "svc", Type: "LoadBalancer"}},
		BrokenIngresses:      []analyzer.BrokenIngress{{Name: "ing"}},
		MisconfiguredHPAs:    []analyzer.MisconfiguredHPA{{Name: "hpa", TargetName: "target-marker", MinReplicas: 2, MaxReplicas: 9, IsActive: true}},
	}
	for i := 0; i < 20; i++ {
		a.OldReplicaSets = append(a.OldReplicaSets, analyzer.OldReplicaSet{Name: fmt.Sprintf("history-%02d", i), OwnerDeployment: "owner-marker"})
	}
	data, body := renderWasteHTMLTest(t, a)
	c := data.Presentation.Counts
	if c.Findings != 30 || c.DistinctResources != 29 || c.Operational != 3 || c.Retention != 22 || c.Review != 5 || c.Findings != c.Operational+c.Retention+c.Review {
		t.Fatalf("counts %+v", c)
	}
	total := data.AbandonedNamespaceCount + data.ZombiePodCount + data.UnmanagedPodCount + data.OrphanedPVCCount + data.StaleJobCount + data.ZeroReplicaCount + data.OrphanedServiceCount + data.BrokenIngressCount + data.MisconfiguredHPACount + data.OldReplicaSetCount
	if total != data.TotalWasteItems || total != c.Findings {
		t.Fatal("HTML category counts do not reconcile")
	}
	for _, want := range []string{"history-19", "owner-marker", "target-marker", ">Replicas:</span> 2-9", ">Restarts:</span> 8", ">Pods:</span> 3", "LoadBalancer", "500 MiB", "kubectl get cronjob cron -n ops -o yaml", "Observed:", "Inference:", "Limitations:", "Review:"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing metadata/evidence %q", want)
		}
	}
	a.OldReplicaSets = append(a.OldReplicaSets, analyzer.OldReplicaSet{Name: "history-20"})
	_, body = renderWasteHTMLTest(t, a)
	if strings.Contains(body, "history-00") || strings.Contains(body, "history-20") || !strings.Contains(body, "21 old ReplicaSets found") {
		t.Fatal("legacy >20 summary behavior changed")
	}
	// Existing exported model fields retain their original types for keyed callers.
	legacy := WasteHTMLData{ScannedAt: time.Now(), CriticalCount: 1, WarningCount: 2, MaxZombieRestarts: 3, OrphanedPVCStorageGB: 4,
		AbandonedNamespaces: a.AbandonedNamespaces, ZombiePods: a.StalePods, UnmanagedPods: a.StalePods, OrphanedPVCs: a.OrphanedPVCs, StaleJobs: a.StaleJobs,
		ZeroReplicaWorkloads: a.ZeroReplicaWorkloads, OrphanedServices: a.OrphanedServices, BrokenIngresses: a.BrokenIngresses, MisconfiguredHPAs: a.MisconfiguredHPAs, OldReplicaSets: a.OldReplicaSets}
	if legacy.OrphanedPVCs[0].RequestedBytes != 500<<20 {
		t.Fatal("legacy typed data lost")
	}
}
