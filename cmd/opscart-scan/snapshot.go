package main

import (
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
)

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
