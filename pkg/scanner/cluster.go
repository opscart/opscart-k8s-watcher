package scanner

import (
	"context"
	"fmt"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Scanner handles cluster scanning operations
type Scanner struct {
	clientset   *kubernetes.Clientset
	clusterName string
	ctx         context.Context
}

// NewScanner creates a new scanner for the given cluster context
func NewScanner(clusterContext string) (*Scanner, error) {
	// Load kubeconfig
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: clusterContext,
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return &Scanner{
		clientset:   clientset,
		clusterName: clusterContext,
		ctx:         context.Background(),
	}, nil
}

// FindEmergencyIssues scans for critical problems that need immediate attention
func (s *Scanner) FindEmergencyIssues(namespace string) ([]models.EmergencyIssue, error) {
	var issues []models.EmergencyIssue

	// Get all pods
	podList, err := s.clientset.CoreV1().Pods(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Analyze each pod for problems
	for _, pod := range podList.Items {
		podIssues := s.analyzePodForIssues(pod)
		issues = append(issues, podIssues...)
	}

	// Check for PVC issues
	pvcIssues, err := s.findPVCIssues(namespace)
	if err == nil {
		issues = append(issues, pvcIssues...)
	}

	return issues, nil
}

// analyzePodForIssues checks a pod for critical issues
func (s *Scanner) analyzePodForIssues(pod corev1.Pod) []models.EmergencyIssue {
	var issues []models.EmergencyIssue
	now := time.Now()
	age := now.Sub(pod.CreationTimestamp.Time)

	// Calculate total restarts
	totalRestarts := 0
	for _, cs := range pod.Status.ContainerStatuses {
		totalRestarts += int(cs.RestartCount)
	}

	// Check pod phase
	switch pod.Status.Phase {
	case corev1.PodFailed:
		issues = append(issues, models.EmergencyIssue{
			Severity:  "critical",
			Resource:  "pod",
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Reason:    "PodFailed",
			Message:   fmt.Sprintf("Pod in Failed state: %s", pod.Status.Reason),
			Age:       age,
			Restarts:  totalRestarts,
		})

	case corev1.PodPending:
		// Check if pending for more than 5 minutes
		if age > 5*time.Minute {
			reason := "Pending"
			message := "Pod pending for extended period"

			// Check for scheduling issues
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
					reason = condition.Reason
					message = condition.Message
				}
			}

			issues = append(issues, models.EmergencyIssue{
				Severity:  "high",
				Resource:  "pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Reason:    reason,
				Message:   message,
				Age:       age,
			})
		}
	}

	// Check container statuses
	for _, cs := range pod.Status.ContainerStatuses {
		// CrashLoopBackOff
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			issues = append(issues, models.EmergencyIssue{
				Severity:  "critical",
				Resource:  "pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Reason:    "CrashLoopBackOff",
				Message:   fmt.Sprintf("Container %s is crash looping: %s", cs.Name, cs.State.Waiting.Message),
				Age:       age,
				Restarts:  int(cs.RestartCount),
			})
		}

		// ImagePullBackOff
		if cs.State.Waiting != nil && (cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull") {
			issues = append(issues, models.EmergencyIssue{
				Severity:  "high",
				Resource:  "pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Reason:    cs.State.Waiting.Reason,
				Message:   fmt.Sprintf("Cannot pull image for container %s: %s", cs.Name, cs.State.Waiting.Message),
				Age:       age,
			})
		}

		// OOMKilled
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			issues = append(issues, models.EmergencyIssue{
				Severity:  "critical",
				Resource:  "pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Reason:    "OOMKilled",
				Message:   fmt.Sprintf("Container %s killed due to out of memory", cs.Name),
				Age:       age,
				Restarts:  int(cs.RestartCount),
			})
		}

		// High restart count
		if cs.RestartCount > 10 && pod.Status.Phase == corev1.PodRunning {
			issues = append(issues, models.EmergencyIssue{
				Severity:  "medium",
				Resource:  "pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Reason:    "HighRestartCount",
				Message:   fmt.Sprintf("Container %s has restarted %d times", cs.Name, cs.RestartCount),
				Age:       age,
				Restarts:  int(cs.RestartCount),
			})
		}
	}

	return issues
}

// findPVCIssues looks for problematic PVCs
func (s *Scanner) findPVCIssues(namespace string) ([]models.EmergencyIssue, error) {
	var issues []models.EmergencyIssue

	pvcList, err := s.clientset.CoreV1().PersistentVolumeClaims(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, pvc := range pvcList.Items {
		if pvc.Status.Phase == corev1.ClaimPending {
			age := time.Since(pvc.CreationTimestamp.Time)
			if age > 2*time.Minute {
				issues = append(issues, models.EmergencyIssue{
					Severity:  "high",
					Resource:  "pvc",
					Namespace: pvc.Namespace,
					Name:      pvc.Name,
					Reason:    "PVCPending",
					Message:   "PersistentVolumeClaim stuck in Pending state",
					Age:       age,
				})
			}
		}

		if pvc.Status.Phase == corev1.ClaimLost {
			issues = append(issues, models.EmergencyIssue{
				Severity:  "critical",
				Resource:  "pvc",
				Namespace: pvc.Namespace,
				Name:      pvc.Name,
				Reason:    "PVCLost",
				Message:   "PersistentVolumeClaim in Lost state - data may be unavailable",
				Age:       time.Since(pvc.CreationTimestamp.Time),
			})
		}
	}

	return issues, nil
}

// TakeSnapshot captures the current state of the cluster
func (s *Scanner) TakeSnapshot(namespace string) (*models.ClusterSnapshot, error) {
	snapshot := &models.ClusterSnapshot{
		ClusterName: s.clusterName,
		Timestamp:   time.Now(),
	}

	// Get pods
	podList, err := s.clientset.CoreV1().Pods(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	snapshot.TotalPods = len(podList.Items)
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning && isPodReady(pod) {
			snapshot.HealthyPods++
		} else {
			snapshot.ProblemPods++
		}
	}

	// Get deployments
	deployList, err := s.clientset.AppsV1().Deployments(namespace).List(s.ctx, metav1.ListOptions{})
	if err == nil {
		for _, deploy := range deployList.Items {
			snapshot.Deployments = append(snapshot.Deployments, models.DeploymentInfo{
				Name:              deploy.Name,
				Namespace:         deploy.Namespace,
				Replicas:          *deploy.Spec.Replicas,
				ReadyReplicas:     deploy.Status.ReadyReplicas,
				AvailableReplicas: deploy.Status.AvailableReplicas,
				Healthy:           deploy.Status.ReadyReplicas == *deploy.Spec.Replicas,
				Age:               time.Since(deploy.CreationTimestamp.Time),
			})
		}
	}

	return snapshot, nil
}

// FindIdleResources identifies resources with zero activity
func (s *Scanner) FindIdleResources(namespace string) ([]models.IdleResource, error) {
	var idle []models.IdleResource

	// This is a placeholder - real implementation would check metrics
	// For now, we'll identify deployments with 0 replicas or pods that haven't restarted in 30 days

	deployList, err := s.clientset.AppsV1().Deployments(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, deploy := range deployList.Items {
		if *deploy.Spec.Replicas == 0 {
			idle = append(idle, models.IdleResource{
				Type:           "deployment",
				Name:           deploy.Name,
				Namespace:      deploy.Namespace,
				IdleDays:       int(time.Since(deploy.CreationTimestamp.Time).Hours() / 24),
				LastActivity:   deploy.CreationTimestamp.Time,
				Recommendation: "Consider deleting if no longer needed",
			})
		}
	}

	return idle, nil
}

// isPodReady checks if all containers in a pod are ready
func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
