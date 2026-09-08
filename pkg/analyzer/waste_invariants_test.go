package analyzer

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// The golden projection was captured from the pre-Phase-1 HEAD detector using
// this same fixture. It excludes intentionally corrected prose/quantities and
// wall-clock timestamps, but retains every identity, subtype, score, slice order,
// and Kubernetes action (including scope and selectors). Do not regenerate it
// from changed detectors to bless a behavior change.
type wasteInvariantFinding struct {
	Category, Namespace, Name, Subtype string
	Score                              float64
}
type wasteInvariantAction struct {
	Verb, Group, Version, Resource, Namespace, Name, Labels, Fields string
}
type wasteInvariantResult struct {
	Findings          []wasteInvariantFinding
	Actions           []wasteInvariantAction
	WarningCategories []string
	LegacyCount       int
}

func wasteInvariantFixture() []interface{} {
	zero, one := int32(0), int32(1)
	yes := true
	meta := func(name string, days int) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "app", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(days)*24*time.Hour - time.Hour))}
	}
	idle := &corev1.Pod{ObjectMeta: meta("bare", 40), Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	managed := idle.DeepCopy()
	managed.Name = "managed"
	managed.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "owner"}}
	pvc := func(name, quantity string, phase corev1.PersistentVolumeClaimPhase, days int) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: meta(name, days), Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(quantity)}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: phase}}
	}
	managed.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "referenced"}}}}
	job := &batchv1.Job{ObjectMeta: meta("partial", 40), Status: batchv1.JobStatus{Active: 1, Succeeded: 1, Failed: 2}}
	retry := job.DeepCopy()
	retry.Name = "retrying"
	retry.Status.Succeeded = 0
	activeJob := job.DeepCopy()
	activeJob.Name = "active-only"
	activeJob.Status = batchv1.JobStatus{Active: 1}
	cron := &batchv1.CronJob{ObjectMeta: meta("annual", 40), Spec: batchv1.CronJobSpec{Schedule: "0 0 1 1 *", Suspend: &yes}}
	recent := cron.DeepCopy()
	recent.Name = "recent-cron"
	recent.CreationTimestamp = meta("", 1).CreationTimestamp
	rs := &appsv1.ReplicaSet{ObjectMeta: meta("history-zero", 40), Spec: appsv1.ReplicaSetSpec{Replicas: &zero}}
	rsNil := rs.DeepCopy()
	rsNil.Name = "history-nil"
	rsNil.Spec.Replicas = nil
	rsLive := rs.DeepCopy()
	rsLive.Name = "live-rs"
	rsLive.Spec.Replicas = &one
	crash := crashLoopPod("crashing", "app")
	crash.CreationTimestamp = meta("", 1).CreationTimestamp
	objects := []interface{}{
		oldNamespace("empty", 40), oldNamespace("app", 40), oldNamespace("young", 1), oldNamespace("kube-system", 40),
		idle, managed, crash,
		pvc("small", "500Mi", corev1.ClaimBound, 40), pvc("fractional", "1536Mi", corev1.ClaimBound, 40),
		pvc("pending", "512Mi", corev1.ClaimPending, 40), pvc("lost", "1Gi", corev1.ClaimLost, 40),
		pvc("referenced", "1Gi", corev1.ClaimBound, 40), pvc("young-pvc", "1Gi", corev1.ClaimBound, 1),
		job, retry, activeJob, cron, recent,
		&appsv1.Deployment{ObjectMeta: meta("zero-deploy", 40), Spec: appsv1.DeploymentSpec{Replicas: &zero}},
		&appsv1.Deployment{ObjectMeta: meta("live-deploy", 40), Spec: appsv1.DeploymentSpec{Replicas: &one}},
		&appsv1.StatefulSet{ObjectMeta: meta("zero-stateful", 40), Spec: appsv1.StatefulSetSpec{Replicas: &zero}},
		rs, rsNil, rsLive,
		oldService("unmatched", "app", map[string]string{"app": "missing"}, corev1.ServiceTypeLoadBalancer),
		oldService("no-selector", "app", nil, corev1.ServiceTypeClusterIP),
		ingressFixture("backend", "app", "app.example", "unmatched", 0),
		hpaFixture("inactive", "app", 1, 1, 1, 1, []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: "ScalingActive", Status: "False", Reason: "FailedGetResourceMetric"}}),
		hpaFixture("at-min", "app", 40, 1, 1, 1, nil), hpaFixture("healthy", "app", 1, 1, 3, 3, nil),
	}
	return objects
}

func captureWasteInvariant(t *testing.T, mode string) wasteInvariantResult {
	t.Helper()
	objects := wasteInvariantFixture()
	w := newTestAuditor(7, objects...)
	client := w.clientset.(*fake.Clientset)
	if mode == "shared" {
		var pods []corev1.Pod
		for _, object := range objects {
			if pod, ok := object.(*corev1.Pod); ok {
				pods = append(pods, *pod)
			}
		}
		w.WithPodSnapshot(pods, true)
	}
	if mode == "partial" {
		client.PrependReactor("list", "endpointslices", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, context.DeadlineExceeded })
	}
	if mode == "hpa-v1-fallback" {
		client.PrependReactor("list", "horizontalpodautoscalers", func(a ktesting.Action) (bool, runtime.Object, error) {
			if a.GetResource().Version == "v2" {
				return true, nil, context.DeadlineExceeded
			}
			return false, nil, nil
		})
	}
	namespace := ""
	if mode == "scoped" {
		namespace = "app"
	}
	a, err := w.AuditWaste(namespace)
	if err != nil {
		t.Fatal(err)
	}
	result := wasteInvariantResult{LegacyCount: a.TotalWasteItems}
	add := func(category, ns, name, subtype string, score float64) {
		// Rounded to 6 decimal places: this baseline locks in detector
		// identity, slice order, and API actions, not bit-exact float64
		// reproducibility. Some Score expressions (e.g. ageDays*0.4 +
		// restarts*0.3) can land on adjacent float64 values a single ULP
		// apart depending on the compiler's multiply-add fusion decisions,
		// which differ across architectures (observed: darwin/arm64 vs a
		// linux/amd64 CI runner). Rounding is far coarser than that noise
		// and far finer than any real score difference that should matter.
		score = math.Round(score*1e6) / 1e6
		result.Findings = append(result.Findings, wasteInvariantFinding{category, ns, name, subtype, score})
	}
	for _, x := range a.AbandonedNamespaces {
		add("namespace", "", x.Name, "", x.Score)
	}
	for _, x := range a.StalePods {
		add("pod", x.Namespace, x.Name, string(x.Kind)+"/"+x.Status+"/"+x.Severity, x.Score)
	}
	for _, x := range a.OrphanedPVCs {
		add("pvc", x.Namespace, x.Name, string(x.Status), x.Score)
	}
	for _, x := range a.StaleJobs {
		add("job", x.Namespace, x.Name, x.JobStatus, x.Score)
	}
	for _, x := range a.ZeroReplicaWorkloads {
		add("zero", x.Namespace, x.Name, x.Kind, x.Score)
	}
	for _, x := range a.OldReplicaSets {
		add("rs", x.Namespace, x.Name, "", x.Score)
	}
	for _, x := range a.OrphanedServices {
		add("service", x.Namespace, x.Name, x.Type, x.Score)
	}
	for _, x := range a.BrokenIngresses {
		add("ingress", x.Namespace, x.Name, "", x.Score)
	}
	for _, x := range a.MisconfiguredHPAs {
		add("hpa", x.Namespace, x.Name, x.Condition, x.Score)
	}
	for _, warning := range a.DetectorWarnings {
		result.WarningCategories = append(result.WarningCategories, warning.Category)
	}
	for _, action := range client.Actions() {
		r := action.GetResource()
		out := wasteInvariantAction{Verb: action.GetVerb(), Group: r.Group, Version: r.Version, Resource: r.Resource, Namespace: action.GetNamespace()}
		if list, ok := action.(ktesting.ListAction); ok {
			restrictions := list.GetListRestrictions()
			out.Labels = restrictions.Labels.String()
			out.Fields = restrictions.Fields.String()
		}
		if get, ok := action.(ktesting.GetAction); ok {
			out.Name = get.GetName()
		}
		result.Actions = append(result.Actions, out)
	}
	return result
}

func TestWastePhase1MatchesPrePhase1Baseline(t *testing.T) {
	data, err := os.ReadFile("testdata/waste_phase1_baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var baseline map[string]wasteInvariantResult
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy", "shared", "scoped", "partial", "hpa-v1-fallback"} {
		t.Run(mode, func(t *testing.T) {
			got := captureWasteInvariant(t, mode)
			if !reflect.DeepEqual(got, baseline[mode]) {
				b, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("detector membership, scores/order or API actions differ from pre-Phase-1 baseline:\n%s", b)
			}
			lists := 0
			for _, action := range got.Actions {
				if action.Verb == "list" {
					lists++
				}
			}
			t.Logf("unchanged: %d findings; %d API actions (%d LISTs)", len(got.Findings), len(got.Actions), lists)
		})
	}
}
