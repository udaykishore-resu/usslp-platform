// Package lightspeed implements the UIG adapter for Lightspeed Retail item
// webhooks.
//
// Lightspeed and Clover are the two sources most often assumed to be
// interchangeable — both are cloud POS vendors serving independent retail, both
// send JSON webhooks — and they are implemented separately here because their
// wire contracts are not merely different in field names, they are different in
// kind.
//
// Lightspeed pushes the *whole item*: the webhook body contains every price the
// item carries, tagged by use type, and the adapter needs nothing else to emit
// a canonical change. Clover pushes only an object reference — an id and the
// word UPDATE — and the price has to be fetched back from Clover's API before
// anything can be emitted. That difference is not cosmetic: it means Clover's
// adapter makes an authenticated outbound call on the ingest path, needs a
// circuit breaker and a retry policy, and can fail in ways that are the
// platform's problem rather than the retailer's. Sharing an implementation
// would mean either burdening Lightspeed with machinery it does not need, or
// hiding Clover's outbound dependency behind an interface that pretends the
// call is not happening. Both are worse than two files.
//
// The one thing they do share is the shape of their price semantics — a default
// price with optional sale and list variants — and that lives in the canonical
// event, which is where shared meaning belongs.
package lightspeed

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "lightspeed"

// HeaderSignature carries the hex HMAC-SHA256 over the raw body.
const HeaderSignature = "X-Lightspeed-Signature"

// Price use types Lightspeed tags its price rows with.
const (
	UseDefault = "Default"
	UseMSRP    = "MSRP"
	UseSale    = "Sale"
)

// Options is the per-binding configuration.
type Options struct {
	// EventTypes the adapter acts on.
	EventTypes []string `json:"event_types,omitempty"`
	// PriceUseType is the use type that becomes the shelf price when no sale
	// price is present.
	PriceUseType string `json:"price_use_type,omitempty"`
	// SaleUseType, when present on an item, overrides the shelf price and
	// pushes the default price into the struck-through was-price.
	SaleUseType string `json:"sale_use_type,omitempty"`
	// ListUseType is the manufacturer's recommended price, used as the
	// was-price when there is no sale.
	ListUseType string `json:"list_use_type,omitempty"`
	// SKUField selects which of Lightspeed's three SKU fields is the shelf SKU:
	// "customSku" (default), "systemSku" or "manufacturerSku". Retailers use
	// all three and only they know which one is printed on their shelf edge.
	SKUField string `json:"sku_field,omitempty"`

	types map[string]bool
}

// DefaultEventTypes are the item events that can change a shelf price.
var DefaultEventTypes = []string{"item.update", "item.create"}

// Adapter ingests Lightspeed webhooks.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

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
	if len(opts.EventTypes) == 0 {
		opts.EventTypes = DefaultEventTypes
	}
	opts.types = make(map[string]bool, len(opts.EventTypes))
	for _, t := range opts.EventTypes {
		opts.types[strings.ToLower(strings.TrimSpace(t))] = true
	}
	if opts.PriceUseType == "" {
		opts.PriceUseType = UseDefault
	}
	if opts.SaleUseType == "" {
		opts.SaleUseType = UseSale
	}
	if opts.ListUseType == "" {
		opts.ListUseType = UseMSRP
	}
	switch opts.SKUField {
	case "", "customSku":
		opts.SKUField = "customSku"
	case "systemSku", "manufacturerSku":
	default:
		return nil, fmt.Errorf("unknown sku_field %q", opts.SKUField)
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	return &Options{
		PriceUseType: UseDefault, SaleUseType: UseSale, ListUseType: UseMSRP,
		SKUField: "customSku",
		types:    map[string]bool{"item.update": true, "item.create": true},
	}
}

// Verify checks the hex HMAC over the raw body.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	return adapter.VerifyHMACSHA256(
		d.Binding.Secrets.HMACKey, d.Body, d.Header(HeaderSignature), adapter.EncodingHex, "")
}

type webhook struct {
	AccountID string      `json:"accountID"`
	Type      string      `json:"type"`
	Timestamp json.Number `json:"timestamp"`
	Payload   payload     `json:"payload"`
}

type payload struct {
	Item *item `json:"Item"`
}

type item struct {
	ItemID          string  `json:"itemID"`
	SystemSKU       string  `json:"systemSku"`
	CustomSKU       string  `json:"customSku"`
	ManufacturerSKU string  `json:"manufacturerSku"`
	Description     string  `json:"description"`
	ShopID          string  `json:"shopID"`
	UnitOfMeasure   string  `json:"unitOfMeasure"`
	TimeStamp       string  `json:"timeStamp"`
	Prices          *prices `json:"Prices"`
}

type prices struct {
	// Lightspeed sends a single object when there is one price and an array
	// when there are several, which is the classic XML-derived JSON quirk. A
	// custom unmarshaller absorbs it here rather than in every consumer.
	ItemPrice priceList `json:"ItemPrice"`
}

type itemPrice struct {
	Amount  string `json:"amount"`
	UseType string `json:"useType"`
}

type priceList []itemPrice

// UnmarshalJSON accepts either a single price object or an array of them.
func (p *priceList) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" || trimmed == "" {
		*p = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []itemPrice
		if err := json.Unmarshal(b, &list); err != nil {
			return err
		}
		*p = list
		return nil
	}
	var one itemPrice
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*p = []itemPrice{one}
	return nil
}

// IdempotencyParts identifies a Lightspeed delivery.
//
// Lightspeed sends no delivery id, so the identity is the account, the event
// type, the item and the event timestamp. That tuple is stable across
// Lightspeed's redeliveries and distinct between two genuine changes to the
// same item, which is exactly the line deduplication has to draw.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	var w webhook
	if err := json.Unmarshal(d.Body, &w); err != nil || w.Payload.Item == nil {
		return nil
	}
	return []string{
		w.AccountID,
		strings.ToLower(w.Type),
		w.Payload.Item.ItemID,
		w.Timestamp.String(),
	}
}

// Ingest converts one item webhook into a price change.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	var w webhook
	dec := json.NewDecoder(strings.NewReader(string(d.Body)))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, adapter.Malformed("json_decode", "the webhook body is not a Lightspeed item event", err)
	}
	if w.Type != "" && !opts.types[strings.ToLower(w.Type)] {
		return nil, nil
	}
	it := w.Payload.Item
	if it == nil {
		return nil, adapter.Malformed("no_item", "the webhook payload carries no Item", nil)
	}
	if n, err := w.Timestamp.Int64(); err == nil && n > 0 {
		d.SourceTime = time.Unix(n, 0).UTC()
	}
	if it.Prices == nil || len(it.Prices.ItemPrice) == 0 {
		return nil, adapter.Malformed("no_prices", "the item carries no Prices block", nil)
	}

	sku := skuOf(it, opts)
	if sku == "" {
		return nil, adapter.Invalid("missing_sku",
			"the item has no "+opts.SKUField+"; nominate a different sku_field on the binding", nil)
	}
	currency := d.Binding.Currency()

	byUse := make(map[string]string, len(it.Prices.ItemPrice))
	for _, p := range it.Prices.ItemPrice {
		use := strings.TrimSpace(p.UseType)
		if use == "" {
			use = UseDefault
		}
		// Lightspeed lists the newest row last for a given use type; keeping
		// the last write means a retailer who corrected a price seconds ago
		// gets the correction rather than the mistake.
		byUse[use] = strings.TrimSpace(p.Amount)
	}

	defaultAmt, hasDefault := byUse[opts.PriceUseType]
	saleAmt, hasSale := byUse[opts.SaleUseType]
	listAmt, hasList := byUse[opts.ListUseType]

	shelf := defaultAmt
	if hasSale && saleAmt != "" {
		shelf = saleAmt
	} else if !hasDefault {
		return nil, adapter.Invalid("no_shelf_price",
			fmt.Sprintf("the item has no %q or %q price row", opts.PriceUseType, opts.SaleUseType), nil)
	}
	price, err := decimal.ToMinor(shelf, currency)
	if err != nil {
		return nil, adapter.Invalid("price_unusable",
			fmt.Sprintf("price %q in %s: %v", shelf, currency, err), err)
	}

	pc := canon.PriceChangeRequested{
		SKU:          canon.SKU(sku),
		StoreID:      canon.StoreID(strings.TrimSpace(it.ShopID)),
		Price:        price,
		UnitMeasure:  strings.TrimSpace(it.UnitOfMeasure),
		SourceSystem: Name,
		Reason:       "lightspeed:" + strings.ToLower(w.Type),
		Attributes: map[string]string{
			"lightspeed_item_id": it.ItemID,
			"lightspeed_account": w.AccountID,
		},
	}
	if it.Description != "" {
		pc.Attributes["description"] = it.Description
	}
	if t, err := time.Parse(time.RFC3339, it.TimeStamp); err == nil {
		pc.EffectiveAt = t.UTC()
	}
	// The was-price is the default price when a sale is running, and the
	// manufacturer's list price otherwise. Showing a struck-through price that
	// is not higher than the shelf price would be misleading, so it is only set
	// when it genuinely is.
	wasCandidate := ""
	switch {
	case hasSale && hasDefault:
		wasCandidate = defaultAmt
	case hasList:
		wasCandidate = listAmt
	}
	if wasCandidate != "" {
		if was, err := decimal.ToMinor(wasCandidate, currency); err == nil && was.Amount > price.Amount {
			pc.WasPrice = &was
		}
	}
	if hasSale {
		pc.Attributes["lightspeed_sale"] = strconv.FormatBool(true)
	}
	return []canon.PriceChangeRequested{pc}, nil
}

func skuOf(it *item, opts *Options) string {
	switch opts.SKUField {
	case "systemSku":
		return strings.TrimSpace(it.SystemSKU)
	case "manufacturerSku":
		return strings.TrimSpace(it.ManufacturerSKU)
	default:
		return strings.TrimSpace(it.CustomSKU)
	}
}
