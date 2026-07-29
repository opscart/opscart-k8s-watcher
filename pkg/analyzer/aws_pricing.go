package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

type AWSGetProductsAPI interface {
	GetProducts(context.Context, *pricing.GetProductsInput, ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

type awsCacheEntry struct {
	result  PriceResult
	expires time.Time
}

type AWSPricingProvider struct {
	client AWSGetProductsAPI
	ttl    time.Duration
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]awsCacheEntry
}

func NewAWSPricingProvider(client AWSGetProductsAPI, ttl time.Duration) *AWSPricingProvider {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &AWSPricingProvider{client: client, ttl: ttl, now: time.Now, cache: make(map[string]awsCacheEntry)}
}
func (p *AWSPricingProvider) Provider() CloudProvider { return CloudProviderAWS }
func (p *AWSPricingProvider) SourceDescription() string {
	return "AWS Price List Query API public EC2 On-Demand pricing"
}
func (p *AWSPricingProvider) Capabilities() PricingCapabilities {
	return PricingCapabilities{OnDemand: true, CapacityTypes: []string{"ON_DEMAND"}}
}
func (p *AWSPricingProvider) LookupOnDemandPrice(ctx context.Context, r PriceRequest) (PriceResult, error) {
	if strings.EqualFold(r.CapacityType, "spot") {
		return PriceResult{}, fmt.Errorf("EC2 Spot pricing is not integrated")
	}
	if r.OS != "" && !strings.EqualFold(r.OS, "linux") {
		return PriceResult{}, fmt.Errorf("only EC2 Linux worker-node pricing is supported")
	}
	if r.InstanceType == "" || r.Region == "" {
		return PriceResult{}, fmt.Errorf("instance type and region are required")
	}
	key := r.Region + "\x00" + r.InstanceType
	p.mu.Lock()
	if hit, ok := p.cache[key]; ok && p.now().Before(hit.expires) {
		p.mu.Unlock()
		return hit.result, nil
	}
	p.mu.Unlock()
	out, err := p.client.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"), MaxResults: aws.Int32(100),
		Filters: []types.Filter{
			{Type: types.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(r.InstanceType)},
			{Type: types.FilterTypeTermMatch, Field: aws.String("regionCode"), Value: aws.String(r.Region)},
			{Type: types.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: types.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: types.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: types.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
	})
	if err != nil {
		return PriceResult{}, fmt.Errorf("AWS Price List query failed: %w", err)
	}
	result, err := parseAWSProducts(out.PriceList, p.now())
	if err != nil {
		return PriceResult{}, err
	}
	p.mu.Lock()
	p.cache[key] = awsCacheEntry{result: result, expires: p.now().Add(p.ttl)}
	p.mu.Unlock()
	return result, nil
}

func parseAWSProducts(products []string, refreshed time.Time) (PriceResult, error) {
	var prices []float64
	for _, raw := range products {
		var doc struct {
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						Unit         string            `json:"unit"`
						PricePerUnit map[string]string `json:"pricePerUnit"`
					} `json:"priceDimensions"`
				} `json:"OnDemand"`
			} `json:"terms"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return PriceResult{}, fmt.Errorf("parsing AWS Price List response: %w", err)
		}
		for _, term := range doc.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				if dim.Unit != "Hrs" {
					continue
				}
				if s := dim.PricePerUnit["USD"]; s != "" {
					if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
						prices = append(prices, v)
					}
				}
			}
		}
	}
	if len(prices) != 1 {
		return PriceResult{}, fmt.Errorf("AWS Price List returned %d unambiguous hourly prices", len(prices))
	}
	return PriceResult{HourlyPrice: prices[0], Currency: "USD", RefreshedAt: refreshed}, nil
}
