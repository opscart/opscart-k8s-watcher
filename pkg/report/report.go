package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReportFormat defines the output format
type ReportFormat string

const (
	FormatHTML ReportFormat = "html"
	FormatJSON ReportFormat = "json"
	FormatCSV  ReportFormat = "csv"
)

// ReportData holds all data for report generation
type ReportData struct {
	ClusterName   string
	GeneratedAt   time.Time
	OverallScore  int
	SecurityScore int
	ResourceScore int
	CostScore     int

	// Critical issues
	CriticalIssues []IssueItem
	WarningIssues  []IssueItem

	// Cost data
	MonthlyCost      float64
	PotentialSavings SavingsRange
	CostBreakdown    []CostItem

	// Resource data
	TotalCPU       float64
	TotalMemory    float64
	UsedCPU        float64
	UsedMemory     float64
	PodCount       int
	NamespaceCount int

	// Security data
	CISScore         int
	ControlsPassed   int
	ControlsFailed   int
	SecurityFindings []SecurityFinding

	// Namespace breakdown
	Namespaces []NamespaceItem

	// Trends (optional - for future)
	TrendData *TrendData
}

type IssueItem struct {
	Severity    string // "critical", "warning", "info"
	Title       string
	Description string
	Count       int
	Details     []string
}

type SavingsRange struct {
	Min float64
	Max float64
}

type CostItem struct {
	Name    string
	Impact  string // "High", "Medium", "Low"
	Savings float64
	Action  string
}

type SecurityFinding struct {
	Control     string
	Status      string // "passed", "failed"
	Severity    string
	Count       int
	Resources   []string
	Remediation string
}

type NamespaceItem struct {
	Name       string
	CPUPercent float64
	MemPercent float64
	PodCount   int
	Cost       float64
	Flags      []string
}

type TrendData struct {
	HealthScoreTrend    []int
	CriticalIssueTrend  []int
	CostEfficiencyTrend []int
	Labels              []string
}

// Generator handles report generation
type Generator struct {
	format     ReportFormat
	outputPath string
}

// NewGenerator creates a new report generator
func NewGenerator(format ReportFormat, outputPath string) *Generator {
	return &Generator{
		format:     format,
		outputPath: outputPath,
	}
}

// Generate creates the report based on format
func (g *Generator) Generate(data *ReportData) (string, error) {
	switch g.format {
	case FormatHTML:
		return g.generateHTML(data)
	case FormatJSON:
		return g.generateJSON(data)
	case FormatCSV:
		return g.generateCSV(data)
	default:
		return "", fmt.Errorf("unsupported format: %s", g.format)
	}
}

// generateHTML creates an HTML report
func (g *Generator) generateHTML(data *ReportData) (string, error) {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"formatFloat": func(f float64) string {
			return fmt.Sprintf("%.1f", f)
		},
		"formatMoney": func(f float64) string {
			return fmt.Sprintf("$%.0f", f)
		},
		"formatPercent": func(f float64) string {
			return fmt.Sprintf("%.1f%%", f)
		},
		"div": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mul": func(a, b float64) float64 {
			return a * b
		},
		"add": func(a, b int) int {
			return a + b
		},
		"contains": func(s, substr string) bool {
			return strings.Contains(s, substr)
		},
	}).Parse(htmlTemplate)

	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Generate filename if not specified
	filename := g.outputPath
	if filename == "" {
		// Create reports/YYYY-MM-DD directory structure
		today := time.Now().Format("2006-01-02")
		reportsDir := filepath.Join("reports", today)

		if err := os.MkdirAll(reportsDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create reports directory: %w", err)
		}

		timestamp := time.Now().Format("1504")
		filename = filepath.Join(reportsDir, fmt.Sprintf("%s-report-%s.html", data.ClusterName, timestamp))
	}

	// Create file
	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Execute template
	if err := tmpl.Execute(file, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return filepath.Abs(filename)
}

// generateJSON creates a JSON report
func (g *Generator) generateJSON(data *ReportData) (string, error) {
	// Generate filename if not specified
	filename := g.outputPath
	if filename == "" {
		// Create reports/YYYY-MM-DD directory structure
		today := time.Now().Format("2006-01-02")
		reportsDir := filepath.Join("reports", today)

		if err := os.MkdirAll(reportsDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create reports directory: %w", err)
		}

		timestamp := time.Now().Format("1504")
		filename = filepath.Join(reportsDir, fmt.Sprintf("%s-report-%s.json", data.ClusterName, timestamp))
	}

	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(data); err != nil {
		return "", fmt.Errorf("failed to encode JSON: %w", err)
	}

	return filepath.Abs(filename)
}

// generateCSV creates a CSV report (summary only)
func (g *Generator) generateCSV(data *ReportData) (string, error) {
	// Generate filename if not specified
	filename := g.outputPath
	if filename == "" {
		// Create reports/YYYY-MM-DD directory structure
		today := time.Now().Format("2006-01-02")
		reportsDir := filepath.Join("reports", today)

		if err := os.MkdirAll(reportsDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create reports directory: %w", err)
		}

		timestamp := time.Now().Format("1504")
		filename = filepath.Join(reportsDir, fmt.Sprintf("%s-report-%s.csv", data.ClusterName, timestamp))
	}

	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write summary
	writer.Write([]string{"Metric", "Value"})
	writer.Write([]string{"Cluster", data.ClusterName})
	writer.Write([]string{"Generated", data.GeneratedAt.Format("2006-01-02 15:04:05")})
	writer.Write([]string{"Overall Score", fmt.Sprintf("%d", data.OverallScore)})
	writer.Write([]string{"Security Score", fmt.Sprintf("%d", data.SecurityScore)})
	writer.Write([]string{"Resource Score", fmt.Sprintf("%d", data.ResourceScore)})
	writer.Write([]string{"Cost Score", fmt.Sprintf("%d", data.CostScore)})
	writer.Write([]string{})

	// Write namespace breakdown
	writer.Write([]string{"Namespace", "CPU %", "Memory %", "Pods", "Cost/Month", "Flags"})
	for _, ns := range data.Namespaces {
		flags := ""
		if len(ns.Flags) > 0 {
			flags = ns.Flags[0]
		}
		writer.Write([]string{
			ns.Name,
			fmt.Sprintf("%.1f", ns.CPUPercent),
			fmt.Sprintf("%.1f", ns.MemPercent),
			fmt.Sprintf("%d", ns.PodCount),
			fmt.Sprintf("%.0f", ns.Cost),
			flags,
		})
	}

	return filepath.Abs(filename)
}

// GenerateSecurityHTML creates a security-focused HTML report (public method)
func (g *Generator) GenerateSecurityHTML(data *ReportData) (string, error) {
	tmpl, err := template.New("security").Funcs(template.FuncMap{
		"formatFloat": func(f float64) string {
			return fmt.Sprintf("%.1f", f)
		},
		"add": func(a, b int) int {
			return a + b
		},
		"ge": func(a, b int) bool {
			return a >= b
		},
		"eq": func(a, b string) bool {
			return a == b
		},
		"ne": func(a, b string) bool {
			return a != b
		},
	}).Parse(securityHTMLTemplate)

	if err != nil {
		return "", fmt.Errorf("failed to parse security template: %w", err)
	}

	// Determine output path
	filename := g.outputPath
	if filename == "" {
		today := time.Now().Format("2006-01-02")
		timestamp := time.Now().Format("1504")
		reportsDir := filepath.Join("reports", today)

		if err := os.MkdirAll(reportsDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create reports directory: %w", err)
		}

		filename = filepath.Join(reportsDir, fmt.Sprintf("%s-security-%s.html", data.ClusterName, timestamp))
	}

	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	if err := tmpl.Execute(file, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return filepath.Abs(filename)
}

// CalculateOverallScore computes overall health score
func CalculateOverallScore(security, resource, cost int) int {
	// Weighted average: Security 40%, Resource 30%, Cost 30%
	return (security*40 + resource*30 + cost*30) / 100
}

// CalculateResourceScore computes resource utilization score
func CalculateResourceScore(usedPercent float64) int {
	// Ideal utilization: 60-80%
	// Too low = waste, too high = risk
	if usedPercent < 40 {
		return 50 // Too much waste
	} else if usedPercent >= 40 && usedPercent < 60 {
		return 70 // Acceptable
	} else if usedPercent >= 60 && usedPercent <= 80 {
		return 100 // Ideal
	} else if usedPercent > 80 && usedPercent <= 90 {
		return 85 // Getting high
	} else {
		return 60 // Too high, risk of issues
	}
}

// CalculateCostScore computes cost efficiency score
func CalculateCostScore(idleResources, spotEligible, total int) int {
	idlePercent := float64(idleResources) / float64(total) * 100
	spotPercent := float64(spotEligible) / float64(total) * 100

	// More idle = worse score
	// More spot eligible = worse score (not utilizing spot)
	idleScore := 100 - int(idlePercent*2)
	spotScore := 100 - int(spotPercent)

	if idleScore < 0 {
		idleScore = 0
	}
	if spotScore < 0 {
		spotScore = 0
	}

	// Average
	return (idleScore + spotScore) / 2
}
