package report

// htmlTemplate is the embedded HTML template for reports
const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>OpsCart Cluster Health Report - {{.ClusterName}}</title>
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
            display: flex;
            align-items: center;
            gap: 10px;
        }
        .health-score {
            background: linear-gradient(135deg, #f6d365 0%, #fda085 100%);
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
        .score-label { font-size: 24px; opacity: 0.9; }
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
            transition: width 0.3s ease;
        }
        .metrics-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
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
        .alert-box {
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 15px;
            border-left: 4px solid;
        }
        .alert-critical {
            background: #fff5f5;
            border-color: #fc8181;
            color: #742a2a;
        }
        .alert-warning {
            background: #fffaf0;
            border-color: #f6ad55;
            color: #7c2d12;
        }
        .alert-title {
            font-weight: 600;
            font-size: 16px;
            margin-bottom: 8px;
        }
        .alert-body { font-size: 14px; opacity: 0.9; }
        .data-table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 20px;
        }
        .data-table th {
            background: #f7fafc;
            padding: 12px;
            text-align: left;
            font-weight: 600;
            border-bottom: 2px solid #e2e8f0;
            font-size: 14px;
        }
        .data-table td {
            padding: 12px;
            border-bottom: 1px solid #e2e8f0;
            font-size: 14px;
        }
        .data-table tr:hover { background: #f7fafc; }
        .badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 12px;
            font-size: 12px;
            font-weight: 600;
        }
        .badge-critical { background: #fed7d7; color: #742a2a; }
        .badge-warning { background: #feebc8; color: #7c2d12; }
        .badge-success { background: #c6f6d5; color: #22543d; }
        .button {
            display: inline-block;
            padding: 10px 20px;
            background: #667eea;
            color: white;
            text-decoration: none;
            border-radius: 6px;
            font-size: 14px;
            font-weight: 500;
            transition: background 0.2s;
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
        .cost-box {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 25px;
            border-radius: 8px;
            margin-top: 20px;
        }
        .cost-row {
            display: flex;
            justify-content: space-between;
            margin: 10px 0;
            font-size: 18px;
        }
        .cost-savings {
            font-size: 32px;
            font-weight: bold;
            margin-top: 15px;
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
            <h1>🛡️ Kubernetes Cluster Health Report</h1>
            <div class="header-meta">
                <strong>Cluster:</strong> {{.ClusterName}} &nbsp;|&nbsp;
                <strong>Generated:</strong> {{.GeneratedAt.Format "January 2, 2006 3:04 PM"}} &nbsp;|&nbsp;
                <strong>Period:</strong> Last 24 hours
            </div>
        </div>
        
        <div class="content">
            <!-- Overall Health Score -->
            <div class="section">
                <div class="health-score">
                    <div class="score-label">Overall Health Score</div>
                    <div class="score-number">{{.OverallScore}}<span style="font-size: 36px;">/100</span></div>
                    <div style="font-size: 18px;">
                        {{if ge .OverallScore 80}}✅ Excellent
                        {{else if ge .OverallScore 60}}🟡 Needs Attention
                        {{else}}🔴 Action Required{{end}}
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill" style="width: {{.OverallScore}}%;"></div>
                    </div>
                </div>
            </div>
            
            <!-- Metrics Grid -->
            <div class="section">
                <div class="section-title">📊 Key Metrics</div>
                <div class="metrics-grid">
                    <div class="metric-card" style="border-color: {{if ge .ResourceScore 80}}#48bb78{{else if ge .ResourceScore 60}}#f6ad55{{else}}#fc8181{{end}};">
                        <div class="metric-label">Resources</div>
                        <div class="metric-value">{{.ResourceScore}}<span style="font-size: 20px;">/100</span></div>
                        <div style="color: {{if ge .ResourceScore 80}}#48bb78{{else if ge .ResourceScore 60}}#f6ad55{{else}}#fc8181{{end}}; font-weight: 600;">
                            {{if ge .ResourceScore 80}}✅ Good
                            {{else if ge .ResourceScore 60}}🟡 Needs Attention
                            {{else}}🔴 Action Required{{end}}
                        </div>
                    </div>
                    <div class="metric-card" style="border-color: {{if ge .SecurityScore 80}}#48bb78{{else if ge .SecurityScore 60}}#f6ad55{{else}}#fc8181{{end}};">
                        <div class="metric-label">Security</div>
                        <div class="metric-value">{{.SecurityScore}}<span style="font-size: 20px;">/100</span></div>
                        <div style="color: {{if ge .SecurityScore 80}}#48bb78{{else if ge .SecurityScore 60}}#f6ad55{{else}}#fc8181{{end}}; font-weight: 600;">
                            {{if ge .SecurityScore 80}}✅ Good
                            {{else if ge .SecurityScore 60}}🟡 Needs Attention
                            {{else}}🔴 Action Required{{end}}
                        </div>
                    </div>
                    <div class="metric-card" style="border-color: {{if ge .CostScore 80}}#48bb78{{else if ge .CostScore 60}}#f6ad55{{else}}#fc8181{{end}};">
                        <div class="metric-label">Cost Efficiency</div>
                        <div class="metric-value">{{.CostScore}}<span style="font-size: 20px;">/100</span></div>
                        <div style="color: {{if ge .CostScore 80}}#48bb78{{else if ge .CostScore 60}}#f6ad55{{else}}#fc8181{{end}}; font-weight: 600;">
                            {{if ge .CostScore 80}}✅ Good
                            {{else if ge .CostScore 60}}🟡 Needs Attention
                            {{else}}🔴 Action Required{{end}}
                        </div>
                    </div>
                </div>
            </div>
            
            <!-- Critical Issues -->
            {{if .CriticalIssues}}
            <div class="section">
                <div class="section-title">🚨 Critical Issues</div>
                {{range .CriticalIssues}}
                <div class="alert-box alert-critical">
                    <div class="alert-title">{{.Title}}</div>
                    <div class="alert-body">
                        {{.Description}}
                        {{if .Details}}
                        <ul style="margin-top: 8px;">
                        {{range .Details}}<li>{{.}}</li>{{end}}
                        </ul>
                        {{end}}
                    </div>
                </div>
                {{end}}
                
                {{range .WarningIssues}}
                <div class="alert-box alert-warning">
                    <div class="alert-title">{{.Title}}</div>
                    <div class="alert-body">{{.Description}}</div>
                </div>
                {{end}}
            </div>
            {{end}}
            
            <!-- Cost Optimization -->
            {{if gt .MonthlyCost 0.0}}
            <div class="section">
                <div class="section-title">💰 Cost Optimization Opportunities</div>
                
                <div class="cost-box">
                    <div class="cost-row">
                        <span>Monthly Cluster Cost:</span>
                        <span style="font-weight: 600;">{{formatMoney .MonthlyCost}}</span>
                    </div>
                    <div class="cost-row">
                        <span>Potential Savings:</span>
                        <span style="font-weight: 600;">{{formatMoney .PotentialSavings.Min}} - {{formatMoney .PotentialSavings.Max}}/month</span>
                    </div>
                    <div class="cost-savings">
                        💡 Cost Reduction Potential
                    </div>
                </div>
                
                {{if .CostBreakdown}}
                <table class="data-table">
                    <thead>
                        <tr>
                            <th>Opportunity</th>
                            <th>Impact</th>
                            <th>Savings/Month</th>
                            <th>Action</th>
                        </tr>
                    </thead>
                    <tbody>
                        {{range .CostBreakdown}}
                        <tr>
                            <td>{{.Name}}</td>
                            <td>
                                <span class="badge {{if eq .Impact "High"}}badge-critical{{else if eq .Impact "Medium"}}badge-warning{{else}}badge-success{{end}}">
                                    {{.Impact}}
                                </span>
                            </td>
                            <td>{{formatMoney .Savings}}</td>
                            <td>{{.Action}}</td>
                        </tr>
                        {{end}}
                    </tbody>
                </table>
                {{end}}
            </div>
            {{end}}
            
            <!-- Namespace Breakdown -->
            {{if .Namespaces}}
            <div class="section">
                <div class="section-title">📊 Namespace Resource Breakdown</div>
                <table class="data-table">
                    <thead>
                        <tr>
                            <th>Namespace</th>
                            <th>CPU %</th>
                            <th>Memory %</th>
                            <th>Pods</th>
                            {{if gt .MonthlyCost 0.0}}<th>Cost/Month</th>{{end}}
                            <th>Flags</th>
                        </tr>
                    </thead>
                    <tbody>
                        {{range .Namespaces}}
                        <tr>
                            <td><strong>{{.Name}}</strong></td>
                            <td>{{formatPercent .CPUPercent}}</td>
                            <td>{{formatPercent .MemPercent}}</td>
                            <td>{{.PodCount}}</td>
                            {{if gt $.MonthlyCost 0.0}}<td>{{formatMoney .Cost}}</td>{{end}}
                            <td>
                                {{range .Flags}}
                                <span class="badge {{if contains . "IDLE"}}badge-critical{{else}}badge-success{{end}}">
                                    {{.}}
                                </span>
                                {{end}}
                            </td>
                        </tr>
                        {{end}}
                    </tbody>
                </table>
            </div>
            {{end}}
            
            <!-- Security Summary -->
            {{if gt .CISScore 0}}
            <div class="section">
                <div class="section-title">🔒 Security Summary</div>
                <div class="metrics-grid">
                    <div class="metric-card" style="border-color: {{if ge .CISScore 80}}#48bb78{{else if ge .CISScore 60}}#f6ad55{{else}}#fc8181{{end}};">
                        <div class="metric-label">CIS Compliance Score</div>
                        <div class="metric-value">{{.CISScore}}<span style="font-size: 20px;">/100</span></div>
                        <div style="font-size: 14px; margin-top: 5px;">{{.ControlsPassed}} of {{add .ControlsPassed .ControlsFailed}} controls passed</div>
                    </div>
                </div>
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
