package models

// RiskCostAnalysis represents financial risk from security vulnerabilities
type RiskCostAnalysis struct {
	SecurityScore           int             `json:"security_score"`
	TotalRiskExposure       CostRange       `json:"total_risk_exposure"`
	RiskCategories          []RiskCategory  `json:"risk_categories"`
	RemediationPlan         RemediationPlan `json:"remediation_plan"`
	PriorityRecommendations []string        `json:"priority_recommendations"`
}

// RiskCategory represents a category of security risk
type RiskCategory struct {
	Name             string    `json:"name"`
	Severity         string    `json:"severity"` // "critical", "high", "medium", "low"
	Count            int       `json:"count"`
	Description      string    `json:"description"`
	RiskExposure     CostRange `json:"risk_exposure"`     // Expected financial loss
	TypicalIncidents []string  `json:"typical_incidents"` // What could happen
	IndustryExamples []string  `json:"industry_examples"` // Real-world incidents
}

// RemediationPlan represents the cost and effort to fix security issues
type RemediationPlan struct {
	TotalHours    float64            `json:"total_hours"`
	CriticalHours float64            `json:"critical_hours"`
	HighHours     float64            `json:"high_hours"`
	MediumHours   float64            `json:"medium_hours"`
	EstimatedCost float64            `json:"estimated_cost"`
	RiskReduction float64            `json:"risk_reduction"`
	ROI           float64            `json:"roi"`            // Risk reduction / cost
	PaybackMonths float64            `json:"payback_months"` // Months to recover investment
	Timeline      string             `json:"timeline"`
	Phases        []RemediationPhase `json:"phases"`
}

// RemediationPhase represents a phase of security remediation
type RemediationPhase struct {
	Name     string  `json:"name"`
	Duration string  `json:"duration"`
	Hours    float64 `json:"hours"`
	Cost     float64 `json:"cost"`
	Priority string  `json:"priority"`
}
