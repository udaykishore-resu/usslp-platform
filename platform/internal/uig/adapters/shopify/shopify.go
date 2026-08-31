// Package shopify implements the UIG adapter for Shopify product webhooks.
//
// Shopify is the platform's highest-volume small-retailer source and its wire
// format has two properties that shape the whole adapter. Prices arrive as
// decimal *strings* — "2.49", never 2.49 — and the webhook carries no currency
// at all, because a Shopify shop has exactly one and it lives in the shop's
// settings rather than in the payload. The first property is a gift: the string
// is the retailer's exact intent and converts to minor units without ever
// touching a float. The second is why the binding carries a default currency
// and why a binding without one cannot ingest Shopify traffic.
//
// The signature is HMAC-SHA256 over the raw request body, base64-encoded, in
// X-Shopify-Hmac-Sha256. It is verified in constant time against the exact
// bytes received; any re-serialisation before verification invalidates it.
package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name and appears in binding configuration,
// metric labels and Envelope.Source.
const Name = "shopify"

// Webhook headers Shopify sends, spelled in net/http's canonical form.
//
// They are constants rather than literals because Verify and IdempotencyParts
// both read them and a typo in one of the two would silently disable
// deduplication rather than fail. The canonical casing matters because
// http.Header canonicalises the key it is given, so a constant written in the
// vendor's own casing would miss on a header installed with a map literal.
const (
	HeaderHMAC       = "X-Shopify-Hmac-Sha256"
	HeaderTopic      = "X-Shopify-Topic"
	HeaderShopDomain = "X-Shopify-Shop-Domain"
	HeaderWebhookID  = "X-Shopify-Webhook-Id"
	HeaderAPIVersion = "X-Shopify-Api-Version"
)

// SKU sources a retailer may nominate.
const (
	// SKUFromSKU uses the variant's own sku field. The default, and what most
	// shops mean by a shelf SKU.
	SKUFromSKU = "sku"
	// SKUFromBarcode uses the variant barcode (EAN/UPC). Grocery retailers
	// label shelves by barcode because that is what the scale and the scanner
	// agree on.
	SKUFromBarcode = "barcode"
	// SKUFromVariantID uses Shopify's numeric variant id, for shops that never
	// populated a SKU field.
	SKUFromVariantID = "variant_id"
)

// Options is the per-binding configuration.
type Options struct {
	// Topics the adapter acts on. Everything else is acknowledged and produces
	// no events — a shop emits inventory, order and customer webhooks on the
	// same endpoint, and answering them with an error would put Shopify's
	// eight-hour redelivery schedule behind messages that will never be wanted.
	Topics []string `json:"topics,omitempty"`
	// SKUSource nominates which variant field is the shelf SKU.
	SKUSource string `json:"sku_source,omitempty"`
	// IgnoreCompareAt suppresses the was-price. A shop that leaves
	// compare_at_price populated from a long-finished promotion would otherwise
	// have "was £3.00" struck through on every shelf indefinitely.
	IgnoreCompareAt bool `json:"ignore_compare_at,omitempty"`
	// RequireSKU rejects variants with no usable SKU instead of skipping them.
	// Off by default: a shop with a handful of unSKU'd variants should still
	// have the rest of its catalogue priced.
	RequireSKU bool `json:"require_sku,omitempty"`

	topics map[string]bool
}

// DefaultTopics are the product webhooks that can change a shelf price.
var DefaultTopics = []string{"products/update", "products/create"}

// Adapter ingests Shopify webhooks.
type Adapter struct{}

// New creates the adapter. It holds no state: one instance serves every Shopify
// retailer on the platform, and everything tenant-specific lives in the
// binding.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// CompileOptions validates and compiles per-binding options.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	opts := &Options{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(opts); err != nil {
			return nil, err
		}
	}
	if len(opts.Topics) == 0 {
		opts.Topics = DefaultTopics
	}
	opts.topics = make(map[string]bool, len(opts.Topics))
	for _, t := range opts.Topics {
		opts.topics[strings.ToLower(strings.TrimSpace(t))] = true
	}
	switch opts.SKUSource {
	case "", SKUFromSKU:
		opts.SKUSource = SKUFromSKU
	case SKUFromBarcode, SKUFromVariantID:
	default:
		return nil, fmt.Errorf("unknown sku_source %q", opts.SKUSource)
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	// A binding installed before options existed still has to work; the
	// defaults are the behaviour every shop had before the knob was added.
	return &Options{SKUSource: SKUFromSKU, topics: map[string]bool{
		"products/update": true, "products/create": true,
	}}
}

// Verify checks the Shopify HMAC over the raw body.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	return adapter.VerifyHMACSHA256(
		d.Binding.Secrets.HMACKey, d.Body, d.Header(HeaderHMAC), adapter.EncodingBase64, "")
}

// IdempotencyParts identifies a Shopify delivery.
//
// Shopify keeps X-Shopify-Webhook-Id constant across every redelivery of the
// same event for the whole eight-hour retry schedule, which makes it exactly
// the right dedupe token. The shop domain and topic are mixed in so that a
// tenant with several shops bound to one endpoint cannot collide.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	id := d.Header(HeaderWebhookID)
	if id == "" {
		return nil
	}
	return []string{d.Header(HeaderShopDomain), strings.ToLower(d.Header(HeaderTopic)), id}
}

// product is the subset of the products/update payload that can change a price.
type product struct {
	ID        json.Number `json:"id"`
	Title     string      `json:"title"`
	Handle    string      `json:"handle"`
	UpdatedAt string      `json:"updated_at"`
	Status    string      `json:"status"`
	Variants  []variant   `json:"variants"`
}

type variant struct {
	ID              json.Number `json:"id"`
	ProductID       json.Number `json:"product_id"`
	Title           string      `json:"title"`
	SKU             string      `json:"sku"`
	Barcode         string      `json:"barcode"`
	Price           string      `json:"price"`
	CompareAtPrice  *string     `json:"compare_at_price"`
	UpdatedAt       string      `json:"updated_at"`
	InventoryItemID json.Number `json:"inventory_item_id"`
	Unit            *unitCost   `json:"unit_price_measurement"`
}

// unitCost is Shopify's unit-price block, which EU shops must populate for
// unit-price display and which maps onto the canonical UnitPrice/UnitMeasure.
type unitCost struct {
	MeasuredType    string      `json:"measured_type"`
	QuantityValue   json.Number `json:"quantity_value"`
	QuantityUnit    string      `json:"quantity_unit"`
	ReferenceValue  json.Number `json:"reference_value"`
	ReferenceUnit   string      `json:"reference_unit"`
	UnitPriceAmount string      `json:"unit_price_amount"`
}

// Ingest converts one product webhook into a price change per variant.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	topic := strings.ToLower(strings.TrimSpace(d.Header(HeaderTopic)))
	if topic != "" && !opts.topics[topic] {
		// Understood and deliberately ignored. Zero changes, no error: Shopify
		// gets a 2xx and stops, instead of redelivering an order webhook every
		// few minutes for eight hours.
		return nil, nil
	}

	var p product
	dec := json.NewDecoder(strings.NewReader(string(d.Body)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, adapter.Malformed("json_decode", "the webhook body is not a Shopify product object", err)
	}
	if len(p.Variants) == 0 {
		return nil, adapter.Malformed("no_variants", "the product carries no variants and therefore no prices", nil)
	}
	if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
		d.SourceTime = t.UTC()
	}

	currency := d.Binding.Currency()
	store := d.Header(HeaderShopDomain)
	out := make([]canon.PriceChangeRequested, 0, len(p.Variants))
	var failures []adapter.RowFailure

	for i, v := range p.Variants {
		sku := skuOf(v, opts)
		if sku == "" {
			if opts.RequireSKU {
				failures = append(failures, adapter.RowFailure{
					Index: i, Ref: v.Title, Reason: "missing_sku",
					Detail: "variant has no " + opts.SKUSource,
				})
			}
			// Without RequireSKU an unSKU'd variant is skipped silently: it is
			// not on a shelf, so it has no label to update.
			continue
		}
		price, err := decimal.ToMinor(strings.TrimSpace(v.Price), currency)
		if err != nil {
			failures = append(failures, adapter.RowFailure{
				Index: i, Ref: sku, Reason: reasonFor(err),
				Detail: fmt.Sprintf("price %q: %v", v.Price, err),
			})
			continue
		}
		pc := canon.PriceChangeRequested{
			SKU:          canon.SKU(sku),
			StoreID:      canon.StoreID(store),
			Price:        price,
			SourceSystem: Name,
			Reason:       "shopify:" + topic,
			Attributes: map[string]string{
				"shopify_product_id": p.ID.String(),
				"shopify_variant_id": v.ID.String(),
			},
		}
		if p.Title != "" {
			pc.Attributes["product_title"] = p.Title
		}
		if v.Title != "" {
			pc.Attributes["variant_title"] = v.Title
		}
		if t, err := time.Parse(time.RFC3339, v.UpdatedAt); err == nil {
			pc.EffectiveAt = t.UTC()
		}
		if !opts.IgnoreCompareAt && v.CompareAtPrice != nil && strings.TrimSpace(*v.CompareAtPrice) != "" {
			was, err := decimal.ToMinor(strings.TrimSpace(*v.CompareAtPrice), currency)
			switch {
			case err != nil:
				// A malformed compare-at is not worth failing a real price
				// over; the shelf simply does not show a struck-through price.
				failures = append(failures, adapter.RowFailure{
					Index: i, Ref: sku, Reason: "was_price_unusable",
					Detail: fmt.Sprintf("compare_at_price %q: %v", *v.CompareAtPrice, err),
				})
			case was.Amount > price.Amount:
				pc.WasPrice = &was
			}
		}
		if v.Unit != nil && strings.TrimSpace(v.Unit.UnitPriceAmount) != "" {
			if up, err := decimal.ToMinor(strings.TrimSpace(v.Unit.UnitPriceAmount), currency); err == nil {
				pc.UnitPrice = &up
				pc.UnitMeasure = unitMeasure(v.Unit)
			}
		}
		out = append(out, pc)
	}

	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: len(p.Variants)}
	}
	return out, nil
}

func skuOf(v variant, opts *Options) string {
	switch opts.SKUSource {
	case SKUFromBarcode:
		return strings.TrimSpace(v.Barcode)
	case SKUFromVariantID:
		return strings.TrimSpace(v.ID.String())
	default:
		return strings.TrimSpace(v.SKU)
	}
}

func unitMeasure(u *unitCost) string {
	ref := strings.TrimSpace(u.ReferenceValue.String())
	unit := strings.TrimSpace(u.ReferenceUnit)
	if ref == "" || unit == "" {
		return strings.TrimSpace(u.QuantityUnit)
	}
	return ref + unit
}

// reasonFor maps a conversion failure to a stable, low-cardinality metric label
// so that "shopify/decimal_syntax is up" localises a defect to one field
// without anyone opening a payload.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, decimal.ErrCurrency):
		return "currency_missing"
	case errors.Is(err, decimal.ErrRange):
		return "decimal_range"
	default:
		return "decimal_syntax"
	}
}
