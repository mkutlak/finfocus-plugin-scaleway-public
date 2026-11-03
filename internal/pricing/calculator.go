// Package pricing prices Scaleway resources from Scaleway's public product
// catalog for FinFocus projected costs.
package pricing

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rshade/finfocus-spec/sdk/go/pluginsdk"
	pbc "github.com/rshade/finfocus-spec/sdk/go/proto/finfocus/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	pluginName = "scaleway-public"
	provider   = "scaleway"
	// priceTTL tells finfocus how long to cache a price. The catalog changes
	// rarely, and core caps the TTL at 7 days.
	priceTTL = 24 * time.Hour
)

// PluginVersion is the plugin's semantic version. Release builds set it with
// -ldflags "-X .../internal/pricing.PluginVersion=<version>".
var PluginVersion = "0.1.0"

// SpecVersion is the finfocus-spec protocol version this plugin was compiled
// against. It follows the finfocus-spec version in go.mod.
const SpecVersion = pluginsdk.SpecVersion

// Calculator implements the FinFocus plugin for Scaleway public prices.
type Calculator struct {
	*pluginsdk.BasePlugin

	catalog *catalog
}

// NewCalculator returns a plugin that reads Scaleway's public catalog.
func NewCalculator() *Calculator {
	return newCalculator(newCatalog(defaultCatalogURL, &http.Client{Timeout: pageTimeout}))
}

func newCalculator(cat *catalog) *Calculator {
	base := pluginsdk.NewBasePlugin(pluginName)
	base.Matcher().AddProvider(provider)
	return &Calculator{BasePlugin: base, catalog: cat}
}

// GetPluginInfo returns metadata for discovery and compatibility checks.
func (c *Calculator) GetPluginInfo(_ context.Context, _ *pbc.GetPluginInfoRequest) (*pbc.GetPluginInfoResponse, error) {
	return &pbc.GetPluginInfoResponse{
		Name:         pluginName,
		Version:      PluginVersion,
		SpecVersion:  SpecVersion,
		Providers:    []string{provider},
		Capabilities: []pbc.PluginCapability{pbc.PluginCapability_PLUGIN_CAPABILITY_PROJECTED_COSTS},
		Metadata: map[string]string{
			"description": "Scaleway projected costs from the public product catalog",
			"author":      "Martin Kutlak",
		},
	}, nil
}

// Supports reports whether the catalog has a price for the resource. A
// catalog that cannot be read is Unavailable, which finfocus treats as
// fail-open and does not cache.
func (c *Calculator) Supports(ctx context.Context, req *pbc.SupportsRequest) (*pbc.SupportsResponse, error) {
	resource := req.GetResource()
	if resource == nil {
		return &pbc.SupportsResponse{Supported: false, Reason: "resource descriptor is required"}, nil
	}
	if !c.Matcher().Supports(resource) {
		return &pbc.SupportsResponse{Supported: false, Reason: "provider " + resource.GetProvider() + " is not supported"}, nil
	}
	if _, err := quoteResource(ctx, c.catalog, resource); err != nil {
		if errors.Is(err, errCatalogUnavailable) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return &pbc.SupportsResponse{Supported: false, Reason: err.Error()}, nil
	}
	return &pbc.SupportsResponse{
		Supported:    true,
		Capabilities: map[string]bool{"projected_costs": true},
	}, nil
}

// GetProjectedCost returns the monthly cost (730 hours) from catalog prices.
func (c *Calculator) GetProjectedCost(
	ctx context.Context,
	req *pbc.GetProjectedCostRequest,
) (*pbc.GetProjectedCostResponse, error) {
	if err := pluginsdk.ValidateProjectedCostRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	resource := req.GetResource()
	if !c.Matcher().Supports(resource) {
		return nil, status.Error(codes.InvalidArgument, pluginsdk.NotSupportedError(resource).Error())
	}
	q, err := quoteResource(ctx, c.catalog, resource)
	if err != nil {
		if errors.Is(err, errCatalogUnavailable) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.NotFound, err.Error())
	}
	resp := c.Calculator().CreateProjectedCostResponse(q.currency, q.hourly, q.detail)
	resp.ExpiresAt = timestamppb.New(time.Now().Add(priceTTL))
	return resp, nil
}
