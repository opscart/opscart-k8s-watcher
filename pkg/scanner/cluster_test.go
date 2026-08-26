package scanner

import (
	"errors"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestFailedPodEvidenceUsesObservedTermination(t *testing.T) {
	finished := metav1.NewTime(time.Date(2026, 7, 19, 20, 49, 13, 0, time.UTC))
	pod := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", Message: "process exited with status 2", FinishedAt: finished,
		}},
	}}}}
	message, observedAt := failedPodEvidence(pod)
	if message != "process exited with status 2" || !observedAt.Equal(finished.Time) {
		t.Fatalf("failed evidence = %q at %s", message, observedAt)
	}
}

func TestFailedPodEvidenceFallbackIsNotBlank(t *testing.T) {
	message, _ := failedPodEvidence(corev1.Pod{})
	if message != "Pod phase is Failed; no termination reason was available." {
		t.Fatalf("fallback = %q", message)
	}
}

func TestPodWorkloadOwnerFollowsExplicitJobToCronJob(t *testing.T) {
	controller := true
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", OwnerReferences: []metav1.OwnerReference{{
		Kind: "Job", Name: "backup-123", Controller: &controller,
	}}}}
	owner := podWorkloadOwner(pod, map[string]workloadOwner{
		"batch/backup-123": {kind: "CronJob", name: "backup"},
	})
	if owner.kind != "CronJob" || owner.name != "backup" {
		t.Fatalf("owner = %+v", owner)
	}
}

func TestPodWorkloadOwnerDoesNotInventMissingOwner(t *testing.T) {
	owner := podWorkloadOwner(corev1.Pod{}, nil)
	if owner != (workloadOwner{}) {
		t.Fatalf("invented owner: %+v", owner)
	}
}

func testControllerRef(kind, name string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{Kind: kind, Name: name, Controller: &controller}
}

func testReplicaSet(namespace, name, deployment string) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
		OwnerReferences: []metav1.OwnerReference{
			testControllerRef("Deployment", deployment),
		},
	}}
}

func testDeployment(namespace, name string, desired, available int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3,
			AvailableReplicas:  available,
		},
	}
}

func testReplicaSetPod(namespace, name, replicaSet string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{testControllerRef("ReplicaSet", replicaSet)},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func testPodFailedIssue(namespace, name, replicaSet string) models.EmergencyIssue {
	return models.EmergencyIssue{
		Severity: "critical", Resource: "pod", Namespace: namespace, Name: name,
		Reason: "PodFailed", Message: "Container app terminated: Error",
		OwnerKind: "ReplicaSet", OwnerName: replicaSet,
	}
}

func TestPodFailedHealthyDeploymentRemovesRetainedReplica(t *testing.T) {
	failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "api-7d8f9c6b5")},
		[]corev1.Pod{failed},
		[]appsv1.ReplicaSet{testReplicaSet("apps", "api-7d8f9c6b5", "api")},
		[]appsv1.Deployment{testDeployment("apps", "api", 1, 1)},
	)
	if len(got) != 0 {
		t.Fatalf("healthy Deployment retained PodFailed findings: %+v", got)
	}
}

func TestPodFailedUnavailableDeploymentKeepsSpecificFindingOnly(t *testing.T) {
	failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
	// The current failure may belong to a newer rollout generation. Real
	// ReplicaSet -> Deployment ownership must join the two generations;
	// matching pod-name hashes cannot.
	crashing := testReplicaSetPod("apps", "api-6c7d8e9f0-live1", "api-6c7d8e9f0", corev1.PodRunning)
	crashIssue := models.EmergencyIssue{
		Severity: "critical", Resource: "pod", Namespace: "apps", Name: crashing.Name,
		Reason: "CrashLoopBackOff", Message: "crash looping",
	}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "api-7d8f9c6b5"), crashIssue},
		[]corev1.Pod{failed, crashing},
		[]appsv1.ReplicaSet{
			testReplicaSet("apps", "api-7d8f9c6b5", "api"),
			testReplicaSet("apps", "api-6c7d8e9f0", "api"),
		},
		[]appsv1.Deployment{testDeployment("apps", "api", 1, 0)},
	)
	if len(got) != 1 || got[0].Reason != "CrashLoopBackOff" {
		t.Fatalf("expected only the specific live finding, got %+v", got)
	}
}

func TestPodFailedUnavailableDeploymentTreatsHighRestartAsCurrentEvidence(t *testing.T) {
	failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
	restarting := testReplicaSetPod("apps", "api-6c7d8e9f0-live1", "api-6c7d8e9f0", corev1.PodRunning)
	restartIssue := models.EmergencyIssue{
		Severity: "medium", Resource: "pod", Namespace: "apps", Name: restarting.Name,
		Reason: "HighRestartCount", Message: "Container app has restarted 97 times", Restarts: 97,
	}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "api-7d8f9c6b5"), restartIssue},
		[]corev1.Pod{failed, restarting},
		[]appsv1.ReplicaSet{
			testReplicaSet("apps", "api-7d8f9c6b5", "api"),
			testReplicaSet("apps", "api-6c7d8e9f0", "api"),
		},
		[]appsv1.Deployment{testDeployment("apps", "api", 1, 0)},
	)
	if len(got) != 1 || got[0].Reason != "HighRestartCount" || got[0].Name != restarting.Name {
		t.Fatalf("expected only current restart evidence, got %+v", got)
	}
}

func TestPodFailedPartialAvailabilityIsNotCalledHealthy(t *testing.T) {
	failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
	ready := testReplicaSetPod("apps", "api-7d8f9c6b5-ready", "api-7d8f9c6b5", corev1.PodRunning)
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "api-7d8f9c6b5")},
		[]corev1.Pod{failed, ready},
		[]appsv1.ReplicaSet{testReplicaSet("apps", "api-7d8f9c6b5", "api")},
		[]appsv1.Deployment{testDeployment("apps", "api", 2, 1)},
	)
	if len(got) != 1 || got[0].Severity != "critical" || got[0].Reason != "PodFailed" {
		t.Fatalf("partially available Deployment was suppressed: %+v", got)
	}
	if got[0].Message == "Container app terminated: Error" {
		t.Fatalf("unavailable Deployment evidence was not added: %+v", got[0])
	}
}

func TestPodFailedScaledToZeroDeploymentRemovesRetainedReplica(t *testing.T) {
	failed := testReplicaSetPod("apps", "worker-7d8f9c6b5-old01", "worker-7d8f9c6b5", corev1.PodFailed)
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "worker-7d8f9c6b5")},
		[]corev1.Pod{failed},
		[]appsv1.ReplicaSet{testReplicaSet("apps", "worker-7d8f9c6b5", "worker")},
		[]appsv1.Deployment{testDeployment("apps", "worker", 0, 0)},
	)
	if len(got) != 0 {
		t.Fatalf("scaled-to-zero Deployment retained PodFailed findings: %+v", got)
	}
}

func TestPodFailedUnavailableDeploymentCollapsesAllTerminalReplicas(t *testing.T) {
	older := testPodFailedIssue("apps", "worker-7d8f9c6b5-z9999", "worker-7d8f9c6b5")
	newer := testPodFailedIssue("apps", "worker-7d8f9c6b5-a1111", "worker-7d8f9c6b5")
	older.FailureObservedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer.FailureObservedAt = older.FailureObservedAt.Add(time.Hour)
	pods := []corev1.Pod{
		testReplicaSetPod("apps", older.Name, "worker-7d8f9c6b5", corev1.PodFailed),
		testReplicaSetPod("apps", newer.Name, "worker-7d8f9c6b5", corev1.PodFailed),
	}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{older, newer}, pods,
		[]appsv1.ReplicaSet{testReplicaSet("apps", "worker-7d8f9c6b5", "worker")},
		[]appsv1.Deployment{testDeployment("apps", "worker", 2, 0)},
	)
	if len(got) != 1 || got[0].Name != newer.Name || got[0].Reason != "PodFailed" {
		t.Fatalf("expected one newest PodFailed representative, got %+v", got)
	}
}

func TestPodFailedDoesNotInventDeploymentForStandaloneReplicaSet(t *testing.T) {
	failed := testReplicaSetPod("apps", "worker-rs-a1111", "worker-rs", corev1.PodFailed)
	standalone := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "worker-rs"}}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "worker-rs")},
		[]corev1.Pod{failed}, []appsv1.ReplicaSet{standalone}, nil,
	)
	if len(got) != 1 || got[0].Severity != "critical" || got[0].OwnerKind != "ReplicaSet" {
		t.Fatalf("standalone ReplicaSet was misattributed or suppressed: %+v", got)
	}
}

func TestPodFailedBarePodBecomesReviewFinding(t *testing.T) {
	bare := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "debug", Name: "one-shot"}, Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	issue := models.EmergencyIssue{
		Severity: "critical", Resource: "pod", Namespace: "debug", Name: bare.Name,
		Reason: "PodFailed", Message: "Container exited: Error",
	}
	got := reconcilePodFailedAgainstDeploymentHealth([]models.EmergencyIssue{issue}, []corev1.Pod{bare}, nil, nil)
	if len(got) != 1 || got[0].Severity != "high" || got[0].Reason != "PodFailed" {
		t.Fatalf("bare terminal pod was not converted to a review finding: %+v", got)
	}
}

func TestPodFailedNonDeploymentOwnersRemainUnchanged(t *testing.T) {
	for _, ownerKind := range []string{"Job", "CronJob", "StatefulSet", "DaemonSet"} {
		t.Run(ownerKind, func(t *testing.T) {
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "apps", Name: "failed-pod",
					OwnerReferences: []metav1.OwnerReference{testControllerRef(ownerKind, "owner")},
				},
				Status: corev1.PodStatus{Phase: corev1.PodFailed},
			}
			issue := models.EmergencyIssue{
				Severity: "critical", Resource: "pod", Namespace: "apps", Name: pod.Name,
				Reason: "PodFailed", OwnerKind: ownerKind, OwnerName: "owner",
			}
			got := reconcilePodFailedAgainstDeploymentHealth([]models.EmergencyIssue{issue}, []corev1.Pod{pod}, nil, nil)
			if len(got) != 1 || got[0].Severity != "critical" || got[0].Reason != "PodFailed" {
				t.Fatalf("%s-owned PodFailed changed unexpectedly: %+v", ownerKind, got)
			}
		})
	}
}

func TestPodFailedSimilarBarePodCannotSettleDeployment(t *testing.T) {
	failed := testReplicaSetPod("apps", "worker-7d8f9c6b5-old01", "worker-7d8f9c6b5", corev1.PodFailed)
	unrelated := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "worker-7d8f9c6b5-ready"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "worker-7d8f9c6b5")},
		[]corev1.Pod{failed, unrelated},
		[]appsv1.ReplicaSet{testReplicaSet("apps", "worker-7d8f9c6b5", "worker")},
		[]appsv1.Deployment{testDeployment("apps", "worker", 1, 0)},
	)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("unrelated similarly named bare pod suppressed the Deployment failure: %+v", got)
	}
}

func TestPodFailedStaleDeploymentStatusPreservesFinding(t *testing.T) {
	failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
	deployment := testDeployment("apps", "api", 1, 1)
	deployment.Status.ObservedGeneration = deployment.Generation - 1
	got := reconcilePodFailedAgainstDeploymentHealth(
		[]models.EmergencyIssue{testPodFailedIssue("apps", failed.Name, "api-7d8f9c6b5")},
		[]corev1.Pod{failed},
		[]appsv1.ReplicaSet{testReplicaSet("apps", "api-7d8f9c6b5", "api")},
		[]appsv1.Deployment{deployment},
	)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("stale controller evidence suppressed PodFailed: %+v", got)
	}
}

func TestPodFailedControllerListErrorsFailOpen(t *testing.T) {
	for _, resource := range []string{"replicasets", "deployments"} {
		t.Run(resource, func(t *testing.T) {
			failed := testReplicaSetPod("apps", "api-7d8f9c6b5-old01", "api-7d8f9c6b5", corev1.PodFailed)
			replicaSet := testReplicaSet("apps", "api-7d8f9c6b5", "api")
			client := fake.NewSimpleClientset(&failed, &replicaSet)
			client.PrependReactor("list", resource, func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("forbidden")
			})

			scanner := NewScannerWithClientset(client, "test")
			got, err := scanner.FindEmergencyIssues("")
			if err != nil {
				t.Fatalf("FindEmergencyIssues returned error: %v", err)
			}
			if len(got) != 1 || got[0].Reason != "PodFailed" || got[0].Severity != "critical" {
				t.Fatalf("%s API failure hid or changed PodFailed: %+v", resource, got)
			}
		})
	}
}
