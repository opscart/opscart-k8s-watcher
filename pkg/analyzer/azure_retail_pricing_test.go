package analyzer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func azureRetailClient(t *testing.T, status int, body string, calls *int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		filter := r.URL.Query().Get("$filter")
		for _, required := range []string{"armSkuName eq 'Standard_E48as_v5'", "armRegionName eq 'centralus'", "serviceName eq 'Virtual Machines'", "priceType eq 'Consumption'"} {
			if !strings.Contains(filter, required) {
				t.Errorf("filter %q does not contain %q", filter, required)
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
}

const retailPriceItems = `{"Items":[
 {"armSkuName":"Standard_E48as_v5","armRegionName":"centralus","serviceName":"Virtual Machines","productName":"Virtual Machines Easv5 Series Windows","skuName":"E48as v5","meterName":"E48as v5","type":"Consumption","unitOfMeasure":"1 Hour","currencyCode":"USD","retailPrice":9.99},
 {"armSkuName":"Standard_E48as_v5","armRegionName":"centralus","serviceName":"Virtual Machines","productName":"Virtual Machines Easv5 Series","skuName":"E48as v5 Spot","meterName":"E48as v5 Spot","type":"Consumption","unitOfMeasure":"1 Hour","currencyCode":"USD","retailPrice":0.25},
 {"armSkuName":"Standard_E48as_v5","armRegionName":"centralus","serviceName":"Virtual Machines","productName":"Virtual Machines Easv5 Series","skuName":"E48as v5","meterName":"E48as v5","type":"Consumption","unitOfMeasure":"1 Hour","currencyCode":"USD","retailPrice":2.75}
]}`

func TestAzureEmbeddedPriceDoesNotCallRetailAPI(t *testing.T) {
	calls := 0
	client := azureRetailClient(t, http.StatusInternalServerError, `{}`, &calls)
	provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)
	got, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "Standard_D4s_v5", Region: "centralus", OS: "linux", CapacityType: "Regular"})
	if err != nil || got.HourlyPrice != AzurePricingCatalog["standard_d4s_v5"].PayAsYouGoHour {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if calls != 0 {
		t.Fatalf("Retail API calls = %d, want 0", calls)
	}
}

func TestAzureRetailPriceFilteringAndCache(t *testing.T) {
	calls := 0
	client := azureRetailClient(t, http.StatusOK, retailPriceItems, &calls)
	provider := newAzurePricingProvider(client, azureRetailPricesURL, 24*time.Hour)
	request := PriceRequest{InstanceType: "Standard_E48as_v5", Region: "centralus", OS: "linux", CapacityType: "Regular"}
	for i := 0; i < 2; i++ {
		got, err := provider.LookupOnDemandPrice(context.Background(), request)
		if err != nil || got.HourlyPrice != 2.75 || got.Currency != "USD" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("Retail API calls = %d, want 1", calls)
	}
}

func TestAzureRetailFailures(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		calls := 0
		client := azureRetailClient(t, http.StatusInternalServerError, `{}`, &calls)
		provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)
		_, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "Standard_E48as_v5", Region: "centralus", OS: "linux"})
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("expected useful HTTP error, got %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		calls := 0
		client := azureRetailClient(t, http.StatusOK, `{"Items":[]}`, &calls)
		provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)
		_, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "Standard_E48as_v5", Region: "centralus", OS: "linux"})
		if err == nil || !strings.Contains(err.Error(), "no exact Linux Consumption hourly price") {
			t.Fatalf("expected useful empty-result error, got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("request timeout")
		})}
		provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)
		_, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "Standard_E48as_v5", Region: "centralus", OS: "linux"})
		if err == nil || !strings.Contains(err.Error(), "Azure Retail Prices API lookup") {
			t.Fatalf("expected useful timeout error, got %v", err)
		}
	})
}

func TestAzureRetailPriceUsedByNodePool(t *testing.T) {
	calls := 0
	client := azureRetailClient(t, http.StatusOK, retailPriceItems, &calls)
	provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)
	nodes := make([]models.NodeInfo, 5)
	for i := range nodes {
		nodes[i] = models.NodeInfo{CPUCapacity: 48, MemGBCapacity: 384}
	}
	analyzer := &NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{CloudProviderAzure: provider}}
	got := (&nodePoolBuilder{name: "userpoolnew", provider: CloudProviderAzure, vmSize: "Standard_E48as_v5", region: "centralus", os: "linux", priority: "Regular", nodes: nodes}).build(analyzer)
	if !got.PricingAvailable || got.PricePerNodeHour != 2.75 || got.PricePerNodeMonth != 2.75*730 || got.TotalMonthly != 2.75*730*5 {
		t.Fatalf("unexpected pool pricing: %+v", got)
	}
	if got.RISavings != 0 || got.RISavings3yr != 0 || got.SpotDiscount != 0 {
		t.Fatalf("API-only SKU invented savings: %+v", got)
	}
}
