package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultCatalogURL is Scaleway's public product catalog. It needs no
// credentials and lists retail prices per zone or region.
//
// shortcut: live v2alpha1 API, switch to an embedded snapshot if it breaks.
const defaultCatalogURL = "https://api.scaleway.com/product-catalog/v2alpha1/public-catalog/products"

const (
	defaultPageSize = 1000
	// catalogMaxPages bounds the pagination loop; the catalog holds about
	// 6 pages of 1000 today.
	catalogMaxPages = 50
	// catalogMaxBody bounds one page response.
	catalogMaxBody = 32 << 20
	// pageTimeout bounds one HTTP request; loadTimeout bounds a whole load.
	pageTimeout = 30 * time.Second
	loadTimeout = 2 * time.Minute
	// defaultRetryAfter is how long a failed load is reported before the
	// next request tries again, so a blocked network fails fast.
	defaultRetryAfter = 30 * time.Second
	nanosPerUnit      = 1e9
)

// errCatalogUnavailable marks a failure to read the catalog itself, as
// opposed to a resource the catalog has no price for.
var errCatalogUnavailable = errors.New("scaleway catalog unavailable")

// hoursPer converts a catalog unit to hours: a per-minute price is 60 per
// hour. Units outside this table are not priced.
var hoursPer = map[string]float64{"hour": 1, "minute": 60, "node": 1, "ip": 1, "gigabyte": 1}

type product struct {
	SKU      string `json:"sku"`
	Product  string `json:"product"`
	Status   string `json:"status"`
	Locality struct {
		Zone   string `json:"zone"`
		Region string `json:"region"`
	} `json:"locality"`
	Price *struct {
		RetailPrice *struct {
			CurrencyCode string      `json:"currency_code"`
			Units        json.Number `json:"units"`
			Nanos        int64       `json:"nanos"`
		} `json:"retail_price"`
	} `json:"price"`
	UnitOfMeasure *struct {
		Unit string `json:"unit"`
		Size int64  `json:"size"`
	} `json:"unit_of_measure"`
}

// price is one catalog entry's retail price per hour of use.
type price struct {
	sku      string
	hourly   float64
	currency string
}

// catalog loads the public catalog in the background, shares one load
// between concurrent callers, and indexes it by locality (a zone such as
// fr-par-1, or a region such as fr-par).
type catalog struct {
	url        string
	client     *http.Client
	pageSize   int
	retryAfter time.Duration

	mu         sync.Mutex
	byLocality map[string][]product
	loadedAt   time.Time
	loading    chan struct{} // closed when the load in flight ends
	lastErr    error
	lastErrAt  time.Time
}

func newCatalog(catalogURL string, client *http.Client) *catalog {
	return &catalog{url: catalogURL, client: client, pageSize: defaultPageSize, retryAfter: defaultRetryAfter}
}

// find returns the price of the product at locality whose SKU starts with
// l.skuPrefix and whose name matches l.name, ignoring case and treating "_"
// as "-" (the provider accepts pro2_s for PRO2-S). The entry's unit must be
// one of l.units.
func (c *catalog) find(ctx context.Context, l line) (price, error) {
	index, err := c.load(ctx)
	if err != nil {
		return price{}, err
	}
	for _, p := range index[l.locality] {
		if strings.HasPrefix(p.SKU, l.skuPrefix) && sameName(p.Product, l.name) {
			return p.hourlyPrice(l.units)
		}
	}
	return price{}, fmt.Errorf("no Scaleway price for %q in %s", l.name, l.locality)
}

func sameName(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "_", "-"), strings.ReplaceAll(b, "_", "-"))
}

func (p product) hourlyPrice(units []string) (price, error) {
	if p.Price == nil || p.Price.RetailPrice == nil {
		return price{}, fmt.Errorf("catalog entry %s has no retail price", p.SKU)
	}
	if p.UnitOfMeasure == nil || p.UnitOfMeasure.Size != 1 || !slices.Contains(units, p.UnitOfMeasure.Unit) {
		return price{}, fmt.Errorf("catalog entry %s is not priced per %s", p.SKU, strings.Join(units, " or "))
	}
	retail := p.Price.RetailPrice
	whole, err := strconv.ParseInt(retail.Units.String(), 10, 64)
	if err != nil {
		return price{}, fmt.Errorf("catalog entry %s has invalid price units: %w", p.SKU, err)
	}
	perUnit := float64(whole) + float64(retail.Nanos)/nanosPerUnit
	return price{sku: p.SKU, hourly: perUnit * hoursPer[p.UnitOfMeasure.Unit], currency: retail.CurrencyCode}, nil
}

// load returns the index, starting a background load when there is none or
// it is older than priceTTL. A stale index keeps serving while it refreshes.
// The load runs on its own deadline, so a short request deadline does not
// discard pages; a caller stops waiting when its own context ends. A failed
// load is reported for retryAfter, then retried.
func (c *catalog) load(ctx context.Context) (map[string][]product, error) {
	c.mu.Lock()
	if c.byLocality != nil {
		index := c.byLocality
		if time.Since(c.loadedAt) > priceTTL {
			c.startLoadLocked()
		}
		c.mu.Unlock()
		return index, nil
	}
	if c.lastErr != nil && time.Since(c.lastErrAt) < c.retryAfter {
		err := c.lastErr
		c.mu.Unlock()
		return nil, err
	}
	done := c.startLoadLocked()
	c.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errCatalogUnavailable, ctx.Err())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byLocality != nil {
		return c.byLocality, nil
	}
	return nil, c.lastErr
}

// startLoadLocked starts a load unless one is in flight and returns the
// channel that closes when it ends. c.mu must be held.
func (c *catalog) startLoadLocked() chan struct{} {
	if c.loading != nil {
		return c.loading
	}
	done := make(chan struct{})
	c.loading = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		index, err := c.fetchAll(ctx)

		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.lastErr, c.lastErrAt = err, time.Now()
		} else {
			c.byLocality, c.loadedAt, c.lastErr = index, time.Now(), nil
		}
		c.loading = nil
		close(done)
	}()
	return done
}

// fetchAll reads pages until a short or empty page. total_count is only a
// second stop, because this alpha API may omit it.
func (c *catalog) fetchAll(ctx context.Context) (map[string][]product, error) {
	index := make(map[string][]product)
	fetched := 0
	for page := 1; page <= catalogMaxPages; page++ {
		products, total, err := c.fetchPage(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, p := range products {
			locality := p.Locality.Zone
			if locality == "" {
				locality = p.Locality.Region
			}
			if p.Status == "retired" || locality == "" {
				continue
			}
			index[locality] = append(index[locality], p)
		}
		fetched += len(products)
		if len(products) < c.pageSize || (total > 0 && fetched >= total) {
			return index, nil
		}
	}
	return nil, fmt.Errorf("%w: more than %d pages", errCatalogUnavailable, catalogMaxPages)
}

func (c *catalog) fetchPage(ctx context.Context, page int) ([]product, int, error) {
	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()
	query := url.Values{
		"page":      {strconv.Itoa(page)},
		"page_size": {strconv.Itoa(c.pageSize)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errCatalogUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "finfocus-plugin-scaleway-public/"+PluginVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errCatalogUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%w: page %d returned HTTP %d", errCatalogUnavailable, page, resp.StatusCode)
	}

	var body struct {
		Products   []product `json:"products"`
		TotalCount int       `json:"total_count"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, catalogMaxBody)).Decode(&body); err != nil {
		return nil, 0, fmt.Errorf("%w: decoding page %d: %w", errCatalogUnavailable, page, err)
	}
	return body.Products, body.TotalCount, nil
}
