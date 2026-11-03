# finfocus-plugin-scaleway-public

A [FinFocus](https://github.com/rshade/finfocus) plugin that prices Scaleway
resources in Pulumi programs for `finfocus cost projected`, `cost estimate`,
`overview`, and `finfocus --web`. Prices come from Scaleway's public product
catalog, so the plugin needs no Scaleway credentials.

## Install

```bash
finfocus plugin install github.com/mkutlak/finfocus-plugin-scaleway-public
```

Then price a preview:

```bash
pulumi preview --json > plan.json
finfocus cost projected --pulumi-json plan.json
```

Pool, database, Redis, and block volume rows need a FinFocus build that
resolves Scaleway's `nodeType` and block volume SKUs (feature
`1925-scaleway-sku-gitlab-docs`). Older builds reject those rows in
pre-flight validation and price only the other types.

## Priced resources

Costs are monthly: the catalog's hourly retail price × 730 hours × quantity,
in EUR. GPU instances, which the catalog prices per minute, count 60 minutes
per hour.

| Pulumi type | Catalog product | Quantity |
| --- | --- | --- |
| `scaleway:instance/server:Server` | Instance `type` in the zone | 1 |
| `scaleway:kubernetes/pool:Pool` | Instance `nodeType` in the zone (`pro2_s` and `PRO2-S` both match) | `size` |
| `scaleway:kubernetes/cluster:Cluster` | Control plane for `type` in the region (`kapsule` is free) | 1 |
| `scaleway:block/volume:Volume` | Low Latency 5K or 15K, picked by `iops`, per GB | `sizeInGb` |
| `scaleway:loadbalancers/loadBalancer:LoadBalancer` | Load Balancer `type` in the zone | 1 |
| `scaleway:databases/instance:Instance` | Database node `nodeType`, plus one regional management fee | Nodes: 2 with `isHaCluster`, else 1 |
| `scaleway:redis/cluster:Cluster` | Main node plus additional nodes | `clusterSize` (default 1) |
| `scaleway:network/publicGateway:PublicGateway` | Public Gateway `type` in the zone | 1 |
| `scaleway:instance/ip:Ip` | Zonal flexible IPv4 or IPv6 by `type` | 1 |

The zone comes from the resource's `zone`, then from the location FinFocus
resolved (which includes `SCW_DEFAULT_ZONE` and `SCW_DEFAULT_REGION`). A
regional resource uses the first zone of its region, where Scaleway lists
zonal prices.

A resource the catalog has no price for is declined, never given a guessed
price.

### Not included

- Database volumes (`volumeType`, `volumeSizeInGb`). The row covers nodes and
  the management fee only.
- A server's inline root volume and a pool's node root volumes; only separate
  `block/volume` resources are priced.
- The IP a load balancer or public gateway creates for itself when you give
  it none, and `network/publicGatewayIp` resources.
- A cluster or flexible IP with no `type` input. FinFocus core needs a SKU
  before it calls the plugin, and these inputs are optional.
- Usage-based services (object storage, serverless, bandwidth), actual costs,
  and the legacy `scaleway:index/*` tokens.

## How it works

On the first request the plugin reads every page of
`https://api.scaleway.com/product-catalog/v2alpha1/public-catalog/products`
in the background, shares that one load between concurrent requests, and
reloads it after 24 hours while the old copy keeps serving. A request waits
only as long as its own deadline. A failed read is reported as `Unavailable`
for 30 seconds, so a blocked network fails fast, and is then retried. Each price is
returned with a 24-hour expiry, so FinFocus caches it for a day. The catalog
API is `v2alpha1`; if Scaleway changes it, prices stop until the plugin is
updated.

## Development

Tool versions are pinned in `mise.toml` (`mise install`).

```bash
make test     # unit tests with -race, against a recorded catalog fixture
make lint
make build    # bin/finfocus-plugin-scaleway-public
make install  # copies the build into ~/.finfocus/plugins/scaleway-public/<version>/
```

`internal/pricing/testdata/products.json` holds real catalog entries. Add one
when you add a priced type.

## Release

Push a signed tag: `git tag -s v0.1.0 -m v0.1.0 && git push origin v0.1.0`.
The Release workflow runs goreleaser, which uploads the archives and
`checksums.txt` that `finfocus plugin install` expects, and attests each
archive's build provenance. Verify a download with:

```bash
gh attestation verify <archive> -R mkutlak/finfocus-plugin-scaleway-public
```

## License

Apache-2.0. See [LICENSE](LICENSE).
