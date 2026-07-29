package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	"github.com/spf13/cobra"
)

var (
	port          string
	cluster       string
	clustersFlag  string
	region        string
	breakdown     string
	namespace     string
	pricingSource string
	cloudProvider string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-dashboard",
		Short: "OpsCart cloud cost dashboard server",
		Long: `Serves a live cloud cost FinOps dashboard for Kubernetes clusters.

Routes:
  GET  /               — HTML dashboard (auto-refreshes every 60s)
  POST /refresh        — trigger an immediate re-scan
  GET  /api/report     — full CloudCostReport as JSON
  GET  /api/overview   — summary KPIs as JSON
  GET  /api/summary    — {monthly_cost, waste_total, security_score, cluster_count, pod_count}
  GET  /api/warroom    — top 5 critical issues (crash pods, privileged containers, unprotected namespaces)
  GET  /warroom        — full War Room page (crash loops, OOMKilled, ImagePullBackOff, unprotected namespaces, orphaned PVCs, zero-replica workloads)
  GET  /healthz        — liveness probe

All data routes accept ?cluster=<context> to target a specific cluster.`,
		RunE: runDashboard,
	}

	rootCmd.Flags().StringVarP(&port, "port", "p", "8080", "Port to listen on")
	rootCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Kubernetes context to scan (default: current context)")
	rootCmd.Flags().StringVar(&clustersFlag, "clusters", "", "Comma-separated Kubernetes contexts for the sidebar selector")
	rootCmd.Flags().StringVar(&region, "region", "", "Region override for pricing by the effective cloud provider (auto-detected from node labels if empty)")
	rootCmd.Flags().StringVar(&pricingSource, "pricing-source", pricingSourceDefault(), "Pricing source: auto, embedded, or aws-api")
	rootCmd.Flags().StringVar(&cloudProvider, "cloud-provider", cloudProviderDefault(), "Cloud provider: auto, azure, or aws")
	rootCmd.Flags().StringVar(&breakdown, "breakdown", "", "Cost breakdown level: '' or 'deployment'")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── Cluster list ─────────────────────────────────────────────────────────────

func parseClusterList() []string {
	seen := map[string]bool{}
	var list []string
	add := func(ctx string) {
		ctx = strings.TrimSpace(ctx)
		if seen[ctx] {
			return
		}
		seen[ctx] = true
		list = append(list, ctx)
	}
	add(cluster)
	if clustersFlag != "" {
		for _, ctx := range strings.Split(clustersFlag, ",") {
			ctx = strings.TrimSpace(ctx)
			if ctx != "" { // only skip empties from the --clusters split, not from --cluster itself
				add(ctx)
			}
		}
	}
	return list
}

// ── runDashboard ──────────────────────────────────────────────────────────────

func runDashboard(_ *cobra.Command, _ []string) error {
	if pricingSource != "auto" && pricingSource != "embedded" && pricingSource != "aws-api" {
		return fmt.Errorf("invalid --pricing-source %q: use auto, embedded, or aws-api", pricingSource)
	}
	if _, err := analyzer.ParseCloudProviderOverride(cloudProvider); err != nil {
		return err
	}
	cl := parseClusterList()
	dbPath := os.Getenv("OPSCART_DB_PATH")
	if dbPath == "" {
		dbPath = "./opscart.db"
	}
	retentionDays := 90
	if v := os.Getenv("OPSCART_RETENTION_DAYS"); v != "" {
		if parsed, err := strconv.Atoi(v); err != nil {
			log.Printf("store: invalid OPSCART_RETENTION_DAYS=%q, using default of %d days", v, retentionDays)
		} else {
			retentionDays = parsed
		}
	}
	var db store.Store
	dbPersistent := false
	if sqlDB, err := store.OpenSQLite(dbPath); err != nil {
		log.Printf("store: persistence disabled (%v)", err)
		db = &store.NullStore{}
	} else {
		db = sqlDB
		dbPersistent = true
		log.Printf("store: operational memory at %s", dbPath)
	}
	defer db.Close()
	srv := newServer(cl, db, retentionDays, dbPersistent)

	log.Printf("Scanning cluster %q ...", displayName(cl[0]))
	if err := srv.getState(cl[0]).refresh(cl); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	go srv.startBackgroundRefresh(60 * time.Second)

	addr := ":" + port
	httpServer := &http.Server{Addr: addr, Handler: srv.newMux()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("Dashboard ready at http://localhost%s", addr)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
		stop()
		log.Printf("shutting down: flushing operational memory")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("http server shutdown: %v", err)
		}
	}

	return nil
}

func pricingSourceDefault() string {
	if value := strings.TrimSpace(os.Getenv("OPSCART_PRICING_SOURCE")); value != "" {
		return value
	}
	return "auto"
}

func cloudProviderDefault() string {
	if value := strings.TrimSpace(os.Getenv("OPSCART_CLOUD_PROVIDER")); value != "" {
		return value
	}
	return "auto"
}
