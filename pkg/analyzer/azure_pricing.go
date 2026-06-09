package analyzer

import (
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// AzurePricingCatalog provides embedded VM pricing for common AKS node sizes.
// Prices are Pay-As-You-Go for Linux in East US 2 (as of 2026).
// Users can override region with --region flag; the tool adjusts with a multiplier.
//
// Sources: Azure Pricing Calculator, Azure Retail Prices API
// These are approximate and updated periodically.
var AzurePricingCatalog = map[string]models.VMPricing{
	// ──────────────────────────────────────────────────────────────────
	// B-series (Burstable) — dev/test
	// ──────────────────────────────────────────────────────────────────
	"standard_b2s": {SKU: "Standard_B2s", Family: "B-series", CPUCores: 2, MemoryGB: 4,
		PayAsYouGoHour: 0.0416, PayAsYouGoMonth: 30.37, SpotHour: 0.0125, OneYearRI: 19.27, ThreeYearRI: 12.41},
	"standard_b2ms": {SKU: "Standard_B2ms", Family: "B-series", CPUCores: 2, MemoryGB: 8,
		PayAsYouGoHour: 0.0832, PayAsYouGoMonth: 60.74, SpotHour: 0.0250, OneYearRI: 38.53, ThreeYearRI: 24.82},
	"standard_b4ms": {SKU: "Standard_B4ms", Family: "B-series", CPUCores: 4, MemoryGB: 16,
		PayAsYouGoHour: 0.1660, PayAsYouGoMonth: 121.18, SpotHour: 0.0498, OneYearRI: 76.89, ThreeYearRI: 49.50},
	"standard_b8ms": {SKU: "Standard_B8ms", Family: "B-series", CPUCores: 8, MemoryGB: 32,
		PayAsYouGoHour: 0.3320, PayAsYouGoMonth: 242.36, SpotHour: 0.0996, OneYearRI: 153.78, ThreeYearRI: 99.01},

	// ──────────────────────────────────────────────────────────────────
	// D-series v3/v4/v5 (General Purpose)
	// ──────────────────────────────────────────────────────────────────
	"standard_d2s_v3": {SKU: "Standard_D2s_v3", Family: "Dsv3-series", CPUCores: 2, MemoryGB: 8,
		PayAsYouGoHour: 0.096, PayAsYouGoMonth: 70.08, SpotHour: 0.0192, OneYearRI: 43.80, ThreeYearRI: 27.96},
	"standard_d4s_v3": {SKU: "Standard_D4s_v3", Family: "Dsv3-series", CPUCores: 4, MemoryGB: 16,
		PayAsYouGoHour: 0.192, PayAsYouGoMonth: 140.16, SpotHour: 0.0384, OneYearRI: 87.60, ThreeYearRI: 55.92},
	"standard_d8s_v3": {SKU: "Standard_D8s_v3", Family: "Dsv3-series", CPUCores: 8, MemoryGB: 32,
		PayAsYouGoHour: 0.384, PayAsYouGoMonth: 280.32, SpotHour: 0.0768, OneYearRI: 175.20, ThreeYearRI: 111.84},
	"standard_d16s_v3": {SKU: "Standard_D16s_v3", Family: "Dsv3-series", CPUCores: 16, MemoryGB: 64,
		PayAsYouGoHour: 0.768, PayAsYouGoMonth: 560.64, SpotHour: 0.1536, OneYearRI: 350.40, ThreeYearRI: 223.68},
	"standard_d32s_v3": {SKU: "Standard_D32s_v3", Family: "Dsv3-series", CPUCores: 32, MemoryGB: 128,
		PayAsYouGoHour: 1.536, PayAsYouGoMonth: 1121.28, SpotHour: 0.3072, OneYearRI: 700.80, ThreeYearRI: 447.36},
	"standard_d48s_v3": {SKU: "Standard_D48s_v3", Family: "Dsv3-series", CPUCores: 48, MemoryGB: 192,
		PayAsYouGoHour: 2.304, PayAsYouGoMonth: 1681.92, SpotHour: 0.4608, OneYearRI: 1051.20, ThreeYearRI: 671.04},
	"standard_d64s_v3": {SKU: "Standard_D64s_v3", Family: "Dsv3-series", CPUCores: 64, MemoryGB: 256,
		PayAsYouGoHour: 3.072, PayAsYouGoMonth: 2242.56, SpotHour: 0.6144, OneYearRI: 1401.60, ThreeYearRI: 894.72},

	"standard_d2s_v4": {SKU: "Standard_D2s_v4", Family: "Dsv4-series", CPUCores: 2, MemoryGB: 8,
		PayAsYouGoHour: 0.096, PayAsYouGoMonth: 70.08, SpotHour: 0.0192, OneYearRI: 43.80, ThreeYearRI: 27.96},
	"standard_d4s_v4": {SKU: "Standard_D4s_v4", Family: "Dsv4-series", CPUCores: 4, MemoryGB: 16,
		PayAsYouGoHour: 0.192, PayAsYouGoMonth: 140.16, SpotHour: 0.0384, OneYearRI: 87.60, ThreeYearRI: 55.92},
	"standard_d8s_v4": {SKU: "Standard_D8s_v4", Family: "Dsv4-series", CPUCores: 8, MemoryGB: 32,
		PayAsYouGoHour: 0.384, PayAsYouGoMonth: 280.32, SpotHour: 0.0768, OneYearRI: 175.20, ThreeYearRI: 111.84},
	"standard_d16s_v4": {SKU: "Standard_D16s_v4", Family: "Dsv4-series", CPUCores: 16, MemoryGB: 64,
		PayAsYouGoHour: 0.768, PayAsYouGoMonth: 560.64, SpotHour: 0.1536, OneYearRI: 350.40, ThreeYearRI: 223.68},
	"standard_d32s_v4": {SKU: "Standard_D32s_v4", Family: "Dsv4-series", CPUCores: 32, MemoryGB: 128,
		PayAsYouGoHour: 1.536, PayAsYouGoMonth: 1121.28, SpotHour: 0.3072, OneYearRI: 700.80, ThreeYearRI: 447.36},

	"standard_d2s_v5": {SKU: "Standard_D2s_v5", Family: "Dsv5-series", CPUCores: 2, MemoryGB: 8,
		PayAsYouGoHour: 0.096, PayAsYouGoMonth: 70.08, SpotHour: 0.0192, OneYearRI: 43.07, ThreeYearRI: 27.48},
	"standard_d4s_v5": {SKU: "Standard_D4s_v5", Family: "Dsv5-series", CPUCores: 4, MemoryGB: 16,
		PayAsYouGoHour: 0.192, PayAsYouGoMonth: 140.16, SpotHour: 0.0384, OneYearRI: 86.14, ThreeYearRI: 54.97},
	"standard_d8s_v5": {SKU: "Standard_D8s_v5", Family: "Dsv5-series", CPUCores: 8, MemoryGB: 32,
		PayAsYouGoHour: 0.384, PayAsYouGoMonth: 280.32, SpotHour: 0.0768, OneYearRI: 172.28, ThreeYearRI: 109.94},
	"standard_d16s_v5": {SKU: "Standard_D16s_v5", Family: "Dsv5-series", CPUCores: 16, MemoryGB: 64,
		PayAsYouGoHour: 0.768, PayAsYouGoMonth: 560.64, SpotHour: 0.1536, OneYearRI: 344.55, ThreeYearRI: 219.87},
	"standard_d32s_v5": {SKU: "Standard_D32s_v5", Family: "Dsv5-series", CPUCores: 32, MemoryGB: 128,
		PayAsYouGoHour: 1.536, PayAsYouGoMonth: 1121.28, SpotHour: 0.3072, OneYearRI: 689.11, ThreeYearRI: 439.75},
	"standard_d48s_v5": {SKU: "Standard_D48s_v5", Family: "Dsv5-series", CPUCores: 48, MemoryGB: 192,
		PayAsYouGoHour: 2.304, PayAsYouGoMonth: 1681.92, SpotHour: 0.4608, OneYearRI: 1033.66, ThreeYearRI: 659.62},
	"standard_d64s_v5": {SKU: "Standard_D64s_v5", Family: "Dsv5-series", CPUCores: 64, MemoryGB: 256,
		PayAsYouGoHour: 3.072, PayAsYouGoMonth: 2242.56, SpotHour: 0.6144, OneYearRI: 1378.22, ThreeYearRI: 879.50},

	// ──────────────────────────────────────────────────────────────────
	// E-series (Memory Optimized) — databases, caches
	// ──────────────────────────────────────────────────────────────────
	"standard_e2s_v3": {SKU: "Standard_E2s_v3", Family: "Esv3-series", CPUCores: 2, MemoryGB: 16,
		PayAsYouGoHour: 0.126, PayAsYouGoMonth: 91.98, SpotHour: 0.0252, OneYearRI: 57.35, ThreeYearRI: 36.79},
	"standard_e4s_v3": {SKU: "Standard_E4s_v3", Family: "Esv3-series", CPUCores: 4, MemoryGB: 32,
		PayAsYouGoHour: 0.252, PayAsYouGoMonth: 183.96, SpotHour: 0.0504, OneYearRI: 114.70, ThreeYearRI: 73.58},
	"standard_e8s_v3": {SKU: "Standard_E8s_v3", Family: "Esv3-series", CPUCores: 8, MemoryGB: 64,
		PayAsYouGoHour: 0.504, PayAsYouGoMonth: 367.92, SpotHour: 0.1008, OneYearRI: 229.40, ThreeYearRI: 147.17},
	"standard_e16s_v3": {SKU: "Standard_E16s_v3", Family: "Esv3-series", CPUCores: 16, MemoryGB: 128,
		PayAsYouGoHour: 1.008, PayAsYouGoMonth: 735.84, SpotHour: 0.2016, OneYearRI: 458.80, ThreeYearRI: 294.34},
	"standard_e32s_v3": {SKU: "Standard_E32s_v3", Family: "Esv3-series", CPUCores: 32, MemoryGB: 256,
		PayAsYouGoHour: 2.016, PayAsYouGoMonth: 1471.68, SpotHour: 0.4032, OneYearRI: 917.60, ThreeYearRI: 588.67},

	"standard_e4s_v5": {SKU: "Standard_E4s_v5", Family: "Esv5-series", CPUCores: 4, MemoryGB: 32,
		PayAsYouGoHour: 0.252, PayAsYouGoMonth: 183.96, SpotHour: 0.0504, OneYearRI: 113.15, ThreeYearRI: 72.26},
	"standard_e8s_v5": {SKU: "Standard_E8s_v5", Family: "Esv5-series", CPUCores: 8, MemoryGB: 64,
		PayAsYouGoHour: 0.504, PayAsYouGoMonth: 367.92, SpotHour: 0.1008, OneYearRI: 226.30, ThreeYearRI: 144.53},
	"standard_e16s_v5": {SKU: "Standard_E16s_v5", Family: "Esv5-series", CPUCores: 16, MemoryGB: 128,
		PayAsYouGoHour: 1.008, PayAsYouGoMonth: 735.84, SpotHour: 0.2016, OneYearRI: 452.59, ThreeYearRI: 289.06},
	"standard_e32s_v5": {SKU: "Standard_E32s_v5", Family: "Esv5-series", CPUCores: 32, MemoryGB: 256,
		PayAsYouGoHour: 2.016, PayAsYouGoMonth: 1471.68, SpotHour: 0.4032, OneYearRI: 905.18, ThreeYearRI: 578.12},

	// ──────────────────────────────────────────────────────────────────
	// F-series (Compute Optimized) — batch, gaming, ML inference
	// ──────────────────────────────────────────────────────────────────
	"standard_f2s_v2": {SKU: "Standard_F2s_v2", Family: "Fsv2-series", CPUCores: 2, MemoryGB: 4,
		PayAsYouGoHour: 0.085, PayAsYouGoMonth: 62.05, SpotHour: 0.017, OneYearRI: 38.69, ThreeYearRI: 24.82},
	"standard_f4s_v2": {SKU: "Standard_F4s_v2", Family: "Fsv2-series", CPUCores: 4, MemoryGB: 8,
		PayAsYouGoHour: 0.170, PayAsYouGoMonth: 124.10, SpotHour: 0.034, OneYearRI: 77.38, ThreeYearRI: 49.64},
	"standard_f8s_v2": {SKU: "Standard_F8s_v2", Family: "Fsv2-series", CPUCores: 8, MemoryGB: 16,
		PayAsYouGoHour: 0.340, PayAsYouGoMonth: 248.20, SpotHour: 0.068, OneYearRI: 154.76, ThreeYearRI: 99.28},
	"standard_f16s_v2": {SKU: "Standard_F16s_v2", Family: "Fsv2-series", CPUCores: 16, MemoryGB: 32,
		PayAsYouGoHour: 0.680, PayAsYouGoMonth: 496.40, SpotHour: 0.136, OneYearRI: 309.53, ThreeYearRI: 198.56},
	"standard_f32s_v2": {SKU: "Standard_F32s_v2", Family: "Fsv2-series", CPUCores: 32, MemoryGB: 64,
		PayAsYouGoHour: 1.360, PayAsYouGoMonth: 992.80, SpotHour: 0.272, OneYearRI: 619.06, ThreeYearRI: 397.12},
	"standard_f48s_v2": {SKU: "Standard_F48s_v2", Family: "Fsv2-series", CPUCores: 48, MemoryGB: 96,
		PayAsYouGoHour: 2.040, PayAsYouGoMonth: 1489.20, SpotHour: 0.408, OneYearRI: 928.58, ThreeYearRI: 595.68},
	"standard_f64s_v2": {SKU: "Standard_F64s_v2", Family: "Fsv2-series", CPUCores: 64, MemoryGB: 128,
		PayAsYouGoHour: 2.720, PayAsYouGoMonth: 1985.60, SpotHour: 0.544, OneYearRI: 1238.11, ThreeYearRI: 794.24},

	// ──────────────────────────────────────────────────────────────────
	// D-series (non-s, no premium storage) — common in older clusters
	// ──────────────────────────────────────────────────────────────────
	"standard_d2_v3": {SKU: "Standard_D2_v3", Family: "Dv3-series", CPUCores: 2, MemoryGB: 8,
		PayAsYouGoHour: 0.096, PayAsYouGoMonth: 70.08, SpotHour: 0.0192, OneYearRI: 43.80, ThreeYearRI: 27.96},
	"standard_d4_v3": {SKU: "Standard_D4_v3", Family: "Dv3-series", CPUCores: 4, MemoryGB: 16,
		PayAsYouGoHour: 0.192, PayAsYouGoMonth: 140.16, SpotHour: 0.0384, OneYearRI: 87.60, ThreeYearRI: 55.92},

	// ──────────────────────────────────────────────────────────────────
	// L-series (Storage Optimized) — big data, databases
	// ──────────────────────────────────────────────────────────────────
	"standard_l8s_v3": {SKU: "Standard_L8s_v3", Family: "Lsv3-series", CPUCores: 8, MemoryGB: 64,
		PayAsYouGoHour: 0.624, PayAsYouGoMonth: 455.52, SpotHour: 0.1248, OneYearRI: 284.70, ThreeYearRI: 182.50},
	"standard_l16s_v3": {SKU: "Standard_L16s_v3", Family: "Lsv3-series", CPUCores: 16, MemoryGB: 128,
		PayAsYouGoHour: 1.248, PayAsYouGoMonth: 911.04, SpotHour: 0.2496, OneYearRI: 569.40, ThreeYearRI: 365.00},
	"standard_l32s_v3": {SKU: "Standard_L32s_v3", Family: "Lsv3-series", CPUCores: 32, MemoryGB: 256,
		PayAsYouGoHour: 2.496, PayAsYouGoMonth: 1822.08, SpotHour: 0.4992, OneYearRI: 1138.80, ThreeYearRI: 730.00},
}

// RegionMultipliers adjust pricing relative to East US 2 baseline
var RegionMultipliers = map[string]float64{
	"eastus":         1.00,
	"eastus2":        1.00,
	"centralus":      1.00,
	"westus":         1.00,
	"westus2":        1.00,
	"westus3":        1.00,
	"northcentralus": 1.00,
	"southcentralus": 1.00,
	"westeurope":     1.15,
	"northeurope":    1.12,
	"uksouth":        1.14,
	"ukwest":         1.14,
	"germanywestcentral": 1.18,
	"francecentral":  1.16,
	"canadacentral":  1.03,
	"canadaeast":     1.03,
	"australiaeast":  1.20,
	"southeastasia":  1.10,
	"japaneast":      1.25,
	"koreacentral":   1.15,
	"brazilsouth":    1.45,
	"indiacentral":   1.08,
}

// LookupVMPrice returns pricing for a given VM SKU (case-insensitive)
func LookupVMPrice(vmSize string, region string) (models.VMPricing, bool) {
	key := strings.ToLower(vmSize)
	pricing, found := AzurePricingCatalog[key]
	if !found {
		return models.VMPricing{}, false
	}

	// Apply region multiplier
	multiplier := 1.0
	if region != "" {
		regionKey := strings.ToLower(strings.ReplaceAll(region, " ", ""))
		if m, ok := RegionMultipliers[regionKey]; ok {
			multiplier = m
		}
	}

	if multiplier != 1.0 {
		pricing.PayAsYouGoHour *= multiplier
		pricing.PayAsYouGoMonth *= multiplier
		pricing.SpotHour *= multiplier
		pricing.OneYearRI *= multiplier
		pricing.ThreeYearRI *= multiplier
	}
	pricing.Region = region

	return pricing, true
}

// EstimateVMFromResources tries to find the best-fit VM SKU for given CPU/memory
// Used when node labels don't have VM size info (generic K8s clusters)
func EstimateVMFromResources(cpuCores float64, memoryGB float64) (models.VMPricing, bool) {
	var bestMatch models.VMPricing
	bestScore := 999999.0
	found := false

	for _, pricing := range AzurePricingCatalog {
		// SKU must be >= node capacity
		if float64(pricing.CPUCores) < cpuCores || pricing.MemoryGB < memoryGB {
			continue
		}
		// Score: closest match (smallest overshoot)
		cpuOvershoot := float64(pricing.CPUCores) - cpuCores
		memOvershoot := pricing.MemoryGB - memoryGB
		score := cpuOvershoot*10 + memOvershoot // weight CPU higher

		if score < bestScore {
			bestScore = score
			bestMatch = pricing
			found = true
		}
	}

	return bestMatch, found
}
