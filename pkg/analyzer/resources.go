package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ResourceAnalyzer analyzes cluster resource usage
type ResourceAnalyzer struct {
	clientset *kubernetes.Clientset
	ctx       context.Context
}

// NewResourceAnalyzer creates a new resource analyzer
func NewResourceAnalyzer(clientset *kubernetes.Clientset) *ResourceAnalyzer {
	return &ResourceAnalyzer{
		clientset: clientset,
		ctx:       context.Background(),
	}
}

// AnalyzeClusterResources performs comprehensive resource analysis
func (ra *ResourceAnalyzer) AnalyzeClusterResources(namespace string) (*models.ClusterResourceAnalysis, error) {
	analysis := &models.ClusterResourceAnalysis{
		Timestamp: time.Now(),
	}

	// Get cluster capacity
	capacity, err := ra.getClusterCapacity()
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster capacity: %w", err)
	}
	analysis.TotalCPUCores = capacity.CPU
	analysis.TotalMemoryGB = capacity.Memory

	// Get all pods
	podList, err := ra.clientset.CoreV1().Pods(namespace).List(ra.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Analyze by namespace
	namespaceMap := make(map[string]*models.NamespaceResourceUsage)

	for _, pod := range podList.Items {
		ns := pod.Namespace

		// Initialize namespace if not exists
		if _, exists := namespaceMap[ns]; !exists {
			namespaceMap[ns] = &models.NamespaceResourceUsage{
				Name:     ns,
				PodCount: 0,
			}
		}

		namespaceMap[ns].PodCount++

		// Sum up resource requests
		podResources := getPodResourceRequests(pod)
		namespaceMap[ns].CPUCoresRequested += podResources.CPU
		namespaceMap[ns].MemoryGBRequested += podResources.Memory

		// Check if pod is idle (no restarts, old)
		if isPodIdle(pod) {
			namespaceMap[ns].IdlePods++
		}

		// Check if spot-eligible (no StatefulSet, no PVC)
		if isSpotEligible(pod) {
			namespaceMap[ns].SpotEligiblePods++
		}
	}

	// Calculate percentages and detect waste
	var namespaces []models.NamespaceResourceUsage
	for _, nsUsage := range namespaceMap {
		// Calculate cluster percentage
		nsUsage.CPUPercent = (nsUsage.CPUCoresRequested / analysis.TotalCPUCores) * 100
		nsUsage.MemoryPercent = (nsUsage.MemoryGBRequested / analysis.TotalMemoryGB) * 100

		// Detect waste patterns
		nsUsage.WasteScore = ra.calculateWasteScore(nsUsage)
		nsUsage.Flags = ra.generateFlags(nsUsage)

		// Calculate totals
		analysis.TotalCPURequested += nsUsage.CPUCoresRequested
		analysis.TotalMemoryRequested += nsUsage.MemoryGBRequested

		namespaces = append(namespaces, *nsUsage)
	}

	// Sort by resource consumption (CPU + Memory percentage)
	sortNamespacesByUsage(namespaces)
	analysis.Namespaces = namespaces

	// Calculate utilization percentages
	analysis.CPUUtilization = (analysis.TotalCPURequested / analysis.TotalCPUCores) * 100
	analysis.MemoryUtilization = (analysis.TotalMemoryRequested / analysis.TotalMemoryGB) * 100

	// Generate optimization opportunities
	analysis.Optimizations = ra.generateOptimizations(namespaces)

	return analysis, nil
}

// getClusterCapacity calculates total cluster capacity
func (ra *ResourceAnalyzer) getClusterCapacity() (models.ResourceCapacity, error) {
	var capacity models.ResourceCapacity

	nodeList, err := ra.clientset.CoreV1().Nodes().List(ra.ctx, metav1.ListOptions{})
	if err != nil {
		return capacity, err
	}

	for _, node := range nodeList.Items {
		// Get allocatable resources (what's available for pods)
		cpuQuantity := node.Status.Allocatable[corev1.ResourceCPU]
		memoryQuantity := node.Status.Allocatable[corev1.ResourceMemory]

		// Convert to float64
		cpu := float64(cpuQuantity.MilliValue()) / 1000.0
		memory := float64(memoryQuantity.Value()) / (1024 * 1024 * 1024) // Convert to GB

		capacity.CPU += cpu
		capacity.Memory += memory
	}

	return capacity, nil
}

// getPodResourceRequests calculates total resource requests for a pod
func getPodResourceRequests(pod corev1.Pod) models.ResourceCapacity {
	var resources models.ResourceCapacity

	for _, container := range pod.Spec.Containers {
		// CPU
		if cpuRequest, exists := container.Resources.Requests[corev1.ResourceCPU]; exists {
			cpu := float64(cpuRequest.MilliValue()) / 1000.0
			resources.CPU += cpu
		}

		// Memory
		if memRequest, exists := container.Resources.Requests[corev1.ResourceMemory]; exists {
			memory := float64(memRequest.Value()) / (1024 * 1024 * 1024)
			resources.Memory += memory
		}
	}

	return resources
}

// isPodIdle checks if a pod appears to be idle
func isPodIdle(pod corev1.Pod) bool {
	// Pod is idle if:
	// 1. Running for more than 7 days
	// 2. No restarts
	// 3. Not in kube-system or istio-system

	age := time.Since(pod.CreationTimestamp.Time)
	if age < 7*24*time.Hour {
		return false
	}

	// Check if system namespace
	if pod.Namespace == "kube-system" || pod.Namespace == "istio-system" {
		return false
	}

	// Check restarts
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return false // Has activity
		}
	}

	return true
}

// isSpotEligible checks if a pod can run on spot instances
func isSpotEligible(pod corev1.Pod) bool {
	// Not spot-eligible if:
	// 1. Has persistent volume claims
	// 2. In production namespace
	// 3. Part of a StatefulSet

	// Check namespace
	if pod.Namespace == "production" || pod.Namespace == "prod" {
		return false
	}

	// Check for PVCs
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			return false
		}
	}

	// Check if StatefulSet
	for _, ownerRef := range pod.OwnerReferences {
		if ownerRef.Kind == "StatefulSet" {
			return false
		}
	}

	return true
}

// calculateWasteScore calculates a waste score (0-100) for a namespace
func (ra *ResourceAnalyzer) calculateWasteScore(ns *models.NamespaceResourceUsage) float64 {
	score := 0.0

	// Idle pods contribute heavily to waste
	if ns.IdlePods > 0 {
		idleRatio := float64(ns.IdlePods) / float64(ns.PodCount)
		score += idleRatio * 40 // Up to 40 points for idle
	}

	// Low pod count with high resource requests suggests over-provisioning
	if ns.PodCount > 0 && ns.PodCount < 5 {
		avgCPUPerPod := ns.CPUCoresRequested / float64(ns.PodCount)
		if avgCPUPerPod > 2.0 {
			score += 30 // 30 points for likely over-provisioning
		}
	}

	// Spot-eligible workloads not on spot
	if ns.SpotEligiblePods > 0 {
		spotRatio := float64(ns.SpotEligiblePods) / float64(ns.PodCount)
		score += spotRatio * 30 // Up to 30 points
	}

	return score
}

// generateFlags generates informational flags for a namespace
func (ra *ResourceAnalyzer) generateFlags(ns *models.NamespaceResourceUsage) []string {
	var flags []string

	if ns.IdlePods > 0 {
		flags = append(flags, fmt.Sprintf("IDLE-%dd", ns.IdlePods*7))
	}

	if ns.SpotEligiblePods > 0 && float64(ns.SpotEligiblePods)/float64(ns.PodCount) > 0.5 {
		flags = append(flags, "SPOT-OK")
	}

	if ns.PodCount > 0 {
		avgCPU := ns.CPUCoresRequested / float64(ns.PodCount)
		if avgCPU > 2.0 {
			flags = append(flags, "OVER-PROV")
		}
	}

	return flags
}

// generateOptimizations creates optimization recommendations
func (ra *ResourceAnalyzer) generateOptimizations(namespaces []models.NamespaceResourceUsage) []models.Optimization {
	var opts []models.Optimization

	for _, ns := range namespaces {
		// High impact: Idle namespaces
		if ns.IdlePods > 0 && ns.WasteScore > 50 {
			opts = append(opts, models.Optimization{
				Priority:  "high",
				Type:      "idle_namespace",
				Namespace: ns.Name,
				Description: fmt.Sprintf("%s idle for %d+ days (%0.1f CPU, %0.1f GB)",
					ns.Name, ns.IdlePods*7, ns.CPUCoresRequested, ns.MemoryGBRequested),
				Action: fmt.Sprintf("kubectl delete namespace %s", ns.Name),
				Impact: fmt.Sprintf("Frees %0.1f CPU, %0.1f GB (%0.1f%% of cluster)",
					ns.CPUCoresRequested, ns.MemoryGBRequested, ns.CPUPercent),
			})
		}

		// Medium impact: Spot-eligible
		if ns.SpotEligiblePods > 2 {
			spotCPU := ns.CPUCoresRequested * (float64(ns.SpotEligiblePods) / float64(ns.PodCount))
			opts = append(opts, models.Optimization{
				Priority:    "medium",
				Type:        "spot_migration",
				Namespace:   ns.Name,
				Description: fmt.Sprintf("%s has %d pods eligible for spot", ns.Name, ns.SpotEligiblePods),
				Action:      "Add spot node toleration and nodeSelector",
				Impact:      fmt.Sprintf("Save ~70%% on %0.1f CPU cores", spotCPU),
			})
		}

		// Medium impact: Over-provisioned
		if ns.PodCount > 0 && ns.PodCount < 5 {
			avgCPU := ns.CPUCoresRequested / float64(ns.PodCount)
			if avgCPU > 2.0 {
				opts = append(opts, models.Optimization{
					Priority:    "medium",
					Type:        "rightsizing",
					Namespace:   ns.Name,
					Description: fmt.Sprintf("%s appears over-provisioned (avg %0.1f CPU/pod)", ns.Name, avgCPU),
					Action:      "Review actual usage and adjust resource requests",
					Impact:      "Potentially free up 50-70% of requested resources",
				})
			}
		}
	}

	return opts
}

// sortNamespacesByUsage sorts namespaces by total resource consumption
func sortNamespacesByUsage(namespaces []models.NamespaceResourceUsage) {
	// Simple bubble sort by weighted average of CPU and Memory percentage
	for i := 0; i < len(namespaces); i++ {
		for j := i + 1; j < len(namespaces); j++ {
			score1 := (namespaces[i].CPUPercent + namespaces[i].MemoryPercent) / 2
			score2 := (namespaces[j].CPUPercent + namespaces[j].MemoryPercent) / 2
			if score2 > score1 {
				namespaces[i], namespaces[j] = namespaces[j], namespaces[i]
			}
		}
	}
}
