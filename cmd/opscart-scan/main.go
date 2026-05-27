package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/config"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/report"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	// Existing flags
	cluster        string
	namespace      string
	allClusters    bool
	format         string // Used by resources, costs, etc.
	securityFormat string // Used by security command
	reportFormat   string // Used by report command
	enhanced       bool
	monthlyCost    float64
	breakdown      string // "" | "deployment"
	showScenarios  bool

	// NEW v0.2 flags
	allClustersFlag  bool
	clusterGroupFlag string
	compareFlag      []string

	// NEW v0.4 flags
	skipNamespacesFlag []string // network command: skip specific namespaces

	// NEW v0.5 flags
	minAgeDays  int    // waste command: minimum resource age to report
	wasteFormat string // waste command: output format (cli, html)
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-scan",
		Short: "OpsCart Kubernetes War Room Scanner",
		Long: `Emergency Kubernetes cluster scanner for war room situations.
Quickly find broken resources, idle workloads, security issues, and generate reports across multiple clusters.`,
	}

	// ================================================================
	// NEW: Config command
	// ================================================================
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage opscart configuration",
		Long:  "Manage multi-cluster configuration for opscart-k8s-watcher",
	}

	configInitCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize config file",
		Long:  "Creates ~/.opscart/config.yaml with sample cluster definitions",
		Run: func(cmd *cobra.Command, args []string) {
			if err := config.InitConfig(); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
		},
	}

	configShowCmd := &cobra.Command{
		Use:   "show",
		Short: "Show current configuration",
		Long:  "Displays all configured clusters and groups",
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := config.LoadConfig()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			cfg.PrintConfig()
		},
	}

	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configShowCmd)

	// ================================================================
	// Emergency command (UPDATED for multi-cluster)
	// ================================================================
	emergencyCmd := &cobra.Command{
		Use:   "emergency",
		Short: "Find critical issues immediately",
		Long:  "Scans cluster for broken pods, failed deployments, and critical issues",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			// Compare mode not supported for emergency (yet)
			if isCompare {
				fmt.Println("Error: --compare not yet supported for emergency command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runEmergencyScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runEmergencyScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	emergencyCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	emergencyCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	emergencyCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	emergencyCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Resources command (UPDATED for multi-cluster)
	// ================================================================
	resourcesCmd := &cobra.Command{
		Use:   "resources",
		Short: "Analyze cluster resource usage",
		Long:  "Show resource consumption, waste patterns, and optimization opportunities",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			// Compare mode not supported for resources (yet)
			if isCompare {
				fmt.Println("Error: --compare not yet supported for resources command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runResourcesScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runResourcesScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	resourcesCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	resourcesCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	resourcesCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	resourcesCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	resourcesCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Security command (UPDATED for multi-cluster + compare)
	// ================================================================
	securityCmd := &cobra.Command{
		Use:   "security",
		Short: "Audit cluster security posture",
		Long:  "Comprehensive security audit checking for privileged containers, missing limits, and best practices",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			// Compare mode
			if isCompare {
				scanner.PrintCompareHeader(clusters[0].Name, clusters[1].Name)

				// Run on both clusters
				if err := runSecurityScan(clusters[0].Context); err != nil {
					fmt.Printf("❌ %s failed: %v\n", clusters[0].Name, err)
				}
				fmt.Println()
				if err := runSecurityScan(clusters[1].Context); err != nil {
					fmt.Printf("❌ %s failed: %v\n", clusters[1].Name, err)
				}

				// Note: Full comparison diff will be enhanced when we wire in result structs
				fmt.Println("\n💡 Full side-by-side comparison coming in next iteration")
				return
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runSecurityScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runSecurityScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	securityCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	securityCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to audit (default: all)")
	securityCmd.Flags().StringVarP(&securityFormat, "format", "f", "table", "Output format (table|json|html)")
	securityCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	securityCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")
	securityCmd.Flags().StringSliceVar(&compareFlag, "compare", nil, "Compare two clusters (provide exactly 2)")

	// ================================================================
	// Optimize command (UPDATED for multi-cluster)
	// ================================================================
	optimizeCmd := &cobra.Command{
		Use:   "optimize",
		Short: "Show optimization opportunities",
		Long:  "Quick check for waste patterns and resource optimization opportunities",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not yet supported for optimize command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runOptimizeScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runOptimizeScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	optimizeCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	optimizeCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	optimizeCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	optimizeCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Costs command (UPDATED for multi-cluster)
	// ================================================================
	costsCmd := &cobra.Command{
		Use:   "costs",
		Short: "Analyze cluster costs and optimization opportunities",
		Long:  "Estimate namespace costs with ranges and generate optimization scenarios",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not yet supported for costs command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runCostsScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runCostsScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	costsCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	costsCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	costsCmd.Flags().Float64VarP(&monthlyCost, "monthly-cost", "m", 0, "Total cluster cost per month (optional; omit for resource-share-only view)")
	costsCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json|html)")
	costsCmd.Flags().StringVar(&breakdown, "breakdown", "", "Drill-down level: deployment (shows per-deployment cost within each namespace)")
	costsCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	costsCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Find command (keeps existing all-clusters flag — already works)
	// ================================================================
	findCmd := &cobra.Command{
		Use:   "find [resource-type]",
		Short: "Find resources across clusters",
		Long: `Search for Kubernetes resources by type (pod, deployment, service).

Examples:
  # Find all pods
  opscart-scan find pod --cluster prod
 
  # Find all deployments
  opscart-scan find deployment --cluster prod
 
  # Filter by status
  opscart-scan find pod --cluster prod --status=Failed
  opscart-scan find pod --cluster prod --status=Running
 
  # Filter by name pattern
  opscart-scan find pod --cluster prod --name=backend
  opscart-scan find deployment --cluster prod --name=api
 
  # Combine filters
  opscart-scan find pod --cluster prod --name=api --status=Running
 
  # Find all resource types
  opscart-scan find all --cluster prod`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			resourceType := args[0]

			// Validate resource type
			validTypes := []string{"pod", "deployment", "service", "all"}
			isValid := false
			for _, t := range validTypes {
				if resourceType == t {
					isValid = true
					break
				}
			}

			if !isValid {
				fmt.Printf("Error: Invalid resource type '%s'. Valid types: pod, deployment, service, all\n", resourceType)
				os.Exit(1)
			}

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

			// Get filter flags
			namePattern, _ := cmd.Flags().GetString("name")
			statusFilter, _ := cmd.Flags().GetString("status")

			results := scanner.FindResources(clusters, resourceType, namePattern, statusFilter)
			scanner.PrintFindResults(results)
		},
	}
	findCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	findCmd.Flags().BoolVarP(&allClusters, "all-clusters", "a", false, "Search all clusters in kubeconfig")
	findCmd.Flags().String("name", "", "Filter by name pattern (optional)")
	findCmd.Flags().String("status", "", "Filter by status (Failed, Pending, Running, etc.)")

	// ================================================================
	// Snapshot command (UPDATED for multi-cluster)
	// ================================================================
	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Take a snapshot of cluster state",
		Long:  "Capture current cluster state including deployments, services, ingresses, PVCs, and network policies",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not yet supported for snapshot command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runSnapshotScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runSnapshotScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	snapshotCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	snapshotCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	snapshotCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	snapshotCmd.Flags().BoolVarP(&enhanced, "enhanced", "e", true, "Include services, ingresses, PVCs (default: true)")
	snapshotCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	snapshotCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Idle command (UPDATED for multi-cluster)
	// ================================================================
	idleCmd := &cobra.Command{
		Use:   "idle",
		Short: "Find idle resources wasting money",
		Long:  "Detect workloads with zero traffic or inactive for specified period",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not yet supported for idle command")
				os.Exit(1)
			}

			// Single cluster (existing behavior)
			if len(clusters) == 1 {
				if err := runIdleScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runIdleScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	idleCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	idleCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to scan (default: all)")
	idleCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	idleCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")

	// ================================================================
	// Report command - NEW in v0.3
	// ================================================================
	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Generate comprehensive cluster report",
		Long:  "Generate HTML/JSON/CSV report combining security, resources, and cost analysis",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not supported for report command")
				os.Exit(1)
			}

			// Single cluster
			if len(clusters) == 1 {
				if err := runReportGeneration(clusters[0].Context, clusters[0].Name); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster
			scanner.PrintMultiClusterHeader(clusters)
			for i, cluster := range clusters {
				fmt.Printf("\n🔄 Generating report for %s (%d/%d)...\n", cluster.Name, i+1, len(clusters))
				if err := runReportGeneration(cluster.Context, cluster.Name); err != nil {
					fmt.Printf("❌ %s failed: %v\n", cluster.Name, err)
				}
			}

			fmt.Println("\n✅ All reports generated!")
		},
	}

	reportCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	reportCmd.Flags().StringVarP(&reportFormat, "format", "f", "html", "Output format (html|json|csv)")
	reportCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Generate reports for all clusters")
	reportCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Generate reports for cluster group")
	reportCmd.Flags().Float64Var(&monthlyCost, "monthly-cost", 0, "Monthly cluster cost (optional)")

	// ================================================================
	// Network command - NEW in v0.4
	// ================================================================
	networkCmd := &cobra.Command{
		Use:   "network",
		Short: "Analyze NetworkPolicy coverage across namespaces",
		Long:  "Scan cluster for NetworkPolicy resources and identify namespaces with no network isolation",
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not supported for network command")
				os.Exit(1)
			}

			// Single cluster
			if len(clusters) == 1 {
				if err := runNetworkScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster
			scanner.PrintMultiClusterHeader(clusters)
			for i, cl := range clusters {
				fmt.Printf("\n🔄 Scanning network policies for %s (%d/%d)...\n", cl.Name, i+1, len(clusters))
				if err := runNetworkScan(cl.Context); err != nil {
					fmt.Printf("❌ %s failed: %v\n", cl.Name, err)
				}
			}
		},
	}
	networkCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	networkCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Limit to specific namespace")
	networkCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	networkCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")
	networkCmd.Flags().StringSliceVar(&skipNamespacesFlag, "skip-namespaces", []string{}, "Additional namespaces to skip (comma-separated)")

	// ================================================================
	// Waste command - NEW in v0.5
	// ================================================================
	wasteCmd := &cobra.Command{
		Use:   "waste",
		Short: "Detect drifted, idle, and orphaned resources",
		Long: `Scan cluster for resources that are old, idle, or orphaned.
Shows data-driven findings and suggestions. Does not delete anything.

Detects:
  - Abandoned namespaces (no running pods, old creation date)
  - Zombie pods (CrashLoopBackOff, OOMKilled for days)
  - Idle pods (old, no restarts, no recent activity)
  - Orphaned PVCs (unbound, released, or bound with no pod)
  - Stale Jobs/CronJobs (completed, failed, or never ran)
  - Zero-replica Deployments and StatefulSets
  - Old ReplicaSets (leftover from rollouts)
  - Services with no endpoints
  - Ingresses with missing backends
  - Misconfigured HPAs`,
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not supported for waste command")
				os.Exit(1)
			}

			// Single cluster
			if len(clusters) == 1 {
				if err := runWasteScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster
			scanner.PrintMultiClusterHeader(clusters)
			for i, cl := range clusters {
				fmt.Printf("\n🔄 Scanning waste for %s (%d/%d)...\n", cl.Name, i+1, len(clusters))
				if err := runWasteScan(cl.Context); err != nil {
					fmt.Printf("❌ %s failed: %v\n", cl.Name, err)
				}
			}
		},
	}
	wasteCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	wasteCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Limit scan to specific namespace")
	wasteCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	wasteCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")
	wasteCmd.Flags().IntVar(&minAgeDays, "min-age-days", 7, "Minimum resource age in days to report (default: 7)")
	wasteCmd.Flags().Float64Var(&monthlyCost, "monthly-cost", 0, "Monthly cluster cost for waste estimation (optional)")
	wasteCmd.Flags().StringVar(&wasteFormat, "format", "cli", "Output format: cli (default) or html")

	// Add all commands
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(emergencyCmd)
	rootCmd.AddCommand(resourcesCmd)
	rootCmd.AddCommand(securityCmd)
	rootCmd.AddCommand(optimizeCmd)
	rootCmd.AddCommand(costsCmd)
	rootCmd.AddCommand(findCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(idleCmd)
	rootCmd.AddCommand(reportCmd)
	rootCmd.AddCommand(networkCmd)
	rootCmd.AddCommand(wasteCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// ================================================================
// Helper: resolveTargetClusters
// ================================================================
// Determines WHAT to scan based on flags
func resolveTargetClusters() ([]config.ClusterConfig, bool, error) {
	isCompare := len(compareFlag) > 0

	// Case 1: --compare flag (exactly 2 clusters)
	if isCompare {
		if len(compareFlag) != 2 {
			return nil, false, fmt.Errorf("--compare requires exactly 2 cluster names")
		}
		cfg, err := config.LoadConfig()
		if err != nil {
			return nil, false, err
		}
		a, err := cfg.GetClusterByName(compareFlag[0])
		if err != nil {
			return nil, false, err
		}
		b, err := cfg.GetClusterByName(compareFlag[1])
		if err != nil {
			return nil, false, err
		}
		return []config.ClusterConfig{*a, *b}, true, nil
	}

	// Case 2: --all-clusters flag
	if allClustersFlag {
		cfg, err := config.LoadConfig()
		if err != nil {
			return nil, false, err
		}
		clusters := cfg.GetAllClusters()
		if len(clusters) == 0 {
			return nil, false, fmt.Errorf("no clusters in config. Run: ./opscart-scan config init")
		}
		return clusters, false, nil
	}

	// Case 3: --cluster-group flag
	if clusterGroupFlag != "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return nil, false, err
		}
		clusters, err := cfg.GetClustersByGroup(clusterGroupFlag)
		if err != nil {
			return nil, false, err
		}
		return clusters, false, nil
	}

	// Case 4: --cluster flag (single cluster — existing behavior)
	if cluster != "" {
		return []config.ClusterConfig{
			{Name: cluster, Context: cluster, Group: "single"},
		}, false, nil
	}

	// Case 5: nothing specified
	return nil, false, fmt.Errorf("specify a target:\n  --cluster <name>           Single cluster\n  --all-clusters             All configured clusters\n  --cluster-group <group>    All clusters in a group\n  --compare <a> <b>          Compare two clusters")
}

// ================================================================
// Extracted scan functions (existing logic moved to functions)
// ================================================================

func runEmergencyScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	s, err := scanner.NewScanner(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	issues, err := s.FindEmergencyIssues(namespace)
	if err != nil {
		return fmt.Errorf("scanning cluster: %w", err)
	}

	scanner.PrintEmergencyIssues(issues)
	return nil
}

func runResourcesScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	ra := analyzer.NewResourceAnalyzer(clientset)
	analysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	analyzer.PrintResourceAnalysis(analysis, format)
	return nil
}

func runSecurityScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)

	// Default to table if not specified
	if securityFormat == "" {
		securityFormat = "table"
	}

	// Check if HTML report requested
	if securityFormat == "html" {
		return generateSecurityReport(clusterContext)
	}

	// Terminal output
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("auditing security: %w", err)
	}

	analyzer.PrintSecurityAudit(audit, securityFormat)
	return nil
}

func runOptimizeScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	ra := analyzer.NewResourceAnalyzer(clientset)
	analysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	analyzer.PrintOptimizationSummary(analysis.Optimizations)
	return nil
}

func runCostsScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// First get resource analysis
	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	// Then perform cost analysis
	ca := analyzer.NewCostAnalyzer(resourceAnalysis)
	costEstimate, err := ca.AnalyzeCosts(monthlyCost) // monthlyCost=0 => resource-share mode
	if err != nil {
		return fmt.Errorf("analyzing costs: %w", err)
	}

	// Inject cluster name for HTML report
	costEstimate.ClusterName = clusterContext

	// Optionally enrich with deployment-level breakdown
	if breakdown == "deployment" {
		da := analyzer.NewDeploymentCostAnalyzer(clientset)
		enriched, err := da.EnrichWithDeployments(costEstimate.NamespaceCosts)
		if err == nil {
			costEstimate.NamespaceCosts = enriched
		}
	}

	if err := analyzer.PrintCostAnalysis(costEstimate, format); err != nil {
		return fmt.Errorf("rendering output: %w", err)
	}
	return nil
}
func runSnapshotScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	s, err := scanner.NewScanner(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	if enhanced {
		// Enhanced snapshot with services, ingresses, PVCs
		snapshot, err := s.TakeEnhancedSnapshot(namespace)
		if err != nil {
			return fmt.Errorf("taking enhanced snapshot: %w", err)
		}
		scanner.PrintEnhancedSnapshot(snapshot, format)
	} else {
		// Basic snapshot
		snapshot, err := s.TakeSnapshot(namespace)
		if err != nil {
			return fmt.Errorf("taking snapshot: %w", err)
		}

		if format == "json" {
			scanner.PrintSnapshotJSON(snapshot)
		} else {
			scanner.PrintSnapshotTable(snapshot)
		}
	}
	return nil
}

func runIdleScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	s, err := scanner.NewScanner(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	idle, err := s.FindIdleResources(namespace)
	if err != nil {
		return fmt.Errorf("finding idle resources: %w", err)
	}

	scanner.PrintIdleResources(idle)
	return nil
}

func runReportGeneration(clusterContext string, clusterName string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterName)
	fmt.Println("📊 Generating comprehensive report...")

	// Get Kubernetes client
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// Run resource analysis for real ResourceScore and CostScore
	fmt.Println("  📦 Running resource analysis...")
	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, raErr := ra.AnalyzeClusterResources(namespace)
	if raErr != nil {
		fmt.Printf("  ⚠️  Resource analysis skipped: %v\n", raErr)
		resourceAnalysis = nil
	}

	// Run REAL security audit
	fmt.Println("  🛡️  Running security audit...")
	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("security audit failed: %w", err)
	}

	// Run network policy audit for CIS 5.7.3 — real coverage data
	fmt.Println("  🌐 Running network policy audit (CIS 5.7.3)...")
	npa := analyzer.NewNetworkPolicyAuditor(clientset)
	netAudit, netErr := npa.AuditNetworkPolicies(namespace)
	if netErr != nil {
		fmt.Printf("  ⚠️  Network policy audit skipped: %v\n", netErr)
		netAudit = nil
	}
	cisResult := analyzer.CalculateCISScore(audit, netAudit)

	// Build report data with REAL security findings
	reportData := &report.ReportData{
		ClusterName:    clusterName,
		GeneratedAt:    time.Now(),
		CISScore:       cisResult.Score,
		SecurityScore:  cisResult.Score,
		ControlsPassed: cisResult.PassedChecks,
		ControlsFailed: cisResult.FailedChecks,
		PodCount:       audit.TotalPodsAudited,
		NamespaceCount: len(audit.Issues),
		MonthlyCost:    monthlyCost,
	}

	namespaceCostBest := map[string]float64{}
	namespaceCostLow := map[string]float64{}
	namespaceCostHigh := map[string]float64{}

	// Calculate savings if cost provided
	if monthlyCost > 0 {
		if resourceAnalysis != nil {
			ca := analyzer.NewCostAnalyzer(resourceAnalysis)
			costEstimate, costErr := ca.AnalyzeCosts(monthlyCost)
			if costErr != nil {
				fmt.Printf("  ⚠️  Cost analysis fallback used: %v\n", costErr)
				reportData.PotentialSavings = report.SavingsRange{
					Min: monthlyCost * 0.24,
					Max: monthlyCost * 0.36,
				}
			} else {
				reportData.PotentialSavings = report.SavingsRange{
					Min: costEstimate.TotalSavingsPotential.Low,
					Max: costEstimate.TotalSavingsPotential.High,
				}
				reportData.CostBreakdown = buildCostBreakdownFromScenarios(costEstimate.OptimizationScenarios, monthlyCost)

				for _, namespaceCost := range costEstimate.NamespaceCosts {
					namespaceCostBest[namespaceCost.Name] = namespaceCost.EstimatedCost.Best
					namespaceCostLow[namespaceCost.Name] = namespaceCost.EstimatedCost.Low
					namespaceCostHigh[namespaceCost.Name] = namespaceCost.EstimatedCost.High
				}
			}
		} else {
			reportData.PotentialSavings = report.SavingsRange{
				Min: monthlyCost * 0.24,
				Max: monthlyCost * 0.36,
			}
		}
	}

	// Extract security risks
	risks := audit.Risks

	// Add critical issues
	if risks.PrivilegedContainers > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d privileged containers detected", risks.PrivilegedContainers),
			Description: "Containers with elevated privileges can escape containment",
			Count:       risks.PrivilegedContainers,
		})
	}

	if risks.HostPathVolumes > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d pods mounting host paths", risks.HostPathVolumes),
			Description: "Host path volumes provide direct access to host filesystem",
			Count:       risks.HostPathVolumes,
		})
	}

	if risks.HostPID > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d containers sharing host PID namespace", risks.HostPID),
			Description: "Host PID namespace sharing allows container processes to see all processes",
			Count:       risks.HostPID,
		})
	}

	// Add warnings
	if risks.RunningAsRoot > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers running as root", risks.RunningAsRoot),
			Description: "Running as root increases attack surface",
			Count:       risks.RunningAsRoot,
		})
	}

	if risks.MissingResourceLimits > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers missing resource limits", risks.MissingResourceLimits),
			Description: "Missing resource limits can lead to resource exhaustion",
			Count:       risks.MissingResourceLimits,
		})
	}

	if risks.HostNetwork > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers using host network", risks.HostNetwork),
			Description: "Host network access bypasses network policies",
			Count:       risks.HostNetwork,
		})
	}

	if risks.HostIPC > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers sharing host IPC namespace", risks.HostIPC),
			Description: "Host IPC namespace sharing can leak sensitive information",
			Count:       risks.HostIPC,
		})
	}

	if risks.PrivilegeEscalation > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers allowing privilege escalation", risks.PrivilegeEscalation),
			Description: "Privilege escalation can lead to container breakout",
			Count:       risks.PrivilegeEscalation,
		})
	}

	if risks.DefaultServiceAccount > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d pods using default service account", risks.DefaultServiceAccount),
			Description: "Default service account may have excessive permissions",
			Count:       risks.DefaultServiceAccount,
		})
	}

	if risks.AddedCapabilities > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers with added capabilities", risks.AddedCapabilities),
			Description: "Unnecessary capabilities increase attack surface",
			Count:       risks.AddedCapabilities,
		})
	}

	// Calculate real resource and cost scores from actual cluster data
	resourceScore := 75 // fallback if resource analysis was skipped
	costScore := 60     // fallback if resource analysis was skipped
	if resourceAnalysis != nil {
		avgUtilization := (resourceAnalysis.CPUUtilization + resourceAnalysis.MemoryUtilization) / 2
		resourceScore = report.CalculateResourceScore(avgUtilization)

		totalPods, idlePods, spotEligible := 0, 0, 0
		for _, ns := range resourceAnalysis.Namespaces {
			totalPods += ns.PodCount
			idlePods += ns.IdlePods
			spotEligible += ns.SpotEligiblePods
		}
		if totalPods > 0 {
			costScore = report.CalculateCostScore(idlePods, spotEligible, totalPods)
		}

		// Populate resource fields in report data
		reportData.TotalCPU = resourceAnalysis.TotalCPUCores
		reportData.TotalMemory = resourceAnalysis.TotalMemoryGB
		reportData.UsedCPU = resourceAnalysis.TotalCPURequested
		reportData.UsedMemory = resourceAnalysis.TotalMemoryRequested

		for _, namespaceUsage := range resourceAnalysis.Namespaces {
			flags := append([]string{}, namespaceUsage.Flags...)
			if namespaceUsage.IdlePods > 0 {
				flags = append(flags, fmt.Sprintf("IDLE-%dp", namespaceUsage.IdlePods))
			}
			if namespaceUsage.SpotEligiblePods > 0 {
				flags = append(flags, fmt.Sprintf("SPOT-OK (%d)", namespaceUsage.SpotEligiblePods))
			}

			weightedSharePct := namespaceUsage.WeightedShare() * 100
			bestCost := namespaceCostBest[namespaceUsage.Name]
			lowCost := namespaceCostLow[namespaceUsage.Name]
			highCost := namespaceCostHigh[namespaceUsage.Name]

			if monthlyCost > 0 && bestCost == 0 {
				bestCost = monthlyCost * namespaceUsage.WeightedShare()
				lowCost = bestCost * 0.8
				highCost = bestCost * 1.2
			}

			reportData.Namespaces = append(reportData.Namespaces, report.NamespaceItem{
				Name:             namespaceUsage.Name,
				CPUPercent:       namespaceUsage.CPUPercent,
				MemPercent:       namespaceUsage.MemoryPercent,
				PodCount:         namespaceUsage.PodCount,
				Cost:             bestCost,
				CostLow:          lowCost,
				CostHigh:         highCost,
				WeightedShare:    weightedSharePct,
				Confidence:       deriveCostConfidence(weightedSharePct),
				IdlePods:         namespaceUsage.IdlePods,
				SpotEligiblePods: namespaceUsage.SpotEligiblePods,
				Flags:            flags,
			})
		}

		if monthlyCost > 0 {
			sort.Slice(reportData.Namespaces, func(i, j int) bool {
				return reportData.Namespaces[i].Cost > reportData.Namespaces[j].Cost
			})
		}
	}
	reportData.ResourceScore = resourceScore
	reportData.CostScore = costScore
	reportData.OverallScore = report.CalculateOverallScore(reportData.SecurityScore, resourceScore, costScore)

	// Default to html if not specified
	if reportFormat == "" {
		reportFormat = "html"
	}

	// Determine format
	var reportFmt report.ReportFormat
	switch reportFormat {
	case "html":
		reportFmt = report.FormatHTML
	case "json":
		reportFmt = report.FormatJSON
	case "csv":
		reportFmt = report.FormatCSV
	default:
		return fmt.Errorf("unsupported format: %s", reportFormat)
	}

	// Generate report
	generator := report.NewGenerator(reportFmt, "")
	outputPath, err := generator.Generate(reportData)
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	// Show success
	fmt.Printf("\n✅ Report generated: %s\n", outputPath)
	if reportFmt == report.FormatHTML {
		fmt.Printf("🌐 Open in browser: file://%s\n", outputPath)
	}
	fmt.Printf("📊 Summary: CIS Score %d/100 | %d Critical | %d Warnings | %d Total Issues\n",
		cisResult.Score, len(reportData.CriticalIssues), len(reportData.WarningIssues), len(audit.Issues))

	return nil
}

func buildCostBreakdownFromScenarios(scenarios []models.OptimizationScenario, totalClusterCost float64) []report.CostItem {
	if len(scenarios) == 0 {
		return nil
	}

	sortedScenarios := make([]models.OptimizationScenario, len(scenarios))
	copy(sortedScenarios, scenarios)
	sort.Slice(sortedScenarios, func(i, j int) bool {
		return sortedScenarios[i].Savings.Best > sortedScenarios[j].Savings.Best
	})

	items := make([]report.CostItem, 0, len(sortedScenarios))
	for _, scenario := range sortedScenarios {
		impact := "Low"
		if totalClusterCost > 0 {
			savingsPercent := (scenario.Savings.Best / totalClusterCost) * 100
			if savingsPercent >= 10 {
				impact = "High"
			} else if savingsPercent >= 5 {
				impact = "Medium"
			}
		}

		action := scenario.Timeline
		if len(scenario.Actions) > 0 {
			action = scenario.Actions[0]
		}

		items = append(items, report.CostItem{
			Name:    scenario.Name,
			Impact:  impact,
			Savings: scenario.Savings.Best,
			Action:  action,
		})
	}

	return items
}

func deriveCostConfidence(weightedSharePct float64) string {
	if weightedSharePct >= 15 {
		return "High"
	}
	if weightedSharePct >= 5 {
		return "Medium"
	}
	if weightedSharePct < 2 {
		return "Low"
	}
	return "Medium"
}

func generateSecurityReport(clusterContext string) error {
	fmt.Println("📊 Generating security report...")

	// Get clientset and run audit
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("auditing security: %w", err)
	}

	// Run network policy audit for CIS 5.7.3 — real coverage data
	npa := analyzer.NewNetworkPolicyAuditor(clientset)
	netAudit, netErr := npa.AuditNetworkPolicies(namespace)
	if netErr != nil {
		netAudit = nil
	}

	// Calculate CIS score with real network data
	cisResult := analyzer.CalculateCISScore(audit, netAudit)

	// Build report data with REAL values
	reportData := &report.ReportData{
		ClusterName:    clusterContext,
		GeneratedAt:    time.Now(),
		CISScore:       cisResult.Score,
		SecurityScore:  cisResult.Score,
		ControlsPassed: cisResult.PassedChecks,
		ControlsFailed: cisResult.FailedChecks,
		PodCount:       audit.TotalPodsAudited,
		NamespaceCount: len(audit.Issues),
	}

	risks := audit.Risks

	// Add critical issues with details
	if risks.PrivilegedContainers > 0 {
		details := extractResourceNames(audit.Issues, "privileged_container", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d privileged containers detected", risks.PrivilegedContainers),
			Description: "Containers with elevated privileges can escape containment and compromise the host",
			Count:       risks.PrivilegedContainers,
			Details:     details,
		})
	}

	if risks.HostPID > 0 {
		details := extractResourceNames(audit.Issues, "host_pid", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d containers sharing host PID namespace", risks.HostPID),
			Description: "Host PID namespace sharing allows container processes to see all host processes",
			Count:       risks.HostPID,
			Details:     details,
		})
	}

	if risks.HostPathVolumes > 0 {
		details := extractResourceNames(audit.Issues, "host_path_volume", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d pods mounting host paths", risks.HostPathVolumes),
			Description: "Host path volumes provide direct access to host filesystem",
			Count:       risks.HostPathVolumes,
			Details:     details,
		})
	}

	// Add warnings with details
	if risks.HostIPC > 0 {
		details := extractResourceNames(audit.Issues, "host_ipc", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers sharing host IPC namespace", risks.HostIPC),
			Description: "Host IPC namespace sharing can leak sensitive information",
			Count:       risks.HostIPC,
			Details:     details,
		})
	}

	if risks.RunningAsRoot > 0 {
		details := extractResourceNames(audit.Issues, "running_as_root", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers running as root", risks.RunningAsRoot),
			Description: "Running as root increases attack surface",
			Count:       risks.RunningAsRoot,
			Details:     details,
		})
	}

	if risks.MissingResourceLimits > 0 {
		details := extractResourceNames(audit.Issues, "missing_resource_limits", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers missing resource limits", risks.MissingResourceLimits),
			Description: "Missing resource limits can lead to resource exhaustion",
			Count:       risks.MissingResourceLimits,
			Details:     details,
		})
	}

	if risks.HostNetwork > 0 {
		details := extractResourceNames(audit.Issues, "host_network", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers using host network", risks.HostNetwork),
			Description: "Host network access bypasses network policies",
			Count:       risks.HostNetwork,
			Details:     details,
		})
	}

	if risks.PrivilegeEscalation > 0 {
		details := extractResourceNames(audit.Issues, "privilege_escalation", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers allowing privilege escalation", risks.PrivilegeEscalation),
			Description: "Privilege escalation can lead to container breakout",
			Count:       risks.PrivilegeEscalation,
			Details:     details,
		})
	}

	if risks.AddedCapabilities > 0 {
		details := extractResourceNames(audit.Issues, "added_capabilities", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers with added capabilities", risks.AddedCapabilities),
			Description: "Unnecessary capabilities increase attack surface",
			Count:       risks.AddedCapabilities,
			Details:     details,
		})
	}

	if risks.DefaultServiceAccount > 0 {
		details := extractResourceNames(audit.Issues, "default_service_account", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d pods using default service account", risks.DefaultServiceAccount),
			Description: "Default service account may have excessive permissions",
			Count:       risks.DefaultServiceAccount,
			Details:     details,
		})
	}

	// Generate HTML report
	generator := report.NewGenerator(report.FormatHTML, "")
	outputPath, err := generator.GenerateSecurityHTML(reportData)
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	fmt.Printf("\n✅ Security report generated: %s\n", outputPath)
	fmt.Printf("🌐 Open in browser: file://%s\n", outputPath)
	fmt.Printf("\n📊 Summary: CIS Score %d/100 | %d Critical | %d Warnings | %d Total Issues\n",
		cisResult.Score, len(reportData.CriticalIssues), len(reportData.WarningIssues), len(audit.Issues))

	return nil
}

// extractResourceNames gets top N resource names (deduplicated with counts)
func extractResourceNames(issues []models.SecurityIssue, issueType string, limit int) []string {
	podCounts := make(map[string]int)

	for _, issue := range issues {
		if issue.Type == issueType {
			key := issue.Namespace + "/" + issue.Name
			podCounts[key]++
		}
	}

	type podInfo struct {
		key   string
		count int
	}
	var pods []podInfo
	for key, count := range podCounts {
		pods = append(pods, podInfo{key, count})
	}

	// Sort by count descending
	for i := 0; i < len(pods)-1; i++ {
		for j := i + 1; j < len(pods); j++ {
			if pods[j].count > pods[i].count {
				pods[i], pods[j] = pods[j], pods[i]
			}
		}
	}

	var resources []string
	for i := 0; i < len(pods) && i < limit; i++ {
		parts := strings.Split(pods[i].key, "/")
		podName := parts[1]
		namespace := parts[0]

		if pods[i].count > 1 {
			resources = append(resources, fmt.Sprintf("%s in namespace %s (%d issues)", podName, namespace, pods[i].count))
		} else {
			resources = append(resources, fmt.Sprintf("%s in namespace %s", podName, namespace))
		}
	}

	remaining := len(pods) - limit
	if remaining > 0 {
		resources = append(resources, fmt.Sprintf("... and %d more pods", remaining))
	}

	return resources
}

// ================================================================
// Existing helper (unchanged)
// ================================================================

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

func runNetworkScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)

	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	npa := analyzer.NewNetworkPolicyAuditor(clientset).
		WithSkipNamespaces(skipNamespacesFlag)
	audit, err := npa.AuditNetworkPolicies(namespace)
	if err != nil {
		return fmt.Errorf("auditing network policies: %w", err)
	}

	analyzer.PrintNetworkPolicyAudit(audit)
	return nil
}

func runWasteScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	fmt.Printf("   Minimum age: %d days\n", minAgeDays)
	if wasteFormat == "html" {
		fmt.Printf("   Format: HTML report\n")
	}

	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	wa, cancel := analyzer.NewWasteAuditor(clientset, minAgeDays)
	defer cancel() // Ensure context cleanup even if we exit early

	audit, err := wa.AuditWaste(namespace)
	if err != nil {
		return fmt.Errorf("auditing waste: %w", err)
	}

	// Output based on format
	switch wasteFormat {
	case "html":
		if err := report.GenerateWasteHTML(audit, clusterContext, minAgeDays); err != nil {
			return fmt.Errorf("generating HTML report: %w", err)
		}
	default:
		analyzer.PrintWasteAudit(audit, minAgeDays)
	}

	return nil
}
