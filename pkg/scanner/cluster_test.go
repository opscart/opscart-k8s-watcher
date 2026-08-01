package scanner

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
