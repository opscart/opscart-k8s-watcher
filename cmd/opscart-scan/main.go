package main

import (
	"fmt"
	"os"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	cluster       string
	namespace     string
	allClusters   bool
	format        string
	enhanced      bool
	monthlyCost   float64
	showScenarios bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-scan",
		Short: "OpsCart Kubernetes War Room Scanner",
		Long: `Emergency Kubernetes cluster scanner for war room situations.
Quickly find broken resources, idle workloads, security issues, and generate reports across multiple clusters.`,
	}

	// Emergency command
	emergencyCmd := &cobra.Command{
		Use:   "emergency",
		Short: "Find critical issues immediately",
		Long:  "Scans cluster for broken pods, failed deployments, and critical issues",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			s, err := scanner.NewScanner(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			issues, err := s.FindEmergencyIssues(namespace)
			if err != nil {
				fmt.Printf("Error scanning cluster: %v\n", err)
				os.Exit(1)
			}

			scanner.PrintEmergencyIssues(issues)
		},
	}
	emergencyCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	emergencyCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	emergencyCmd.MarkFlagRequired("cluster")

	// Resources command
	resourcesCmd := &cobra.Command{
		Use:   "resources",
		Short: "Analyze cluster resource usage",
		Long:  "Show resource consumption, waste patterns, and optimization opportunities",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			clientset, err := getKubernetesClient(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			ra := analyzer.NewResourceAnalyzer(clientset)
			analysis, err := ra.AnalyzeClusterResources(namespace)
			if err != nil {
				fmt.Printf("Error analyzing resources: %v\n", err)
				os.Exit(1)
			}

			analyzer.PrintResourceAnalysis(analysis, format)
		},
	}
	resourcesCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	resourcesCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	resourcesCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	resourcesCmd.MarkFlagRequired("cluster")

	// Security command
	securityCmd := &cobra.Command{
		Use:   "security",
		Short: "Audit cluster security posture",
		Long:  "Comprehensive security audit checking for privileged containers, missing limits, and best practices",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			clientset, err := getKubernetesClient(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			sa := analyzer.NewSecurityAuditor(clientset)
			audit, err := sa.AuditClusterSecurity(namespace)
			if err != nil {
				fmt.Printf("Error auditing security: %v\n", err)
				os.Exit(1)
			}

			analyzer.PrintSecurityAudit(audit, format)
		},
	}
	securityCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	securityCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to audit (default: all)")
	securityCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	securityCmd.MarkFlagRequired("cluster")

	// Optimize command
	optimizeCmd := &cobra.Command{
		Use:   "optimize",
		Short: "Show optimization opportunities",
		Long:  "Quick check for waste patterns and resource optimization opportunities",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			clientset, err := getKubernetesClient(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			ra := analyzer.NewResourceAnalyzer(clientset)
			analysis, err := ra.AnalyzeClusterResources(namespace)
			if err != nil {
				fmt.Printf("Error analyzing resources: %v\n", err)
				os.Exit(1)
			}

			analyzer.PrintOptimizationSummary(analysis.Optimizations)
		},
	}
	optimizeCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	optimizeCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	optimizeCmd.MarkFlagRequired("cluster")

	// Costs command
	costsCmd := &cobra.Command{
		Use:   "costs",
		Short: "Analyze cluster costs and optimization opportunities",
		Long:  "Estimate namespace costs with ranges and generate optimization scenarios",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			if monthlyCost <= 0 {
				fmt.Println("Error: --monthly-cost required (provide your total cluster cost)")
				fmt.Println("Example: opscart-scan costs --cluster prod-aks-01 --monthly-cost 10000")
				os.Exit(1)
			}

			clientset, err := getKubernetesClient(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			// First get resource analysis
			ra := analyzer.NewResourceAnalyzer(clientset)
			resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
			if err != nil {
				fmt.Printf("Error analyzing resources: %v\n", err)
				os.Exit(1)
			}

			// Then perform cost analysis
			ca := analyzer.NewCostAnalyzer(resourceAnalysis)
			costEstimate, err := ca.AnalyzeCosts(monthlyCost)
			if err != nil {
				fmt.Printf("Error analyzing costs: %v\n", err)
				os.Exit(1)
			}

			analyzer.PrintCostAnalysis(costEstimate, format)
		},
	}
	costsCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	costsCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	costsCmd.Flags().Float64VarP(&monthlyCost, "monthly-cost", "m", 0, "Total cluster cost per month (required)")
	costsCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	costsCmd.MarkFlagRequired("cluster")
	costsCmd.MarkFlagRequired("monthly-cost")

	// Find command
	findCmd := &cobra.Command{
		Use:   "find [resource-name]",
		Short: "Find a resource across clusters",
		Long:  "Search for deployments, pods, services by name pattern",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			pattern := args[0]

			if cluster == "" && !allClusters {
				fmt.Println("Error: specify --cluster or --all-clusters")
				os.Exit(1)
			}

			var clusters []string
			if allClusters {
				clusters = scanner.GetAllClusters()
			} else {
				clusters = []string{cluster}
			}

			results := scanner.FindResources(clusters, pattern)
			scanner.PrintFindResults(results)
		},
	}
	findCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	findCmd.Flags().BoolVarP(&allClusters, "all-clusters", "a", false, "Search all clusters in kubeconfig")

	// Snapshot command
	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Take a snapshot of cluster state",
		Long:  "Capture current cluster state including deployments, services, ingresses, PVCs, and network policies",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			s, err := scanner.NewScanner(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			if enhanced {
				// Enhanced snapshot with services, ingresses, PVCs
				snapshot, err := s.TakeEnhancedSnapshot(namespace)
				if err != nil {
					fmt.Printf("Error taking enhanced snapshot: %v\n", err)
					os.Exit(1)
				}
				scanner.PrintEnhancedSnapshot(snapshot, format)
			} else {
				// Basic snapshot
				snapshot, err := s.TakeSnapshot(namespace)
				if err != nil {
					fmt.Printf("Error taking snapshot: %v\n", err)
					os.Exit(1)
				}

				if format == "json" {
					scanner.PrintSnapshotJSON(snapshot)
				} else {
					scanner.PrintSnapshotTable(snapshot)
				}
			}
		},
	}
	snapshotCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	snapshotCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	snapshotCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	snapshotCmd.Flags().BoolVarP(&enhanced, "enhanced", "e", true, "Include services, ingresses, PVCs (default: true)")
	snapshotCmd.MarkFlagRequired("cluster")

	// Idle command
	idleCmd := &cobra.Command{
		Use:   "idle",
		Short: "Find idle resources wasting money",
		Long:  "Detect workloads with zero traffic or inactive for specified period",
		Run: func(cmd *cobra.Command, args []string) {
			if cluster == "" {
				fmt.Println("Error: --cluster required")
				os.Exit(1)
			}

			s, err := scanner.NewScanner(cluster)
			if err != nil {
				fmt.Printf("Error connecting to cluster: %v\n", err)
				os.Exit(1)
			}

			idle, err := s.FindIdleResources(namespace)
			if err != nil {
				fmt.Printf("Error finding idle resources: %v\n", err)
				os.Exit(1)
			}

			scanner.PrintIdleResources(idle)
		},
	}
	idleCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name (required)")
	idleCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	idleCmd.MarkFlagRequired("cluster")

	// Add all commands
	rootCmd.AddCommand(emergencyCmd)
	rootCmd.AddCommand(resourcesCmd)
	rootCmd.AddCommand(securityCmd)
	rootCmd.AddCommand(optimizeCmd)
	rootCmd.AddCommand(costsCmd)
	rootCmd.AddCommand(findCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(idleCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// getKubernetesClient creates a Kubernetes clientset for the given cluster
func getKubernetesClient(clusterContext string) (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: clusterContext,
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return clientset, nil
}
