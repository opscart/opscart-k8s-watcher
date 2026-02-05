package report

// securityHTMLTemplate is a focused security audit report
const securityHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Security Audit Report - {{.ClusterName}}</title>
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
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
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
        }
        .score-card {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            padding: 30px;
            border-radius: 8px;
            text-align: center;
            color: white;
        }
        .score-number {
            font-size: 72px;
            font-weight: bold;
            margin: 20px 0;
        }
        .progress-bar {
            width: 100%;
            height: 30px;
            background: rgba(255,255,255,0.3);
            border-radius: 15px;
            overflow: hidden;
            margin-top: 20px;
        }
        .progress-fill {
            height: 100%;
            background: white;
        }
        .metrics-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            margin-top: 20px;
        }
        .metric-card {
            background: #f7fafc;
            padding: 20px;
            border-radius: 8px;
            border-left: 4px solid #667eea;
        }
        .metric-value {
            font-size: 32px;
            font-weight: bold;
            color: #2d3748;
            margin: 10px 0;
        }
        .metric-label { color: #718096; font-size: 14px; }
        .finding-box {
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 20px;
            border-left: 4px solid;
        }
        .finding-critical {
            background: #fff5f5;
            border-color: #fc8181;
        }
        .finding-warning {
            background: #fffaf0;
            border-color: #f6ad55;
        }
        .finding-pass {
            background: #f0fff4;
            border-color: #68d391;
        }
        .finding-title {
            font-weight: 600;
            font-size: 16px;
            margin-bottom: 10px;
        }
        .finding-body { font-size: 14px; }
        .resource-list {
            background: #f7fafc;
            padding: 15px;
            border-radius: 4px;
            margin-top: 10px;
            font-family: monospace;
            font-size: 13px;
        }
        .badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 12px;
            font-size: 12px;
            font-weight: 600;
            margin: 2px;
        }
        .badge-critical { background: #fed7d7; color: #742a2a; }
        .badge-warning { background: #feebc8; color: #7c2d12; }
        .badge-pass { background: #c6f6d5; color: #22543d; }
        .button {
            display: inline-block;
            padding: 10px 20px;
            background: #667eea;
            color: white;
            text-decoration: none;
            border-radius: 6px;
            font-size: 14px;
            font-weight: 500;
            border: none;
            cursor: pointer;
        }
        .button:hover { background: #5a67d8; }
        .button-secondary { background: #e2e8f0; color: #2d3748; }
        .button-secondary:hover { background: #cbd5e0; }
        .actions {
            margin-top: 30px;
            padding-top: 20px;
            border-top: 1px solid #e2e8f0;
            display: flex;
            gap: 10px;
        }
        @media print {
            body { background: white; padding: 0; }
            .button { display: none; }
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🛡️ Security Audit Report</h1>
            <div class="header-meta">
                <strong>Cluster:</strong> {{.ClusterName}} &nbsp;|&nbsp;
                <strong>Generated:</strong> {{.GeneratedAt.Format "January 2, 2006 3:04 PM"}}
            </div>
        </div>
        
        <div class="content">
            <!-- CIS Compliance Score -->
            <div class="section">
                <div class="score-card">
                    <div style="font-size: 18px; opacity: 0.9;">CIS Compliance Score</div>
                    <div class="score-number">{{.CISScore}}<span style="font-size: 36px;">/100</span></div>
                    <div style="font-size: 18px;">
                        {{if ge .CISScore 80}}✅ Excellent
                        {{else if ge .CISScore 60}}🟡 Needs Attention
                        {{else}}🔴 Action Required{{end}}
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill" style="width: {{.CISScore}}%;"></div>
                    </div>
                </div>
            </div>
            
            <!-- Summary Metrics -->
            <div class="section">
                <div class="section-title">📊 Summary</div>
                <div class="metrics-grid">
                    {{if .PodCount}}
                    <div class="metric-card">
                        <div class="metric-label">Pods Scanned</div>
                        <div class="metric-value">{{.PodCount}}</div>
                    </div>
                    {{end}}
                    {{if .NamespaceCount}}
                    <div class="metric-card">
                        <div class="metric-label">Issues Found</div>
                        <div class="metric-value" style="color: #fc8181;">{{.NamespaceCount}}</div>
                    </div>
                    {{end}}
                    <div class="metric-card">
                        <div class="metric-label">Controls Passed</div>
                        <div class="metric-value" style="color: #48bb78;">{{.ControlsPassed}}</div>
                    </div>
                    <div class="metric-card">
                        <div class="metric-label">Controls Failed</div>
                        <div class="metric-value" style="color: #fc8181;">{{.ControlsFailed}}</div>
                    </div>
                </div>
            </div>
            
            <!-- Critical Findings -->
            {{if .CriticalIssues}}
            <div class="section">
                <div class="section-title">🔴 Critical Findings</div>
                {{range .CriticalIssues}}
                <div class="finding-box finding-critical">
                    <div class="finding-title">{{.Title}}</div>
                    <div class="finding-body">{{.Description}}</div>
                    {{if .Details}}
                    <div class="resource-list">
                        <strong>Top affected resources:</strong><br>
                        {{range .Details}}• {{.}}<br>{{end}}
                    </div>
                    {{end}}
                </div>
                {{end}}
            </div>
            {{end}}
            
            <!-- Warnings -->
            {{if .WarningIssues}}
            <div class="section">
                <div class="section-title">🟡 Warnings</div>
                {{range .WarningIssues}}
                <div class="finding-box finding-warning">
                    <div class="finding-title">{{.Title}}</div>
                    <div class="finding-body">{{.Description}}</div>
                    {{if .Details}}
                    <div class="resource-list">
                        <strong>Top affected resources:</strong><br>
                        {{range .Details}}• {{.}}<br>{{end}}
                    </div>
                    {{end}}
                </div>
                {{end}}
            </div>
            {{end}}
            
            <!-- Security Findings Details -->
            {{if .SecurityFindings}}
            <div class="section">
                <div class="section-title">🔍 Detailed Findings</div>
                {{range .SecurityFindings}}
                <div class="finding-box {{if eq .Status "passed"}}finding-pass{{else if eq .Severity "critical"}}finding-critical{{else}}finding-warning{{end}}">
                    <div class="finding-title">
                        {{if eq .Status "passed"}}✅{{else}}❌{{end}} {{.Control}}
                        <span class="badge {{if eq .Status "passed"}}badge-pass{{else if eq .Severity "critical"}}badge-critical{{else}}badge-warning{{end}}">
                            {{.Status}}
                        </span>
                    </div>
                    {{if ne .Status "passed"}}
                    <div class="finding-body">
                        <strong>Found:</strong> {{.Count}} issue(s)<br>
                        {{if .Remediation}}<strong>Remediation:</strong> {{.Remediation}}{{end}}
                    </div>
                    {{if .Resources}}
                    <div class="resource-list">
                        Affected Resources:<br>
                        {{range .Resources}}• {{.}}<br>{{end}}
                    </div>
                    {{end}}
                    {{end}}
                </div>
                {{end}}
            </div>
            {{end}}
            
            <!-- Recommended Actions -->
            <div class="section">
                <div class="section-title">📋 Recommended Actions (Priority Order)</div>
                <div style="background: #f7fafc; padding: 20px; border-radius: 8px; border-left: 4px solid #667eea;">
                    <ol style="margin-left: 20px; line-height: 2;">
                        <li>Remove hostPath volumes (critical filesystem access)</li>
                        <li>Fix privileged containers (highest risk)</li>
                        <li>Review and minimize hostNetwork usage</li>
                        <li>Configure pods to run as non-root user</li>
                        <li>Create dedicated ServiceAccounts with minimal permissions</li>
                        <li>Add resource limits to all pods</li>
                        <li>Set allowPrivilegeEscalation: false</li>
                    </ol>
                </div>
            </div>
            
            <!-- Validation Steps -->
            <div class="section">
                <div class="section-title">✅ Validation Steps</div>
                <div style="background: #f7fafc; padding: 20px; border-radius: 8px; border-left: 4px solid #48bb78;">
                    <ol style="margin-left: 20px; line-height: 2;">
                        <li>Test fixes in staging environment first</li>
                        <li>Verify application functionality after changes</li>
                        <li>Run kube-bench for complete CIS assessment</li>
                        <li>Re-scan cluster after remediation</li>
                    </ol>
                </div>
            </div>
            
            <!-- Issue Count Breakdown -->
            {{if .NamespaceCount}}
            <div class="section">
                <div class="section-title">📊 Issue Count Breakdown</div>
                <table style="width: 100%; border-collapse: collapse;">
                    <thead>
                        <tr style="background: #f7fafc; border-bottom: 2px solid #e2e8f0;">
                            <th style="padding: 12px; text-align: left; font-weight: 600;">Issue Type</th>
                            <th style="padding: 12px; text-align: right; font-weight: 600;">Count</th>
                        </tr>
                    </thead>
                    <tbody>
                        {{range .CriticalIssues}}
                        <tr style="border-bottom: 1px solid #e2e8f0;">
                            <td style="padding: 12px;">{{.Title}}</td>
                            <td style="padding: 12px; text-align: right; color: #fc8181; font-weight: 600;">{{.Count}}</td>
                        </tr>
                        {{end}}
                        {{range .WarningIssues}}
                        <tr style="border-bottom: 1px solid #e2e8f0;">
                            <td style="padding: 12px;">{{.Title}}</td>
                            <td style="padding: 12px; text-align: right; color: #f6ad55; font-weight: 600;">{{.Count}}</td>
                        </tr>
                        {{end}}
                        <tr style="background: #f7fafc; font-weight: 700; border-top: 2px solid #2d3748;">
                            <td style="padding: 12px;">TOTAL ISSUES</td>
                            <td style="padding: 12px; text-align: right;">{{.NamespaceCount}}</td>
                        </tr>
                    </tbody>
                </table>
            </div>
            {{end}}
            
            <!-- Actions -->
            <div class="actions">
                <button class="button" onclick="window.print()">📥 Download PDF</button>
                <a href="https://opscart.com" target="_blank" class="button button-secondary">🌐 Visit OpsCart.com</a>
            </div>
        </div>
    </div>
    
    <div style="text-align: center; margin-top: 30px; padding: 20px; color: #718096; font-size: 14px;">
        Generated by <strong>OpsCart Kubernetes Watcher v0.3</strong><br>
        <a href="https://opscart.com" style="color: #667eea;">opscart.com</a>
    </div>
</body>
</html>`
