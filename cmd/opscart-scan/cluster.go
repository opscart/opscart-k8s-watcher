package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// recentPodEventMessages fetches the Kubernetes events recorded against a
// specific pod and returns just their Message text — all
// hasProbeFailureSignature (emergency.go) needs to check.
//
// This is a namespaced, field-selector query — the same pattern
// cmd/opscart-dashboard/investigation.go's podEvents uses — rather than
// listing cluster-wide events and filtering client-side, since the CLI
// had no existing per-pod event fetch of its own to reuse.
//
// clientset is accepted as the kubernetes.Interface rather than the
// concrete *kubernetes.Clientset (which is what getKubernetesClient
// returns and satisfies) so this can be exercised against a fake
// clientset in tests without touching a real cluster.
func recentPodEventMessages(clientset kubernetes.Interface, namespace, podName string) ([]string, error) {
	evList, err := clientset.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		return nil, err
	}

	messages := make([]string, 0, len(evList.Items))
	for _, e := range evList.Items {
		messages = append(messages, e.Message)
	}
	return messages, nil
}
