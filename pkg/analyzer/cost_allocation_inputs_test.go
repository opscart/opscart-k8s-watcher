package analyzer

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testContainer(name, cpu, mem string) corev1.Container {
	reqs := corev1.ResourceList{}
	if cpu != "" {
		reqs[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		reqs[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: reqs}}
}

func restartableInit(name, cpu, mem string) corev1.Container {
	c := testContainer(name, cpu, mem)
	policy := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &policy
	return c
}

func boolp(v bool) *bool { return &v }

func owner(kind, name string, uid types.UID, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name, UID: uid, Controller: boolp(controller)}
}

func TestEffectivePodRequests(t *testing.T) {
	tests := []struct {
		name    string
		pod     corev1.Pod
		wantCPU int64
		wantMem int64
	}{
		{
			name: "regular containers sum",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				testContainer("a", "250m", "128Mi"), testContainer("b", "750m", "256Mi"),
			}}},
			wantCPU: 1000, wantMem: 384 * 1024 * 1024,
		},
		{
			name: "simple init exceeds app sum",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{testContainer("app", "500m", "128Mi")},
				InitContainers: []corev1.Container{testContainer("init", "2", "1Gi")},
			}},
			wantCPU: 2000, wantMem: 1024 * 1024 * 1024,
		},
		{
			name: "app sum exceeds init",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{testContainer("a", "1", "1Gi"), testContainer("b", "1", "1Gi")},
				InitContainers: []corev1.Container{testContainer("init", "500m", "128Mi")},
			}},
			wantCPU: 2000, wantMem: 2 * 1024 * 1024 * 1024,
		},
		{
			name: "multiple non restartable init use peak not sum",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{testContainer("app", "100m", "64Mi")},
				InitContainers: []corev1.Container{
					testContainer("i1", "1", "256Mi"), testContainer("i2", "2", "128Mi"),
				},
			}},
			wantCPU: 2000, wantMem: 256 * 1024 * 1024,
		},
		{
			name: "restartable init before normal init contributes to stage",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{testContainer("app", "500m", "128Mi")},
				InitContainers: []corev1.Container{
					restartableInit("sidecar", "300m", "64Mi"),
					testContainer("init", "1", "512Mi"),
				},
			}},
			wantCPU: 1300, wantMem: 576 * 1024 * 1024,
		},
		{
			name: "multiple restartable init accumulate",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{testContainer("app", "100m", "64Mi")},
				InitContainers: []corev1.Container{
					restartableInit("s1", "200m", "32Mi"),
					restartableInit("s2", "300m", "64Mi"),
				},
			}},
			wantCPU: 600, wantMem: 160 * 1024 * 1024,
		},
		{
			name: "restartable init included in application sum",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{testContainer("app", "1", "1Gi")},
				InitContainers: []corev1.Container{restartableInit("sidecar", "500m", "512Mi")},
			}},
			wantCPU: 1500, wantMem: 1536 * 1024 * 1024,
		},
		{
			name: "overhead added after max",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{testContainer("app", "500m", "128Mi")},
				Overhead: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
			}},
			wantCPU: 600, wantMem: 160 * 1024 * 1024,
		},
		{
			name: "missing requests are zero and ephemeral containers ignored",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app"}},
				EphemeralContainers: []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name: "debug",
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10"),
							corev1.ResourceMemory: resource.MustParse("10Gi"),
						}},
					},
				}},
			}},
			wantCPU: 0, wantMem: 0,
		},
		{
			name: "cpu and memory independently select maxima",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{testContainer("app", "2", "128Mi")},
				InitContainers: []corev1.Container{testContainer("init", "500m", "2Gi")},
			}},
			wantCPU: 2000, wantMem: 2 * 1024 * 1024 * 1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := EffectivePodRequests(tc.pod)
			if cpu != tc.wantCPU || mem != tc.wantMem {
				t.Fatalf("EffectivePodRequests() = (%d, %d), want (%d, %d)", cpu, mem, tc.wantCPU, tc.wantMem)
			}
		})
	}
}

func TestClassifyPodCostEligibility(t *testing.T) {
	known := map[string]struct{}{"node-a": {}}
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name            string
		pod             corev1.Pod
		wantEligibility PodCostEligibility
		wantReason      PodCostReason
	}{
		{"running bound", corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, PodCostEligible, PodCostReasonBoundRunning},
		{"running unbound", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, PodCostUnresolved, PodCostReasonUnassignedRunning},
		{"pending bound", corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, PodCostEligible, PodCostReasonBoundPending},
		{"pending unbound", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, PodCostUnresolved, PodCostReasonUnassignedPending},
		{"succeeded", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, PodCostExcluded, PodCostReasonSucceeded},
		{"failed", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, PodCostExcluded, PodCostReasonFailed},
		{"deleting running", corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}, Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, PodCostExcluded, PodCostReasonDeleting},
		{"unknown bound", corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, PodCostEligible, PodCostReasonUnknownPhaseBound},
		{"unknown unbound", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, PodCostUnresolved, PodCostReasonUnknownPhaseUnbound},
		{"node absent", corev1.Pod{Spec: corev1.PodSpec{NodeName: "gone"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, PodCostUnresolved, PodCostReasonNodeMissing},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEligibility, gotReason, _ := ClassifyPodCostEligibility(tc.pod, known)
			if gotEligibility != tc.wantEligibility || gotReason != tc.wantReason {
				t.Fatalf("got (%q, %q), want (%q, %q)", gotEligibility, gotReason, tc.wantEligibility, tc.wantReason)
			}
		})
	}
}

func TestResolvePodWorkload(t *testing.T) {
	rsUID := types.UID("rs-uid")
	depUID := types.UID("dep-uid")
	jobUID := types.UID("job-uid")
	cronUID := types.UID("cron-uid")

	indexes := NewControllerIndexes(
		[]appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "web-rs", UID: rsUID,
				OwnerReferences: []metav1.OwnerReference{owner("Deployment", "web", depUID, true)},
			},
		}},
		[]batchv1.Job{{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "backup-123", UID: jobUID,
				OwnerReferences: []metav1.OwnerReference{owner("CronJob", "backup", cronUID, true)},
			},
		}},
	)

	t.Run("controller owner need not be first", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "p",
			OwnerReferences: []metav1.OwnerReference{
				owner("ConfigMap", "not-controller", "x", false),
				owner("StatefulSet", "db", "sts", true),
			},
		}}
		kind, name, _, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "StatefulSet" || name != "db" || res.Status != OwnerResolutionResolved {
			t.Fatalf("unexpected resolution: %s/%s %+v", kind, name, res)
		}
	})

	t.Run("bare pod", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare", UID: "pod-uid"}}
		kind, name, uid, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "Pod" || name != "bare" || uid != "pod-uid" || res.Status != OwnerResolutionBarePod {
			t.Fatalf("unexpected bare pod resolution: %s/%s/%s %+v", kind, name, uid, res)
		}
	})

	t.Run("ambiguous controllers", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", OwnerReferences: []metav1.OwnerReference{
			owner("StatefulSet", "a", "a", true), owner("DaemonSet", "b", "b", true),
		}}}
		kind, _, _, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "UnknownOwner" || res.Status != OwnerResolutionAmbiguous {
			t.Fatalf("unexpected ambiguous resolution: %s %+v", kind, res)
		}
	})

	t.Run("replicaset to deployment", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "web-rs", rsUID, true)}}}
		kind, name, uid, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "Deployment" || name != "web" || uid != depUID || res.Status != OwnerResolutionResolved {
			t.Fatalf("unexpected rs resolution: %s/%s/%s %+v", kind, name, uid, res)
		}
	})

	t.Run("unresolved replicaset preserves name", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "api-7d9f9c", "missing", true)}}}
		kind, name, _, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "ReplicaSet" || name != "api-7d9f9c" || res.Status != OwnerResolutionUnresolvedReplicaSet {
			t.Fatalf("unexpected unresolved rs: %s/%s %+v", kind, name, res)
		}
	})

	for _, kind := range []string{"StatefulSet", "DaemonSet"} {
		t.Run(kind, func(t *testing.T) {
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner(kind, "thing", "uid", true)}}}
			gotKind, gotName, _, _, _, _, res := ResolvePodWorkload(pod, indexes)
			if gotKind != kind || gotName != "thing" || res.Status != OwnerResolutionResolved {
				t.Fatalf("unexpected direct resolution: %s/%s %+v", gotKind, gotName, res)
			}
		})
	}

	t.Run("job with cronjob parent", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", OwnerReferences: []metav1.OwnerReference{owner("Job", "backup-123", jobUID, true)}}}
		kind, name, uid, pk, pn, puid, res := ResolvePodWorkload(pod, indexes)
		if kind != "Job" || name != "backup-123" || uid != jobUID || pk != "CronJob" || pn != "backup" || puid != cronUID || res.Status != OwnerResolutionResolved {
			t.Fatalf("unexpected job resolution: %s/%s parent=%s/%s %+v", kind, name, pk, pn, res)
		}
	})

	t.Run("custom controller", func(t *testing.T) {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner("Rollout", "checkout", "rollout-uid", true)}}}
		kind, name, uid, _, _, _, res := ResolvePodWorkload(pod, indexes)
		if kind != "Rollout" || name != "checkout" || uid != "rollout-uid" || res.Status != OwnerResolutionResolved {
			t.Fatalf("unexpected custom resolution: %s/%s/%s %+v", kind, name, uid, res)
		}
	})
}

func completeAzureNode() corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{
			"agentpool":                             "userpool",
			"node.kubernetes.io/instance-type":      "Standard_D4s_v3",
			"kubernetes.azure.com/scalesetpriority": "regular",
			"topology.kubernetes.io/region":         "centralus",
			"kubernetes.io/os":                      "linux",
			"kubernetes.io/arch":                    "amd64",
		}},
		Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/x/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"},
	}
}

func TestBuildNodeCostPoolKey(t *testing.T) {
	node := completeAzureNode()
	key, reasons := BuildNodeCostPoolKey(node)
	if key == nil || len(reasons) != 0 {
		t.Fatalf("expected resolved key, got key=%+v reasons=%v", key, reasons)
	}
	if key.Provider != "azure" || key.PoolName != "userpool" || key.InstanceType != "Standard_D4s_v3" ||
		key.CapacityType != "regular" || key.Region != "centralus" || key.OS != "linux" || key.Architecture != "amd64" {
		t.Fatalf("unexpected key: %+v", key)
	}

	mutations := []struct {
		name   string
		mutate func(*corev1.Node)
	}{
		{"region", func(n *corev1.Node) { n.Labels["topology.kubernetes.io/region"] = "eastus" }},
		{"capacity", func(n *corev1.Node) { n.Labels["kubernetes.azure.com/scalesetpriority"] = "spot" }},
		{"os", func(n *corev1.Node) { n.Labels["kubernetes.io/os"] = "windows" }},
		{"arch", func(n *corev1.Node) { n.Labels["kubernetes.io/arch"] = "arm64" }},
		{"instance", func(n *corev1.Node) { n.Labels["node.kubernetes.io/instance-type"] = "Standard_D8s_v3" }},
	}
	for _, tc := range mutations {
		t.Run("identity differs by "+tc.name, func(t *testing.T) {
			other := completeAzureNode()
			tc.mutate(&other)
			otherKey, otherReasons := BuildNodeCostPoolKey(other)
			if otherKey == nil || len(otherReasons) != 0 {
				t.Fatalf("unexpected unresolved key: %v", otherReasons)
			}
			if *otherKey == *key {
				t.Fatalf("expected key to differ by %s", tc.name)
			}
		})
	}

	t.Run("missing metadata stays unresolved", func(t *testing.T) {
		broken := completeAzureNode()
		delete(broken.Labels, "kubernetes.azure.com/scalesetpriority")
		got, reasons := BuildNodeCostPoolKey(broken)
		if got != nil {
			t.Fatalf("expected unresolved key, got %+v", got)
		}
		found := false
		for _, reason := range reasons {
			if reason == "capacity type" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected capacity type reason, got %v", reasons)
		}
	})
}

func TestBuildPodCostInputRequiresResolvedPool(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{testContainer("app", "250m", "128Mi")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	known := map[string]struct{}{"node-a": {}}
	input := BuildPodCostInput(pod, known, map[string]*CostPoolKey{}, ControllerIndexes{})
	if input.Eligibility != PodCostUnresolved || input.EligibilityReason != PodCostReasonNodeMissing || input.PoolKey != nil {
		t.Fatalf("expected unresolved pool input, got %+v", input)
	}
}
