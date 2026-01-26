package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// PrintEmergencyIssues displays critical issues in war room format
func PrintEmergencyIssues(issues []models.EmergencyIssue) {
	if len(issues) == 0 {
		fmt.Println("✅ No critical issues found!")
		return
	}

	// Group by severity
	critical := []models.EmergencyIssue{}
	high := []models.EmergencyIssue{}
	medium := []models.EmergencyIssue{}

	for _, issue := range issues {
		switch issue.Severity {
		case "critical":
			critical = append(critical, issue)
		case "high":
			high = append(high, issue)
		case "medium":
			medium = append(medium, issue)
		}
	}

	// Print summary
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║             WAR ROOM - EMERGENCY ISSUES                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Printf("\n🔴 CRITICAL: %d    🟡 HIGH: %d    🟠 MEDIUM: %d\n\n", len(critical), len(high), len(medium))

	// Print critical issues
	if len(critical) > 0 {
		fmt.Println("🔴 CRITICAL ISSUES:")
		fmt.Println(strings.Repeat("═", 80))
		for _, issue := range critical {
			printIssue(issue)
		}
		fmt.Println()
	}

	// Print high priority issues
	if len(high) > 0 {
		fmt.Println("🟡 HIGH PRIORITY:")
		fmt.Println(strings.Repeat("═", 80))
		for _, issue := range high {
			printIssue(issue)
		}
		fmt.Println()
	}

	// Print medium priority issues
	if len(medium) > 0 {
		fmt.Println("🟠 MEDIUM PRIORITY:")
		fmt.Println(strings.Repeat("═", 80))
		for _, issue := range medium {
			printIssue(issue)
		}
	}
}

func printIssue(issue models.EmergencyIssue) {
	fmt.Printf("  %s/%s\n", issue.Namespace, issue.Name)
	fmt.Printf("  └─ Status: %s", issue.Reason)
	if issue.Restarts > 0 {
		fmt.Printf(" | Restarts: %d", issue.Restarts)
	}
	fmt.Printf(" | Age: %s\n", formatDuration(issue.Age))
	fmt.Printf("  └─ %s\n\n", issue.Message)
}

// PrintSnapshotJSON outputs snapshot as JSON
func PrintSnapshotJSON(snapshot *models.ClusterSnapshot) {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// PrintSnapshotTable outputs snapshot in table format
func PrintSnapshotTable(snapshot *models.ClusterSnapshot) {
	fmt.Printf("═══════════════════════════════════════════════════════════\n")
	fmt.Printf(" Cluster: %s\n", snapshot.ClusterName)
	fmt.Printf(" Snapshot: %s\n", snapshot.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("═══════════════════════════════════════════════════════════\n\n")

	// Pod summary
	fmt.Printf("PODS: %d total | %d healthy | %d problems\n\n",
		snapshot.TotalPods, snapshot.HealthyPods, snapshot.ProblemPods)

	// Deployments
	if len(snapshot.Deployments) > 0 {
		fmt.Printf("DEPLOYMENTS (%d):\n", len(snapshot.Deployments))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tREPLICAS\tREADY\tSTATUS\tAGE")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 70))

		for _, deploy := range snapshot.Deployments {
			status := "✅ Healthy"
			if !deploy.Healthy {
				status = "⚠️  Degraded"
			}
			fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%s\t%s\n",
				deploy.Namespace,
				deploy.Name,
				deploy.Replicas,
				deploy.ReadyReplicas,
				status,
				formatDuration(deploy.Age))
		}
		w.Flush()
		fmt.Println()
	}
}

// PrintIdleResources displays idle resources
func PrintIdleResources(idle []models.IdleResource) {
	if len(idle) == 0 {
		fmt.Println("✅ No idle resources found")
		return
	}

	fmt.Printf("Found %d idle resources:\n\n", len(idle))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tNAMESPACE\tNAME\tIDLE DAYS\tRECOMMENDATION")
	fmt.Fprintln(w, strings.Repeat("─", 80))

	for _, resource := range idle {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			resource.Type,
			resource.Namespace,
			resource.Name,
			resource.IdleDays,
			resource.Recommendation)
	}
	w.Flush()
}

// FindResources searches for resources across clusters
func FindResources(clusters []string, resourceType, namePattern, statusFilter string) []models.ResourceSearchResult {
	var results []models.ResourceSearchResult

	// Convert filters to lowercase for case-insensitive matching
	namePattern = strings.ToLower(namePattern)
	statusFilter = strings.ToLower(statusFilter)

	for _, cluster := range clusters {
		scanner, err := NewScanner(cluster)
		if err != nil {
			fmt.Printf("Warning: Could not connect to cluster %s: %v\n", cluster, err)
			continue
		}

		// Search based on resource type
		if resourceType == "deployment" || resourceType == "all" {
			searchDeployments(scanner, cluster, namePattern, statusFilter, &results)
		}

		if resourceType == "pod" || resourceType == "all" {
			searchPods(scanner, cluster, namePattern, statusFilter, &results)
		}

		if resourceType == "service" || resourceType == "all" {
			searchServices(scanner, cluster, namePattern, statusFilter, &results)
		}
	}

	return results
}

func searchDeployments(scanner *Scanner, cluster, namePattern, statusFilter string, results *[]models.ResourceSearchResult) {
	deployList, err := scanner.clientset.AppsV1().Deployments("").List(scanner.ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, deploy := range deployList.Items {
		// Filter by name if pattern provided
		if namePattern != "" && !strings.Contains(strings.ToLower(deploy.Name), namePattern) {
			continue
		}

		status := fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, *deploy.Spec.Replicas)

		// Filter by status if provided
		if statusFilter != "" {
			// Check if deployment is healthy
			isHealthy := deploy.Status.ReadyReplicas == *deploy.Spec.Replicas
			if statusFilter == "healthy" && !isHealthy {
				continue
			}
			if statusFilter == "unhealthy" && isHealthy {
				continue
			}
		}

		*results = append(*results, models.ResourceSearchResult{
			ClusterName: cluster,
			Type:        "deployment",
			Namespace:   deploy.Namespace,
			Name:        deploy.Name,
			Status:      status,
		})
	}
}

func searchPods(scanner *Scanner, cluster, namePattern, statusFilter string, results *[]models.ResourceSearchResult) {
	podList, err := scanner.clientset.CoreV1().Pods("").List(scanner.ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, pod := range podList.Items {
		// Filter by name if pattern provided
		if namePattern != "" && !strings.Contains(strings.ToLower(pod.Name), namePattern) {
			continue
		}

		podStatus := string(pod.Status.Phase)

		// Filter by status if provided
		if statusFilter != "" && strings.ToLower(podStatus) != statusFilter {
			continue
		}

		*results = append(*results, models.ResourceSearchResult{
			ClusterName: cluster,
			Type:        "pod",
			Namespace:   pod.Namespace,
			Name:        pod.Name,
			Status:      podStatus,
		})
	}
}

func searchServices(scanner *Scanner, cluster, namePattern, statusFilter string, results *[]models.ResourceSearchResult) {
	svcList, err := scanner.clientset.CoreV1().Services("").List(scanner.ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, svc := range svcList.Items {
		// Filter by name if pattern provided
		if namePattern != "" && !strings.Contains(strings.ToLower(svc.Name), namePattern) {
			continue
		}

		// Services don't have a meaningful status filter, skip if status requested
		if statusFilter != "" {
			continue
		}

		*results = append(*results, models.ResourceSearchResult{
			ClusterName: cluster,
			Type:        "service",
			Namespace:   svc.Namespace,
			Name:        svc.Name,
			Status:      string(svc.Spec.Type),
		})
	}
}

// PrintFindResults displays search results
func PrintFindResults(results []models.ResourceSearchResult) {
	if len(results) == 0 {
		fmt.Println("No resources found matching search criteria")
		return
	}

	fmt.Printf("Found %d matching resources:\n\n", len(results))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CLUSTER\tTYPE\tNAMESPACE\tNAME\tSTATUS")
	fmt.Fprintln(w, strings.Repeat("─", 80))

	for _, result := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			result.ClusterName,
			result.Type,
			result.Namespace,
			result.Name,
			result.Status)
	}
	w.Flush()
}

// GetAllClusters returns all cluster contexts from kubeconfig
func GetAllClusters() []string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := loadingRules.Load()
	if err != nil {
		return []string{}
	}

	clusters := []string{}
	for name := range config.Contexts {
		clusters = append(clusters, name)
	}
	return clusters
}

// formatDuration formats time.Duration in human-readable format
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
