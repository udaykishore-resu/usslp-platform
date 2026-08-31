package ncr

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const (
	keyID     = "AKID-USSLP-01"
	secretKey = "ncr-shared-secret"
	org       = "acme-retail"
	path      = "/v1/ingest/acme/ncr-prod"
)

// xmlMessage is the shape an older Advanced-Store estate posts.
const xmlMessage = `<?xml version="1.0" encoding="UTF-8"?>
<ItemPriceMessage xmlns="http://ncr.com/retail/pricing/v1">
  <MessageId>NCR-20260830-000173</MessageId>
  <SiteId>0042</SiteId>
  <Currency>USD</Currency>
  <GeneratedAt>2026-08-30T14:02:11Z</GeneratedAt>
  <Item>
    <ItemCode>0000123456</ItemCode>
    <Description>Espresso Beans 1kg</Description>
    <PriceMode>REGULAR</PriceMode>
    <UnitPrice>12.99</UnitPrice>
    <WasPrice>15.50</WasPrice>
    <UnitOfMeasure>EA</UnitOfMeasure>
    <EffectiveDate>2026-09-01T00:00:00Z</EffectiveDate>
  </Item>
  <Item>
    <ItemCode>0000123457</ItemCode>
    <PriceMode>PROMOTION</PriceMode>
    <UnitPrice>3.75</UnitPrice>
    <PromotionId>PROMO-77</PromotionId>
    <ExpiryDate>2026-09-30</ExpiryDate>
  </Item>
  <Item>
    <ItemCode>0000123458</ItemCode>
    <PriceMode>COST</PriceMode>
    <UnitPrice>1.10</UnitPrice>
  </Item>
</ItemPriceMessage>`

// jsonMessage is the same message from a current-BSP site.
const jsonMessage = `{
  "messageId": "NCR-20260830-000173",
  "siteId": "0042",
  "currency": "USD",
  "generatedAt": "2026-08-30T14:02:11Z",
  "items": [
    {"itemCode":"0000123456","priceMode":"REGULAR","unitPrice":"12.99","wasPrice":"15.50","unitOfMeasure":"EA"},
    {"itemCode":"0000123457","priceMode":"PROMOTION","unitPrice":"3.75","promotionId":"PROMO-77"}
  ]
}`

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "ncr-prod", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "USD",
		Secrets:         adapter.Secrets{APIKeyID: keyID, APIKey: secretKey},
	}
	if optionsJSON == "" {
		optionsJSON = `{"organization":"` + org + `"}`
	}
	b.Options = json.RawMessage(optionsJSON)
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "ncr-prod")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDelivery(t *testing.T, body, contentType, optionsJSON string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, optionsJSON)
	now := time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC)
	date := now.Format(time.RFC1123)
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Method: http.MethodPost, Path: path, ContentType: contentType,
		Body:       []byte(body),
		ReceivedAt: now,
		Headers:    http.Header{},
	}
	// Set rather than a map literal: http.Header canonicalises key case on Get,
	// and a literal written in the wire's own casing would silently miss.
	d.Headers.Set(HeaderAPIKey, keyID)
	d.Headers.Set(HeaderDate, date)
	d.Headers.Set(HeaderOrganization, org)
	d.Headers.Set(HeaderCorrelation, "ncr-corr-9")
	canonical := CanonicalString(d.Method, d.Path, contentType, date, org, d.Body)
	sig := adapter.EncodeSignature(adapter.SignHMACSHA256(secretKey, []byte(canonical)), adapter.EncodingBase64)
	d.Headers.Set(HeaderAuthorization, AuthScheme+keyID+":"+sig)
	return d
}

func TestIngestXMLAndJSONProduceIdenticalChanges(t *testing.T) {
	a := New()
	ctx := context.Background()

	xmlDelivery := testDelivery(t, xmlMessage, "application/xml", "")
	if err := a.Verify(ctx, xmlDelivery); err != nil {
		t.Fatalf("Verify(xml): %v", err)
	}
	fromXML, err := a.Ingest(ctx, xmlDelivery)
	if err != nil {
		t.Fatalf("Ingest(xml): %v", err)
	}
	// The COST item is not a shelf price and is skipped without being an error.
	if len(fromXML) != 2 {
		t.Fatalf("xml produced %d changes, want 2", len(fromXML))
	}

	jsonDelivery := testDelivery(t, jsonMessage, "application/json", "")
	if err := a.Verify(ctx, jsonDelivery); err != nil {
		t.Fatalf("Verify(json): %v", err)
	}
	fromJSON, err := a.Ingest(ctx, jsonDelivery)
	if err != nil {
		t.Fatalf("Ingest(json): %v", err)
	}
	if len(fromJSON) != 2 {
		t.Fatalf("json produced %d changes, want 2", len(fromJSON))
	}

	// The whole point of supporting both encodings in one adapter is that a
	// chain running a mixture gets identical canonical events from both.
	for i := range fromXML {
		x, j := fromXML[i], fromJSON[i]
		if x.SKU != j.SKU || x.Price != j.Price || x.PromotionID != j.PromotionID {
			t.Errorf("encoding %d differs: xml %+v json %+v", i, x, j)
		}
	}

	c := fromXML[0]
	if c.SKU != "0000123456" || c.StoreID != "0042" {
		t.Errorf("identity = %q @ %q", c.SKU, c.StoreID)
	}
	if c.Price.Amount != 1299 || c.Price.Currency != "USD" {
		t.Errorf("price = %+v", c.Price)
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if c.Attributes["ncr_correlation_id"] != "ncr-corr-9" {
		t.Errorf("the retailer's correlation id was dropped: %v", c.Attributes)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if fromXML[1].ExpiresAt == nil {
		t.Error("the promotion's expiry date was dropped")
	}
	if fromXML[1].PromotionID != "PROMO-77" {
		t.Errorf("promotion = %q", fromXML[1].PromotionID)
	}
}

func TestContentTypeSniffingForMislabelledStoreServers(t *testing.T) {
	a := New()
	// A store server posting XML as text/plain is common enough that refusing
	// it would be technically correct and commercially useless.
	d := testDelivery(t, xmlMessage, "text/plain", "")
	changes, err := a.Ingest(context.Background(), d)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %d, err = %v", len(changes), err)
	}
	// A UTF-8 byte order mark ahead of the declaration must not defeat the
	// sniff.
	withBOM := testDelivery(t, utf8BOM+xmlMessage, "text/plain", "")
	if changes, err := a.Ingest(context.Background(), withBOM); err != nil || len(changes) != 2 {
		t.Fatalf("BOM-prefixed XML: %d changes, err = %v", len(changes), err)
	}
}

func TestVerifyBindsTheSignatureToMethodPathAndBody(t *testing.T) {
	a := New()
	ctx := context.Background()

	// A signature captured from one request must not authorise another path.
	d := testDelivery(t, jsonMessage, "application/json", "")
	d.Path = "/v1/ingest/acme/other-binding"
	if err := a.Verify(ctx, d); err == nil {
		t.Fatal("a signature was accepted for a different path")
	}

	// Nor a different body.
	d2 := testDelivery(t, jsonMessage, "application/json", "")
	d2.Body = []byte(`{"messageId":"X","items":[]}`)
	if err := a.Verify(ctx, d2); err == nil {
		t.Fatal("a signature was accepted for a different body")
	}

	// Nor a different key.
	d3 := testDelivery(t, jsonMessage, "application/json", "")
	d3.Binding.Secrets.APIKey = "rotated"
	if err := a.Verify(ctx, d3); err == nil {
		t.Fatal("a signature was accepted under a rotated key")
	}

	// A credential naming another organization must be refused even when the
	// signature checks out, since a shared key would otherwise let one retailer
	// post another's prices.
	d4 := testDelivery(t, jsonMessage, "application/json", "")
	d4.Headers.Set(HeaderOrganization, "someone-else")
	if adapter.Classify(a.Verify(ctx, d4)).Reason != "wrong_organization" {
		t.Error("the organization check did not fire")
	}

	// Malformed credentials are unauthorized, not malformed: an attacker must
	// not be able to choose which error path they take.
	for _, auth := range []string{"", "Bearer x", AuthScheme + "nocolon", AuthScheme + "id:"} {
		d5 := testDelivery(t, jsonMessage, "application/json", "")
		d5.Headers.Set(HeaderAuthorization, auth)
		if cls := adapter.Classify(a.Verify(ctx, d5)); cls.Kind != adapter.FailureUnauthorized {
			t.Errorf("auth %q classified as %s", auth, cls.Kind)
		}
	}
}

func TestVerifyRejectsAStaleDate(t *testing.T) {
	a := New()
	d := testDelivery(t, jsonMessage, "application/json", "")
	// A captured request replayed a day later must fail even though its
	// signature is still valid, which is exactly what the Date being inside the
	// signed string buys.
	d.ReceivedAt = d.ReceivedAt.Add(24 * time.Hour)
	if cls := adapter.Classify(a.Verify(context.Background(), d)); cls.Reason != "stale_date" {
		t.Fatalf("reason = %q, want stale_date", cls.Reason)
	}

	// A site whose clock cannot be fixed can disable the check explicitly.
	relaxed := testDelivery(t, jsonMessage, "application/json",
		`{"organization":"`+org+`","clock_skew":"0s"}`)
	relaxed.ReceivedAt = relaxed.ReceivedAt.Add(24 * time.Hour)
	if err := a.Verify(context.Background(), relaxed); err != nil {
		t.Fatalf("with the check disabled: %v", err)
	}
}

func TestIdempotencyUsesTheMessageID(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, xmlMessage, "application/xml", ""))
	if len(parts) != 2 || parts[0] != "0042" || parts[1] != "NCR-20260830-000173" {
		t.Fatalf("parts = %v", parts)
	}
	// The XML and JSON encodings of the same message must dedupe against each
	// other: a store re-sending the same outbox entry in a different encoding
	// is still the same price decision.
	jsonParts := a.IdempotencyParts(testDelivery(t, jsonMessage, "application/json", ""))
	if len(jsonParts) != 2 || jsonParts[1] != parts[1] {
		t.Fatalf("json parts = %v, want the same identity as %v", jsonParts, parts)
	}
	if got := a.IdempotencyParts(testDelivery(t, `{"items":[]}`, "application/json", "")); got != nil {
		t.Errorf("a message with no id must defer to the body digest, got %v", got)
	}
}

func TestBadRowsAreIsolated(t *testing.T) {
	a := New()
	body := `{"messageId":"M","siteId":"S","currency":"USD","items":[
	  {"itemCode":"A","priceMode":"REGULAR","unitPrice":"1.00"},
	  {"itemCode":"","priceMode":"REGULAR","unitPrice":"2.00"},
	  {"itemCode":"C","priceMode":"REGULAR","unitPrice":"not a price"},
	  {"itemCode":"D","priceMode":"REGULAR","unitPrice":"4.00"}
	]}`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, "application/json", ""))
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want the two usable ones", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 2 {
		t.Fatalf("err = %v", err)
	}
	if partial.Failures[0].Reason != "missing_item_code" || partial.Failures[1].Reason != "price_unusable" {
		t.Errorf("failures = %+v", partial.Failures)
	}
	if partial.Total != 4 {
		t.Errorf("Total = %d, want 4", partial.Total)
	}
}

func TestMalformedMessagesAreQuarantinable(t *testing.T) {
	a := New()
	for name, tc := range map[string]struct{ body, ct string }{
		"broken xml":  {`<ItemPriceMessage><Item>`, "application/xml"},
		"broken json": {`{"messageId":`, "application/json"},
		"no items":    {`{"messageId":"M","items":[]}`, "application/json"},
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, tc.body, tc.ct, ""))
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d, a body that will never parse must not be retried", name, cls.Kind.HTTPStatus())
		}
	}
}

func TestPriceModeFiltering(t *testing.T) {
	a := New()
	d := testDelivery(t, xmlMessage, "application/xml",
		`{"organization":"`+org+`","price_modes":["COST"]}`)
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].SKU != "0000123458" {
		t.Fatalf("changes = %+v, want only the COST item", changes)
	}
}

func TestCompileOptionsRejectsNonsense(t *testing.T) {
	a := New()
	for _, bad := range []string{`{"clock_skew":"soon"}`, `{"clock_skew":"-5m"}`, `{"mystery":1}`} {
		if _, err := a.CompileOptions(json.RawMessage(bad)); err == nil {
			t.Errorf("options %s were accepted", bad)
		}
	}
}
