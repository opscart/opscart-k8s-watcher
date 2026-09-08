package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type CloudProvider string

const (
	CloudProviderAzure   CloudProvider = "azure"
	CloudProviderAWS     CloudProvider = "aws"
	CloudProviderUnknown CloudProvider = "unknown"
	CloudProviderMixed   CloudProvider = "mixed"
)

type PriceProvenance string

const (
	PriceProvenanceProviderAPIExact     PriceProvenance = "provider_api_exact"
	PriceProvenanceProviderAPIEstimated PriceProvenance = "provider_api_estimated"
	PriceProvenanceUserSupplied         PriceProvenance = "user_supplied"
	PriceProvenanceUnavailable          PriceProvenance = "unavailable"
)

func ParseCloudProviderOverride(value string) (CloudProvider, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return CloudProviderUnknown, nil
	case "azure":
		return CloudProviderAzure, nil
	case "aws":
		return CloudProviderAWS, nil
	default:
		return CloudProviderUnknown, fmt.Errorf("invalid cloud provider %q: use auto, azure, or aws", value)
	}
}

type PriceRequest struct {
	InstanceType string
	Region       string
	OS           string
	CapacityType string
}

type PriceResult struct {
	HourlyPrice float64
	Currency    string
	RefreshedAt time.Time
	Provenance  PriceProvenance
	Source      string
}

type PricingCapabilities struct {
	OnDemand      bool
	Spot          bool
	Reservations  bool
	SavingsData   bool
	CapacityTypes []string
}

type PricingProvider interface {
	Provider() CloudProvider
	LookupOnDemandPrice(context.Context, PriceRequest) (PriceResult, error)
	Capabilities() PricingCapabilities
	SourceDescription() string
}

func NewAzurePricingProvider() PricingProvider {
	return newAzurePricingProvider(nil, azureRetailPricesURL, 24*time.Hour)
}
func (p *azurePricingProvider) Provider() CloudProvider { return CloudProviderAzure }
func (p *azurePricingProvider) SourceDescription() string {
	return "Azure Retail Prices API public/list pricing"
}
func (p *azurePricingProvider) Capabilities() PricingCapabilities {
	// Spot, reservation and savings-plan pricing need their own exact API
	// resolution paths. Do not advertise them until those paths are wired.
	return PricingCapabilities{OnDemand: true, CapacityTypes: []string{"Regular"}}
}
func (p *azurePricingProvider) LookupOnDemandPrice(ctx context.Context, r PriceRequest) (PriceResult, error) {
	return p.lookupRetailPrice(ctx, r)
}

func DetectNodeProvider(node corev1.Node) CloudProvider {
	id := strings.ToLower(node.Spec.ProviderID)
	switch {
	case strings.HasPrefix(id, "azure://"):
		return CloudProviderAzure
	case strings.HasPrefix(id, "aws://"):
		return CloudProviderAWS
	default:
		return CloudProviderUnknown
	}
}

func DetectClusterProvider(nodes []corev1.Node) CloudProvider {
	seen := map[CloudProvider]bool{}
	for _, node := range nodes {
		seen[DetectNodeProvider(node)] = true
	}
	if len(seen) == 1 {
		for provider := range seen {
			return provider
		}
	}
	if len(seen) > 1 {
		return CloudProviderMixed
	}
	return CloudProviderUnknown
}
