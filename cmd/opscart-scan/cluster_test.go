package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRecentPodEventMessages_ReturnsMessages(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "sample-worker.startup", Namespace: "prod"},
			InvolvedObject: corev1.ObjectReference{
				Kind: "Pod", Name: "sample-worker-abc123", Namespace: "prod",
			},
			Type:    "Warning",
			Reason:  "Unhealthy",
			Message: "Startup probe failed: HTTP probe failed with statuscode: 500",
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "sample-worker.killing", Namespace: "prod"},
			InvolvedObject: corev1.ObjectReference{
				Kind: "Pod", Name: "sample-worker-abc123", Namespace: "prod",
			},
			Type:    "Normal",
			Reason:  "Killing",
			Message: "Container app failed startup probe, will be restarted",
		},
	)

	messages, err := recentPodEventMessages(clientset, "prod", "sample-worker-abc123")
	if err != nil {
		t.Fatalf("recentPodEventMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(messages), messages)
	}

	want := map[string]bool{
		"Startup probe failed: HTTP probe failed with statuscode: 500": true,
		"Container app failed startup probe, will be restarted":        true,
	}
	for _, m := range messages {
		if !want[m] {
			t.Errorf("unexpected message %q", m)
		}
	}
}

func TestRecentPodEventMessages_NoEvents(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	messages, err := recentPodEventMessages(clientset, "prod", "quiet-pod")
	if err != nil {
		t.Fatalf("recentPodEventMessages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected no messages, got %+v", messages)
	}
}
