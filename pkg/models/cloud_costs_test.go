package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCloudCostReportJSONExposesPricingCapabilities(t *testing.T) {
	report := CloudCostReport{PricingCapabilities: PricingCapabilities{
		OnDemand: true, CapacityTypes: []string{"Regular"},
	}}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(encoded)
	for _, want := range []string{
		`"pricing_capabilities"`, `"on_demand":true`, `"spot":false`,
		`"reservations":false`, `"savings_data":false`, `"capacity_types":["Regular"]`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Errorf("CloudCostReport JSON missing %s: %s", want, jsonText)
		}
	}
}
