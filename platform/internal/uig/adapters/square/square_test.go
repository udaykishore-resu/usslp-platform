package square

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const (
	signingKey  = "square-signature-key"
	notifyURL   = "https://uig.example/v1/ingest/acme/square-us"
	merchantID  = "MLEFGH1234567"
	catalogBody = `{
  "merchant_id": "MLEFGH1234567",
  "type": "catalog.object.updated",
  "event_id": "e2f1b0a4-8f7e-4bcb-9d55-6f2b1f0a7c31",
  "created_at": "2026-08-30T14:02:11.123Z",
  "data": {
    "type": "catalog",
    "id": "ITEM_VAR_ABC",
    "object": {
      "type": "ITEM_VARIATION",
      "id": "ITEM_VAR_ABC",
      "updated_at": "2026-08-30T14:02:10Z",
      "version": 1756563730123,
      "is_deleted": false,
      "item_variation_data": {
        "item_id": "ITEM_ABC",
        "name": "Regular",
        "sku": "SQ-ESP-REG",
        "pricing_type": "FIXED_PRICING",
        "price_money": {"amount": 249, "currency": "USD"},
        "location_overrides": [
          {"location_id": "LOC-DOWNTOWN", "price_money": {"amount": 269, "currency": "USD"}},
          {"location_id": "LOC-AIRPORT", "price_money": {"amount": 349, "currency": "USD"}}
        ]
      }
    }
  }
}`
)

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "square-us", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "USD",
		Secrets: adapter.Secrets{
			HMACKey:         signingKey,
			NotificationURL: notifyURL,
		},
	}
	if optionsJSON != "" {
		b.Options = json.RawMessage(optionsJSON)
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "square-us")
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
		Method: http.MethodPost, URL: notifyURL, Path: "/v1/ingest/acme/square-us",
		ContentType: "application/json",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
	// Square signs the notification URL concatenated with the body — not the
	// body alone.
	signed := append([]byte(notifyURL), d.Body...)
	d.Headers.Set(HeaderSignature,
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, signed), adapter.EncodingBase64))
	return d
}

func TestIngestCatalogWithLocationOverrides(t *testing.T) {
	a := New()
	d := testDelivery(t, catalogBody, "")
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// One base price plus two per-location overrides: three shelves, three
	// prices, all correct.
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3", len(changes))
	}
	if changes[0].Price.Amount != 249 || changes[0].StoreID != "" {
		t.Errorf("base change = %+v", changes[0])
	}
	if changes[1].StoreID != "LOC-DOWNTOWN" || changes[1].Price.Amount != 269 {
		t.Errorf("first override = %+v", changes[1])
	}
	if changes[2].StoreID != "LOC-AIRPORT" || changes[2].Price.Amount != 349 {
		t.Errorf("second override = %+v", changes[2])
	}
	// Attribute maps must not be shared between the changes derived from one
	// variation, or the last location would overwrite the others.
	if changes[1].Attributes["square_location_id"] != "LOC-DOWNTOWN" {
		t.Errorf("attributes leaked between overrides: %v", changes[1].Attributes)
	}
	if _, ok := changes[0].Attributes["square_location_id"]; ok {
		t.Error("the base change should carry no location attribute")
	}
	if !d.SourceTime.Equal(time.Date(2026, 8, 30, 14, 2, 11, 123000000, time.UTC)) {
		t.Errorf("source time = %s", d.SourceTime)
	}
	for _, c := range changes {
		if c.SKU != "SQ-ESP-REG" || c.Price.Currency != "USD" {
			t.Errorf("change = %+v", c)
		}
	}
}

func TestVerifySignsTheURLTogetherWithTheBody(t *testing.T) {
	a := New()
	d := testDelivery(t, catalogBody, "")

	// A signature computed over the body alone — the mistake every first
	// implementation makes — must be rejected.
	d.Headers.Set(HeaderSignature,
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d.Body), adapter.EncodingBase64))
	if err := a.Verify(context.Background(), d); err == nil {
		t.Fatal("a body-only signature was accepted")
	}

	// A binding whose notification URL does not match what Square was
	// configured with must fail, and the message must say why.
	d2 := testDelivery(t, catalogBody, "")
	d2.Binding.Secrets.NotificationURL = "https://uig.example/other"
	if err := a.Verify(context.Background(), d2); err == nil {
		t.Fatal("a mismatched notification URL was accepted")
	}

	d3 := testDelivery(t, catalogBody, "")
	d3.Binding.Secrets.NotificationURL = ""
	err := a.Verify(context.Background(), d3)
	if adapter.Classify(err).Reason != "no_notification_url" {
		t.Errorf("reason = %q; the adapter must refuse to guess the URL", adapter.Classify(err).Reason)
	}

	// A tampered body fails.
	d4 := testDelivery(t, catalogBody, "")
	d4.Body = append(d4.Body, ' ')
	if err := a.Verify(context.Background(), d4); err == nil {
		t.Fatal("a tampered body was accepted")
	}
}

func TestIdempotencyUsesSquareEventID(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, catalogBody, ""))
	if len(parts) != 3 || parts[0] != merchantID ||
		parts[2] != "e2f1b0a4-8f7e-4bcb-9d55-6f2b1f0a7c31" {
		t.Fatalf("parts = %v", parts)
	}
	if got := a.IdempotencyParts(testDelivery(t, `{"merchant_id":"M"}`, "")); got != nil {
		t.Errorf("an event with no id must defer to the body digest, got %v", got)
	}
}

func TestNestedItemVariations(t *testing.T) {
	a := New()
	body := `{
	  "merchant_id":"M1","type":"catalog.object.updated","event_id":"E1",
	  "data":{"type":"catalog","id":"ITEM_1","object":{
	    "type":"ITEM","id":"ITEM_1","updated_at":"2026-08-30T10:00:00Z",
	    "item_data":{"name":"Coffee","variations":[
	      {"type":"ITEM_VARIATION","id":"V1","item_variation_data":{"item_id":"ITEM_1","sku":"A","price_money":{"amount":100,"currency":"USD"}}},
	      {"type":"ITEM_VARIATION","id":"V2","item_variation_data":{"item_id":"ITEM_1","sku":"B","price_money":{"amount":200,"currency":"USD"}}}
	    ]}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 || changes[0].SKU != "A" || changes[1].SKU != "B" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestVariablePricingAndDeletionsProduceNoPrice(t *testing.T) {
	a := New()
	variable := `{"merchant_id":"M","type":"catalog.object.updated","event_id":"E",
	  "data":{"object":{"type":"ITEM_VARIATION","id":"V","item_variation_data":
	  {"item_id":"I","sku":"S","pricing_type":"VARIABLE_PRICING"}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, variable, ""))
	if err != nil || len(changes) != 0 {
		t.Fatalf("variable pricing produced %+v, %v; there is no price to display", changes, err)
	}

	deleted := `{"merchant_id":"M","type":"catalog.object.updated","event_id":"E",
	  "data":{"object":{"type":"ITEM_VARIATION","id":"V","is_deleted":true,"item_variation_data":
	  {"item_id":"I","sku":"S","price_money":{"amount":100,"currency":"USD"}}}}}`
	changes, err = a.Ingest(context.Background(), testDelivery(t, deleted, ""))
	if err != nil || len(changes) != 0 {
		t.Fatalf("a deleted variation produced %+v, %v", changes, err)
	}
}

func TestUnwantedEventTypesAreIgnored(t *testing.T) {
	a := New()
	body := `{"merchant_id":"M","type":"payment.created","event_id":"E","data":{}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil || changes != nil {
		t.Fatalf("changes = %v, err = %v", changes, err)
	}
}

func TestEmitBasePriceCanBeSuppressed(t *testing.T) {
	a := New()
	d := testDelivery(t, catalogBody, `{"emit_base_price":false}`)
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want only the two overrides", len(changes))
	}
	for _, c := range changes {
		if c.StoreID == "" {
			t.Errorf("base price was emitted despite emit_base_price=false: %+v", c)
		}
	}
}

func TestMalformedBodyIsClassifiedForQuarantine(t *testing.T) {
	a := New()
	_, err := a.Ingest(context.Background(), testDelivery(t, `{"merchant_id":`, ""))
	cls := adapter.Classify(err)
	if cls.Kind != adapter.FailureMalformed || cls.Kind.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("classification = %+v (status %d)", cls, cls.Kind.HTTPStatus())
	}
}

func TestNonIntegerAmountIsIsolatedNotFatal(t *testing.T) {
	a := New()
	// A malformed amount on one variation must not discard the item's other
	// prices, so it is reported as a row failure.
	body := `{"merchant_id":"M","type":"catalog.object.updated","event_id":"E",
	  "data":{"object":{"type":"ITEM","id":"I","item_data":{"variations":[
	    {"type":"ITEM_VARIATION","id":"V1","item_variation_data":{"sku":"OK","price_money":{"amount":100,"currency":"USD"}}},
	    {"type":"ITEM_VARIATION","id":"V2","item_variation_data":{"sku":"BAD","price_money":{"amount":"12.5","currency":"USD"}}}
	  ]}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if len(changes) != 1 || changes[0].SKU != "OK" {
		t.Fatalf("changes = %+v", changes)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 1 || partial.Failures[0].Ref != "BAD" {
		t.Fatalf("err = %v", err)
	}
}
