package shopify

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const signingKey = "hush-hush-shopify-secret"

// productsUpdate is a recorded-shape products/update webhook: variants priced
// as decimal strings, a compare-at price on one of them, and an EU unit-price
// block on another.
const productsUpdate = `{
  "id": 788032119674292900,
  "title": "Espresso Beans",
  "handle": "espresso-beans",
  "updated_at": "2026-08-30T10:02:11-04:00",
  "status": "active",
  "variants": [
    {
      "id": 1070325020,
      "product_id": 788032119674292900,
      "title": "1 kg",
      "sku": "ESP-1KG",
      "barcode": "5012345678900",
      "price": "12.99",
      "compare_at_price": "15.50",
      "updated_at": "2026-08-30T10:02:11-04:00",
      "inventory_item_id": 808950810,
      "unit_price_measurement": {
        "measured_type": "weight",
        "quantity_value": "1000",
        "quantity_unit": "g",
        "reference_value": "1",
        "reference_unit": "kg",
        "unit_price_amount": "12.99"
      }
    },
    {
      "id": 1070325021,
      "product_id": 788032119674292900,
      "title": "250 g",
      "sku": "ESP-250G",
      "barcode": "5012345678917",
      "price": "3.75",
      "compare_at_price": null,
      "updated_at": "2026-08-30T10:02:11-04:00"
    },
    {
      "id": 1070325022,
      "product_id": 788032119674292900,
      "title": "Sample",
      "sku": "",
      "price": "0.00",
      "compare_at_price": null,
      "updated_at": "2026-08-30T10:02:11-04:00"
    }
  ]
}`

// bindWith installs a binding through the real registry and binding store, so
// that the options these tests exercise are compiled exactly the way the
// service compiles them at start-up.
func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "shopify-uk", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "GBP",
		Secrets:         adapter.Secrets{HMACKey: signingKey},
	}
	if optionsJSON != "" {
		b.Options = json.RawMessage(optionsJSON)
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("installing binding: %v", err)
	}
	got, err := store.Get("acme", "shopify-uk")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDelivery(t *testing.T, body, optionsJSON string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, optionsJSON)
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Method: http.MethodPost, Path: "/v1/ingest/acme/shopify-uk",
		ContentType: "application/json",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
	// Set rather than a map literal: http.Header canonicalises key case on Get,
	// and a literal written in the wire's own casing would silently miss.
	d.Headers.Set(HeaderTopic, "products/update")
	d.Headers.Set(HeaderShopDomain, "acme-uk.myshopify.com")
	d.Headers.Set(HeaderWebhookID, "b54557e4-bcc5-4f0b-9a5f-1a3d59b8e0e7")
	d.Headers.Set(HeaderAPIVersion, "2026-07")
	d.Headers.Set(HeaderHMAC,
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d.Body), adapter.EncodingBase64))
	return d
}

func TestIngestProductsUpdate(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, "")
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// The third variant has no SKU and is skipped without being an error: it is
	// not on a shelf, so it has no label to update.
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}

	c := changes[0]
	if c.SKU != "ESP-1KG" {
		t.Errorf("sku = %q", c.SKU)
	}
	// "12.99" is a decimal string and must land on exactly 1299 minor units
	// with no float in between.
	if c.Price.Amount != 1299 || c.Price.Currency != "GBP" {
		t.Errorf("price = %+v, want 1299 GBP", c.Price)
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was price = %+v, want 1550", c.WasPrice)
	}
	if c.UnitPrice == nil || c.UnitPrice.Amount != 1299 {
		t.Errorf("unit price = %+v", c.UnitPrice)
	}
	if c.UnitMeasure != "1kg" {
		t.Errorf("unit measure = %q", c.UnitMeasure)
	}
	// The store is left as the source's own identifier; the pipeline's
	// enrichment step turns it into a USSLP store id.
	if c.StoreID != "acme-uk.myshopify.com" {
		t.Errorf("store = %q", c.StoreID)
	}
	if c.Attributes["shopify_variant_id"] != "1070325020" {
		t.Errorf("attributes = %v", c.Attributes)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 8, 30, 14, 2, 11, 0, time.UTC)) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if d.SourceTime.IsZero() {
		t.Error("the product's updated_at should become the delivery's source time")
	}

	if changes[1].Price.Amount != 375 || changes[1].WasPrice != nil {
		t.Errorf("second variant = %+v", changes[1])
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, "")
	// A single character changed after signing — the classic replay-with-edits
	// attempt — must fail.
	d.Body = []byte(`{"id":1,"variants":[{"sku":"ESP-1KG","price":"0.01"}]}`)
	err := a.Verify(context.Background(), d)
	if err == nil {
		t.Fatal("a tampered body was accepted")
	}
	cls := adapter.Classify(err)
	if cls.Kind != adapter.FailureUnauthorized || cls.Reason != "bad_signature" {
		t.Fatalf("classification = %+v", cls)
	}

	d2 := testDelivery(t, productsUpdate, "")
	d2.Headers.Del(HeaderHMAC)
	if adapter.Classify(a.Verify(context.Background(), d2)).Reason != "missing_signature" {
		t.Error("a missing signature header must be reported as such")
	}

	d3 := testDelivery(t, productsUpdate, "")
	d3.Binding.Secrets.HMACKey = ""
	if err := a.Verify(context.Background(), d3); err == nil {
		t.Fatal("a binding with no signing key must fail closed")
	}
}

func TestIgnoresUnwantedTopics(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, "")
	d.Headers.Set(HeaderTopic, "orders/create")
	changes, err := a.Ingest(context.Background(), d)
	// Understood and deliberately ignored: an error here would put Shopify's
	// eight-hour redelivery schedule behind messages nobody wants.
	if err != nil || changes != nil {
		t.Fatalf("changes = %v, err = %v", changes, err)
	}
}

func TestIdempotencyPartsUseTheWebhookID(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, "")
	parts := a.IdempotencyParts(d)
	if len(parts) != 3 || parts[2] != "b54557e4-bcc5-4f0b-9a5f-1a3d59b8e0e7" {
		t.Fatalf("parts = %v", parts)
	}
	// Two shops on one endpoint must not be able to collide.
	d2 := testDelivery(t, productsUpdate, "")
	d2.Headers.Set(HeaderShopDomain, "other.myshopify.com")
	if a.IdempotencyParts(d2)[0] == parts[0] {
		t.Error("the shop domain is not part of the identity")
	}
	d3 := testDelivery(t, productsUpdate, "")
	d3.Headers.Del(HeaderWebhookID)
	if a.IdempotencyParts(d3) != nil {
		t.Error("with no webhook id the adapter must defer to the body digest")
	}
}

func TestMalformedBodiesAreClassifiedForQuarantine(t *testing.T) {
	a := New()
	for name, body := range map[string]string{
		"not json":    `<html>maintenance</html>`,
		"no variants": `{"id":1,"title":"x","variants":[]}`,
	} {
		d := testDelivery(t, body, "")
		_, err := a.Ingest(context.Background(), d)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s, want malformed so it is quarantined with a 4xx", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d — a body that will never parse must not be retried",
				name, cls.Kind.HTTPStatus())
		}
	}
}

func TestBadVariantPriceIsIsolated(t *testing.T) {
	a := New()
	body := `{"id":1,"variants":[
	  {"id":1,"sku":"GOOD","price":"1.50","updated_at":"2026-08-30T10:00:00Z"},
	  {"id":2,"sku":"BAD","price":"one fifty","updated_at":"2026-08-30T10:00:00Z"},
	  {"id":3,"sku":"ALSO-GOOD","price":"2.00","updated_at":"2026-08-30T10:00:00Z"}
	]}`
	d := testDelivery(t, body, "")
	changes, err := a.Ingest(context.Background(), d)
	// One unusable variant must cost one product, not the whole webhook.
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok {
		t.Fatalf("err = %v, want a PartialError", err)
	}
	if len(partial.Failures) != 1 || partial.Failures[0].Ref != "BAD" {
		t.Fatalf("failures = %+v", partial.Failures)
	}
	if partial.Failures[0].Reason != "decimal_syntax" {
		t.Errorf("reason = %q, want a low-cardinality parse-error label", partial.Failures[0].Reason)
	}
}

func TestCompareAtIsSuppressedWhenItIsNotHigher(t *testing.T) {
	a := New()
	body := `{"id":1,"variants":[{"id":1,"sku":"A","price":"5.00","compare_at_price":"4.00","updated_at":"2026-08-30T10:00:00Z"}]}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatal(err)
	}
	// A struck-through price lower than the shelf price would be misleading on
	// a label and is dropped rather than displayed.
	if changes[0].WasPrice != nil {
		t.Errorf("was price = %+v, want none", changes[0].WasPrice)
	}
}

func TestSKUSourceOptions(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, `{"sku_source":"barcode","ignore_compare_at":true}`)
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].SKU != "5012345678900" {
		t.Errorf("sku = %q, want the barcode", changes[0].SKU)
	}
	if changes[0].WasPrice != nil {
		t.Error("ignore_compare_at did not suppress the was-price")
	}
	if _, err := a.CompileOptions(json.RawMessage(`{"sku_source":"magic"}`)); err == nil {
		t.Error("an unknown sku_source must be rejected at install time")
	}
	if _, err := a.CompileOptions(json.RawMessage(`{"unknown_key":1}`)); err == nil {
		t.Error("an unknown option key must be rejected rather than silently ignored")
	}
}

func TestMissingCurrencyIsAnActionableFailure(t *testing.T) {
	a := New()
	d := testDelivery(t, productsUpdate, "")
	// Shopify product webhooks carry no currency; a binding without a default
	// simply cannot ingest them, and the failure must say so per variant rather
	// than producing a price in no currency.
	d.Binding.DefaultCurrency = ""
	changes, err := a.Ingest(context.Background(), d)
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if partial.Failures[0].Reason != "currency_missing" {
		t.Errorf("reason = %q", partial.Failures[0].Reason)
	}
}
