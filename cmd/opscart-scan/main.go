package main

import (
	"fmt"
	"os"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/config"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	// Existing flags
	cluster       string
	namespace     string
	allClusters   bool
	format        string
	enhanced      bool
	monthlyCost   float64
	showScenarios bool

	// NEW v0.2 flags
	allClustersFlag  bool
	clusterGroupFlag string
	compareFlag      []string
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
	securityCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
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

			if monthlyCost <= 0 {
				fmt.Println("Error: --monthly-cost required")
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
	costsCmd.Flags().Float64VarP(&monthlyCost, "monthly-cost", "m", 0, "Total cluster cost per month (required)")
	costsCmd.Flags().StringVarP(&format, "format", "f", "table", "Output format (table|json)")
	costsCmd.Flags().BoolVar(&allClustersFlag, "all-clusters", false, "Scan all configured clusters")
	costsCmd.Flags().StringVar(&clusterGroupFlag, "cluster-group", "", "Scan all clusters in a group")
	costsCmd.MarkFlagRequired("monthly-cost")

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
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("auditing security: %w", err)
	}

	analyzer.PrintSecurityAudit(audit, format)
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
	costEstimate, err := ca.AnalyzeCosts(monthlyCost)
	if err != nil {
		return fmt.Errorf("analyzing costs: %w", err)
	}

	analyzer.PrintCostAnalysis(costEstimate, format)
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
