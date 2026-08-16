package models

// NodeConditionSeverity is the shared product severity policy for unhealthy
// Kubernetes Node conditions. Callers provide findings already validated by
// node-condition detection; mutable placement evidence never participates.
func NodeConditionSeverity(conditionType string) (string, bool) {
	switch conditionType {
	case "Ready":
		return "critical", true
	case "DiskPressure", "MemoryPressure", "PIDPressure", "NetworkUnavailable":
		return "high", true
	default:
		return "", false
	}
}
