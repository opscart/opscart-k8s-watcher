package analyzer

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildCanonicalAllocationFromSnapshotsReconcilesPricedPool(t *testing.T) {
	info := models.NodeInfo{
		Name: "node-a", NodePool: "userpool", VMSize: "Standard_D4s_v3",
		Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure",
		Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8,
	}
	pool := models.NodePoolCost{
		Name: "userpool", VMSize: "Standard_D4s_v3", NodeCount: 1,
		Priority: "Regular", OS: "linux", Provider: "azure", Region: "centralus",
		PricingAvailable: true, TotalMonthly: 1000,
		TotalCPUCapacity: 4, TotalMemoryCapacity: 8,
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	got := BuildCanonicalAllocationFromSnapshots(
		[]models.NodePoolCost{pool}, []models.NodeInfo{info}, []corev1.Pod{pod},
	)

	if got.ResolvedMonthlyCost != 1000 {
		t.Fatalf("resolved=%f want 1000", got.ResolvedMonthlyCost)
	}
	if got.AllocatedMonthly != 250 || got.IdleMonthly != 750 || got.UnallocatedMonthly != 0 {
		t.Fatalf("allocation=%f idle=%f unallocated=%f", got.AllocatedMonthly, got.IdleMonthly, got.UnallocatedMonthly)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0].EstimatedCost.Best != 250 {
		t.Fatalf("unexpected namespace allocation: %+v", got.Namespaces)
	}
	if got.Namespaces[0].EstimatedCost.Low != got.Namespaces[0].EstimatedCost.Best ||
		got.Namespaces[0].EstimatedCost.High != got.Namespaces[0].EstimatedCost.Best {
		t.Fatalf("current cost must be a point estimate: %+v", got.Namespaces[0].EstimatedCost)
	}
}
