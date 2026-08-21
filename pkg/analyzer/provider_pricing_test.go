package analyzer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNode(providerID string, labels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: labels}, Spec: corev1.NodeSpec{ProviderID: providerID}}
}

func TestProviderDetection(t *testing.T) {
	tests := []struct {
		name  string
		nodes []corev1.Node
		want  CloudProvider
	}{
		{"azure", []corev1.Node{testNode("azure:///subscriptions/x", nil)}, CloudProviderAzure},
		{"aws", []corev1.Node{testNode("aws:///us-east-1a/i-1", nil)}, CloudProviderAWS},
		{"unknown", []corev1.Node{testNode("", nil)}, CloudProviderUnknown},
		{"mixed", []corev1.Node{testNode("azure:///x", nil), testNode("aws:///x", nil)}, CloudProviderMixed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectClusterProvider(tt.nodes); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAWSNodeMetadataExtraction(t *testing.T) {
	node := testNode("aws:///us-east-1a/i-1", map[string]string{
		"node.kubernetes.io/instance-type": "m7i.large",
		"topology.kubernetes.io/region":    "us-east-1",
		"topology.kubernetes.io/zone":      "us-east-1a",
		"eks.amazonaws.com/nodegroup":      "workers",
		"eks.amazonaws.com/capacityType":   "SPOT",
	})
	info := (&NodePoolCostAnalyzer{}).extractNodeInfo(node)
	if info.VMSize != "m7i.large" || info.Region != "us-east-1" || info.Zone != "us-east-1a" ||
		info.NodePool != "workers" || info.Priority != "SPOT" || info.Provider != "aws" {
		t.Fatalf("unexpected metadata: %+v", info)
	}
}

type fakeProductsClient struct {
	calls  int
	inputs []*pricing.GetProductsInput
	output *pricing.GetProductsOutput
	err    error
}

func (f *fakeProductsClient) GetProducts(_ context.Context, input *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	f.calls++
	f.inputs = append(f.inputs, input)
	return f.output, f.err
}

func awsProduct(price string) string {
	return `{"terms":{"OnDemand":{"term":{"priceDimensions":{"dim":{"unit":"Hrs","pricePerUnit":{"USD":"` + price + `"}}}}}}}`
}

func TestAWSPriceParsingAndCache(t *testing.T) {
	client := &fakeProductsClient{output: &pricing.GetProductsOutput{PriceList: []string{awsProduct("0.1234")}}}
	provider := NewAWSPricingProvider(client, 24*time.Hour)
	request := PriceRequest{InstanceType: "m7i.large", Region: "us-east-1", CapacityType: "ON_DEMAND"}
	for i := 0; i < 2; i++ {
		got, err := provider.LookupOnDemandPrice(context.Background(), request)
		if err != nil || got.HourlyPrice != 0.1234 || got.Currency != "USD" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	}
	if client.calls != 1 {
		t.Fatalf("GetProducts calls = %d, want 1", client.calls)
	}
}

func TestAWSFailureAndSpotUnavailable(t *testing.T) {
	provider := NewAWSPricingProvider(&fakeProductsClient{err: errors.New("AccessDenied")}, time.Hour)
	if _, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "m7i.large", Region: "us-east-1"}); err == nil {
		t.Fatal("expected API failure")
	}
	if _, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "m7i.large", Region: "us-east-1", CapacityType: "SPOT"}); err == nil {
		t.Fatal("expected Spot pricing to be unavailable")
	}
}

func TestAWSUnknownEC2TypeIsUnpriced(t *testing.T) {
	client := &fakeProductsClient{output: &pricing.GetProductsOutput{}}
	provider := NewAWSPricingProvider(client, time.Hour)
	builder := nodePoolBuilder{
		name: "workers", provider: CloudProviderAWS, vmSize: "opscart-test-unknown-ec2-type",
		region: "us-east-1", os: "linux", priority: "ON_DEMAND",
		nodes: []models.NodeInfo{{CPUCapacity: 4, MemGBCapacity: 16}},
	}
	got := builder.build(&NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{CloudProviderAWS: provider}})
	if got.Provider != string(CloudProviderAWS) {
		t.Fatalf("provider = %q, want AWS", got.Provider)
	}
	if got.PricingAvailable || got.PricePerNodeHour != 0 || got.TotalMonthly != 0 {
		t.Fatalf("unknown EC2 type was priced: %+v", got)
	}
	if !strings.Contains(got.PricingWarning, "returned 0 unambiguous hourly prices") {
		t.Fatalf("unexpected pricing warning %q", got.PricingWarning)
	}
	if client.calls != 1 {
		t.Fatalf("GetProducts calls = %d, want 1", client.calls)
	}
}

func TestAWSOnDemandNodePoolAggregationUsesProviderResult(t *testing.T) {
	const providerPrice = "0.2468"
	request := PriceRequest{InstanceType: "m7i.large", Region: "us-west-2", OS: "linux", CapacityType: "ON_DEMAND"}
	want, err := NewAWSPricingProvider(&fakeProductsClient{output: &pricing.GetProductsOutput{PriceList: []string{awsProduct(providerPrice)}}}, time.Hour).LookupOnDemandPrice(context.Background(), request)
	if err != nil {
		t.Fatalf("getting provider baseline: %v", err)
	}
	provider := NewAWSPricingProvider(&fakeProductsClient{output: &pricing.GetProductsOutput{PriceList: []string{awsProduct(providerPrice)}}}, time.Hour)
	nodes := []models.NodeInfo{{CPUCapacity: 2, MemGBCapacity: 8}, {CPUCapacity: 2, MemGBCapacity: 8}, {CPUCapacity: 2, MemGBCapacity: 8}}
	got := (&nodePoolBuilder{name: "workers", provider: CloudProviderAWS, vmSize: request.InstanceType, region: request.Region, os: request.OS, priority: request.CapacityType, nodes: nodes}).build(
		&NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{CloudProviderAWS: provider}},
	)
	if !got.PricingAvailable || got.PricePerNodeHour != want.HourlyPrice || got.PricePerNodeMonth != want.HourlyPrice*730 || got.TotalMonthly != want.HourlyPrice*730*3 {
		t.Fatalf("unexpected AWS pool aggregation: %+v; provider result %+v", got, want)
	}
	if got.SpotDiscount != 0 || got.RISavings != 0 || got.RISavings3yr != 0 {
		t.Fatalf("AWS pool invented Spot/RI savings: %+v", got)
	}
}

func TestAWSRegionFiltersAndMissingRegion(t *testing.T) {
	for _, region := range []string{"us-east-1", "us-east-2", "us-west-2"} {
		t.Run(region, func(t *testing.T) {
			client := &fakeProductsClient{output: &pricing.GetProductsOutput{PriceList: []string{awsProduct("0.1")}}}
			provider := NewAWSPricingProvider(client, time.Hour)
			_, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "m7i.large", Region: region, OS: "linux", CapacityType: "ON_DEMAND"})
			if err != nil {
				t.Fatalf("lookup failed: %v", err)
			}
			if got := awsFilterValue(client.inputs[0].Filters, "regionCode"); got != region {
				t.Fatalf("regionCode filter = %q, want %q", got, region)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		client := &fakeProductsClient{output: &pricing.GetProductsOutput{}}
		provider := NewAWSPricingProvider(client, time.Hour)
		_, err := provider.LookupOnDemandPrice(context.Background(), PriceRequest{InstanceType: "m7i.large", OS: "linux", CapacityType: "ON_DEMAND"})
		if err == nil || !strings.Contains(err.Error(), "instance type and region are required") {
			t.Fatalf("expected missing-region error, got %v", err)
		}
		if client.calls != 0 {
			t.Fatalf("GetProducts calls = %d, want 0", client.calls)
		}
	})
}

func awsFilterValue(filters []types.Filter, field string) string {
	for _, filter := range filters {
		if aws.ToString(filter.Field) == field {
			return aws.ToString(filter.Value)
		}
	}
	return ""
}

func TestAWSDoesNotUseAzureFallback(t *testing.T) {
	builder := nodePoolBuilder{provider: CloudProviderAWS, vmSize: "Standard_D4s_v3", name: "aws", region: "us-east-1", priority: "ON_DEMAND",
		nodes: []models.NodeInfo{{CPUCapacity: 4, MemGBCapacity: 16}}}
	analyzer := &NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{}}
	got := builder.build(analyzer)
	if got.PricingAvailable || got.TotalMonthly != 0 {
		t.Fatalf("AWS node received fallback pricing: %+v", got)
	}
}

func TestMinikubeAutoRemainsUnknownAndUnpriced(t *testing.T) {
	node := testNode("minikube://minikube", map[string]string{"node.kubernetes.io/instance-type": "minikube"})
	if got := DetectNodeProvider(node); got != CloudProviderUnknown {
		t.Fatalf("Minikube detected as %q, want unknown", got)
	}
	builder := nodePoolBuilder{provider: CloudProviderUnknown, vmSize: "Standard_D4s_v3", nodes: []models.NodeInfo{{CPUCapacity: 4, MemGBCapacity: 16}}}
	got := builder.build(&NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{CloudProviderAzure: NewAzurePricingProvider()}})
	if got.PricingAvailable || got.TotalMonthly != 0 {
		t.Fatalf("auto mode used Azure fallback: %+v", got)
	}
}

func TestManualAzureRequiresCompatibleSKU(t *testing.T) {
	analyzer := &NodePoolCostAnalyzer{ctx: context.Background(), providerOverride: CloudProviderAzure, providers: map[CloudProvider]PricingProvider{CloudProviderAzure: NewAzurePricingProvider()}}
	unsupported := (&nodePoolBuilder{provider: CloudProviderAzure, vmSize: "minikube", nodes: []models.NodeInfo{{CPUCapacity: 4, MemGBCapacity: 16}}}).build(analyzer)
	if unsupported.PricingAvailable {
		t.Fatalf("unsupported Minikube instance type was priced: %+v", unsupported)
	}
	compatible := (&nodePoolBuilder{provider: CloudProviderAzure, vmSize: "Standard_D4s_v3", priority: "Regular", nodes: []models.NodeInfo{{CPUCapacity: 4, MemGBCapacity: 16}}}).build(analyzer)
	if !compatible.PricingAvailable || compatible.TotalMonthly <= 0 {
		t.Fatalf("explicit compatible Azure SKU was not priced: %+v", compatible)
	}
}

func TestManualAWSStillRequiresPricingProvider(t *testing.T) {
	builder := nodePoolBuilder{provider: CloudProviderAWS, vmSize: "m7i.large", region: "us-east-1", nodes: []models.NodeInfo{{CPUCapacity: 2, MemGBCapacity: 8}}}
	got := builder.build(&NodePoolCostAnalyzer{ctx: context.Background(), providers: map[CloudProvider]PricingProvider{CloudProviderAzure: NewAzurePricingProvider()}})
	if got.PricingAvailable {
		t.Fatalf("manual AWS bypassed aws-api opt-in: %+v", got)
	}
}

func TestInvalidCloudProviderOverride(t *testing.T) {
	if _, err := ParseCloudProviderOverride("gcp"); err == nil {
		t.Fatal("expected invalid cloud-provider value to fail")
	}
}
