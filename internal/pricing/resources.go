package pricing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	pbc "github.com/rshade/finfocus-spec/sdk/go/proto/finfocus/v1"
)

// Pulumi type tokens of the pulumiverse Scaleway provider that this plugin prices.
const (
	typeServer        = "scaleway:instance/server:Server"
	typePool          = "scaleway:kubernetes/pool:Pool"
	typeCluster       = "scaleway:kubernetes/cluster:Cluster"
	typeVolume        = "scaleway:block/volume:Volume"
	typeLoadBalancer  = "scaleway:loadbalancers/loadBalancer:LoadBalancer"
	typeDatabase      = "scaleway:databases/instance:Instance"
	typeRedis         = "scaleway:redis/cluster:Cluster"
	typePublicGateway = "scaleway:network/publicGateway:PublicGateway"
	typeIP            = "scaleway:instance/ip:Ip"
)

// Catalog units each kind of product is billed in. GPU instances are priced
// per minute; storage is per GB-hour.
var (
	perHour     = []string{"hour"}
	perInstance = []string{"hour", "minute"}
	perNode     = []string{"node"}
	perIP       = []string{"ip"}
	perGBHour   = []string{"gigabyte"}
)

// zonePattern matches a Scaleway zone such as fr-par-1; a region is fr-par.
var zonePattern = regexp.MustCompile(`^[a-z]{2}-[a-z]{3}-[0-9]+$`)

// errInvalidResource marks inputs this plugin cannot price, such as a missing
// size or an unknown type.
var errInvalidResource = errors.New("cannot price resource")

// line is one catalog product, the units it may be billed in, and how many
// of it the resource uses.
type line struct {
	locality  string
	skuPrefix string
	name      string
	units     []string
	quantity  float64
}

// quote is the hourly price of a resource and the catalog entries behind it.
type quote struct {
	hourly   float64
	currency string
	detail   string
}

// quoteResource prices one resource from the catalog.
func quoteResource(ctx context.Context, cat *catalog, r *pbc.ResourceDescriptor) (quote, error) {
	lines, err := linesFor(r)
	if err != nil {
		return quote{}, err
	}
	var q quote
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		p, err := cat.find(ctx, l)
		if err != nil {
			if errors.Is(err, errCatalogUnavailable) {
				return quote{}, err
			}
			return quote{}, fmt.Errorf("%w: %w", errInvalidResource, err)
		}
		if q.currency != "" && p.currency != q.currency {
			return quote{}, fmt.Errorf("%w: mixed currencies %s and %s", errInvalidResource, q.currency, p.currency)
		}
		q.currency = p.currency
		q.hourly += p.hourly * l.quantity
		parts = append(parts, fmt.Sprintf("%s x %g at %g %s/h", p.sku, l.quantity, p.hourly, p.currency))
	}
	q.detail = strings.Join(parts, "; ")
	return q, nil
}

// linesFor maps a resource to the catalog products it is billed for.
func linesFor(r *pbc.ResourceDescriptor) ([]line, error) {
	tags := r.GetTags()
	zone, region := location(r)
	switch r.GetResourceType() {
	case typeServer:
		return single(zone, "/instance/server/", tags["type"], perInstance, 1)
	case typePool:
		size, err := count(tags, "size")
		if err != nil {
			return nil, err
		}
		return single(zone, "/instance/server/", tags["nodeType"], perInstance, size)
	case typeCluster:
		name, err := controlPlaneName(tags["type"])
		if err != nil {
			return nil, err
		}
		return single(region, "/k8s/", name, perHour, 1)
	case typeVolume:
		return volumeLines(zone, tags)
	case typeLoadBalancer:
		return single(zone, "/network/loadbalancer/", tags["type"], perNode, 1)
	case typeDatabase:
		return databaseLines(zone, region, tags)
	case typeRedis:
		return redisLines(zone, tags)
	case typePublicGateway:
		size, ok := strings.CutPrefix(strings.ToUpper(tags["type"]), "VPC-GW-")
		if !ok || size == "" {
			return nil, fmt.Errorf("%w: public gateway type %q", errInvalidResource, tags["type"])
		}
		return single(zone, "/network/vpc/public-gateway/", "VPC Public Gateway "+size, perHour, 1)
	case typeIP:
		return ipLines(zone, tags["type"])
	default:
		return nil, fmt.Errorf("%w: resource type %s is not supported", errInvalidResource, r.GetResourceType())
	}
}

func single(locality, skuPrefix, name string, units []string, quantity float64) ([]line, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: no commercial type", errInvalidResource)
	}
	if locality == "" {
		return nil, fmt.Errorf("%w: no zone or region", errInvalidResource)
	}
	return []line{{locality: locality, skuPrefix: skuPrefix, name: name, units: units, quantity: quantity}}, nil
}

// controlPlaneName maps a cluster type to its catalog product name.
func controlPlaneName(clusterType string) (string, error) {
	switch {
	case clusterType == "kapsule":
		return "Kapsule Mutualized", nil
	case clusterType == "multicloud":
		return "Kosmos Mutualized", nil
	case strings.HasPrefix(clusterType, "kapsule-dedicated-"):
		return "Kapsule Dedicated-" + strings.TrimPrefix(clusterType, "kapsule-dedicated-"), nil
	case strings.HasPrefix(clusterType, "multicloud-dedicated-"):
		return "Kosmos Dedicated-" + strings.TrimPrefix(clusterType, "multicloud-dedicated-"), nil
	default:
		return "", fmt.Errorf("%w: cluster type %q", errInvalidResource, clusterType)
	}
}

// volumeLines prices a block volume per GB at the tier its iops selects.
func volumeLines(zone string, tags map[string]string) ([]line, error) {
	var tier string
	switch tags["iops"] {
	case "5000":
		tier = "Volume Low Latency 5K"
	case "15000":
		tier = "Volume Low Latency 15K"
	default:
		return nil, fmt.Errorf("%w: block volume iops %q", errInvalidResource, tags["iops"])
	}
	size, err := count(tags, "sizeInGb")
	if err != nil {
		return nil, err
	}
	return single(zone, "/storage/block/", tier, perGBHour, size)
}

// databaseLines prices a managed database as its node (two with high
// availability) plus one regional management fee. The data volume is billed
// separately and not included.
func databaseLines(zone, region string, tags map[string]string) ([]line, error) {
	nodeType := tags["nodeType"]
	if nodeType == "" {
		return nil, fmt.Errorf("%w: no commercial type", errInvalidResource)
	}
	nodes := 1.0
	if tags["isHaCluster"] == "true" {
		nodes = 2
	}
	lines, err := single(zone, "/storage/rdb/node/", "Database Node "+nodeType, perHour, nodes)
	if err != nil {
		return nil, err
	}
	management, err := single(region, "/storage/rdb/instance/", "Database Management "+nodeType, perHour, 1)
	if err != nil {
		return nil, err
	}
	return append(lines, management...), nil
}

// redisLines prices one main node plus clusterSize-1 additional nodes.
// clusterSize defaults to 1, the provider's standalone mode.
func redisLines(zone string, tags map[string]string) ([]line, error) {
	size := 1.0
	if tags["clusterSize"] != "" {
		var err error
		if size, err = count(tags, "clusterSize"); err != nil {
			return nil, err
		}
	}
	lines, err := single(zone, "/storage/redis/main-node/", tags["nodeType"], perNode, 1)
	if err != nil || size <= 1 {
		return lines, err
	}
	return append(lines, line{
		locality: zone, skuPrefix: "/storage/redis/additional-node/", name: tags["nodeType"],
		units: perNode, quantity: size - 1,
	}), nil
}

// ipLines prices a flexible IP. routed_ipv4 and nat are both IPv4.
func ipLines(zone, ipType string) ([]line, error) {
	switch strings.ReplaceAll(strings.ToLower(ipType), "_", "") {
	case "routedipv4", "nat":
		return single(zone, "/network/ipam/ipv4/", "Zonal Flexible IPv4", perIP, 1)
	case "routedipv6":
		return single(zone, "/network/ipam/ipv6/", "Zonal Flexible IPv6", perIP, 1)
	default:
		return nil, fmt.Errorf("%w: IP type %q", errInvalidResource, ipType)
	}
}

// count parses a whole, non-negative quantity input such as a node count or a
// size in GB.
func count(tags map[string]string, key string) (float64, error) {
	value, err := strconv.ParseUint(tags[key], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q is not a whole number", errInvalidResource, key, tags[key])
	}
	return float64(value), nil
}

// location returns the resource's zone and region. finfocus puts the zone, or
// the region when there is no zone, in the descriptor's region field; the
// zone and region inputs, when present, win. A regional resource falls back to
// the region's first zone, where Scaleway lists zonal prices.
func location(r *pbc.ResourceDescriptor) (string, string) {
	tags := r.GetTags()
	zone, region := tags["zone"], tags["region"]
	if zonePattern.MatchString(r.GetRegion()) {
		if zone == "" {
			zone = r.GetRegion()
		}
	} else if region == "" {
		region = r.GetRegion()
	}
	if region == "" && zonePattern.MatchString(zone) {
		region = zone[:strings.LastIndex(zone, "-")]
	}
	if zone == "" && region != "" {
		zone = region + "-1"
	}
	return zone, region
}
