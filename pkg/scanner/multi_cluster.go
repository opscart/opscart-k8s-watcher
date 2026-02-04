package scanner

import (
	"fmt"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/config"
)

// ClusterResult holds scan result for one cluster
type ClusterResult struct {
	ClusterName string
	Context     string
	Group       string
	Duration    time.Duration
	Error       error
	// Add your existing result types here as you wire them up:
	// SecurityResult   *models.SecurityResult
	// EmergencyResult  *models.EmergencyResult
	// ResourceResult   *models.ResourceResult
	// CostResult       *models.CostResult
	// SnapshotResult   *models.EnhancedClusterSnapshot
}

// MultiClusterRunner orchestrates scans across multiple clusters
type MultiClusterRunner struct {
	clusters []config.ClusterConfig
	scanFunc func(clusterContext string) (*ClusterResult, error) // injected scan function
	parallel bool
}

// NewMultiClusterRunner creates a runner for the given clusters
func NewMultiClusterRunner(clusters []config.ClusterConfig, scanFunc func(string) (*ClusterResult, error)) *MultiClusterRunner {
	return &MultiClusterRunner{
		clusters: clusters,
		scanFunc: scanFunc,
		parallel: false,
	}
}

// RunAll executes scans across all clusters
func (r *MultiClusterRunner) RunAll() []ClusterResult {
	results := make([]ClusterResult, len(r.clusters))

	if r.parallel {
		r.runParallel(results)
	} else {
		r.runSequential(results)
	}

	return results
}

// runParallel scans all clusters concurrently
func (r *MultiClusterRunner) runParallel(results []ClusterResult) {
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, cluster := range r.clusters {
		wg.Add(1)
		go func(idx int, c config.ClusterConfig) {
			defer wg.Done()

			fmt.Printf("🔄 Scanning %s...\n", c.Name)
			start := time.Now()

			result, err := r.scanFunc(c.Context)
			duration := time.Since(start)

			mu.Lock()
			if err != nil {
				results[idx] = ClusterResult{
					ClusterName: c.Name,
					Context:     c.Context,
					Group:       c.Group,
					Duration:    duration,
					Error:       err,
				}
				fmt.Printf("❌ %s failed: %v\n", c.Name, err)
			} else {
				result.ClusterName = c.Name
				result.Context = c.Context
				result.Group = c.Group
				result.Duration = duration
				results[idx] = *result
				fmt.Printf("✅ %s done (%v)\n", c.Name, duration.Round(time.Millisecond))
			}
			mu.Unlock()
		}(i, cluster)
	}

	wg.Wait()
}

// runSequential scans clusters one at a time
func (r *MultiClusterRunner) runSequential(results []ClusterResult) {
	for i, cluster := range r.clusters {
		fmt.Printf("🔄 Scanning %s (%d/%d)...\n", cluster.Name, i+1, len(r.clusters))
		start := time.Now()

		result, err := r.scanFunc(cluster.Context)
		duration := time.Since(start)

		if err != nil {
			results[i] = ClusterResult{
				ClusterName: cluster.Name,
				Context:     cluster.Context,
				Group:       cluster.Group,
				Duration:    duration,
				Error:       err,
			}
			fmt.Printf("❌ %s failed: %v\n", cluster.Name, err)
		} else {
			result.ClusterName = cluster.Name
			result.Context = cluster.Context
			result.Group = cluster.Group
			result.Duration = duration
			results[i] = *result
			fmt.Printf("✅ %s done (%v)\n", cluster.Name, duration.Round(time.Millisecond))
		}
	}
}

// PrintMultiClusterHeader prints the header for multi-cluster output
func PrintMultiClusterHeader(clusters []config.ClusterConfig) {
	fmt.Println()
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║           MULTI-CLUSTER SCAN                              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Printf("  📦 Scanning %d clusters...\n", len(clusters))
	fmt.Println("  ─────────────────────────────────────────────")
	for _, c := range clusters {
		fmt.Printf("  • %-20s [%s]\n", c.Name, c.Group)
	}
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Println()
}

// PrintMultiClusterSummary prints a summary across all results
func PrintMultiClusterSummary(results []ClusterResult) {
	fmt.Println()
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║           MULTI-CLUSTER SUMMARY                           ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  %-20s %-12s %-10s\n", "CLUSTER", "GROUP", "STATUS")
	fmt.Println("  ─────────────────────────────────────────────")

	success := 0
	failed := 0
	for _, r := range results {
		if r.Error != nil {
			fmt.Printf("  %-20s %-12s ❌ %v\n", r.ClusterName, r.Group, r.Error)
			failed++
		} else {
			fmt.Printf("  %-20s %-12s ✅ (%v)\n", r.ClusterName, r.Group, r.Duration.Round(time.Millisecond))
			success++
		}
	}

	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Printf("  ✅ Success: %d  |  ❌ Failed: %d  |  📦 Total: %d\n", success, failed, len(results))
	fmt.Println()
}
