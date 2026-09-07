package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func investigationReplicaSet(namespace, name, deployment string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
		OwnerReferences: []metav1.OwnerReference{{
			Kind: "Deployment", Name: deployment,
		}},
	}}
}

func investigationOwnedPod(namespace, name, ownerKind, ownerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{{
				Kind: ownerKind, Name: ownerName,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true,
			}},
		},
	}
}

func replicaSetGETCount(client *fake.Clientset) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "replicasets" {
			count++
		}
	}
	return count
}

func requestPodInvestigation(t *testing.T, client *fake.Clientset, podName, namespace string) string {
	t.Helper()
	srv := newTestServer()
	srv.logsEnabled = false
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return client, nil }
	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster="+bogusClusterCtx+"&ns="+namespace+"&pod="+podName+"&type=crash_loop", nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Investigation status = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestInvestigationOwnerCacheFetchesReplicaSetOncePerRequest(t *testing.T) {
	const namespace = "payments"
	client := fake.NewSimpleClientset(
		investigationReplicaSet(namespace, "api-abc123", "api"),
		investigationOwnedPod(namespace, "api-1", "ReplicaSet", "api-abc123"),
		investigationOwnedPod(namespace, "api-2", "ReplicaSet", "api-abc123"),
		investigationOwnedPod(namespace, "api-3", "ReplicaSet", "api-abc123"),
	)
	body := requestPodInvestigation(t, client, "api-1", namespace)
	if got := replicaSetGETCount(client); got != 1 {
		t.Fatalf("ReplicaSet GETs = %d, want 1", got)
	}
	for _, want := range []string{"Deployment/api", "api-1", "api-2", "api-3"} {
		if !strings.Contains(body, want) {
			t.Errorf("Investigation output missing %q", want)
		}
	}
}

func TestInvestigationOwnerCacheFetchesEachDistinctReplicaSetOnce(t *testing.T) {
	const namespace = "payments"
	client := fake.NewSimpleClientset(
		investigationReplicaSet(namespace, "api-abc123", "api"),
		investigationReplicaSet(namespace, "worker-def456", "worker"),
		investigationOwnedPod(namespace, "api-1", "ReplicaSet", "api-abc123"),
		investigationOwnedPod(namespace, "api-2", "ReplicaSet", "api-abc123"),
		investigationOwnedPod(namespace, "worker-1", "ReplicaSet", "worker-def456"),
	)
	requestPodInvestigation(t, client, "api-1", namespace)
	if got := replicaSetGETCount(client); got != 2 {
		t.Fatalf("ReplicaSet GETs = %d, want 2 distinct lookups", got)
	}
}

func TestOwnerCacheIsolatesIdenticalReplicaSetNamesByNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		investigationReplicaSet("one", "api-abc123", "api-one"),
		investigationReplicaSet("two", "api-abc123", "api-two"),
	)
	resolver := newOwnerResolver(client)
	kindOne, nameOne := resolver.resolve(investigationOwnedPod("one", "api-1", "ReplicaSet", "api-abc123"))
	kindTwo, nameTwo := resolver.resolve(investigationOwnedPod("two", "api-1", "ReplicaSet", "api-abc123"))
	if kindOne != "Deployment" || nameOne != "api-one" || kindTwo != "Deployment" || nameTwo != "api-two" {
		t.Fatalf("namespace-isolated results = %s/%s and %s/%s", kindOne, nameOne, kindTwo, nameTwo)
	}
	if got := replicaSetGETCount(client); got != 2 {
		t.Fatalf("ReplicaSet GETs = %d, want 2", got)
	}
}

func TestOwnerCacheDirectOwnersDoNotFetchReplicaSets(t *testing.T) {
	client := fake.NewSimpleClientset()
	resolver := newOwnerResolver(client)
	for _, tc := range []struct{ kind, name string }{{"StatefulSet", "database"}, {"DaemonSet", "agent"}} {
		kind, name := resolver.resolve(investigationOwnedPod("app", tc.name+"-0", tc.kind, tc.name))
		if kind != tc.kind || name != tc.name {
			t.Errorf("direct owner = %s/%s, want %s/%s", kind, name, tc.kind, tc.name)
		}
	}
	if got := replicaSetGETCount(client); got != 0 {
		t.Fatalf("ReplicaSet GETs = %d, want 0", got)
	}
}

func TestInvestigationOwnerCacheIsFreshForEachHTTPRequest(t *testing.T) {
	const namespace = "payments"
	client := fake.NewSimpleClientset(
		investigationReplicaSet(namespace, "api-abc123", "api"),
		investigationOwnedPod(namespace, "api-1", "ReplicaSet", "api-abc123"),
	)
	requestPodInvestigation(t, client, "api-1", namespace)
	requestPodInvestigation(t, client, "api-1", namespace)
	if got := replicaSetGETCount(client); got != 2 {
		t.Fatalf("ReplicaSet GETs across two requests = %d, want 2", got)
	}
}

func TestOwnerCacheDoesNotCacheFailedReplicaSetLookups(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "replicasets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("temporary lookup failure")
	})
	resolver := newOwnerResolver(client)
	pod := investigationOwnedPod("app", "api-1", "ReplicaSet", "api-abc123")
	for i := 0; i < 2; i++ {
		kind, name := resolver.resolve(pod)
		if kind != "ReplicaSet" || name != "api-abc123" {
			t.Fatalf("failed lookup result = %s/%s", kind, name)
		}
	}
	if got := replicaSetGETCount(client); got != 2 {
		t.Fatalf("failed ReplicaSet GETs = %d, want 2 uncached attempts", got)
	}
}
