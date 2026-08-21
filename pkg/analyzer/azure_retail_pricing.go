package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const azureRetailPricesURL = "https://prices.azure.com/api/retail/prices"

type azureRetailCacheEntry struct {
	result    PriceResult
	expiresAt time.Time
}

type azurePricingProvider struct {
	client   *http.Client
	endpoint string
	ttl      time.Duration
	mu       sync.Mutex
	cache    map[string]azureRetailCacheEntry
}

func newAzurePricingProvider(client *http.Client, endpoint string, ttl time.Duration) *azurePricingProvider {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &azurePricingProvider{client: client, endpoint: endpoint, ttl: ttl, cache: make(map[string]azureRetailCacheEntry)}
}

type azureRetailPricesResponse struct {
	Items []azureRetailPrice `json:"Items"`
}

type azureRetailPrice struct {
	ArmSkuName    string  `json:"armSkuName"`
	ArmRegionName string  `json:"armRegionName"`
	ServiceName   string  `json:"serviceName"`
	ProductName   string  `json:"productName"`
	SkuName       string  `json:"skuName"`
	MeterName     string  `json:"meterName"`
	Type          string  `json:"type"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
	CurrencyCode  string  `json:"currencyCode"`
	RetailPrice   float64 `json:"retailPrice"`
}

func (p *azurePricingProvider) lookupRetailPrice(ctx context.Context, request PriceRequest) (PriceResult, error) {
	if strings.EqualFold(request.CapacityType, "spot") {
		return PriceResult{}, fmt.Errorf("instance type %q is not in the embedded Azure catalog; Azure Retail Prices API fallback does not provide Spot estimates", request.InstanceType)
	}
	if request.Region == "" {
		return PriceResult{}, fmt.Errorf("instance type %q is not in the embedded Azure catalog; Azure Retail Prices API fallback requires a region", request.InstanceType)
	}

	key := strings.ToLower(request.InstanceType) + "\x00" + strings.ToLower(request.Region)
	now := time.Now()
	p.mu.Lock()
	if cached, ok := p.cache[key]; ok && now.Before(cached.expiresAt) {
		p.mu.Unlock()
		return cached.result, nil
	}
	p.mu.Unlock()

	filter := fmt.Sprintf("armSkuName eq '%s' and armRegionName eq '%s' and serviceName eq 'Virtual Machines' and priceType eq 'Consumption'",
		strings.ReplaceAll(request.InstanceType, "'", "''"), strings.ReplaceAll(request.Region, "'", "''"))
	query := url.Values{"$filter": []string{filter}, "currencyCode": []string{"USD"}}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return PriceResult{}, fmt.Errorf("Azure Retail Prices API request for %q in %q: %w", request.InstanceType, request.Region, err)
	}
	response, err := p.client.Do(httpRequest)
	if err != nil {
		return PriceResult{}, fmt.Errorf("Azure Retail Prices API lookup for %q in %q: %w", request.InstanceType, request.Region, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return PriceResult{}, fmt.Errorf("Azure Retail Prices API lookup for %q in %q returned HTTP %d", request.InstanceType, request.Region, response.StatusCode)
	}

	var prices azureRetailPricesResponse
	if err := json.NewDecoder(response.Body).Decode(&prices); err != nil {
		return PriceResult{}, fmt.Errorf("decoding Azure Retail Prices API response for %q in %q: %w", request.InstanceType, request.Region, err)
	}
	for _, item := range prices.Items {
		if !validAzureLinuxConsumptionMeter(item, request) {
			continue
		}
		result := PriceResult{HourlyPrice: item.RetailPrice, Currency: item.CurrencyCode, RefreshedAt: now}
		p.mu.Lock()
		p.cache[key] = azureRetailCacheEntry{result: result, expiresAt: now.Add(p.ttl)}
		p.mu.Unlock()
		return result, nil
	}
	return PriceResult{}, fmt.Errorf("instance type %q is not in the embedded Azure catalog and no exact Linux Consumption hourly price was found in region %q", request.InstanceType, request.Region)
}

func validAzureLinuxConsumptionMeter(item azureRetailPrice, request PriceRequest) bool {
	if !strings.EqualFold(item.ArmSkuName, request.InstanceType) ||
		!strings.EqualFold(item.ArmRegionName, request.Region) ||
		!strings.EqualFold(item.ServiceName, "Virtual Machines") ||
		!strings.EqualFold(item.Type, "Consumption") ||
		!strings.EqualFold(item.UnitOfMeasure, "1 Hour") || item.RetailPrice <= 0 {
		return false
	}
	description := strings.ToLower(item.ProductName + " " + item.SkuName + " " + item.MeterName)
	for _, excluded := range []string{"windows", "spot", "low priority", "dev/test"} {
		if strings.Contains(description, excluded) {
			return false
		}
	}
	return request.OS == "" || strings.EqualFold(request.OS, "linux")
}
