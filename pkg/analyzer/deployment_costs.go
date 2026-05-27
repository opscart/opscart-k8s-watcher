package analyzer

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// DeploymentCostAnalyzer enriches NamespaceCostInfo with per-deployment breakdown
type DeploymentCostAnalyzer struct {
	clientset *kubernetes.Clientset
	ctx       context.Context
}

// NewDeploymentCostAnalyzer creates a new deployment cost analyzer
func NewDeploymentCostAnalyzer(clientset *kubernetes.Clientset) *DeploymentCostAnalyzer {
	return &DeploymentCostAnalyzer{clientset: clientset, ctx: context.Background()}
}

// EnrichWithDeployments adds deployment-level cost breakdown to each namespace (skips system namespaces)
func (da *DeploymentCostAnalyzer) EnrichWithDeployments(nsCosts []models.NamespaceCostInfo) ([]models.NamespaceCostInfo, error) {
	for i, ns := range nsCosts {
		if isSystemNamespaceForOptimization(ns.Name) {
			continue // system namespaces have expected DaemonSets/infra — skip breakdown
		}
		deployments, err := da.getDeploymentCosts(ns)
		if err != nil {
			continue // non-fatal
		}
		nsCosts[i].Deployments = deployments
	}
	return nsCosts, nil
}

func (da *DeploymentCostAnalyzer) getDeploymentCosts(ns models.NamespaceCostInfo) ([]models.DeploymentCostInfo, error) {
	var results []models.DeploymentCostInfo

	totalCPU := ns.CPUCores
	totalMem := ns.MemoryGB
	nsCostBest := ns.EstimatedCost.Best
	nsCostLow := ns.EstimatedCost.Low
	nsCostHigh := ns.EstimatedCost.High

	addResult := func(name, kind string, replicas int, cpuPerPod, memPerPod float64) {
		totalCPUReq := cpuPerPod * float64(replicas)
		totalMemReq := memPerPod * float64(replicas)
		share := deploymentWeightedShare(totalCPUReq, totalMemReq, totalCPU, totalMem)
		results = append(results, models.DeploymentCostInfo{
			Name:      name,
			Kind:      kind,
			Namespace: ns.Name,
			Replicas:  replicas,
			CPUCores:  totalCPUReq,
			MemoryGB:  totalMemReq,
			NSShare:   share,
			EstimatedCost: models.CostRange{
				Low:  nsCostLow * share,
				Best: nsCostBest * share,
				High: nsCostHigh * share,
			},
		})
	}

	// ── Deployments ────────────────────────────────────────────────
	deps, err := da.clientset.AppsV1().Deployments(ns.Name).List(da.ctx, metav1.ListOptions{})
	if err == nil {
		for _, d := range deps.Items {
			cpu, mem := sumContainerResourceRequests(d.Spec.Template.Spec.Containers)
			replicas := 1
			if d.Spec.Replicas != nil {
				replicas = int(*d.Spec.Replicas)
			}
			addResult(d.Name, "Deployment", replicas, cpu, mem)
		}
	}

	// ── StatefulSets ───────────────────────────────────────────────
	stss, err := da.clientset.AppsV1().StatefulSets(ns.Name).List(da.ctx, metav1.ListOptions{})
	if err == nil {
		for _, s := range stss.Items {
			cpu, mem := sumContainerResourceRequests(s.Spec.Template.Spec.Containers)
			replicas := 1
			if s.Spec.Replicas != nil {
				replicas = int(*s.Spec.Replicas)
			}
			addResult(s.Name, "StatefulSet", replicas, cpu, mem)
		}
	}

	// ── DaemonSets ─────────────────────────────────────────────────
	dss, err := da.clientset.AppsV1().DaemonSets(ns.Name).List(da.ctx, metav1.ListOptions{})
	if err == nil {
		for _, d := range dss.Items {
			cpu, mem := sumContainerResourceRequests(d.Spec.Template.Spec.Containers)
			replicas := int(d.Status.DesiredNumberScheduled)
			if replicas == 0 {
				replicas = 1
			}
			addResult(d.Name, "DaemonSet", replicas, cpu, mem)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].EstimatedCost.Best != results[j].EstimatedCost.Best {
			return results[i].EstimatedCost.Best > results[j].EstimatedCost.Best
		}
		return results[i].CPUCores > results[j].CPUCores
	})

	return results, nil
}

// deploymentWeightedShare returns (CPU+Mem)/2 share of a workload within its namespace
func deploymentWeightedShare(wCPU, wMem, totalCPU, totalMem float64) float64 {
	cpuShare := 0.0
	if totalCPU > 0 {
		cpuShare = wCPU / totalCPU
	}
	memShare := 0.0
	if totalMem > 0 {
		memShare = wMem / totalMem
	}
	if cpuShare == 0 && memShare == 0 {
		return 0
	}
	return (cpuShare + memShare) / 2.0
}

// sumContainerResourceRequests totals CPU (cores) and memory (GB) requests across containers
func sumContainerResourceRequests(containers []corev1.Container) (float64, float64) {
	var cpu, mem float64
	for _, c := range containers {
		if cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += float64(cpuReq.MilliValue()) / 1000.0
		}
		if memReq, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += float64(memReq.Value()) / (1024 * 1024 * 1024)
		}
	}
	return cpu, mem
}
