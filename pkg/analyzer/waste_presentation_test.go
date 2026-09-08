package analyzer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"k8s.io/client-go/kubernetes/fake"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWastePresentationCountsIdentityAndPurity(t *testing.T) {
	zero := int32(0)
	a := &WasteAudit{
		ScannedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), TotalWasteItems: 999,
		AbandonedNamespaces:  []AbandonedNamespace{{Name: "same", PodCount: 0, Score: 11}},
		StalePods:            []StalePod{{Name: "same", Namespace: "a", Kind: StalePodIdle, Score: 12}, {Name: "same", Namespace: "b", Kind: StalePodZombie, Score: 13}},
		OrphanedPVCs:         []OrphanedPVC{{Name: "same", Namespace: "a", Status: PVCBoundNoPod, RequestKnown: true, RequestedBytes: 500 << 20, Score: 14}},
		OrphanedServices:     []OrphanedService{{Name: "same", Namespace: "a", Score: 15}},
		StaleJobs:            []StaleJob{{Name: "same", Namespace: "a", IsCronJob: true, JobStatus: "NeverScheduled", Score: 16}, {Name: "same", Namespace: "a", IsCronJob: true, JobStatus: "NoHistoryLimit", Score: 17}},
		ZeroReplicaWorkloads: []ZeroReplicaWorkload{{Name: "same", Namespace: "a", Kind: "StatefulSet", Score: 18}},
		BrokenIngresses:      []BrokenIngress{{Name: "same", Namespace: "a", IsActive: true, Reason: "Backend currently has no ready endpoints.", Score: 19}},
		MisconfiguredHPAs:    []MisconfiguredHPA{{Name: "same", Namespace: "a", IsActive: true, Condition: "FailedGetResourceMetric", Reason: "HPA currently reports ScalingActive=False.", Score: 20}},
		OldReplicaSets:       []OldReplicaSet{{Name: "same", Namespace: "a", DesiredReplicas: &zero, Score: 21}},
	}
	before, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	p := BuildWastePresentation(a)
	want := WasteCounts{Findings: 11, DistinctResources: 10, Operational: 3, Retention: 3, Review: 5}
	if p.Counts != want {
		t.Fatalf("counts=%+v, want %+v", p.Counts, want)
	}
	after, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("adapter mutated input")
	}
	if p.ScannedAt != a.ScannedAt || !strings.Contains(p.ScanTimeLabel(), "2025-01-02") {
		t.Fatal("scan timestamp replaced")
	}
	ids := map[string]bool{}
	for i, f := range p.Findings {
		if f.Priority != float64(11+i) {
			t.Errorf("score/order changed at %d: %+v", i, f)
		}
		if ids[f.ID] {
			t.Errorf("finding identity collision: %q", f.ID)
		}
		ids[f.ID] = true
		if f.Observed == "" || f.Inference == "" || f.Limitations == "" || f.Review == "" || f.ConfidenceReason == "" {
			t.Errorf("incomplete contract: %+v", f)
		}
		if !strings.HasPrefix(f.Command, "kubectl get ") || !strings.HasSuffix(f.Command, " -o yaml") {
			t.Errorf("non-inspection command %q", f.Command)
		}
	}
	if p.Findings[5].Command != "kubectl get cronjob same -n a -o yaml" {
		t.Fatal(p.Findings[5].Command)
	}
	if p.Findings[0].Namespace != "" || p.Findings[0].Name != "same" {
		t.Fatal("namespace resource identity lost")
	}
}

func TestWastePresentationSemanticContracts(t *testing.T) {
	zero := int32(0)
	a := &WasteAudit{
		AbandonedNamespaces:  []AbandonedNamespace{{Name: "ns", PodCount: 2}},
		StalePods:            []StalePod{{Name: "pod", Kind: StalePodIdle}, {Name: "failure", Kind: StalePodZombie, Status: "ProbeFailure"}},
		OrphanedPVCs:         []OrphanedPVC{{Name: "pending", Status: PVCNeverBound}, {Name: "lost", Status: PVCReleased}, {Name: "bound", Status: PVCBoundNoPod}, {Name: "unknown"}},
		OrphanedServices:     []OrphanedService{{Name: "svc", Type: "LoadBalancer"}},
		StaleJobs:            []StaleJob{{Name: "succeeded", JobStatus: "Completed", AttemptCountsKnown: true, SucceededPods: 1, FailedPods: 2}, {Name: "failed", JobStatus: "Failed"}, {Name: "schedule", IsCronJob: true, JobStatus: "NeverScheduled"}, {Name: "retention", IsCronJob: true, JobStatus: "NoHistoryLimit"}},
		ZeroReplicaWorkloads: []ZeroReplicaWorkload{{Name: "zero", Kind: "Deployment"}},
		BrokenIngresses:      []BrokenIngress{{Name: "ing", Reason: "Ingress backend currently has no ready endpoints."}},
		MisconfiguredHPAs:    []MisconfiguredHPA{{Name: "min", Condition: "AlwaysAtMin", MinReplicas: 2}, {Name: "inactive", IsActive: true, Reason: "HPA currently reports ScalingActive=False. Review the reported condition and target configuration; application impact was not measured."}},
		OldReplicaSets:       []OldReplicaSet{{Name: "nil"}, {Name: "rs", DesiredReplicas: &zero}},
	}
	expected := map[string]string{"ns": "none were in Running", "pod": "no owner reference matches", "failure": "Detector classification", "pending": "currently reports Pending", "lost": "currently reports Lost", "bound": "no currently listed Pod", "unknown": "without a recognized phase", "svc": "selector matching zero", "succeeded": "1 successful and 2 failed", "failed": "terminal failure and stopped retries were not established", "schedule": "no lastScheduleTime", "retention": "fields were absent", "zero": "currently set to zero", "ing": "no ready endpoints", "min": "at this scan", "inactive": "ScalingActive=False", "nil": "unspecified", "rs": "Desired replicas: 0"}
	for _, f := range BuildWastePresentation(a).Findings {
		if !strings.Contains(f.Observed, expected[f.Name]) {
			t.Errorf("%s observation %q missing %q", f.Name, f.Observed, expected[f.Name])
		}
		if strings.Contains(f.Observed, "Review the reported") {
			t.Errorf("review mixed into fact: %s", f.Observed)
		}
		text := f.Observed + f.Inference + f.Limitations + f.Review
		for _, bad := range []string{"safe to delete", "SAFE TO DELETE", "unused for", "zero workloads", "incurring cloud cost", "guaranteed savings", "no data loss risk", "safe to clean up", "never scaled up", "never ran", "for 40 days", "will accumulate indefinitely", "has been set to 0 replicas for"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s overclaims: %s", f.Name, bad)
			}
		}
		if f.Name == "retention" && !strings.Contains(f.Limitations, "default to 3 successful Jobs and 1 failed Job") {
			t.Fatal("default retention missing")
		}
		if f.Confidence != "Not assessed" {
			t.Fatal("unknown phase has fabricated confidence")
		}
		if f.Name == "zero" && !strings.Contains(f.Observed, "Resource age reflects when this workload was created, not how long it has been at zero replicas") {
			t.Errorf("zero-replica observation does not separate creation age from duration at zero replicas: %q", f.Observed)
		}
		if f.Name == "min" && !strings.Contains(f.Observed, "Resource age reflects when this HPA was created, not how long it has remained at minReplicas") {
			t.Errorf("AlwaysAtMin observation does not separate creation age from duration at minReplicas: %q", f.Observed)
		}
	}
}

// TestWasteCountContractFindingsMayExceedDistinctResources locks in the count
// contract audited in Phase 3: Findings counts add() calls, DistinctResources
// counts unique Kind+Namespace+Name keys, and a single Kubernetes object (a
// CronJob triggering both NeverScheduled and NoHistoryLimit) can legitimately
// own more than one finding. This is not deduplicated.
func TestWasteCountContractFindingsMayExceedDistinctResources(t *testing.T) {
	t.Run("general invariant", func(t *testing.T) {
		a := &WasteAudit{
			AbandonedNamespaces: []AbandonedNamespace{{Name: "ns"}},
			StalePods:           []StalePod{{Name: "pod", Namespace: "a", Kind: StalePodIdle}},
			OrphanedPVCs:        []OrphanedPVC{{Name: "pvc", Namespace: "a", Status: PVCBoundNoPod}},
		}
		counts := BuildWastePresentation(a).Counts
		if counts.Findings < counts.DistinctResources {
			t.Fatalf("Findings (%d) must never be less than DistinctResources (%d)", counts.Findings, counts.DistinctResources)
		}
		if counts.Findings != 3 || counts.DistinctResources != 3 {
			t.Fatalf("counts=%+v, want 3 findings across 3 distinct resources for this fixture", counts)
		}
	})

	t.Run("one CronJob legitimately emits two findings for one ResourceKey", func(t *testing.T) {
		a := &WasteAudit{
			StaleJobs: []StaleJob{
				{Name: "annual", Namespace: "batch", IsCronJob: true, JobStatus: "NeverScheduled"},
				{Name: "annual", Namespace: "batch", IsCronJob: true, JobStatus: "NoHistoryLimit"},
			},
		}
		counts := BuildWastePresentation(a).Counts
		if counts.Findings != 2 {
			t.Fatalf("Findings = %d, want 2 (one per legitimate observation about the same CronJob)", counts.Findings)
		}
		if counts.DistinctResources != 1 {
			t.Fatalf("DistinctResources = %d, want 1 (both findings share Kind+Namespace+Name)", counts.DistinctResources)
		}
		if counts.Findings < counts.DistinctResources {
			t.Fatalf("Findings (%d) must never be less than DistinctResources (%d)", counts.Findings, counts.DistinctResources)
		}
	})
}

func TestWastePresentationEmptyWarningsAndLegacyQuantities(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a        *WasteAudit
		coverage string
	}{{"missing", nil, "Audit unavailable"}, {"empty", &WasteAudit{}, "No warnings reported"}, {"warnings", &WasteAudit{DetectorWarnings: []WasteDetectorWarning{{Category: "Pods", Error: "forbidden"}}}, "Warnings reported"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := BuildWastePresentation(tc.a)
			if p.Coverage != tc.coverage || p.Counts.Findings != 0 || p.ScanTimeLabel() != "Unknown" {
				t.Fatalf("%+v", p)
			}
		})
	}
	p := BuildWastePresentation(&WasteAudit{OrphanedPVCStorageGB: 999, RequestedStorageBytes: 999, OrphanedPVCs: []OrphanedPVC{{SizeGB: 500}}})
	if p.RequestedStorageBytes != 0 || p.UnknownStorageRequests != 1 || p.Findings[0].Storage != "Unknown" {
		t.Fatal("ambiguous legacy quantities were guessed")
	}
}

func TestFormatWasteBytesUsesExplicitBinaryUnits(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1024, "1 KiB"},
		{500 << 20, "500 MiB"},
		{1536 << 20, "1.5 GiB"},
		{2 << 30, "2 GiB"},
		{2 << 40, "2 TiB"},
	} {
		if got := FormatWasteBytes(tc.bytes); got != tc.want {
			t.Errorf("FormatWasteBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestPVCBytesAggregationAndLegacyScores(t *testing.T) {
	age := metav1.NewTime(time.Now().Add(-40*24*time.Hour - time.Hour))
	cases := []struct {
		name, quantity string
		phase          corev1.PersistentVolumeClaimPhase
		score          float64
		label          string
	}{
		{"small", "500Mi", corev1.ClaimBound, 170, "500 MiB"},
		{"fractional", "1536Mi", corev1.ClaimBound, 20.3, "1.5 GiB"},
		{"pending", "512Mi", corev1.ClaimPending, 228.8, "512 MiB"},
		{"lost", "1Gi", corev1.ClaimLost, 28, "1 GiB"},
		{"unknown", "1Ki", "", 12, "1 KiB"}, // unrecognized phase: explicit PVCUnrecognizedPhase fallback, score = ageDays*0.3 (40*0.3), not a fabricated 0.
		{"missing", "", corev1.ClaimBound, 20, "Unknown"},
	}
	var objects []interface{}
	var total int64
	for _, c := range cases {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: "app", CreationTimestamp: age}, Status: corev1.PersistentVolumeClaimStatus{Phase: c.phase}}
		if c.quantity != "" {
			q := resource.MustParse(c.quantity)
			pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: q}
			total += q.Value()
		}
		objects = append(objects, pvc)
	}
	w := newTestAuditor(7, objects...)
	a := &WasteAudit{}
	if err := w.detectOrphanedPVCs(a, ""); err != nil {
		t.Fatal(err)
	}
	if len(a.OrphanedPVCs) != len(cases) || a.RequestedStorageBytes != total {
		t.Fatalf("membership/bytes: %+v", a)
	}
	assertWasteListActions(t, w, []string{"persistentvolumeclaims", "pods"})
	p := BuildWastePresentation(a)
	if p.RequestedStorageBytes != total || p.UnknownStorageRequests != 1 {
		t.Fatalf("aggregation=%+v", p)
	}
	for _, c := range cases {
		var found bool
		for i, x := range a.OrphanedPVCs {
			if x.Name != c.name {
				continue
			}
			found = true
			if math.Abs(x.Score-c.score) > 1e-9 {
				t.Errorf("%s legacy score=%g want %g", c.name, x.Score, c.score)
			}
			if p.Findings[i].Storage != c.label {
				t.Errorf("%s storage=%s", c.name, p.Findings[i].Storage)
			}
			if x.SizeGB != int(x.RequestedBytes/(1<<30)) {
				t.Errorf("deprecated whole-GiB value incorrect: %+v", x)
			}
		}
		if !found {
			t.Errorf("missing %s", c.name)
		}
	}
	if a.OrphanedPVCs[0].Name != "pending" || a.OrphanedPVCs[1].Name != "small" {
		t.Fatal("legacy ranking changed")
	}
	if a.OrphanedPVCStorageGB != int(total/(1<<30)) {
		t.Fatal("whole GiB total truncated per item")
	}
}

func TestWasteDetectorMembershipPreservedForMisleadingStates(t *testing.T) {
	age := metav1.NewTime(time.Now().Add(-40*24*time.Hour - time.Hour))
	zero := int32(0)
	suspended := true
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "app", CreationTimestamp: age}
	}
	w := newTestAuditor(7,
		&batchv1.Job{ObjectMeta: meta("partial"), Status: batchv1.JobStatus{Active: 1, Succeeded: 1, Failed: 2}},
		&batchv1.Job{ObjectMeta: meta("retrying"), Status: batchv1.JobStatus{Active: 1, Failed: 2}},
		&batchv1.CronJob{ObjectMeta: meta("annual"), Spec: batchv1.CronJobSpec{Schedule: "0 0 1 1 *", Suspend: &suspended}},
		&appsv1.Deployment{ObjectMeta: meta("deployment"), Spec: appsv1.DeploymentSpec{Replicas: &zero}},
		&appsv1.StatefulSet{ObjectMeta: meta("statefulset"), Spec: appsv1.StatefulSetSpec{Replicas: &zero}},
		&appsv1.ReplicaSet{ObjectMeta: meta("nil")},
		&appsv1.ReplicaSet{ObjectMeta: meta("zero"), Spec: appsv1.ReplicaSetSpec{Replicas: &zero}},
	)
	a := &WasteAudit{}
	if err := w.detectStaleJobs(a, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.detectZeroReplicaWorkloads(a, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.detectOldReplicaSets(a, ""); err != nil {
		t.Fatal(err)
	}
	if len(a.StaleJobs) != 4 || len(a.ZeroReplicaWorkloads) != 2 || len(a.OldReplicaSets) != 2 {
		t.Fatalf("membership changed: %+v", a)
	}
	scores := map[string]float64{"partial": 24, "retrying": 30, "annual/NeverScheduled": 16, "annual/NoHistoryLimit": 8}
	for _, x := range a.StaleJobs {
		if x.IsCronJob && (x.Schedule != "0 0 1 1 *" || x.Suspended == nil || !*x.Suspended) {
			t.Fatalf("CronJob metadata lost: %+v", x)
		}
		key := x.Name
		if x.IsCronJob {
			key += "/" + x.JobStatus
		}
		if x.Score != scores[key] {
			t.Errorf("%s score %g", key, x.Score)
		}
	}
	for _, x := range a.ZeroReplicaWorkloads {
		want := 20.
		if x.Kind == "StatefulSet" {
			want = 30
		}
		if x.Score != want {
			t.Errorf("zero replica score %g", x.Score)
		}
		if strings.Contains(x.Reason, "for 40 days") {
			t.Fatal(x.Reason)
		}
		if !strings.Contains(x.Reason, "created 40 days ago") || !strings.Contains(x.Reason, "how long it has been at zero replicas was not established") {
			t.Fatalf("reason does not explicitly separate creation age from duration at zero replicas: %q", x.Reason)
		}
	}
	for _, x := range a.OldReplicaSets {
		if x.Score != 12 {
			t.Fatal("RS score changed")
		}
	}
	assertWasteListActions(t, w, []string{"jobs", "cronjobs", "deployments", "statefulsets", "replicasets"})
	if got := []string{a.StaleJobs[0].Name, a.StaleJobs[1].Name, a.StaleJobs[2].JobStatus, a.StaleJobs[3].JobStatus}; !reflect.DeepEqual(got, []string{"retrying", "partial", "NeverScheduled", "NoHistoryLimit"}) {
		t.Fatalf("Job ranking changed: %v", got)
	}
	if a.ZeroReplicaWorkloads[0].Kind != "StatefulSet" {
		t.Fatal("zero-replica ranking changed")
	}
}

func TestWasteCLIContract(t *testing.T) {
	// Capture the unchanged stdout-oriented entry point without a pipe-size limit.
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	previous := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = previous }()
	PrintWasteAudit(&WasteAudit{OldReplicaSets: []OldReplicaSet{{Name: "history"}}, DetectorWarnings: []WasteDetectorWarning{{Category: "Jobs", Error: "forbidden"}}}, 7)
	os.Stdout = previous
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"Finding count: 1 | Distinct resource count: 1", "Housekeeping/retention findings: 1", "Scanned: Unknown", "Check warning: Jobs: forbidden", "history", "Observed:", "Inference:", "Limitations:", "Review:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"SAFE TO DELETE", "Cluster looks clean", "safe to delete"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsupported %q", bad)
		}
	}
}

func assertWasteListActions(t *testing.T, w *WasteAuditor, resources []string) {
	t.Helper()
	actions := w.clientset.(*fake.Clientset).Actions()
	var got []string
	for _, a := range actions {
		if a.GetVerb() != "list" || a.GetNamespace() != "" {
			t.Fatalf("unexpected API action: %#v", a)
		}
		got = append(got, a.GetResource().Resource)
	}
	if !reflect.DeepEqual(got, resources) {
		t.Fatalf("LIST actions=%v, want %v", got, resources)
	}
}

func captureWasteCLI(t *testing.T, a *WasteAudit) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	previous := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = previous }()
	PrintWasteAudit(a, 7)
	os.Stdout = previous
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWasteCLIWarningOnlyAndCategoryStructure(t *testing.T) {
	if out := captureWasteCLI(t, nil); !strings.Contains(out, "Coverage: Audit unavailable") || !strings.Contains(out, "Scanned: Unknown") {
		t.Fatal("missing audit confused with completed scan")
	}

	out := captureWasteCLI(t, &WasteAudit{DetectorWarnings: []WasteDetectorWarning{{Category: "Pods", Error: "forbidden"}}})
	for _, want := range []string{"Finding count: 0 | Distinct resource count: 0", "Scanned: Unknown", "Check warning: Pods: forbidden", "No findings were reported by the available checks"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning-only output missing %q", want)
		}
	}
	a := &WasteAudit{
		AbandonedNamespaces:  []AbandonedNamespace{{Name: "namespace-marker", PodCount: 3}},
		StalePods:            []StalePod{{Name: "idle-marker", Kind: StalePodIdle, Score: 100}, {Name: "zombie-marker", Kind: StalePodZombie, RestartCount: 8, Score: 1}},
		OrphanedPVCs:         []OrphanedPVC{{Name: "pvc-marker", Status: PVCNeverBound, RequestKnown: true, RequestedBytes: 500 << 20}},
		StaleJobs:            []StaleJob{{Name: "cron-marker", Namespace: "app", IsCronJob: true, JobStatus: "NoHistoryLimit"}},
		ZeroReplicaWorkloads: []ZeroReplicaWorkload{{Name: "zero-marker", Kind: "StatefulSet"}},
		OrphanedServices:     []OrphanedService{{Name: "service-marker", Type: "LoadBalancer", IsLB: true}},
		BrokenIngresses:      []BrokenIngress{{Name: "ingress-marker", Hosts: []string{"example.test"}}},
		MisconfiguredHPAs:    []MisconfiguredHPA{{Name: "hpa-marker", TargetName: "target-marker", MinReplicas: 2, MaxReplicas: 9}},
	}
	for i := 0; i < 11; i++ {
		a.OldReplicaSets = append(a.OldReplicaSets, OldReplicaSet{Name: fmt.Sprintf("rs-marker-%02d", i), OwnerDeployment: "owner-marker"})
	}
	out = captureWasteCLI(t, a)
	previous := -1
	for _, name := range []string{"namespace-marker", "zombie-marker", "idle-marker", "pvc-marker", "cron-marker", "zero-marker", "rs-marker-00", "service-marker", "ingress-marker", "hpa-marker"} {
		position := strings.Index(out, name)
		if position <= previous {
			t.Fatalf("category grouping/order changed at %q", name)
		}
		previous = position
	}
	for _, want := range []string{"rs-marker-09", "... and 1 more old ReplicaSets", "owner-marker", "target-marker", "Replicas: min=2 max=9", "example.test", "Restarts: 8", "500 MiB", "kubectl get cronjob cron-marker -n app -o yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("metadata/command missing %q", want)
		}
	}
	for _, bad := range []string{"rs-marker-10", "500GB", "SAFE TO DELETE", "safe to delete", "unused for", "never ran", "rollout leftovers", "incurring cloud cost", "Active operational findings"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsupported or unintended output %q", bad)
		}
	}
	if !strings.Contains(out, "Finding count: 20 | Distinct resource count: 20") {
		t.Fatal("CLI total omits findings")
	}
}

func TestWastePresentationRetainsSelectorAndCronJobMetadata(t *testing.T) {
	suspended := true
	a := &WasteAudit{
		OrphanedServices: []OrphanedService{{Name: "service", Type: "ClusterIP", Selector: map[string]string{"app": "api"}}},
		StaleJobs:        []StaleJob{{Name: "cron", IsCronJob: true, JobStatus: "NeverScheduled", Schedule: "0 0 1 1 *", Suspended: &suspended}},
	}
	before, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	p := BuildWastePresentation(a)
	if !strings.Contains(p.Findings[0].Observed, "Selector: map[app:api]") || !strings.Contains(p.Findings[1].Observed, `Schedule: "0 0 1 1 *". Suspended: true.`) {
		t.Fatalf("resource metadata lost: %+v", p.Findings)
	}
	after, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("adapter mutated nested metadata")
	}
}
