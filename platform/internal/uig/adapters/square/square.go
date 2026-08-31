// Package square implements the UIG adapter for Square catalog webhooks.
//
// Square differs from every other webhook source the platform speaks to in one
// respect that cannot be papered over: the signed message is the notification
// URL concatenated with the raw body, not the body alone. That means the URL
// Square was configured with is a credential-adjacent piece of configuration —
// if a load balancer rewrites the path, or the retailer registered the endpoint
// with a trailing slash, every signature fails and the failure looks exactly
// like a wrong secret. The binding therefore carries the notification URL
// explicitly rather than the adapter reconstructing it from the request, and
// the error message says so.
//
// Square's money is already exact: price_money carries an integer amount in
// minor units together with its currency, so there is no decimal conversion to
// get wrong. What Square does have is per-location price overrides, and those
// are the reason one webhook produces several canonical changes: a variation
// priced at 2.49 with an override of 2.69 at one location is two different
// shelves showing two different, both correct, prices.
package square

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "square"

// HeaderSignature carries the base64 HMAC-SHA256 over notificationURL+body.
// Square sends it as X-Square-HmacSha256-Signature; the constant is spelled in
// net/http's canonical form because that is the form a header map holds.
const HeaderSignature = "X-Square-Hmacsha256-Signature"

// Options is the per-binding configuration.
type Options struct {
	// EventTypes the adapter acts on. Square posts catalog, order, payment and
	// inventory events to the same endpoint.
	EventTypes []string `json:"event_types,omitempty"`
	// EmitBasePrice publishes the variation's own price in addition to its
	// per-location overrides. On by default: the base price is what applies at
	// every location the retailer did not override, and suppressing it would
	// leave those shelves stale.
	EmitBasePrice *bool `json:"emit_base_price,omitempty"`
	// SKUFromVariationID uses Square's opaque variation id as the shelf SKU for
	// merchants who never populated the sku field.
	SKUFromVariationID bool `json:"sku_from_variation_id,omitempty"`

	types map[string]bool
}

// DefaultEventTypes are the catalogue events that can change a shelf price.
var DefaultEventTypes = []string{"catalog.object.updated", "catalog.version.updated"}

// Adapter ingests Square webhooks.
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
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	return &Options{types: map[string]bool{
		"catalog.object.updated": true, "catalog.version.updated": true,
	}}
}

func (o *Options) emitBase() bool { return o.EmitBasePrice == nil || *o.EmitBasePrice }

// Verify checks Square's signature over notificationURL + body.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	notifyURL := d.Binding.Secrets.NotificationURL
	if notifyURL == "" {
		// Falling back to the request URL would appear to work in development
		// and fail in production behind a proxy that rewrites the host, which
		// is a worse outcome than refusing to guess.
		return adapter.Unauthorized("no_notification_url",
			"binding has no square notification_url configured; square signs the URL together with the body")
	}
	signed := make([]byte, 0, len(notifyURL)+len(d.Body))
	signed = append(signed, notifyURL...)
	signed = append(signed, d.Body...)
	return adapter.VerifyHMACSHA256(
		d.Binding.Secrets.HMACKey, signed, d.Header(HeaderSignature), adapter.EncodingBase64, "")
}

// event is the Square webhook envelope.
type event struct {
	MerchantID string    `json:"merchant_id"`
	Type       string    `json:"type"`
	EventID    string    `json:"event_id"`
	CreatedAt  string    `json:"created_at"`
	Data       eventData `json:"data"`
}

type eventData struct {
	Type   string        `json:"type"`
	ID     string        `json:"id"`
	Object catalogObject `json:"object"`
}

type catalogObject struct {
	Type      string      `json:"type"`
	ID        string      `json:"id"`
	UpdatedAt string      `json:"updated_at"`
	Version   json.Number `json:"version"`
	IsDeleted bool        `json:"is_deleted"`
	Variation *variation  `json:"item_variation_data,omitempty"`
	// Item wraps a full item whose variations arrive nested, which is the shape
	// a catalog.object.updated for an ITEM carries.
	Item *itemData `json:"item_data,omitempty"`
}

type itemData struct {
	Name       string          `json:"name"`
	Variations []catalogObject `json:"variations,omitempty"`
}

type variation struct {
	ItemID            string      `json:"item_id"`
	Name              string      `json:"name"`
	SKU               string      `json:"sku"`
	UPC               string      `json:"upc"`
	PricingType       string      `json:"pricing_type"`
	PriceMoney        *money      `json:"price_money"`
	LocationOverrides []override  `json:"location_overrides,omitempty"`
	MeasurementUnitID string      `json:"measurement_unit_id,omitempty"`
	Ordinal           json.Number `json:"ordinal,omitempty"`
}

type override struct {
	LocationID string `json:"location_id"`
	PriceMoney *money `json:"price_money"`
	// TrackInventory and SoldOut are present in the real payload and ignored
	// here; they are decoded rather than dropped so a future need for them does
	// not require re-reading Square's documentation.
	PricingType string `json:"pricing_type,omitempty"`
}

type money struct {
	// Amount is already in minor units. Decoded as json.Number so a merchant
	// with a very large amount, or a field Square sends as a string, does not
	// pass through a float.
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

// IdempotencyParts uses Square's own event id, which is stable across the
// retries Square performs when an endpoint is slow or returns non-2xx.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	var e event
	if err := json.Unmarshal(d.Body, &e); err != nil || e.EventID == "" {
		return nil
	}
	return []string{e.MerchantID, e.Type, e.EventID}
}

// Ingest converts a catalog webhook into one price change per priced location.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	var e event
	dec := json.NewDecoder(strings.NewReader(string(d.Body)))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return nil, adapter.Malformed("json_decode", "the webhook body is not a Square event", err)
	}
	if e.Type != "" && !opts.types[strings.ToLower(e.Type)] {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
		d.SourceTime = t.UTC()
	}

	variations := collectVariations(e.Data.Object)
	if len(variations) == 0 {
		// A catalogue event that touched something other than a priced
		// variation — a category rename, a modifier list. Understood, ignored.
		return nil, nil
	}

	out := make([]canon.PriceChangeRequested, 0, len(variations))
	var failures []adapter.RowFailure
	total := 0

	for i, obj := range variations {
		v := obj.Variation
		if obj.IsDeleted {
			continue
		}
		sku := strings.TrimSpace(v.SKU)
		if opts.SKUFromVariationID || sku == "" {
			sku = strings.TrimSpace(obj.ID)
		}
		if sku == "" {
			failures = append(failures, adapter.RowFailure{
				Index: i, Ref: v.Name, Reason: "missing_sku",
				Detail: "variation carries neither a sku nor an id",
			})
			continue
		}
		base := canon.PriceChangeRequested{
			SKU:          canon.SKU(sku),
			SourceSystem: Name,
			Reason:       "square:" + e.Type,
			Attributes: map[string]string{
				"square_variation_id": obj.ID,
				"square_item_id":      v.ItemID,
				"square_merchant_id":  e.MerchantID,
			},
		}
		if v.Name != "" {
			base.Attributes["variation_name"] = v.Name
		}
		if t, err := time.Parse(time.RFC3339, obj.UpdatedAt); err == nil {
			base.EffectiveAt = t.UTC()
		}
		// VARIABLE_PRICING means the cashier types the price at the till; there
		// is no price to put on a shelf and emitting one would be a lie.
		if strings.EqualFold(v.PricingType, "VARIABLE_PRICING") && len(v.LocationOverrides) == 0 {
			continue
		}

		if opts.emitBase() && v.PriceMoney != nil {
			total++
			m, err := convert(v.PriceMoney, d.Binding.Currency())
			if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: i, Ref: sku, Reason: "price_unusable", Detail: err.Error(),
				})
			} else {
				pc := clone(base)
				pc.Price = m
				out = append(out, pc)
			}
		}
		for _, ov := range v.LocationOverrides {
			if ov.PriceMoney == nil {
				continue
			}
			total++
			m, err := convert(ov.PriceMoney, d.Binding.Currency())
			if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: i, Ref: sku + "@" + ov.LocationID, Reason: "price_unusable", Detail: err.Error(),
				})
				continue
			}
			pc := clone(base)
			pc.Price = m
			pc.StoreID = canon.StoreID(ov.LocationID)
			pc.Attributes["square_location_id"] = ov.LocationID
			out = append(out, pc)
		}
	}

	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: total}
	}
	return out, nil
}

// collectVariations flattens the two shapes Square uses: an event about a
// variation directly, and an event about an item whose variations are nested.
func collectVariations(obj catalogObject) []catalogObject {
	if obj.Variation != nil {
		return []catalogObject{obj}
	}
	if obj.Item == nil {
		return nil
	}
	out := make([]catalogObject, 0, len(obj.Item.Variations))
	for _, v := range obj.Item.Variations {
		if v.Variation != nil {
			out = append(out, v)
		}
	}
	return out
}

func convert(m *money, fallbackCurrency string) (canon.Money, error) {
	cur := strings.TrimSpace(m.Currency)
	if cur == "" {
		cur = fallbackCurrency
	}
	amt, err := m.Amount.Int64()
	if err != nil {
		return canon.Money{}, fmt.Errorf("price_money.amount %q is not an integer minor amount", m.Amount.String())
	}
	return decimal.FromMinorUnits(amt, cur)
}

// clone copies a change including its attribute map, so that per-location
// changes derived from one variation do not share — and then overwrite — each
// other's attributes.
func clone(pc canon.PriceChangeRequested) canon.PriceChangeRequested {
	attrs := make(map[string]string, len(pc.Attributes)+1)
	for k, v := range pc.Attributes {
		attrs[k] = v
	}
	pc.Attributes = attrs
	return pc
}
