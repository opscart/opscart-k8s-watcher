package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/config"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/spf13/cobra"
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

	// NEW v0.7 flags (cloud costs)
	region        string // cloud-costs: Azure region for pricing lookup
	autoPrice     bool   // cloud-costs: auto-detect pricing from node labels
	costFormat    string // cloud-costs: output format (table|json|html)
	pricingSource string
	cloudProvider string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-scan",
		Short: "OpsCart Kubernetes War Room Scanner",
		Long: `Emergency Kubernetes cluster scanner for war room situations.
Quickly find broken resources, idle workloads, security issues, and generate reports across multiple clusters.`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			opStore = initStore()
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			opStore.Close()
		},
	}

	rootCmd.PersistentFlags().BoolVar(&stateless, "stateless", false, "Disable operational memory persistence (restores fully-stateless behavior)")
	rootCmd.PersistentFlags().StringVar(&dbPath, "db-path", "", "Path to operational memory database (default: ~/.opscart/scan.db; ignored with --stateless)")

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

	// ================================================================
	// Cloud Costs command (NEW v0.7 — accurate pricing from node pools)
	// ================================================================
	cloudCostsCmd := &cobra.Command{
		Use:   "cloud-costs",
		Short: "Accurate cloud cost analysis from node pool VM pricing",
		Long: `Compute real costs by detecting AKS node pool VM sizes and looking up Azure retail pricing.
Breaks down costs by node pool, namespace, and deployment.

Examples:
  opscart-scan cloud-costs                           # Auto-detect region, compute from node labels
  opscart-scan cloud-costs --region eastus2          # Specify region for pricing
  opscart-scan cloud-costs --breakdown deployment    # Show per-deployment costs
  opscart-scan cloud-costs --format html             # Generate HTML report
  opscart-scan cloud-costs -n my-namespace           # Single namespace`,
		Run: func(cmd *cobra.Command, args []string) {
			clusters, isCompare, err := resolveTargetClusters()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if isCompare {
				fmt.Println("Error: --compare not yet supported for cloud-costs command")
				os.Exit(1)
			}

			if len(clusters) == 1 {
				if err := runCloudCostsScan(clusters[0].Context); err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			// Multi-cluster mode
			scanner.PrintMultiClusterHeader(clusters)
			scanFunc := func(context string) (*scanner.ClusterResult, error) {
				err := runCloudCostsScan(context)
				return &scanner.ClusterResult{}, err
			}

			runner := scanner.NewMultiClusterRunner(clusters, scanFunc)
			results := runner.RunAll()
			scanner.PrintMultiClusterSummary(results)
		},
	}
	cloudCostsCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Cluster context name")
	cloudCostsCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")
	cloudCostsCmd.Flags().StringVar(&region, "region", "", "Region override for pricing by the effective cloud provider (auto-detected from node labels if empty)")
	cloudCostsCmd.Flags().StringVar(&pricingSource, "pricing-source", scanPricingSourceDefault(), "Pricing source: auto, embedded, or aws-api")
	cloudCostsCmd.Flags().StringVar(&cloudProvider, "cloud-provider", scanCloudProviderDefault(), "Cloud provider: auto, azure, or aws")
	cloudCostsCmd.Flags().StringVar(&breakdown, "breakdown", "", "Drill-down: deployment")
	cloudCostsCmd.Flags().StringVarP(&costFormat, "format", "f", "table", "Output format (table|json|html)")
	cloudCostsCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	cloudCostsCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")
	cloudCostsCmd.Flags().BoolVar(&showScenarios, "scenarios", true, "Show optimization scenarios")

	// Add all commands
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(emergencyCmd)
	rootCmd.AddCommand(resourcesCmd)
	rootCmd.AddCommand(securityCmd)
	rootCmd.AddCommand(optimizeCmd)
	rootCmd.AddCommand(costsCmd)
	rootCmd.AddCommand(cloudCostsCmd)
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

func scanPricingSourceDefault() string {
	if value := strings.TrimSpace(os.Getenv("OPSCART_PRICING_SOURCE")); value != "" {
		return value
	}
	return "auto"
}

func scanCloudProviderDefault() string {
	if value := strings.TrimSpace(os.Getenv("OPSCART_CLOUD_PROVIDER")); value != "" {
		return value
	}
	return "auto"
}
