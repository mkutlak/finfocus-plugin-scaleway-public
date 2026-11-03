package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rshade/finfocus-spec/sdk/go/pluginsdk"
	pbc "github.com/rshade/finfocus-spec/sdk/go/proto/finfocus/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fixturePageSize splits testdata/products.json (recorded from the live
// catalog) into several pages so pagination is exercised.
const fixturePageSize = 7

type serverOptions struct {
	failFirst int32         // the first n requests return HTTP 503
	omitTotal bool          // leave total_count out, as the alpha API might
	delay     time.Duration // wait before answering
}

// catalogServer serves the fixture like the public catalog, honoring
// page and page_size.
func catalogServer(t *testing.T, opts serverOptions) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	raw, err := os.ReadFile("testdata/products.json")
	require.NoError(t, err)
	var products []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &products))

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= opts.failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(opts.delay)
		page, err1 := strconv.Atoi(r.URL.Query().Get("page"))
		size, err2 := strconv.Atoi(r.URL.Query().Get("page_size"))
		if err1 != nil || err2 != nil || page < 1 || size < 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		start := min((page-1)*size, len(products))
		end := min(start+size, len(products))
		body := map[string]any{"products": products[start:end]}
		if !opts.omitTotal {
			body["total_count"] = len(products)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func fixtureCatalog(srv *httptest.Server) *catalog {
	cat := newCatalog(srv.URL, srv.Client())
	cat.pageSize = fixturePageSize
	return cat
}

func testCalculator(t *testing.T) *Calculator {
	t.Helper()
	srv, _ := catalogServer(t, serverOptions{})
	return newCalculator(fixtureCatalog(srv))
}

// descriptor builds a request the way finfocus core does: the SKU is the
// type or nodeType, the region is the zone, else the region.
func descriptor(resourceType string, tags map[string]string) *pbc.ResourceDescriptor {
	sku := tags["type"]
	if sku == "" {
		sku = tags["nodeType"]
	}
	if sku == "" {
		sku = "volume"
	}
	region := tags["zone"]
	if region == "" {
		region = tags["region"]
	}
	return &pbc.ResourceDescriptor{
		Provider: "scaleway", ResourceType: resourceType, Sku: sku, Region: region, Tags: tags,
	}
}

func TestCalculatorName(t *testing.T) {
	t.Parallel()
	pluginsdk.NewTestPlugin(t, NewCalculator()).TestName(pluginName)
}

func TestGetPluginInfo(t *testing.T) {
	t.Parallel()
	resp, err := NewCalculator().GetPluginInfo(context.Background(), &pbc.GetPluginInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, pluginName, resp.GetName())
	assert.Equal(t, PluginVersion, resp.GetVersion())
	assert.Equal(t, SpecVersion, resp.GetSpecVersion())
	assert.Equal(t, []string{"scaleway"}, resp.GetProviders())
	assert.Equal(t, []pbc.PluginCapability{pbc.PluginCapability_PLUGIN_CAPABILITY_PROJECTED_COSTS}, resp.GetCapabilities())
}

func TestGetProjectedCost(t *testing.T) {
	t.Parallel()
	calc := testCalculator(t)

	// wantHourly comes from the fixture's retail prices; monthly is 730 hours.
	tests := []struct {
		name         string
		resourceType string
		tags         map[string]string
		wantHourly   float64
	}{
		{"server", typeServer, map[string]string{"type": "DEV1-S", "zone": "fr-par-1"}, 0.008976},
		{"GPU server priced per minute", typeServer, map[string]string{"type": "L4-1-24G", "zone": "fr-par-2"}, 0.013125 * 60},
		{"pool sized by size in another zone", typePool,
			map[string]string{"nodeType": "PRO2-S", "size": "3", "zone": "nl-ams-1", "region": "nl-ams"}, 0.22338 * 3},
		{"pool with only a region uses its first zone", typePool,
			map[string]string{"nodeType": "PRO2-S", "size": "1", "region": "fr-par"}, 0.22338},
		{"pool nodeType in snake case", typePool,
			map[string]string{"nodeType": "pro2_s", "size": "2", "zone": "fr-par-1"}, 0.22338 * 2},
		{"empty pool is free", typePool, map[string]string{"nodeType": "PRO2-S", "size": "0", "zone": "fr-par-1"}, 0},
		{"mutualized cluster is free", typeCluster, map[string]string{"type": "kapsule", "region": "fr-par"}, 0},
		{"dedicated cluster", typeCluster, map[string]string{"type": "kapsule-dedicated-4", "region": "fr-par"}, 0.11},
		{"kosmos cluster", typeCluster, map[string]string{"type": "multicloud", "region": "fr-par"}, 0.1444},
		{"5k volume per GB", typeVolume,
			map[string]string{"iops": "5000", "sizeInGb": "50", "zone": "fr-par-1"}, 0.000129999 * 50},
		{"15k volume per GB", typeVolume,
			map[string]string{"iops": "15000", "sizeInGb": "100", "zone": "fr-par-1"}, 0.000177 * 100},
		{"load balancer", typeLoadBalancer, map[string]string{"type": "LB-S", "zone": "fr-par-1"}, 0.023},
		{"load balancer type is case-insensitive", typeLoadBalancer,
			map[string]string{"type": "lb-gp-m", "zone": "fr-par-1"}, 0.054},
		{"database node plus management", typeDatabase,
			map[string]string{"nodeType": "DB-DEV-S", "region": "fr-par"}, 0.0136 + 0.002},
		{"HA database doubles the node, not the management fee", typeDatabase,
			map[string]string{"nodeType": "db-pro2-xxs", "isHaCluster": "true", "region": "fr-par"}, 0.0583*2 + 0.0517},
		{"standalone redis", typeRedis, map[string]string{"nodeType": "RED1-XS", "zone": "fr-par-1"}, 0.137},
		{"HA redis adds one node", typeRedis,
			map[string]string{"nodeType": "RED1-XS", "clusterSize": "2", "zone": "fr-par-1"}, 0.137 + 0.082},
		{"redis cluster adds nodes", typeRedis,
			map[string]string{"nodeType": "RED1-XS", "clusterSize": "3", "zone": "fr-par-1"}, 0.137 + 0.082*2},
		{"public gateway", typePublicGateway, map[string]string{"type": "VPC-GW-S", "zone": "fr-par-1"}, 0.026},
		{"routed IPv4", typeIP, map[string]string{"type": "routed_ipv4", "zone": "fr-par-1"}, 0.005},
		{"routed IPv6 is free", typeIP, map[string]string{"type": "routed_ipv6", "zone": "fr-par-1"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := descriptor(tt.resourceType, tt.tags)

			supports, err := calc.Supports(context.Background(), &pbc.SupportsRequest{Resource: r})
			require.NoError(t, err)
			require.True(t, supports.GetSupported(), supports.GetReason())

			resp, err := calc.GetProjectedCost(context.Background(), &pbc.GetProjectedCostRequest{Resource: r})
			require.NoError(t, err)
			assert.Equal(t, "EUR", resp.GetCurrency())
			assert.InDelta(t, tt.wantHourly, resp.GetUnitPrice(), 1e-9)
			assert.InDelta(t, tt.wantHourly*730, resp.GetCostPerMonth(), 1e-6)
			assert.NotEmpty(t, resp.GetBillingDetail())
			assert.WithinDuration(t, time.Now().Add(priceTTL), resp.GetExpiresAt().AsTime(), time.Minute)
		})
	}
}

func TestSupportsDeclines(t *testing.T) {
	t.Parallel()
	calc := testCalculator(t)

	tests := []struct {
		name string
		r    *pbc.ResourceDescriptor
	}{
		{"zone with no price", descriptor(typeServer, map[string]string{"type": "DEV1-S", "zone": "pl-waw-3"})},
		{"commercial type with no price", descriptor(typeServer, map[string]string{"type": "NOPE-1", "zone": "fr-par-1"})},
		{"unpriced resource type", descriptor("scaleway:vpc/privateNetwork:PrivateNetwork",
			map[string]string{"type": "x", "region": "fr-par"})},
		{"legacy token", descriptor("scaleway:index/instanceServer:InstanceServer",
			map[string]string{"type": "DEV1-S", "zone": "fr-par-1"})},
		{"volume without iops", descriptor(typeVolume, map[string]string{"sizeInGb": "50", "zone": "fr-par-1"})},
		{"volume without size", descriptor(typeVolume, map[string]string{"iops": "5000", "zone": "fr-par-1"})},
		{"pool without size", descriptor(typePool, map[string]string{"nodeType": "PRO2-S", "zone": "fr-par-1"})},
		{"pool size NaN", descriptor(typePool, map[string]string{"nodeType": "PRO2-S", "size": "NaN", "zone": "fr-par-1"})},
		{"pool size fraction", descriptor(typePool, map[string]string{"nodeType": "PRO2-S", "size": "1.5", "zone": "fr-par-1"})},
		{"negative redis size", descriptor(typeRedis,
			map[string]string{"nodeType": "RED1-XS", "clusterSize": "-1", "zone": "fr-par-1"})},
		{"database without management price", descriptor(typeDatabase,
			map[string]string{"nodeType": "DB-DEV-S", "region": "nl-ams"})},
		{"unknown cluster type", descriptor(typeCluster, map[string]string{"type": "k3s", "region": "fr-par"})},
		{"snapshot is not a server", descriptor(typeServer,
			map[string]string{"type": "Instance Local SSD Snapshot", "zone": "fr-par-1"})},
		{"other provider", &pbc.ResourceDescriptor{
			Provider: "aws", ResourceType: "aws:ec2/instance:Instance", Sku: "t3.micro", Region: "us-east-1",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := calc.Supports(context.Background(), &pbc.SupportsRequest{Resource: tt.r})
			require.NoError(t, err)
			assert.False(t, resp.GetSupported())
			assert.NotEmpty(t, resp.GetReason())
		})
	}
}

func TestGetProjectedCostRejectsUnpriced(t *testing.T) {
	t.Parallel()
	calc := testCalculator(t)

	r := descriptor(typeServer, map[string]string{"type": "NOPE-1", "zone": "fr-par-1"})
	_, err := calc.GetProjectedCost(context.Background(), &pbc.GetProjectedCostRequest{Resource: r})
	assert.Equal(t, codes.NotFound, status.Code(err))

	_, err = calc.GetProjectedCost(context.Background(), &pbc.GetProjectedCostRequest{
		Resource: &pbc.ResourceDescriptor{Provider: "scaleway", ResourceType: typeServer},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestHourlyPriceRejectsOtherUnits(t *testing.T) {
	t.Parallel()
	var p product
	require.NoError(t, json.Unmarshal([]byte(`{
		"sku": "/dedibox/x/monthly/fr-par",
		"price": {"retail_price": {"currency_code": "EUR", "units": 15, "nanos": 0}},
		"unit_of_measure": {"unit": "month", "size": 1}
	}`), &p))
	_, err := p.hourlyPrice(perInstance)
	require.Error(t, err)
}

func TestCatalogFailureIsUnavailableThenRetried(t *testing.T) {
	t.Parallel()
	srv, calls := catalogServer(t, serverOptions{failFirst: 1})
	cat := fixtureCatalog(srv)
	cat.retryAfter = 0
	calc := newCalculator(cat)
	r := descriptor(typeServer, map[string]string{"type": "DEV1-S", "zone": "fr-par-1"})

	_, err := calc.Supports(context.Background(), &pbc.SupportsRequest{Resource: r})
	assert.Equal(t, codes.Unavailable, status.Code(err))

	resp, err := calc.GetProjectedCost(context.Background(), &pbc.GetProjectedCostRequest{Resource: r})
	require.NoError(t, err)
	assert.InDelta(t, 0.008976, resp.GetUnitPrice(), 1e-9)

	loaded := calls.Load()
	_, err = calc.GetProjectedCost(context.Background(), &pbc.GetProjectedCostRequest{Resource: r})
	require.NoError(t, err)
	assert.Equal(t, loaded, calls.Load(), "a loaded catalog is not fetched again")
}

func TestCatalogFailureIsRememberedForRetryAfter(t *testing.T) {
	t.Parallel()
	srv, calls := catalogServer(t, serverOptions{failFirst: 1})
	calc := newCalculator(fixtureCatalog(srv))
	r := descriptor(typeServer, map[string]string{"type": "DEV1-S", "zone": "fr-par-1"})

	for range 3 {
		_, err := calc.Supports(context.Background(), &pbc.SupportsRequest{Resource: r})
		assert.Equal(t, codes.Unavailable, status.Code(err))
	}
	assert.Equal(t, int32(1), calls.Load(), "callers within retryAfter fail fast without a new fetch")
}

func TestConcurrentFirstUseFetchesOnce(t *testing.T) {
	t.Parallel()
	srv, calls := catalogServer(t, serverOptions{delay: 20 * time.Millisecond})
	cat := fixtureCatalog(srv)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := cat.load(context.Background())
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	// 22 fixture products in pages of 7: four requests for one load.
	assert.Equal(t, int32(4), calls.Load())
}

func TestWaiterStopsOnItsOwnContext(t *testing.T) {
	t.Parallel()
	srv, _ := catalogServer(t, serverOptions{delay: 200 * time.Millisecond})
	cat := fixtureCatalog(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := cat.load(ctx)
	require.ErrorIs(t, err, errCatalogUnavailable)

	// The load kept running on its own deadline and now serves.
	index, err := cat.load(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, index["fr-par-1"])
}

func TestPaginationWithoutTotalCount(t *testing.T) {
	t.Parallel()
	srv, _ := catalogServer(t, serverOptions{omitTotal: true})
	index, err := fixtureCatalog(srv).load(context.Background())
	require.NoError(t, err)
	// /instance/server/pro2_s/nl-ams-1 is on the last page.
	assert.NotEmpty(t, index["nl-ams-1"])
}

func TestRetiredProductsAreSkipped(t *testing.T) {
	t.Parallel()
	calc := testCalculator(t)
	index, err := calc.catalog.load(context.Background())
	require.NoError(t, err)
	for _, products := range index {
		for _, p := range products {
			assert.NotEqual(t, "retired", p.Status, p.SKU)
		}
	}
}

func TestLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		r          *pbc.ResourceDescriptor
		wantZone   string
		wantRegion string
	}{
		{"zone in descriptor", &pbc.ResourceDescriptor{Region: "nl-ams-2"}, "nl-ams-2", "nl-ams"},
		{"region in descriptor", &pbc.ResourceDescriptor{Region: "pl-waw"}, "pl-waw-1", "pl-waw"},
		{"tags win", &pbc.ResourceDescriptor{Region: "fr-par-1", Tags: map[string]string{
			"zone": "fr-par-2", "region": "fr-par",
		}}, "fr-par-2", "fr-par"},
		{"malformed zone tag does not panic", &pbc.ResourceDescriptor{Tags: map[string]string{"zone": "par1"}},
			"par1", ""},
		{"nothing", &pbc.ResourceDescriptor{}, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			zone, region := location(tt.r)
			assert.Equal(t, tt.wantZone, zone)
			assert.Equal(t, tt.wantRegion, region)
		})
	}
}
