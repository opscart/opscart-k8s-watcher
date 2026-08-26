package scanner

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Scanner handles cluster scanning operations
type Scanner struct {
	clientset   kubernetes.Interface
	clusterName string
	ctx         context.Context
}

// NewScannerWithClientset reuses an already-configured Kubernetes client for
// scanners participating in a larger scan pipeline.
func NewScannerWithClientset(clientset kubernetes.Interface, clusterName string) *Scanner {
	return &Scanner{clientset: clientset, clusterName: clusterName, ctx: context.Background()}
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
	// Default client-go limits (QPS 5, Burst 10) cause skipped sub-scans on
	// large clusters ("context deadline exceeded").
	config.QPS = 50
	config.Burst = 100

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

	jobOwners := s.jobOwnerIndex(namespace)
	// Analyze each pod for problems
	for _, pod := range podList.Items {
		podIssues := s.analyzePodForIssuesWithOwners(pod, jobOwners)
		issues = append(issues, podIssues...)
	}

	// A Failed pod is terminal evidence, not automatically a current
	// workload outage. Reconcile ReplicaSet-owned failures against their
	// actual Deployment controller and current Deployment status. Controller
	// lookups are best-effort but never silent: on an API error the original
	// findings are preserved (fail open to visibility) and a warning is logged.
	var replicaSets []appsv1.ReplicaSet
	var deployments []appsv1.Deployment
	if hasReplicaSetPodFailed(issues) {
		replicaSetList, listErr := s.clientset.AppsV1().ReplicaSets(namespace).List(s.ctx, metav1.ListOptions{})
		if listErr != nil {
			log.Printf("scanner: PodFailed reconciliation skipped for ReplicaSet-owned pods: list ReplicaSets: %v", listErr)
		} else {
			replicaSets = replicaSetList.Items
			deploymentList, deploymentErr := s.clientset.AppsV1().Deployments(namespace).List(s.ctx, metav1.ListOptions{})
			if deploymentErr != nil {
				log.Printf("scanner: PodFailed reconciliation skipped for ReplicaSet-owned pods: list Deployments: %v", deploymentErr)
			} else {
				deployments = deploymentList.Items
			}
		}
	}
	issues = reconcilePodFailedAgainstDeploymentHealth(issues, podList.Items, replicaSets, deployments)

	// Check for PVC issues
	pvcIssues, err := s.findPVCIssues(namespace)
	if err == nil {
		issues = append(issues, pvcIssues...)
	}

	return issues, nil
}

type deploymentKey struct {
	namespace string
	name      string
}

type deploymentHealth struct {
	desired   int32
	available int32
	current   bool
}

func hasReplicaSetPodFailed(issues []models.EmergencyIssue) bool {
	for _, issue := range issues {
		if issue.Reason == "PodFailed" && issue.OwnerKind == "ReplicaSet" {
			return true
		}
	}
	return false
}

// reconcilePodFailedAgainstDeploymentHealth removes retained terminal pod
// evidence from active triage only when Kubernetes provides sufficient,
// current controller evidence. Ownership follows the explicit chain
// Pod -> ReplicaSet -> Deployment; pod-name shape is never used.
//
//   - healthy or intentionally scaled-to-zero Deployment: omit PodFailed;
//   - unavailable Deployment with a more specific live pod finding: omit the
//     redundant PodFailed and keep the specific finding;
//   - unavailable Deployment with no specific diagnosis: keep exactly one
//     critical PodFailed representative for that Deployment;
//   - unresolved/stale controller evidence: preserve PodFailed unchanged;
//   - genuinely bare failed pod: keep it as a high-severity review finding;
//   - Job/CronJob/StatefulSet/DaemonSet/standalone ReplicaSet: unchanged.
func reconcilePodFailedAgainstDeploymentHealth(
	issues []models.EmergencyIssue,
	pods []corev1.Pod,
	replicaSets []appsv1.ReplicaSet,
	deployments []appsv1.Deployment,
) []models.EmergencyIssue {
	podsByKey := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		podsByKey[pod.Namespace+"/"+pod.Name] = pod
	}

	replicaSetDeployments := make(map[deploymentKey]deploymentKey, len(replicaSets))
	for _, replicaSet := range replicaSets {
		deploymentName, ok := controllingOwnerName(replicaSet.OwnerReferences, "Deployment")
		if !ok {
			continue // standalone ReplicaSet or ownership is not authoritative
		}
		replicaSetDeployments[deploymentKey{replicaSet.Namespace, replicaSet.Name}] = deploymentKey{replicaSet.Namespace, deploymentName}
	}

	podDeployments := make(map[string]deploymentKey, len(pods))
	for _, pod := range pods {
		replicaSetName, ok := controllingOwnerName(pod.OwnerReferences, "ReplicaSet")
		if !ok {
			continue
		}
		if deployment, exists := replicaSetDeployments[deploymentKey{pod.Namespace, replicaSetName}]; exists {
			podDeployments[pod.Namespace+"/"+pod.Name] = deployment
		}
	}

	healthByDeployment := make(map[deploymentKey]deploymentHealth, len(deployments))
	for _, deployment := range deployments {
		desired := int32(1) // Kubernetes default when spec.replicas is omitted
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		healthByDeployment[deploymentKey{deployment.Namespace, deployment.Name}] = deploymentHealth{
			desired:   desired,
			available: deployment.Status.AvailableReplicas,
			current:   deployment.Status.ObservedGeneration >= deployment.Generation,
		}
	}

	specificFinding := make(map[deploymentKey]bool)
	for _, issue := range issues {
		if issue.Resource != "pod" || issue.Reason == "PodFailed" {
			continue
		}
		if deployment, ok := podDeployments[issue.Namespace+"/"+issue.Name]; ok {
			specificFinding[deployment] = true
		}
	}

	// Pick one stable representative for an unavailable Deployment that has
	// no more specific diagnosis. The selection is independent of API list
	// order: newest observed failure wins, then lexical pod name.
	representative := make(map[deploymentKey]int)
	failedCount := make(map[deploymentKey]int)
	for i, issue := range issues {
		if issue.Reason != "PodFailed" {
			continue
		}
		deployment, ok := podDeployments[issue.Namespace+"/"+issue.Name]
		if !ok {
			continue
		}
		health, known := healthByDeployment[deployment]
		if !known || !health.current || health.available >= health.desired || specificFinding[deployment] {
			continue
		}
		failedCount[deployment]++
		prior, exists := representative[deployment]
		if !exists || preferFailedPodRepresentative(issue, issues[prior]) {
			representative[deployment] = i
		}
	}

	out := make([]models.EmergencyIssue, 0, len(issues))
	for i, issue := range issues {
		if issue.Reason != "PodFailed" {
			out = append(out, issue)
			continue
		}

		deployment, resolved := podDeployments[issue.Namespace+"/"+issue.Name]
		if resolved {
			health, known := healthByDeployment[deployment]
			if !known || !health.current {
				out = append(out, issue) // incomplete/stale evidence: preserve visibility
				continue
			}
			if health.available >= health.desired || specificFinding[deployment] {
				continue // historical or redundant terminal evidence
			}
			if representative[deployment] != i {
				continue
			}
			issue.Message = fmt.Sprintf(
				"Deployment %q is unavailable (%d/%d replicas available). %d retained failed pod(s) were observed; representative evidence: %s",
				deployment.name, health.available, health.desired, failedCount[deployment], issue.Message,
			)
			out = append(out, issue)
			continue
		}

		// Only a pod with no OwnerReferences at all is safely called bare.
		// Unknown/non-controller ownership remains critical rather than being
		// guessed away.
		if pod, ok := podsByKey[issue.Namespace+"/"+issue.Name]; ok && len(pod.OwnerReferences) == 0 {
			issue.Severity = "high"
			issue.Message = "Bare pod is terminal and has no controller to declare replacement intent; review whether it is still required. " + issue.Message
		}
		out = append(out, issue)
	}

	return out
}

func controllingOwnerName(refs []metav1.OwnerReference, kind string) (string, bool) {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.Kind == kind {
			return ref.Name, true
		}
	}
	return "", false
}

func preferFailedPodRepresentative(candidate, current models.EmergencyIssue) bool {
	if candidate.FailureObservedAt.After(current.FailureObservedAt) {
		return true
	}
	if current.FailureObservedAt.After(candidate.FailureObservedAt) {
		return false
	}
	return candidate.Name < current.Name
}

type workloadOwner struct{ kind, name, execution string }

// jobOwnerIndex follows only explicit Job owner references. A missing or
// unreadable Job list leaves ownership unknown rather than guessing from names.
func (s *Scanner) jobOwnerIndex(namespace string) map[string]workloadOwner {
	jobs, err := s.clientset.BatchV1().Jobs(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	return jobOwnersFromJobs(jobs.Items)
}

func jobOwnersFromJobs(jobs []batchv1.Job) map[string]workloadOwner {
	owners := make(map[string]workloadOwner, len(jobs))
	for _, job := range jobs {
		owners[job.Namespace+"/"+job.Name] = ownerForJob(job)
	}
	return owners
}

func ownerForJob(job batchv1.Job) workloadOwner {
	for _, ref := range job.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Kind == "CronJob" {
			return workloadOwner{kind: "CronJob", name: ref.Name, execution: job.Name}
		}
	}
	return workloadOwner{kind: "Job", name: job.Name, execution: job.Name}
}

// analyzePodForIssues checks a pod for critical issues
func (s *Scanner) analyzePodForIssues(pod corev1.Pod) []models.EmergencyIssue {
	return s.analyzePodForIssuesWithOwners(pod, nil)
}

func (s *Scanner) analyzePodForIssuesWithOwners(pod corev1.Pod, jobOwners map[string]workloadOwner) []models.EmergencyIssue {
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
		owner := podWorkloadOwner(pod, jobOwners)
		message, failedAt := failedPodEvidence(pod)
		issues = append(issues, models.EmergencyIssue{
			Severity:          "critical",
			Resource:          "pod",
			Namespace:         pod.Namespace,
			Name:              pod.Name,
			Reason:            "PodFailed",
			Message:           message,
			Age:               age,
			Restarts:          totalRestarts,
			OwnerKind:         owner.kind,
			OwnerName:         owner.name,
			OwnerExecution:    owner.execution,
			FailureObservedAt: failedAt,
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
				Container: cs.Name,
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
				Container: cs.Name,
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
				Container: cs.Name,
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
				Container: cs.Name,
				Age:       age,
				Restarts:  int(cs.RestartCount),
			})
		}
	}

	return issues
}

func podWorkloadOwner(pod corev1.Pod, jobOwners map[string]workloadOwner) workloadOwner {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind == "Job" {
			if owner, ok := jobOwners[pod.Namespace+"/"+ref.Name]; ok {
				return owner
			}
			return workloadOwner{kind: "Job", name: ref.Name, execution: ref.Name}
		}
		return workloadOwner{kind: ref.Kind, name: ref.Name}
	}
	return workloadOwner{}
}

func failedPodEvidence(pod corev1.Pod) (string, time.Time) {
	for _, cs := range pod.Status.ContainerStatuses {
		if term := cs.State.Terminated; term != nil {
			if term.Message != "" {
				return term.Message, term.FinishedAt.Time
			}
			if term.Reason != "" {
				return fmt.Sprintf("Container %s terminated: %s", cs.Name, term.Reason), term.FinishedAt.Time
			}
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message, time.Time{}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason, time.Time{}
	}
	return "Pod phase is Failed; no termination reason was available.", time.Time{}
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
