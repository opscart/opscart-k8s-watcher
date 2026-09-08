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
        .exec-summary {
            background: #1a202c;
            color: #e2e8f0;
            border-radius: 8px;
            padding: 24px 30px;
            margin-bottom: 30px;
        }
        .exec-summary-title {
            font-size: 18px;
            font-weight: 700;
            color: #fff;
            margin-bottom: 20px;
            letter-spacing: 0.5px;
        }
        .exec-stats {
            display: flex;
            gap: 30px;
            margin-bottom: 20px;
            flex-wrap: wrap;
        }
        .exec-stat { text-align: center; min-width: 80px; }
        .exec-stat-value {
            font-size: 32px;
            font-weight: 800;
            line-height: 1.1;
        }
        .exec-stat-value.critical { color: #fc8181; }
        .exec-stat-value.warning  { color: #f6ad55; }
        .exec-stat-value.total    { color: #fff; }
        .exec-stat-value.storage  { color: #76e4f7; }
        .exec-stat-label {
            font-size: 12px;
            color: #a0aec0;
            margin-top: 4px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }
        .exec-divider { border: none; border-top: 1px solid #2d3748; margin: 16px 0; }
        .exec-findings { display: flex; gap: 40px; flex-wrap: wrap; }
        .exec-col { flex: 1; min-width: 220px; }
        .exec-col-title {
            font-size: 13px;
            font-weight: 700;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            margin-bottom: 10px;
        }
        .exec-col-title.critical { color: #fc8181; }
        .exec-col-title.warning  { color: #f6ad55; }
        .exec-col-title.info     { color: #76e4f7; }
        .exec-finding { font-size: 14px; color: #cbd5e0; margin-bottom: 6px; padding-left: 4px; }
        .exec-finding.none { color: #4a5568; font-style: italic; }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🗑️ Waste & Drift Analysis</h1>
            <div class="header-meta">
                <div>Cluster: <strong>{{.ClusterContext}}</strong></div>
                <div>Scanned: {{.Presentation.ScanTimeLabel}}</div>
                <div>Minimum resource age for age-gated checks: {{.MinAgeDays}} days</div>
            </div>
        </div>

        <div class="content">

            <!-- Executive Summary -->
            <div class="exec-summary">
                <div class="exec-summary-title">📊 Executive Summary</div>
                <div class="exec-stats">
                    <div class="exec-stat">
                        <div class="exec-stat-value total">{{.TotalWasteItems}}</div>
                        <div class="exec-stat-label">Finding count</div>
                    </div>
                    <div class="exec-stat">
                        <div class="exec-stat-value critical">{{.Presentation.Counts.DistinctResources}}</div>
                        <div class="exec-stat-label">Distinct resources</div>
                    </div>
                    <div class="exec-stat">
                        <div class="exec-stat-value warning">{{.Presentation.Counts.Operational}}</div>
                        <div class="exec-stat-label">Operational findings</div>
                    </div>
                    {{if gt .Presentation.RequestedStorageBytes 0}}
                    <div class="exec-stat">
                        <div class="exec-stat-value storage">{{.RequestedStorage}}</div>
                        <div class="exec-stat-label">Candidate PVC requests</div>
                    </div>
                    {{end}}
                </div>
                <hr class="exec-divider">
                <div class="exec-findings">
                    <div class="exec-col"><div class="exec-col-title critical">Operational findings</div><div class="exec-finding">{{.Presentation.Counts.Operational}} audit findings; this is not an incident-store count. Historical and inferred evidence may be included.</div></div>
                    <div class="exec-col"><div class="exec-col-title warning">Other review findings</div><div class="exec-finding">{{.Presentation.Counts.Review}} findings. Priority is the unchanged heuristic score, not confidence or savings.</div></div>
                    <div class="exec-col"><div class="exec-col-title info">Housekeeping / retention</div><div class="exec-finding">{{.Presentation.Counts.Retention}} findings, included in the finding count.</div></div>
                </div>
            </div>
            <p>Coverage: {{.Presentation.Coverage}}. Candidate PVC requests: {{.RequestedStorage}} ({{.Presentation.UnknownStorageRequests}} quantities unknown); not measured idle or billable storage.</p>
            {{range .Presentation.Warnings}}<p>Check warning: {{.Category}}: {{.Error}}</p>{{end}}

            <!-- Scorecard -->
            <div class="scorecard">
                <div class="score-card {{if gt .AbandonedNamespaceCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Namespace Activity Review</div>
                    <div class="score-value">{{.AbandonedNamespaceCount}}</div>
                </div>
                <div class="score-card {{if gt .ZombiePodCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Pod Failure Evidence</div>
                    <div class="score-value">{{.ZombiePodCount}}</div>
                </div>
                <div class="score-card {{if gt .UnmanagedPodCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">Pod Ownership Review</div>
                    <div class="score-value">{{.UnmanagedPodCount}}</div>
                </div>
                <div class="score-card {{if gt .OrphanedPVCCount 0}}critical{{else}}success{{end}}">
                    <div class="score-label">PVC State / Reference Review</div>
                    <div class="score-value">{{.OrphanedPVCCount}}</div>
                </div>
                <div class="score-card {{if gt .StaleJobCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Job / CronJob Retention Review</div>
                    <div class="score-value">{{.StaleJobCount}}</div>
                </div>
                <div class="score-card {{if gt .ZeroReplicaCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Zero-Replica Workloads</div>
                    <div class="score-value">{{.ZeroReplicaCount}}</div>
                </div>
                <div class="score-card {{if gt .OrphanedServiceCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Service Selector Review</div>
                    <div class="score-value">{{.OrphanedServiceCount}}</div>
                </div>
                <div class="score-card {{if gt .BrokenIngressCount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Ingress Backend Evidence</div>
                    <div class="score-value">{{.BrokenIngressCount}}</div>
                </div>
                <div class="score-card {{if gt .MisconfiguredHPACount 0}}warning{{else}}success{{end}}">
                    <div class="score-label">HPA Configuration Review</div>
                    <div class="score-value">{{.MisconfiguredHPACount}}</div>
                </div>
                <div class="score-card {{if gt .TotalWasteItems 0}}warning{{else}}success{{end}}">
                    <div class="score-label">Finding count</div>
                    <div class="score-value">{{.TotalWasteItems}}</div>
                </div>
            </div>

            {{if eq .TotalWasteItems 0}}
            <div class="empty-state">
                <div class="empty-state-icon">✅</div>
                <h2>No findings reported</h2>
                <p>No findings were reported by the available checks. This does not establish a clean cluster.</p>
            </div>
            {{else}}

            <!-- Namespace Activity Review -->
            {{if gt .AbandonedNamespaceCount 0}}
            <div class="section">
                <div class="section-title">📁 Namespace Activity Review ({{.AbandonedNamespaceCount}})</div>
                {{range $i, $item := .AbandonedNamespaces}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Pods:</span> {{.PodCount}}</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "AbandonedNamespaces" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Pod Failure Evidence -->
            {{if gt .ZombiePodCount 0}}
            <div class="section">
                <div class="section-title">💀 Pod Failure Evidence ({{.ZombiePodCount}})</div>
                {{range $i, $item := .ZombiePods}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}} <span class="badge badge-critical">{{.Status}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Restarts:</span> {{.RestartCount}}</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "ZombiePods" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Pod Ownership Review -->
            {{if gt .UnmanagedPodCount 0}}
            <div class="section">
                <div class="section-title">🔓 Pod Ownership Review ({{.UnmanagedPodCount}})</div>
                {{range $i, $item := .UnmanagedPods}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">Owner kind review</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Restarts:</span> {{.RestartCount}}</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "UnmanagedPods" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- PVC State / Reference Review -->
            {{if gt .OrphanedPVCCount 0}}
            <div class="section">
                <div class="section-title">💾 PVC State / Reference Review ({{.OrphanedPVCCount}}) &nbsp;—&nbsp; Candidate requests: <strong>{{.RequestedStorage}}</strong></div>
                {{range $i, $item := .OrphanedPVCs}}
                <div class="item-box critical">
                    <div class="item-title">{{.Name}} <span class="badge badge-critical">State review</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Requested:</span> {{(index $.Evidence "OrphanedPVCs" $i).Storage}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "OrphanedPVCs" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Stale Jobs -->
            {{if gt .StaleJobCount 0}}
            <div class="section">
                <div class="section-title">⏰ Job / CronJob Retention Review ({{.StaleJobCount}})</div>
                {{range $i, $item := .StaleJobs}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} {{if .IsCronJob}}<span class="badge badge-warning">CronJob</span>{{else}}<span class="badge badge-warning">Job</span>{{end}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Review:</span> Status / retention</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "StaleJobs" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Other Waste Items (if any) -->
            {{if gt .ZeroReplicaCount 0}}
            <div class="section">
                <div class="section-title">📦 Zero-Replica Workloads ({{.ZeroReplicaCount}})</div>
                {{range $i, $item := .ZeroReplicaWorkloads}}
                <div class="item-box low">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Kind}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "ZeroReplicaWorkloads" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Service Selector Review -->
            {{if gt .OrphanedServiceCount 0}}
            <div class="section">
                <div class="section-title">🔌 Service Selector Review ({{.OrphanedServiceCount}})</div>
                {{range $i, $item := .OrphanedServices}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Type}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "OrphanedServices" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Ingress Backend Evidence -->
            {{if gt .BrokenIngressCount 0}}
            <div class="section">
                <div class="section-title">🌐 Ingress Backend Evidence ({{.BrokenIngressCount}})</div>
                {{range $i, $item := .BrokenIngresses}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "BrokenIngresses" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- HPA Configuration Review -->
            {{if gt .MisconfiguredHPACount 0}}
            <div class="section">
                <div class="section-title">📈 HPA Configuration Review ({{.MisconfiguredHPACount}})</div>
                {{range $i, $item := .MisconfiguredHPAs}}
                <div class="item-box warning">
                    <div class="item-title">{{.Name}} <span class="badge badge-warning">{{.Condition}}</span></div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Target:</span> {{.TargetName}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                        <div class="item-meta-item"><span class="item-meta-label">Replicas:</span> {{.MinReplicas}}-{{.MaxReplicas}}</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "MisconfiguredHPAs" $i)}}
                </div>
                {{end}}
            </div>
            {{end}}

            <!-- Housekeeping Items (included in findings) -->
            {{if gt .OldReplicaSetCount 0}}
            <div class="section">
                <div class="section-title">📋 ReplicaSet Retention Review ({{.OldReplicaSetCount}})</div>
                <p style="color: #718096; font-size: 14px; margin-bottom: 15px;">
                    Retention findings are included in the finding count. Age and desired replicas do not establish obsolescence or rollback requirements.
                </p>
                {{if gt .OldReplicaSetCount 20}}
                <div class="item-box low">
                    <div class="item-title">{{.OldReplicaSetCount}} old ReplicaSets found</div>
                    <div class="item-finding">
                        Observed: {{.OldReplicaSetCount}} ReplicaSets met the existing age/desired-replica check.
                        Inference: retention may warrant review. Limitations: actual replicas, revision history, rollback needs, and retention policy were not checked. Evidence confidence: Not assessed. Review retention requirements with the owner.
                    </div>
                    <div class="item-suggest">kubectl get rs -A -o yaml</div>
                </div>
                {{else}}
                {{range $i, $item := .OldReplicaSets}}
                <div class="item-box low">
                    <div class="item-title">{{.Name}}</div>
                    <div class="item-meta">
                        <div class="item-meta-item"><span class="item-meta-label">Namespace:</span> {{.Namespace}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Owner:</span> {{.OwnerDeployment}}</div>
                        <div class="item-meta-item"><span class="item-meta-label">Age:</span> {{.AgeDays}} days</div>
                    </div>
                    {{template "waste-evidence" (index $.Evidence "OldReplicaSets" $i)}}
                </div>
                {{end}}
                {{end}}
            </div>
            {{end}}

            {{end}}

            <!-- Footer -->
            <div class="footer">
                <p><strong>Note:</strong> This report separates observations from inferences. Review intent and retention requirements with the owner.</p>
                <p><em>All findings include retention and operational findings. Distinct resources are counted by kind, namespace, and name.</em></p>
                <p>Audit scanned: {{.Presentation.ScanTimeLabel}} · Generated by opscart-k8s-watcher</p>
            </div>
        </div>
    </div>
</body>
</html>
{{define "waste-evidence"}}<div class="item-finding">Observed: {{.Observed}}</div>
<div class="item-finding">Inference: {{.Inference}}</div>
<div class="item-finding">Limitations: {{.Limitations}}</div>
<div class="item-finding">Evidence confidence: {{.Confidence}} — {{.ConfidenceReason}} Priority score: {{.Priority}} (legacy heuristic).</div>
<div class="item-suggest">Review: {{.Review}}<br>{{.Command}}</div>{{end}}
`

type WasteHTMLData struct {
	// Additive presentation contract. Legacy summary fields below remain available
	// for source compatibility; templates use Presentation.Counts instead.
	Presentation     analyzer.WastePresentation
	RequestedStorage string
	Evidence         map[string][]analyzer.WasteFinding

	ClusterContext string
	ScannedAt      time.Time
	MinAgeDays     int

	// Summary counts for executive section
	CriticalCount     int
	WarningCount      int
	MaxZombieRestarts int32

	// Counts
	AbandonedNamespaceCount int
	ZombiePodCount          int
	UnmanagedPodCount       int
	OrphanedPVCCount        int
	OrphanedPVCStorageGB    int
	StaleJobCount           int
	ZeroReplicaCount        int
	OrphanedServiceCount    int
	BrokenIngressCount      int
	MisconfiguredHPACount   int
	OldReplicaSetCount      int
	TotalWasteItems         int // Canonical finding count; includes ReplicaSet retention findings.

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

func buildWasteHTMLData(audit *analyzer.WasteAudit, clusterContext string, minAgeDays int) WasteHTMLData {
	p := analyzer.BuildWastePresentation(audit)
	if audit == nil {
		audit = &analyzer.WasteAudit{}
	}
	// Separate zombie from unmanaged pods
	zombiePods := []analyzer.StalePod{}
	unmanagedPods := []analyzer.StalePod{}
	maxZombieRestarts := int32(0)
	for _, p := range audit.StalePods {
		if p.Kind == analyzer.StalePodZombie {
			zombiePods = append(zombiePods, p)
			if p.RestartCount > maxZombieRestarts {
				maxZombieRestarts = p.RestartCount
			}
		} else {
			unmanagedPods = append(unmanagedPods, p)
		}
	}

	// Derive executive summary counts
	criticalCount := len(audit.AbandonedNamespaces) + len(zombiePods) + len(audit.OrphanedPVCs)
	warningCount := len(audit.StaleJobs) + len(audit.ZeroReplicaWorkloads) +
		len(audit.OrphanedServices) + len(audit.BrokenIngresses) + len(audit.MisconfiguredHPAs)

	data := WasteHTMLData{
		ClusterContext:          clusterContext,
		ScannedAt:               audit.ScannedAt,
		MinAgeDays:              minAgeDays,
		CriticalCount:           criticalCount,
		WarningCount:            warningCount,
		MaxZombieRestarts:       maxZombieRestarts,
		AbandonedNamespaceCount: len(audit.AbandonedNamespaces),
		ZombiePodCount:          len(zombiePods),
		UnmanagedPodCount:       len(unmanagedPods),
		OrphanedPVCCount:        len(audit.OrphanedPVCs),
		OrphanedPVCStorageGB:    audit.OrphanedPVCStorageGB,
		StaleJobCount:           len(audit.StaleJobs),
		ZeroReplicaCount:        len(audit.ZeroReplicaWorkloads),
		OrphanedServiceCount:    len(audit.OrphanedServices),
		BrokenIngressCount:      len(audit.BrokenIngresses),
		MisconfiguredHPACount:   len(audit.MisconfiguredHPAs),
		OldReplicaSetCount:      len(audit.OldReplicaSets),
		TotalWasteItems:         p.Counts.Findings,
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

	data.Presentation = p
	data.RequestedStorage = analyzer.FormatWasteBytes(p.RequestedStorageBytes)
	data.Evidence = map[string][]analyzer.WasteFinding{
		"AbandonedNamespaces":  p.FindingsInCategory("Namespace activity review"),
		"ZombiePods":           p.FindingsInCategory("Pod failure evidence"),
		"UnmanagedPods":        p.FindingsInCategory("Pod ownership review"),
		"OrphanedPVCs":         p.FindingsInCategory("PVC state / reference review"),
		"StaleJobs":            p.FindingsInCategory("Job / CronJob retention review"),
		"ZeroReplicaWorkloads": p.FindingsInCategory("Zero-replica workload"),
		"OrphanedServices":     p.FindingsInCategory("Service selector review"),
		"BrokenIngresses":      p.FindingsInCategory("Ingress backend evidence"),
		"MisconfiguredHPAs":    p.FindingsInCategory("HPA configuration review"),
		"OldReplicaSets":       p.FindingsInCategory("ReplicaSet retention review"),
	}
	return data
}

func GenerateWasteHTML(audit *analyzer.WasteAudit, clusterContext string, minAgeDays int) error {
	data := buildWasteHTMLData(audit, clusterContext, minAgeDays)
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
