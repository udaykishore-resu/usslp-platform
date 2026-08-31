package generic

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const signingKey = "kassenwerk-shared-secret"

// kassenwerkMapping describes a POS shape none of the hand-written adapters
// handles: a fictional "Kassenwerk 4000" ERP that nests price rows under site
// headers, sends integer mantissas with a per-row decimal-shift field,
// lower-cases its currency codes, and signs its bodies with a hex HMAC behind a
// "sha256=" prefix.
//
// Integrating it takes this document and nothing else — no Go, no build, no
// deploy. That is the whole claim this adapter exists to make good on.
const kassenwerkMapping = `{
  "version": 1,
  "name": "kassenwerk-4000",
  "source_system": "kw4000",
  "verify": {"type":"hmac_sha256","header":"X-KW-Signature","encoding":"hex","prefix":"sha256="},
  "group": "$.kw4000.sites[*]",
  "root": "$.rows[*]",
  "idempotency": ["$$.kw4000.batch.id", "$$.kw4000.batch.generated"],
  "fields": {
    "sku":          {"path":"$.art", "strip_leading_zeros": true, "trim": true},
    "store":        {"path":"$^.siteCode"},
    "currency":     {"path":"$^.cur", "upper": true},
    "price":        {"type":"shifted", "path":"$.amt", "scale_path":"$.exp"},
    "was_price":    {"type":"shifted", "path":"$.prev", "scale_path":"$.exp", "optional": true},
    "effective_at": {"type":"time", "path":"$.from", "layout":"compact_date", "optional": true},
    "promotion_id": {"path":"$.promo", "optional": true},
    "reason":       {"const":"kw4000-nightly"}
  },
  "attributes": {
    "kw_batch": {"path":"$$.kw4000.batch.id"}
  }
}`

const kassenwerkPayload = `{
  "kw4000": {
    "batch": {"id":"B-88231","generated":"20260830T100211"},
    "sites": [
      {"siteCode":"K-01","cur":"kwd","rows":[
        {"art":"0000012345","amt":"2499","exp":3,"prev":"2999","from":"20260901","promo":"P7"},
        {"art":"0000067890","amt":"7505","exp":4}
      ]},
      {"siteCode":"K-02","cur":"jpy","rows":[
        {"art":"0000012345","amt":"24904","exp":2}
      ]}
    ]
  }
}`

func bindWith(t *testing.T, mapping string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "kw4000", TenantID: "acme", Adapter: Name,
		Secrets: adapter.Secrets{HMACKey: signingKey},
		Options: json.RawMessage(mapping),
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("installing the mapping binding: %v", err)
	}
	got, err := store.Get("acme", "kw4000")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDelivery(t *testing.T, body, mapping string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, mapping)
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Method: http.MethodPost, Path: "/v1/ingest/acme/kw4000",
		ContentType: "application/json",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 10, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
	d.Headers.Set("X-KW-Signature",
		"sha256="+adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d.Body), adapter.EncodingHex))
	return d
}

func TestIngestANovelShapeWithNoGoCode(t *testing.T) {
	a := New()
	d := testDelivery(t, kassenwerkPayload, kassenwerkMapping)
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3", len(changes))
	}

	c := changes[0]
	if c.SKU != "12345" {
		t.Errorf("sku = %q, want the zero-stripped form", c.SKU)
	}
	if c.StoreID != "K-01" {
		t.Errorf("store = %q; the group scope is what makes a site header reachable from a row", c.StoreID)
	}
	// KWD is a three-decimal currency: 2499 shifted by 3 is 2.499 KWD.
	if c.Price.Currency != "KWD" || c.Price.Amount != 2499 || c.Price.String() != "2.499 KWD" {
		t.Errorf("price = %+v (%s)", c.Price, c.Price.String())
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 2999 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if c.Reason != "kw4000-nightly" || c.SourceSystem != "kw4000" {
		t.Errorf("constants = %q / %q", c.Reason, c.SourceSystem)
	}
	if c.Attributes["kw_batch"] != "B-88231" {
		t.Errorf("attributes = %v", c.Attributes)
	}
	// 7505 shifted by 4 is 0.7505 KWD, which rounds half away from zero to 751
	// fils rather than truncating to 750.
	if changes[1].Price.Amount != 751 {
		t.Errorf("sub-unit rounding = %d, want 751", changes[1].Price.Amount)
	}
	// JPY has no minor units: 24904 shifted by 2 is 249 yen.
	if changes[2].Price.Currency != "JPY" || changes[2].Price.Amount != 249 {
		t.Errorf("JPY = %+v", changes[2].Price)
	}
}

func TestVerifyHonoursTheMappingsSignatureSpec(t *testing.T) {
	a := New()
	ctx := context.Background()

	d := testDelivery(t, kassenwerkPayload, kassenwerkMapping)
	d.Body = append(d.Body, ' ')
	if err := a.Verify(ctx, d); err == nil {
		t.Fatal("a tampered body was accepted")
	}
	d2 := testDelivery(t, kassenwerkPayload, kassenwerkMapping)
	d2.Headers.Set("X-KW-Signature",
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d2.Body), adapter.EncodingHex))
	if err := a.Verify(ctx, d2); err == nil {
		t.Fatal("a signature missing the configured prefix was accepted")
	}

	// A mapping that declares no verification at all is refused rather than
	// treated as unauthenticated-by-choice: configuration that silently opens a
	// price endpoint is the failure this check exists to prevent.
	noVerify := `{"name":"open","fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"USD"}}}`
	d3 := testDelivery(t, `{"sku":"A","price":"1.00"}`, noVerify)
	if cls := adapter.Classify(a.Verify(ctx, d3)); cls.Reason != "no_verification" {
		t.Errorf("reason = %q, want no_verification", cls.Reason)
	}

	// Stating it explicitly is allowed, because then it is a decision on the
	// record rather than an omission.
	explicit := `{"name":"open","verify":{"type":"none"},
	  "fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"USD"}}}`
	d4 := testDelivery(t, `{"sku":"A","price":"1.00"}`, explicit)
	if err := a.Verify(ctx, d4); err != nil {
		t.Errorf("an explicit \"none\" was rejected: %v", err)
	}

	// A shared-secret header is the other supported shape.
	shared := `{"name":"tok","verify":{"type":"shared_secret","header":"X-Token"},
	  "fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"USD"}}}`
	d5 := testDelivery(t, `{"sku":"A","price":"1.00"}`, shared)
	d5.Binding.Secrets.SharedToken = "abc"
	d5.Headers.Set("X-Token", "abc")
	if err := a.Verify(ctx, d5); err != nil {
		t.Errorf("a matching shared secret was rejected: %v", err)
	}
	d5.Headers.Set("X-Token", "abd")
	if err := a.Verify(ctx, d5); err == nil {
		t.Error("a mismatched shared secret was accepted")
	}
}

func TestSquareStyleURLSigningThroughAMapping(t *testing.T) {
	a := New()
	doc := `{"name":"urlsigned","verify":{"type":"hmac_sha256","header":"X-Sig","sign_url":true},
	  "fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"USD"}}}`
	d := testDelivery(t, `{"sku":"A","price":"1.00"}`, doc)
	d.Binding.Secrets.NotificationURL = "https://uig.example/hook"
	signed := append([]byte("https://uig.example/hook"), d.Body...)
	d.Headers.Set("X-Sig", adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, signed), adapter.EncodingBase64))
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("URL-signed verification failed: %v", err)
	}
	// Without the configured URL the adapter refuses to guess.
	d.Binding.Secrets.NotificationURL = ""
	if cls := adapter.Classify(a.Verify(context.Background(), d)); cls.Reason != "no_notification_url" {
		t.Errorf("reason = %q", cls.Reason)
	}
}

func TestIdempotencyComesFromTheMapping(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, kassenwerkPayload, kassenwerkMapping))
	if len(parts) != 2 || parts[0] != "B-88231" {
		t.Fatalf("parts = %v", parts)
	}
	// A mapping with no idempotency block defers to the body digest.
	doc := `{"name":"x","verify":{"type":"none"},
	  "fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"USD"}}}`
	if got := a.IdempotencyParts(testDelivery(t, `{"sku":"A","price":"1.00"}`, doc)); got != nil {
		t.Errorf("parts = %v, want nil", got)
	}
}

func TestPayloadMismatchesAreQuarantinable(t *testing.T) {
	a := New()
	for name, body := range map[string]string{
		"not json":     `<xml/>`,
		"root missing": `{"kw4000":{"batch":{"id":"B"},"sites":[]}}`,
		"bad shift":    `{"kw4000":{"batch":{"id":"B"},"sites":[{"siteCode":"S","cur":"usd","rows":[{"art":"A","amt":"1","exp":"x"}]}]}}`,
		"bad amount":   `{"kw4000":{"batch":{"id":"B"},"sites":[{"siteCode":"S","cur":"usd","rows":[{"art":"A","amt":"free","exp":2}]}]}}`,
		"bad currency": `{"kw4000":{"batch":{"id":"B"},"sites":[{"siteCode":"S","cur":"dollars","rows":[{"art":"A","amt":"1","exp":2}]}]}}`,
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, body, kassenwerkMapping))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s, want malformed so it is quarantined and replayable", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d — a mapping failure will never fix itself on retry",
				name, cls.Kind.HTTPStatus())
		}
	}
}

func TestABrokenMappingIsRefusedAtInstallTime(t *testing.T) {
	a := New()
	// The whole safety story for a configuration-driven adapter: a typo fails
	// in front of the engineer who wrote it, not at midnight in front of a
	// shopper.
	for name, doc := range map[string]string{
		"empty":          ``,
		"typo in field":  `{"name":"x","fields":{"prcie":{"path":"$.p"},"sku":{"path":"$.s"},"price":{"path":"$.p"},"currency":{"const":"USD"}}}`,
		"bad selector":   `{"name":"x","fields":{"sku":{"path":"nope"},"price":{"path":"$.p"},"currency":{"const":"USD"}}}`,
		"missing sku":    `{"name":"x","fields":{"price":{"path":"$.p"},"currency":{"const":"USD"}}}`,
		"unknown verify": `{"name":"x","verify":{"type":"magic"},"fields":{"sku":{"path":"$.s"},"price":{"path":"$.p"},"currency":{"const":"USD"}}}`,
	} {
		var raw json.RawMessage
		if doc != "" {
			raw = json.RawMessage(doc)
		}
		if _, err := a.CompileOptions(raw); err == nil {
			t.Errorf("%s: a broken mapping was installed", name)
		}
	}
	if _, err := a.CompileOptions(json.RawMessage(kassenwerkMapping)); err != nil {
		t.Fatalf("a valid mapping was rejected: %v", err)
	}
}

func TestAnUncompiledBindingFailsSafely(t *testing.T) {
	a := New()
	// A binding whose options never compiled must not silently ingest
	// unverified traffic.
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: "kw4000",
		Binding: &adapter.Binding{ID: "kw4000", TenantID: "acme", Adapter: Name},
		Body:    []byte(`{}`),
		Headers: http.Header{},
	}
	if err := a.Verify(context.Background(), d); err == nil {
		t.Error("a binding with no compiled mapping was verified")
	}
	if _, err := a.Ingest(context.Background(), d); err == nil {
		t.Error("a binding with no compiled mapping was ingested")
	}
	if got := a.IdempotencyParts(d); got != nil {
		t.Errorf("parts = %v, want nil", got)
	}
}
