package main

import (
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// resolveTargetClusters determines which cluster(s) to target based on flags:
// --compare, --all-clusters, --cluster-group, or --cluster.
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

// getKubernetesClient builds a Kubernetes clientset for the given context name.
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
	// Default client-go limits (QPS 5, Burst 10) cause skipped sub-scans on
	// large clusters ("context deadline exceeded").
	config.QPS = 50
	config.Burst = 100

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return clientset, nil
}
