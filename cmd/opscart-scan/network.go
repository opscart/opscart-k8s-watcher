package main

import (
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
)

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
