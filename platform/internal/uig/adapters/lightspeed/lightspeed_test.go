package lightspeed

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const signingKey = "lightspeed-webhook-secret"

// itemUpdate is a recorded-shape item.update webhook. Lightspeed pushes the
// whole item, with every price tagged by use type — which is why this adapter
// needs no callback and Clover's does.
const itemUpdate = `{
  "accountID": "163521",
  "type": "item.update",
  "timestamp": 1756563731,
  "payload": {
    "Item": {
      "itemID": "9014",
      "systemSku": "LS-9014",
      "customSku": "ESP-1KG",
      "manufacturerSku": "MFR-77",
      "description": "Espresso Beans 1kg",
      "shopID": "3",
      "unitOfMeasure": "EA",
      "timeStamp": "2026-08-30T14:02:11Z",
      "Prices": {
        "ItemPrice": [
          {"amount": "12.99", "useType": "Default"},
          {"amount": "15.50", "useType": "MSRP"}
        ]
      }
    }
  }
}`

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "ls-main", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "CAD",
		Secrets:         adapter.Secrets{HMACKey: signingKey},
	}
	if optionsJSON != "" {
		b.Options = json.RawMessage(optionsJSON)
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "ls-main")
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
		Method: http.MethodPost, Path: "/v1/ingest/acme/ls-main",
		ContentType: "application/json",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
	d.Headers.Set(HeaderSignature,
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d.Body), adapter.EncodingHex))
	return d
}

func TestIngestItemUpdate(t *testing.T) {
	a := New()
	d := testDelivery(t, itemUpdate, "")
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	c := changes[0]
	if c.SKU != "ESP-1KG" {
		t.Errorf("sku = %q, want the customSku", c.SKU)
	}
	if c.StoreID != "3" {
		t.Errorf("store = %q, want the shop id", c.StoreID)
	}
	if c.Price.Amount != 1299 || c.Price.Currency != "CAD" {
		t.Errorf("price = %+v", c.Price)
	}
	// With no sale running the manufacturer's list price is the struck-through
	// one.
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if c.UnitMeasure != "EA" {
		t.Errorf("unit measure = %q", c.UnitMeasure)
	}
	if !d.SourceTime.Equal(time.Unix(1756563731, 0).UTC()) {
		t.Errorf("source time = %s", d.SourceTime)
	}
}

func TestSalePriceBecomesTheShelfPrice(t *testing.T) {
	a := New()
	body := `{"accountID":"1","type":"item.update","timestamp":1,"payload":{"Item":{
	  "itemID":"1","customSku":"A","shopID":"3","Prices":{"ItemPrice":[
	    {"amount":"12.99","useType":"Default"},
	    {"amount":"15.50","useType":"MSRP"},
	    {"amount":"9.99","useType":"Sale"}
	  ]}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatal(err)
	}
	c := changes[0]
	if c.Price.Amount != 999 {
		t.Errorf("price = %d, want the sale price", c.Price.Amount)
	}
	// When a sale is running, the default price is what the shopper is saving
	// against — not the MSRP nobody was charging.
	if c.WasPrice == nil || c.WasPrice.Amount != 1299 {
		t.Errorf("was = %+v, want the default price", c.WasPrice)
	}
	if c.Attributes["lightspeed_sale"] != "true" {
		t.Errorf("attributes = %v", c.Attributes)
	}
}

func TestSinglePriceObjectRatherThanArray(t *testing.T) {
	a := New()
	// Lightspeed sends an object when there is one price and an array when
	// there are several — the classic XML-derived JSON quirk.
	body := `{"accountID":"1","type":"item.update","timestamp":1,"payload":{"Item":{
	  "itemID":"1","customSku":"A","shopID":"3",
	  "Prices":{"ItemPrice":{"amount":"4.25","useType":"Default"}}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 1 || changes[0].Price.Amount != 425 {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestSKUFieldSelection(t *testing.T) {
	a := New()
	for field, want := range map[string]string{
		"":                "ESP-1KG",
		"customSku":       "ESP-1KG",
		"systemSku":       "LS-9014",
		"manufacturerSku": "MFR-77",
	} {
		opts := ""
		if field != "" {
			opts = `{"sku_field":"` + field + `"}`
		}
		changes, err := a.Ingest(context.Background(), testDelivery(t, itemUpdate, opts))
		if err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		if string(changes[0].SKU) != want {
			t.Errorf("sku_field %q gave %q, want %q", field, changes[0].SKU, want)
		}
	}
	if _, err := a.CompileOptions(json.RawMessage(`{"sku_field":"invented"}`)); err == nil {
		t.Error("an unknown sku_field must be rejected at install time")
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	a := New()
	d := testDelivery(t, itemUpdate, "")
	d.Body = append(d.Body, ' ')
	if err := a.Verify(context.Background(), d); err == nil {
		t.Fatal("a tampered body was accepted")
	}
	d2 := testDelivery(t, itemUpdate, "")
	d2.Binding.Secrets.HMACKey = ""
	if err := a.Verify(context.Background(), d2); err == nil {
		t.Fatal("a binding with no signing key must fail closed")
	}
}

func TestIdempotencyIdentifiesTheEvent(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, itemUpdate, ""))
	if len(parts) != 4 || parts[0] != "163521" || parts[2] != "9014" || parts[3] != "1756563731" {
		t.Fatalf("parts = %v", parts)
	}
	// Lightspeed sends no delivery id, so two genuine changes to the same item
	// are told apart by their event timestamps.
	later := `{"accountID":"163521","type":"item.update","timestamp":1756563999,
	  "payload":{"Item":{"itemID":"9014","customSku":"A","shopID":"3",
	  "Prices":{"ItemPrice":{"amount":"1.00","useType":"Default"}}}}}`
	if a.IdempotencyParts(testDelivery(t, later, ""))[3] == parts[3] {
		t.Error("two events at different times produced the same identity")
	}
}

func TestUnwantedTypesAndMalformedBodies(t *testing.T) {
	a := New()
	other := `{"accountID":"1","type":"sale.update","timestamp":1,"payload":{}}`
	if changes, err := a.Ingest(context.Background(), testDelivery(t, other, "")); err != nil || changes != nil {
		t.Fatalf("an unwanted type produced %v, %v", changes, err)
	}
	for name, body := range map[string]string{
		"not json":  `<xml/>`,
		"no item":   `{"accountID":"1","type":"item.update","payload":{}}`,
		"no prices": `{"accountID":"1","type":"item.update","payload":{"Item":{"itemID":"1","customSku":"A"}}}`,
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s", name, cls.Kind)
		}
	}
	// A price that will not convert is an invalid record rather than an
	// unparseable body, but either way it is a 4xx and never a 5xx.
	bad := `{"accountID":"1","type":"item.update","timestamp":1,"payload":{"Item":{
	  "itemID":"1","customSku":"A","shopID":"3","Prices":{"ItemPrice":{"amount":"free","useType":"Default"}}}}}`
	_, err := a.Ingest(context.Background(), testDelivery(t, bad, ""))
	if cls := adapter.Classify(err); cls.Kind.HTTPStatus() >= 500 {
		t.Errorf("status %d for an unusable price", cls.Kind.HTTPStatus())
	}
}

func TestWasPriceIsSuppressedWhenNotHigher(t *testing.T) {
	a := New()
	body := `{"accountID":"1","type":"item.update","timestamp":1,"payload":{"Item":{
	  "itemID":"1","customSku":"A","shopID":"3","Prices":{"ItemPrice":[
	    {"amount":"12.99","useType":"Default"},
	    {"amount":"9.99","useType":"MSRP"}
	  ]}}}}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].WasPrice != nil {
		t.Errorf("was = %+v; a struck-through price below the shelf price is misleading", changes[0].WasPrice)
	}
}
