// Package clover implements the UIG adapter for Clover merchant webhooks.
//
// Clover is the platform's only *pull* integration wearing a push interface.
// Its webhook carries no prices at all — only an app id, a merchant id, and a
// list of object references saying "item I:ABC123 changed". The price has to be
// fetched back from Clover's API before a canonical event can be produced. That
// makes it the one adapter that performs an authenticated outbound call on the
// ingest path, and it is why the machinery in this file exists at all:
//
//   - A circuit breaker per merchant, so that a Clover outage — or one
//     merchant's revoked token — costs a microsecond per delivery instead of
//     the connect timeout that would consume the gateway's entire 50ms budget
//     and then some.
//   - A short, aggressive retry policy, because the price path has no room for
//     the platform's general-purpose schedule.
//   - A careful split between failures that are the retailer's problem (item
//     deleted, item has no price) and failures that are the platform's (Clover
//     unreachable). The first is a row failure and a 4xx; the second is a 503
//     that Clover's own redelivery will fix. Confusing the two either loses a
//     price change or wedges a webhook queue.
//
// See the lightspeed package for why the two are not one adapter.
package clover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// Name is the adapter's registered name.
const Name = "clover"

// HeaderAuth carries the verification code Clover was configured with. Clover
// sends the merchant's own code rather than a signature, so the check is a
// constant-time comparison of a shared secret.
const HeaderAuth = "X-Clover-Auth"

// Object-reference prefixes Clover uses. Only inventory items carry prices.
const (
	prefixItem = "I:"
)

// Change types Clover reports.
const (
	ChangeCreate = "CREATE"
	ChangeUpdate = "UPDATE"
	ChangeDelete = "DELETE"
)

// Options is the per-binding configuration.
type Options struct {
	// BaseURL is the Clover API root for the merchant's region, e.g.
	// https://api.clover.com. It is per binding because Clover runs separate
	// production hosts per region and a sandbox host besides, and pointing a
	// live merchant at the sandbox returns 404 for every item.
	BaseURL string `json:"base_url"`
	// FetchTimeout bounds one outbound call. It defaults to a value well inside
	// the gateway's latency budget: a Clover call that has not answered in
	// 800ms has already cost more than the whole hop is allowed.
	FetchTimeout string `json:"fetch_timeout,omitempty"`
	// FailureThreshold and Cooldown tune the per-merchant circuit breaker.
	FailureThreshold int    `json:"failure_threshold,omitempty"`
	Cooldown         string `json:"cooldown,omitempty"`
	// SKUFromItemID uses Clover's opaque item id as the shelf SKU for merchants
	// who never populated the sku field.
	SKUFromItemID bool `json:"sku_from_item_id,omitempty"`

	timeout  time.Duration
	cooldown time.Duration
	base     *url.URL
}

// DefaultFetchTimeout bounds one Clover call.
const DefaultFetchTimeout = 800 * time.Millisecond

// Item is the subset of a Clover inventory item the price path needs.
type Item struct {
	ID string `json:"id"`
	// Name is the merchant's product name.
	Name string `json:"name"`
	SKU  string `json:"sku"`
	Code string `json:"code"`
	// Price is already in minor units of the merchant's currency, which Clover
	// does not repeat per item.
	Price json.Number `json:"price"`
	// PriceType is FIXED, VARIABLE or PER_UNIT. Only FIXED and PER_UNIT have a
	// price a shelf can show.
	PriceType string `json:"priceType"`
	// Unit names the measure for a PER_UNIT item.
	Unit string `json:"unitName"`
	// Hidden items are not on a shelf.
	Hidden bool `json:"hidden"`
	// Deleted marks an item removed since the webhook was queued.
	Deleted      bool        `json:"deleted"`
	ModifiedTime json.Number `json:"modifiedTime"`
}

// ItemFetcher retrieves an item from Clover.
//
// It is an interface so the adapter can be tested against a recorded-shape HTTP
// server, and so a deployment that fronts Clover with its own cache can supply
// one without touching the adapter.
type ItemFetcher interface {
	// FetchItem returns one item. A missing item must be reported with an error
	// satisfying errors.Is(err, ErrItemGone) so the adapter can treat it as a
	// row failure rather than an outage.
	FetchItem(ctx context.Context, baseURL *url.URL, merchantID, itemID, bearerToken string) (*Item, error)
}

// ErrItemGone means Clover no longer has the item. It is a permanent, per-item
// condition and never opens the circuit: a merchant deleting products is normal
// behaviour, not an outage.
var ErrItemGone = errors.New("uig/clover: item no longer exists")

// ErrUpstream means Clover could not be reached or returned a server error.
var ErrUpstream = errors.New("uig/clover: clover api unavailable")

// Adapter ingests Clover webhooks.
type Adapter struct {
	fetcher  ItemFetcher
	breakers *reliability.BreakerSet
	policy   retry.Policy
}

// New creates the adapter.
//
// A nil fetcher installs the HTTP one; a nil breaker set installs a default.
// Both are injectable because the outbound call is the part of this adapter
// most worth testing and least worth making a real network request for.
func New(fetcher ItemFetcher, breakers *reliability.BreakerSet) *Adapter {
	if fetcher == nil {
		fetcher = NewHTTPFetcher(nil)
	}
	if breakers == nil {
		breakers = reliability.NewBreakerSet(reliability.BreakerConfig{
			FailureThreshold: 5, Cooldown: 15 * time.Second, HalfOpenProbes: 1,
		})
	}
	return &Adapter{
		fetcher:  fetcher,
		breakers: breakers,
		// Aggressive rather than Default: the gateway owns 50ms of a 3-second
		// budget, and a schedule that can spend a second retrying has already
		// lost.
		policy: retry.Aggressive,
	}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// Breakers exposes the per-merchant breakers for metrics and the health
// endpoint.
func (a *Adapter) Breakers() *reliability.BreakerSet { return a.breakers }

// CompileOptions validates per-binding options.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	opts := &Options{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(opts); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("clover bindings must set base_url; the API host differs per region and sandbox")
	}
	u, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("base_url %q is not an absolute URL", opts.BaseURL)
	}
	opts.base = u
	opts.timeout = DefaultFetchTimeout
	if opts.FetchTimeout != "" {
		d, err := time.ParseDuration(opts.FetchTimeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("fetch_timeout %q must be a positive duration", opts.FetchTimeout)
		}
		opts.timeout = d
	}
	if opts.Cooldown != "" {
		d, err := time.ParseDuration(opts.Cooldown)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("cooldown %q must be a positive duration", opts.Cooldown)
		}
		opts.cooldown = d
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) (*Options, error) {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o, nil
	}
	return nil, adapter.Invalid("no_options",
		"this clover binding has no base_url configured", nil)
}

// Verify compares the merchant's verification code in constant time.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	return adapter.VerifySharedToken(
		d.Binding.Secrets.SharedToken, strings.TrimSpace(d.Header(HeaderAuth)), "clover auth code")
}

type webhook struct {
	AppID string `json:"appId"`
	// Merchants maps a merchant id to the objects that changed. Clover batches
	// several merchants into one delivery for a multi-merchant app.
	Merchants map[string][]objectRef `json:"merchants"`
}

type objectRef struct {
	ObjectID string      `json:"objectId"`
	Type     string      `json:"type"`
	TS       json.Number `json:"ts"`
}

// IdempotencyParts identifies a Clover delivery by the objects it names.
//
// Clover sends no delivery id and no batch id, so the identity is the app, and
// every (merchant, object, type, timestamp) tuple in sorted order. Sorting
// matters: Go's map iteration is randomised, and an unsorted key would make the
// same redelivery hash differently on every attempt, defeating deduplication
// entirely.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	var w webhook
	if err := json.Unmarshal(d.Body, &w); err != nil || len(w.Merchants) == 0 {
		return nil
	}
	parts := make([]string, 0, 8)
	merchants := make([]string, 0, len(w.Merchants))
	for m := range w.Merchants {
		merchants = append(merchants, m)
	}
	sort.Strings(merchants)
	for _, m := range merchants {
		refs := append([]objectRef(nil), w.Merchants[m]...)
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].ObjectID != refs[j].ObjectID {
				return refs[i].ObjectID < refs[j].ObjectID
			}
			return refs[i].TS.String() < refs[j].TS.String()
		})
		for _, r := range refs {
			parts = append(parts, m+"|"+r.ObjectID+"|"+r.Type+"|"+r.TS.String())
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return append([]string{w.AppID}, parts...)
}

// Ingest fetches each referenced item and converts it to a price change.
func (a *Adapter) Ingest(ctx context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts, err := optionsOf(d)
	if err != nil {
		return nil, err
	}
	var w webhook
	dec := json.NewDecoder(strings.NewReader(string(d.Body)))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, adapter.Malformed("json_decode", "the webhook body is not a Clover notification", err)
	}
	if len(w.Merchants) == 0 {
		return nil, adapter.Malformed("no_merchants", "the notification names no merchants", nil)
	}

	merchants := make([]string, 0, len(w.Merchants))
	for m := range w.Merchants {
		merchants = append(merchants, m)
	}
	sort.Strings(merchants)

	currency := d.Binding.Currency()
	var out []canon.PriceChangeRequested
	var failures []adapter.RowFailure
	total, index := 0, 0
	newest := time.Time{}

	for _, merchant := range merchants {
		br := a.breakers.Get(Name + "/" + merchant)
		for _, ref := range w.Merchants[merchant] {
			if !strings.HasPrefix(ref.ObjectID, prefixItem) {
				// A merchant, order or employee reference. Not a price.
				continue
			}
			if strings.EqualFold(ref.Type, ChangeDelete) {
				// A deleted item keeps its current shelf price until a human
				// removes the label; inventing a price for it would be worse.
				continue
			}
			total++
			pos := index
			index++
			itemID := strings.TrimPrefix(ref.ObjectID, prefixItem)
			if n, err := ref.TS.Int64(); err == nil && n > 0 {
				if t := time.UnixMilli(n).UTC(); t.After(newest) {
					newest = t
				}
			}

			item, err := a.fetch(ctx, br, opts, merchant, itemID, d.Binding.Secrets.BearerToken)
			switch {
			case errors.Is(err, ErrItemGone):
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: itemID, Reason: "item_gone",
					Detail: "clover no longer has this item; it was probably deleted after the webhook was queued",
				})
				continue
			case err != nil:
				// An outage is the platform's problem, not the retailer's. It
				// aborts the whole delivery with a retryable classification so
				// Clover redelivers, rather than emitting a partial price book.
				return nil, adapter.Unavailable("clover_unreachable",
					fmt.Sprintf("could not fetch item %s for merchant %s from clover", itemID, merchant), err)
			}
			if item.Deleted || item.Hidden {
				continue
			}
			if strings.EqualFold(item.PriceType, "VARIABLE") {
				// Priced at the till; there is nothing to display.
				continue
			}
			sku := strings.TrimSpace(item.SKU)
			if opts.SKUFromItemID || sku == "" {
				sku = strings.TrimSpace(item.ID)
			}
			if sku == "" {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: itemID, Reason: "missing_sku",
					Detail: "item carries neither a sku nor an id",
				})
				continue
			}
			amount, err := item.Price.Int64()
			if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: sku, Reason: "price_unusable",
					Detail: fmt.Sprintf("price %q is not an integer minor amount", item.Price.String()),
				})
				continue
			}
			price, err := decimal.FromMinorUnits(amount, currency)
			if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: sku, Reason: "currency_missing",
					Detail: err.Error(),
				})
				continue
			}
			pc := canon.PriceChangeRequested{
				SKU:          canon.SKU(sku),
				StoreID:      canon.StoreID(merchant),
				Price:        price,
				UnitMeasure:  strings.TrimSpace(item.Unit),
				SourceSystem: Name,
				Reason:       "clover:" + strings.ToLower(ref.Type),
				Attributes: map[string]string{
					"clover_item_id":  item.ID,
					"clover_merchant": merchant,
					"clover_app_id":   w.AppID,
				},
			}
			if item.Name != "" {
				pc.Attributes["description"] = item.Name
			}
			if item.Code != "" {
				pc.Attributes["clover_code"] = item.Code
			}
			if n, err := item.ModifiedTime.Int64(); err == nil && n > 0 {
				pc.EffectiveAt = time.UnixMilli(n).UTC()
			}
			out = append(out, pc)
		}
	}
	if !newest.IsZero() {
		d.SourceTime = newest
	}
	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: total}
	}
	return out, nil
}

// fetch performs one guarded, retried outbound call.
func (a *Adapter) fetch(
	ctx context.Context,
	br *reliability.Breaker,
	opts *Options,
	merchant, itemID, token string,
) (*Item, error) {
	var item *Item
	err := retry.Do(ctx, a.policy, func(ctx context.Context, _ int) error {
		callCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		defer cancel()
		// gone is carried out of the breaker's closure rather than returned
		// through it. A deleted item is Clover answering correctly and quickly;
		// letting it reach the breaker as an error would open a circuit against
		// a healthy dependency the first time a merchant tidies their menu.
		var gone error
		berr := br.Do(callCtx, func(ctx context.Context) error {
			it, err := a.fetcher.FetchItem(ctx, opts.base, merchant, itemID, token)
			switch {
			case errors.Is(err, ErrItemGone):
				gone = err
				return nil
			case err != nil:
				return err
			}
			item = it
			return nil
		})
		if gone != nil {
			return retry.Stop(gone)
		}
		if errors.Is(berr, reliability.ErrCircuitOpen) {
			// Retrying an open circuit only spends the latency budget on
			// sleeps that cannot change the answer.
			return retry.Stop(berr)
		}
		return berr
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// HTTP fetcher
// ---------------------------------------------------------------------------

// HTTPFetcher calls the real Clover API.
type HTTPFetcher struct {
	client *http.Client
}

// NewHTTPFetcher creates a fetcher. A nil client installs one with connection
// pooling and no timeout of its own — the per-call timeout comes from the
// binding, because a shared client timeout would apply to every merchant
// equally regardless of how much budget the delivery had left.
func NewHTTPFetcher(client *http.Client) *HTTPFetcher {
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &HTTPFetcher{client: client}
}

// FetchItem implements ItemFetcher.
func (f *HTTPFetcher) FetchItem(ctx context.Context, base *url.URL, merchantID, itemID, token string) (*Item, error) {
	if base == nil {
		return nil, fmt.Errorf("%w: no base url", ErrUpstream)
	}
	if token == "" {
		// Without a token every call is a 401, which would open the circuit and
		// look like a Clover outage. Failing here says what is actually wrong.
		return nil, fmt.Errorf("%w: binding has no clover bearer token", ErrUpstream)
	}
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/v3/merchants/" + url.PathEscape(merchantID) + "/items/" + url.PathEscape(itemID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	// The response is bounded before reading: an upstream that starts streaming
	// gigabytes must not be able to exhaust the gateway's memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrUpstream, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%w: item %s", ErrItemGone, itemID)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: clover returned %d", ErrUpstream, resp.StatusCode)
	}
	var item Item
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&item); err != nil {
		return nil, fmt.Errorf("%w: clover returned an unparseable item: %v", ErrUpstream, err)
	}
	return &item, nil
}
