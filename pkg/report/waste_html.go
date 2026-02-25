package report

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
)

const wasteHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Waste & Drift Analysis - {{.ClusterContext}}</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
            line-height: 1.6;
            color: #333;
            background: #f5f7fa;
            padding: 20px;
        }
        .container {
            max-width: 1200px;
            margin: 0 auto;
            background: white;
            border-radius: 8px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
        }
        .header {
            background: linear-gradient(135deg, #326ce5 0%, #1a4d8f 100%);
            color: white;
            padding: 30px;
            border-radius: 8px 8px 0 0;
        }
        .header h1 { font-size: 28px; margin-bottom: 10px; }
        .header-meta { opacity: 0.9; font-size: 14px; }
        .content { padding: 30px; }
        .section { margin-bottom: 40px; }
        .section-title {
            font-size: 20px;
            font-weight: 600;
            margin-bottom: 20px;
            color: #2d3748;
            border-bottom: 2px solid #326ce5;
            padding-bottom: 10px;
        }
        .scorecard {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            margin-bottom: 40px;
        }
        .score-card {
            background: #f7fafc;
            padding: 20px;
            border-radius: 8px;
            border-left: 4px solid;
        }
        .score-card.critical { border-color: #fc8181; }
        .score-card.warning { border-color: #f6ad55; }
        .score-card.success { border-color: #326ce5; }
        .score-value {
            font-size: 36px;
            font-weight: bold;
            margin: 10px 0;
        }
        .score-label { color: #718096; font-size: 14px; }
        .item-box {
            padding: 20px;
            margin-bottom: 15px;
            border-radius: 8px;
            background: #f7fafc;
            border-left: 4px solid #326ce5;
        }
        .item-box.critical { background: #fff5f5; border-color: #fc8181; }
        .item-box.warning { background: #fffaf0; border-color: #f6ad55; }
        .item-box.low { background: #f0fff4; border-color: #9ae6b4; }
        .item-title {
            font-size: 16px;
            font-weight: 600;
            margin-bottom: 10px;
            color: #2d3748;
        }
        .item-meta {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
            gap: 10px;
            margin: 10px 0;
            font-size: 14px;
        }
        .item-meta-item {
            color: #4a5568;
        }
        .item-meta-label {
            font-weight: 600;
            color: #2d3748;
        }
        .item-finding {
            margin: 15px 0;
            padding: 15px;
            background: white;
            border-radius: 4px;
            font-size: 14px;
            color: #4a5568;
            line-height: 1.8;
        }
        .item-suggest {
            margin-top: 10px;
            padding: 10px;
            background: #edf2f7;
            border-radius: 4px;
            font-family: 'Monaco', 'Courier New', monospace;
            font-size: 12px;
            color: #2d3748;
        }
        .badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 12px;
            font-size: 12px;
            font-weight: 600;
        }
        .badge-critical { background: #fed7d7; color: #c53030; }
        .badge-warning { background: #feebc8; color: #c05621; }
        .badge-success { background: #dbeafe; color: #1e40af; }
        .empty-state {
            text-align: center;
            padding: 60px 20px;
            color: #718096;
        }
        .empty-state-icon {
            font-size: 64px;
            margin-bottom: 20px;
        }
        .footer {
            margin-top: 40px;
            padding: 20px;
            background: #f7fafc;
            border-radius: 8px;
            text-align: center;
            color: #718096;
            font-size: 14px;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🗑️ Waste & Drift Analysis</h1>
            <div class="header-meta">
                <div>Cluster: <strong>{{.ClusterContext}}</strong></div>
                <div>Scanned: {{.ScannedAt.Format "2006-01-02 15:04:05 MST"}}</div>
                <div>Minimum Age: {{.MinAgeDays}} days</div>
            </div>
        </div>

        <div class="content">
            <!-- Scorecard -->
            <div class="scorecard">
                <div class="score-card {{if gt .AbandonedNamespaceCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Abandoned Namespaces</div>
                    <div class="score-value">{{.AbandonedNamespaceCount}}</div>
                </div>
                <div class="score-card {{if gt .ZombiePodCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Zombie Pods</div>
                    <div class="score-value">{{.ZombiePodCount}}</div>
                </div>
                <div class="score-card {{if gt .UnmanagedPodCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Unmanaged Pods</div>
                    <div class="score-value">{{.UnmanagedPodCount}}</div>
                </div>
                <div class="score-card {{if gt .OrphanedPVCCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Orphaned PVCs</div>
                    <div class="score-value">{{.OrphanedPVCCount}}</div>
                </div>
                <div class="score-card {{if gt .StaleJobCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Stale Jobs/CronJobs</div>
                    <div class="score-value">{{.StaleJobCount}}</div>
                </div>
                <div class="score-card {{if gt .ZeroReplicaCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Zero-Replica Workloads</div>
                    <div class="score-value">{{.ZeroReplicaCount}}</div>
                </div>
                <div class="score-card {{if gt .OrphanedServiceCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Orphaned Services</div>
                    <div class="score-value">{{.OrphanedServiceCount}}</div>
                </div>
                <div class="score-card {{if gt .BrokenIngressCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Broken Ingresses</div>
                    <div class="score-value">{{.BrokenIngressCount}}</div>
                </div>
                <div class="score-card {{if gt .MisconfiguredHPACount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Misconfigured HPAs</div>
                    <div class="score-value">{{.MisconfiguredHPACount}}</div>
                </div>
                <div class="score-card {{if gt .TotalWasteItems 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Total Items</div>
                    <div class="score-value">{{.TotalWasteItems}}</div>
                </div>
            </div>

            {{if eq .TotalWasteItems 0}}
            <div class="empty-state">
                <div class="empty-state-icon">✅</div>
                <h2>No Waste Detected</h2>
                <p>Your cluster looks clean! No abandoned, idle, or orphaned resources found.</p>
            </div>
            {{else}}

            <!-- Abandoned Namespaces -->
            {{if gt .AbandonedNamespaceCount 0}}
            <div class="section">
                <div class="section-title">📁 Abandoned Namespaces ({{.AbandonedNamespaceCount}})</div>
                {{range .AbandonedNamespaces}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Pods:</span> {{.PodCount}}</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl get all -n {{.Name}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Zombie Pods -->
            {{if gt .ZombiePodCount 0}}
            <div class="section">
                <div class="section-title">💀 Zombie Pods ({{.ZombiePodCount}})</div>
                {{range .ZombiePods}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}} <span class="badge badge-critical">{{.Status}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Restarts:</span> {{.RestartCount}}</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl logs {{.Name}} -n {{.Namespace}} --previous</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Unmanaged Pods -->
            {{if gt .UnmanagedPodCount 0}}
            <div class="section">
                <div class="section-title">🔓 Unmanaged Pods ({{.UnmanagedPodCount}})</div>
                {{range .UnmanagedPods}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">No Controller</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Restarts:</span> {{.RestartCount}}</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl describe pod {{.Name}} -n {{.Namespace}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Orphaned PVCs -->
            {{if gt .OrphanedPVCCount 0}}
            <div class="section">
                <div class="section-title">💾 Orphaned PVCs ({{.OrphanedPVCCount}})</div>
                {{range .OrphanedPVCs}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}} <span class="badge badge-critical">{{.Status}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Size:</span> {{.SizeGB}}GB</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl get pvc {{.Name}} -n {{.Namespace}} -o yaml</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Stale Jobs -->
            {{if gt .StaleJobCount 0}}
            <div class="section">
                <div class="section-title">⏰ Stale Jobs & CronJobs ({{.StaleJobCount}})</div>
                {{range .StaleJobs}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} {{if .IsCronJob}}<span class="badge badge-warning">CronJob</span>{{else}}<span class="badge badge-warning">Job</span>{{end}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Status:</span> {{.JobStatus}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl describe {{if .IsCronJob}}cronjob{{else}}job{{end}} {{.Name}} -n {{.Namespace}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Other Waste Items (if any) -->
            {{if gt .ZeroReplicaCount 0}}
            <div class="section">
                <div class="section-title">📦 Zero-Replica Workloads ({{.ZeroReplicaCount}})</div>
                {{range .ZeroReplicaWorkloads}}
                <div class="item-box low">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Kind}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Orphaned Services -->
            {{if gt .OrphanedServiceCount 0}}
            <div class="section">
                <div class="section-title">🔌 Orphaned Services ({{.OrphanedServiceCount}})</div>
                {{range .OrphanedServices}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Type}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl describe service {{.Name}} -n {{.Namespace}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Broken Ingresses -->
            {{if gt .BrokenIngressCount 0}}
            <div class="section">
                <div class="section-title">🌐 Broken Ingresses ({{.BrokenIngressCount}})</div>
                {{range .BrokenIngresses}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl describe ingress {{.Name}} -n {{.Namespace}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Misconfigured HPAs -->
            {{if gt .MisconfiguredHPACount 0}}
            <div class="section">
                <div class="section-title">📈 Misconfigured HPAs ({{.MisconfiguredHPACount}})</div>
                {{range .MisconfiguredHPAs}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Condition}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Target:</span> {{.TargetName}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Replicas:</span> {{.MinReplicas}}-{{.MaxReplicas}}</div>
                    </div>
                    <div class="item-finding">{{.Reason}}</div>
                    <div class="item-suggest">kubectl describe hpa {{.Name}} -n {{.Namespace}}</div>
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Housekeeping Items (not counted in total) -->
            {{if gt .OldReplicaSetCount 0}}
            <div class="section">
                <div class="section-title">📋 Old ReplicaSets - Rollout Leftovers ({{.OldReplicaSetCount}})</div>
                <p style="color: #718096; font-size: 14px; margin-bottom: 15px;">
                    ℹ️ These are leftover ReplicaSets from deployment rollouts. Safe to clean up but not counted in total waste items.
                </p>
                {{if gt .OldReplicaSetCount 20}}
                <div class="item-box low">
                    <div class="item-title">{{.OldReplicaSetCount}} old ReplicaSets found</div>
                    <div class="item-finding">
                        Kubernetes retains old ReplicaSets for rollback history. These accumulate over time from deployment updates.
                        Most are safe to delete, but check with team first.
                    </div>
                    <div class="item-suggest">kubectl get rs -A | awk '$3==0 && $4==0'</div>
                </div>
                {{else}}
                {{range .OldReplicaSets}}
                <div class="item-box low">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Owner:</span> {{.OwnerDeployment}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                </div>
                {{end}}
                {{end}}
            </div>
            {{end}}

            {{end}}

            <!-- Footer -->
            <div class="footer">
                <p><strong>Note:</strong> This report shows SUGGESTIONS based on observed data. Always verify with the owning team before removing resources.</p>
                <p><em>Old ReplicaSets are shown separately and not counted in the total waste items (they're low-severity housekeeping from deployment rollouts).</em></p>
                <p>Generated by opscart-k8s-watcher on {{.ScannedAt.Format "2006-01-02 15:04:05 MST"}}</p>
            </div>
        </div>
    </div>
</body>
</html>`

type WasteHTMLData struct {
	ClusterContext string
	ScannedAt      time.Time
	MinAgeDays     int

	// Counts
	AbandonedNamespaceCount int
	ZombiePodCount          int
	UnmanagedPodCount       int
	OrphanedPVCCount        int
	StaleJobCount           int
	ZeroReplicaCount        int
	OrphanedServiceCount    int
	BrokenIngressCount      int
	MisconfiguredHPACount   int
	OldReplicaSetCount      int
	TotalWasteItems         int

	// Data
	AbandonedNamespaces  []analyzer.AbandonedNamespace
	ZombiePods           []analyzer.StalePod
	UnmanagedPods        []analyzer.StalePod
	OrphanedPVCs         []analyzer.OrphanedPVC
	StaleJobs            []analyzer.StaleJob
	ZeroReplicaWorkloads []analyzer.ZeroReplicaWorkload
	OrphanedServices     []analyzer.OrphanedService
	BrokenIngresses      []analyzer.BrokenIngress
	MisconfiguredHPAs    []analyzer.MisconfiguredHPA
	OldReplicaSets       []analyzer.OldReplicaSet
}

func GenerateWasteHTML(audit *analyzer.WasteAudit, clusterContext string, minAgeDays int) error {
	// Separate zombie from unmanaged pods
	zombiePods := []analyzer.StalePod{}
	unmanagedPods := []analyzer.StalePod{}
	for _, p := range audit.StalePods {
		if p.Kind == analyzer.StalePodZombie {
			zombiePods = append(zombiePods, p)
		} else {
			unmanagedPods = append(unmanagedPods, p)
		}
	}

	data := WasteHTMLData{
		ClusterContext:          clusterContext,
		ScannedAt:               audit.ScannedAt,
		MinAgeDays:              minAgeDays,
		AbandonedNamespaceCount: len(audit.AbandonedNamespaces),
		ZombiePodCount:          len(zombiePods),
		UnmanagedPodCount:       len(unmanagedPods),
		OrphanedPVCCount:        len(audit.OrphanedPVCs),
		StaleJobCount:           len(audit.StaleJobs),
		ZeroReplicaCount:        len(audit.ZeroReplicaWorkloads),
		OrphanedServiceCount:    len(audit.OrphanedServices),
		BrokenIngressCount:      len(audit.BrokenIngresses),
		MisconfiguredHPACount:   len(audit.MisconfiguredHPAs),
		OldReplicaSetCount:      len(audit.OldReplicaSets),
		TotalWasteItems:         audit.TotalWasteItems,
		AbandonedNamespaces:     audit.AbandonedNamespaces,
		ZombiePods:              zombiePods,
		UnmanagedPods:           unmanagedPods,
		OrphanedPVCs:            audit.OrphanedPVCs,
		StaleJobs:               audit.StaleJobs,
		ZeroReplicaWorkloads:    audit.ZeroReplicaWorkloads,
		OrphanedServices:        audit.OrphanedServices,
		BrokenIngresses:         audit.BrokenIngresses,
		MisconfiguredHPAs:       audit.MisconfiguredHPAs,
		OldReplicaSets:          audit.OldReplicaSets,
	}

	tmpl, err := template.New("waste").Parse(wasteHTMLTemplate)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	// Create reports directory
	reportsDir := "reports"
	dateDir := time.Now().Format("2006-01-02")
	fullPath := filepath.Join(reportsDir, dateDir)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return fmt.Errorf("creating reports directory: %w", err)
	}

	// Generate filename
	timestamp := time.Now().Format("1504")
	filename := fmt.Sprintf("opscart-waste-%s.html", timestamp)
	filepath := filepath.Join(fullPath, filename)

	file, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("creating report file: %w", err)
	}
	defer file.Close()

	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	fmt.Printf("\n✅ HTML report saved: %s\n", filepath)
	return nil
}
