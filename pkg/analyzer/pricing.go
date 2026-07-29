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

type azurePricingProvider struct{}

func NewAzurePricingProvider() PricingProvider       { return azurePricingProvider{} }
func (azurePricingProvider) Provider() CloudProvider { return CloudProviderAzure }
func (azurePricingProvider) SourceDescription() string {
	return "Embedded Azure public retail pricing catalog"
}
func (azurePricingProvider) Capabilities() PricingCapabilities {
	return PricingCapabilities{OnDemand: true, Spot: true, Reservations: true, SavingsData: true, CapacityTypes: []string{"Regular", "Spot"}}
}
func (azurePricingProvider) LookupOnDemandPrice(_ context.Context, r PriceRequest) (PriceResult, error) {
	p, ok := LookupVMPrice(r.InstanceType, r.Region)
	if !ok {
		return PriceResult{}, fmt.Errorf("instance type %q is not in the embedded Azure catalog", r.InstanceType)
	}
	hourly := p.PayAsYouGoHour
	if strings.EqualFold(r.CapacityType, "spot") {
		hourly = p.SpotHour
	}
	if hourly <= 0 {
		return PriceResult{}, fmt.Errorf("no %s price for %q", r.CapacityType, r.InstanceType)
	}
	return PriceResult{HourlyPrice: hourly, Currency: "USD"}, nil
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
