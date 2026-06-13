package analyzer

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestAuditor creates a WasteAuditor backed by a fake Kubernetes client.
func newTestAuditor(minAgeDays int, objs ...interface{}) *WasteAuditor {
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o.(runtime.Object))
	}
	return &WasteAuditor{
		clientset:  fake.NewSimpleClientset(runtimeObjs...),
		minAgeDays: minAgeDays,
		ctx:        context.Background(),
	}
}

func TestZombieBypassesMinAge(t *testing.T) {
	tests := []struct {
		name          string
		waitingReason string
		wantZombie    bool
	}{
		{"CrashLoopBackOff", "CrashLoopBackOff", true},
		{"OOMKilled", "OOMKilled", true},
		{"ImagePullBackOff", "ImagePullBackOff", true},
		{"Error", "Error", true},
		{"healthy running pod not flagged", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "crash-pod",
					Namespace: "default",
					// Age = 0 — well below minAgeDays=7
					CreationTimestamp: metav1.Now(),
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			}

			if tc.waitingReason != "" {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name:         "app",
						RestartCount: 5,
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason: tc.waitingReason,
							},
						},
					},
				}
			}

			wa := newTestAuditor(7, pod)
			audit, err := wa.AuditWaste("")
			if err != nil {
				t.Fatalf("AuditWaste: %v", err)
			}

			found := false
			for _, sp := range audit.StalePods {
				if sp.Kind == StalePodZombie && sp.Name == "crash-pod" {
					found = true
					break
				}
			}
			if found != tc.wantZombie {
				t.Errorf("zombie found = %v, want %v; StalePods = %+v", found, tc.wantZombie, audit.StalePods)
			}
		})
	}
}

func TestDefaultNamespaceNotSkipped(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-crasher",
			Namespace: "default",
			// 2 days old — above minAgeDays=1 but the zombie path ignores age anyway
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 20,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}

	wa := newTestAuditor(1, pod)
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	found := false
	for _, sp := range audit.StalePods {
		if sp.Namespace == "default" && sp.Name == "default-crasher" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pod in 'default' namespace not detected; StalePods = %+v", audit.StalePods)
	}
}
