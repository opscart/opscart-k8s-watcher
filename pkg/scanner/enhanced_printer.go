package scanner

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintEnhancedSnapshot displays enhanced snapshot in war room format
func PrintEnhancedSnapshot(snapshot *models.EnhancedClusterSnapshot, format string) {
	if format == "json" {
		printEnhancedSnapshotJSON(snapshot)
		return
	}

	printEnhancedSnapshotTable(snapshot)
}

// printEnhancedSnapshotTable outputs enhanced snapshot as formatted tables
func printEnhancedSnapshotTable(snapshot *models.EnhancedClusterSnapshot) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           ENHANCED CLUSTER SNAPSHOT                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("Cluster: %s\n", snapshot.ClusterName)
	fmt.Printf("Timestamp: %s\n", snapshot.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Println()

	// Pod summary
	fmt.Printf("📦 PODS: %d total | %d healthy | %d with issues\n\n",
		snapshot.TotalPods, snapshot.HealthyPods, snapshot.ProblemPods)

	// Deployments
	if len(snapshot.Deployments) > 0 {
		fmt.Printf("🚀 DEPLOYMENTS (%d):\n", len(snapshot.Deployments))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tREPLICAS\tREADY\tSTATUS\tAGE")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 80))

		for _, deploy := range snapshot.Deployments {
			status := "✅"
			if !deploy.Healthy {
				status = "⚠️ "
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

	// Services
	if len(snapshot.Services) > 0 {
		fmt.Printf("🌐 SERVICES (%d):\n", len(snapshot.Services))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tTYPE\tCLUSTER-IP\tEXTERNAL-IP\tPORTS\tENDPOINTS")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 100))

		for _, svc := range snapshot.Services {
			externalIP := svc.ExternalIP
			if externalIP == "" {
				externalIP = "<none>"
			}

			ports := fmt.Sprintf("%v", svc.Ports)

			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				svc.Namespace,
				svc.Name,
				svc.Type,
				svc.ClusterIP,
				externalIP,
				ports,
				svc.Endpoints)
		}
		w.Flush()
		fmt.Println()
	}

	// Ingresses
	if len(snapshot.Ingresses) > 0 {
		fmt.Printf("🔀 INGRESSES (%d):\n", len(snapshot.Ingresses))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tHOSTS\tTLS\tBACKEND\tAGE")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 90))

		for _, ing := range snapshot.Ingresses {
			hosts := strings.Join(ing.Hosts, ", ")
			if hosts == "" {
				hosts = "*"
			}

			tls := "No"
			if ing.TLSEnabled {
				tls = "Yes"
			}

			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				ing.Namespace,
				ing.Name,
				hosts,
				tls,
				ing.Backend,
				ing.Age)
		}
		w.Flush()
		fmt.Println()
	}

	// PVCs
	if len(snapshot.PVCDetails) > 0 {
		fmt.Printf("💾 PERSISTENT VOLUME CLAIMS (%d):\n", len(snapshot.PVCDetails))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tSTATUS\tSIZE\tSTORAGE CLASS\tUSED BY\tAGE")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 100))

		for _, pvc := range snapshot.PVCDetails {
			usedBy := pvc.UsedBy
			if usedBy == "" {
				usedBy = "<none>"
			}

			status := pvc.Status
			if status == "Bound" {
				status = "✅ Bound"
			} else if status == "Pending" {
				status = "⚠️  Pending"
			}

			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				pvc.Namespace,
				pvc.Name,
				status,
				pvc.Size,
				pvc.StorageClass,
				usedBy,
				pvc.Age)
		}
		w.Flush()
		fmt.Println()
	}

	// ConfigMaps and Secrets count
	if len(snapshot.ConfigMaps) > 0 || len(snapshot.Secrets) > 0 {
		fmt.Println("📄 CONFIGURATION:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tCONFIGMAPS\tSECRETS")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 50))

		// Merge ConfigMaps and Secrets by namespace
		configData := make(map[string][2]int)
		for _, cm := range snapshot.ConfigMaps {
			data := configData[cm.Namespace]
			data[0] = cm.Count
			configData[cm.Namespace] = data
		}
		for _, secret := range snapshot.Secrets {
			data := configData[secret.Namespace]
			data[1] = secret.Count
			configData[secret.Namespace] = data
		}

		for ns, counts := range configData {
			fmt.Fprintf(w, "  %s\t%d\t%d\n", ns, counts[0], counts[1])
		}
		w.Flush()
		fmt.Println()
	}

	// Network Policies
	if len(snapshot.NetworkPolicies) > 0 {
		fmt.Printf("🔒 NETWORK POLICIES (%d):\n", len(snapshot.NetworkPolicies))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tNAME\tPOD SELECTOR\tPOLICY TYPES")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 80))

		for _, np := range snapshot.NetworkPolicies {
			policyTypes := strings.Join(np.PolicyTypes, ", ")
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
				np.Namespace,
				np.Name,
				np.PodSelector,
				policyTypes)
		}
		w.Flush()
		fmt.Println()
	} else {
		fmt.Println("⚠️  No Network Policies found - consider adding them for better security")
		fmt.Println()
	}

	// Summary
	fmt.Println("SUMMARY:")
	fmt.Printf("  • %d deployments (%d healthy)\n", len(snapshot.Deployments), countHealthyDeployments(snapshot.Deployments))
	fmt.Printf("  • %d services (%d with endpoints)\n", len(snapshot.Services), countServicesWithEndpoints(snapshot.Services))
	fmt.Printf("  • %d ingresses\n", len(snapshot.Ingresses))
	fmt.Printf("  • %d PVCs (%d bound)\n", len(snapshot.PVCDetails), countBoundPVCs(snapshot.PVCDetails))
	if len(snapshot.NetworkPolicies) > 0 {
		fmt.Printf("  • %d network policies\n", len(snapshot.NetworkPolicies))
	}
}

// printEnhancedSnapshotJSON outputs enhanced snapshot as JSON
func printEnhancedSnapshotJSON(snapshot *models.EnhancedClusterSnapshot) {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// Helper functions
func countHealthyDeployments(deployments []models.DeploymentInfo) int {
	count := 0
	for _, d := range deployments {
		if d.Healthy {
			count++
		}
	}
	return count
}

func countServicesWithEndpoints(services []models.ServiceDetail) int {
	count := 0
	for _, s := range services {
		if s.Endpoints > 0 {
			count++
		}
	}
	return count
}

func countBoundPVCs(pvcs []models.PVCDetail) int {
	count := 0
	for _, p := range pvcs {
		if p.Status == "Bound" {
			count++
		}
	}
	return count
}
