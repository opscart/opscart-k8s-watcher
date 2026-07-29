package report

import (
	"strings"
	"testing"
)

func TestSecurityTemplateUsesReviewOrientedActions(t *testing.T) {
	for _, want := range []string{
		"Review hostPath mounts and verify which workloads require host access",
		"Review containers missing CPU or memory limits",
	} {
		if !strings.Contains(securityHTMLTemplate, want) {
			t.Errorf("security template missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"Remove hostPath volumes",
		"Add resource limits to all pods",
		"CIS Compliance Score",
		"🛡️", "✅", "❌", "🔴", "🟡", "📊", "📋", "🔍",
	} {
		if strings.Contains(securityHTMLTemplate, forbidden) {
			t.Errorf("security template contains unsupported wording or emoji %q", forbidden)
		}
	}
}
