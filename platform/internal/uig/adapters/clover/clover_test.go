package clover

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
)

const (
	authCode     = "clover-verification-code"
	bearer       = "clover-oauth-token"
	notification = `{
  "appId": "ACME_ESL_APP",
  "merchants": {
    "MERCH_ONE": [
      {"objectId": "I:ITEM_ESP", "type": "UPDATE", "ts": 1756563731000},
      {"objectId": "I:ITEM_TEA", "type": "CREATE", "ts": 1756563732000},
      {"objectId": "I:ITEM_GONE", "type": "UPDATE", "ts": 1756563733000},
      {"objectId": "I:ITEM_DEAD", "type": "DELETE", "ts": 1756563734000},
      {"objectId": "O:ORDER_1", "type": "UPDATE", "ts": 1756563735000}
    ]
  }
}`
)

// fakeFetcher stands in for the Clover API. It is a fake rather than an HTTP
// server for the tests that care about breaker behaviour, because those need to
// fail deterministically without a socket in the way.
type fakeFetcher struct {
	mu     sync.Mutex
	items  map[string]*Item
	fail   error
	calls  atomic.Int32
	perr   map[string]error
	seenTk []string
}

func (f *fakeFetcher) FetchItem(_ context.Context, _ *url.URL, _, itemID, token string) (*Item, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seenTk = append(f.seenTk, token)
	if f.fail != nil {
		return nil, f.fail
	}
	if err, ok := f.perr[itemID]; ok {
		return nil, err
	}
	it, ok := f.items[itemID]
	if !ok {
		return nil, ErrItemGone
	}
	return it, nil
}

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New(nil, nil))
	store := adapter.NewBindingStore(reg)
	if optionsJSON == "" {
		optionsJSON = `{"base_url":"https://api.clover.com"}`
	}
	b := &adapter.Binding{
		ID: "clover-1", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "USD",
		Secrets:         adapter.Secrets{SharedToken: authCode, BearerToken: bearer},
		Options:         json.RawMessage(optionsJSON),
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "clover-1")
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
		Method: http.MethodPost, Path: "/v1/ingest/acme/clover-1",
		ContentType: "application/json",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
	d.Headers.Set(HeaderAuth, authCode)
	return d
}

func defaultItems() map[string]*Item {
	return map[string]*Item{
		"ITEM_ESP": {
			ID: "ITEM_ESP", Name: "Espresso 1kg", SKU: "ESP-1KG",
			Price: json.Number("1299"), PriceType: "FIXED", ModifiedTime: json.Number("1756563731000"),
		},
		"ITEM_TEA": {
			ID: "ITEM_TEA", Name: "Loose Tea", SKU: "TEA-500",
			Price: json.Number("450"), PriceType: "PER_UNIT", Unit: "100g",
		},
	}
}

func TestIngestFetchesEachReferencedItem(t *testing.T) {
	f := &fakeFetcher{items: defaultItems()}
	a := New(f, reliability.NewBreakerSet(reliability.BreakerConfig{}))
	d := testDelivery(t, notification, "")

	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)

	// ITEM_GONE was deleted between the webhook being queued and our fetch,
	// which is normal merchant behaviour and a row failure rather than an
	// outage. ITEM_DEAD is a DELETE and is skipped. O:ORDER_1 is not an item.
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(changes), changes)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 1 || partial.Failures[0].Reason != "item_gone" {
		t.Fatalf("err = %v", err)
	}

	c := changes[0]
	if c.SKU != "ESP-1KG" || c.StoreID != "MERCH_ONE" {
		t.Errorf("identity = %q @ %q", c.SKU, c.StoreID)
	}
	// Clover prices are already in minor units, so no conversion can go wrong.
	if c.Price.Amount != 1299 || c.Price.Currency != "USD" {
		t.Errorf("price = %+v", c.Price)
	}
	if c.Attributes["clover_app_id"] != "ACME_ESL_APP" {
		t.Errorf("attributes = %v", c.Attributes)
	}
	if !c.EffectiveAt.Equal(time.UnixMilli(1756563731000).UTC()) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if changes[1].UnitMeasure != "100g" {
		t.Errorf("per-unit item measure = %q", changes[1].UnitMeasure)
	}
	// The binding's bearer token, not the webhook's auth code, authenticates
	// the outbound call.
	for _, tk := range f.seenTk {
		if tk != bearer {
			t.Errorf("outbound call used token %q", tk)
		}
	}
}

func TestVerifyComparesTheAuthCode(t *testing.T) {
	a := New(&fakeFetcher{items: defaultItems()}, nil)
	ctx := context.Background()

	d := testDelivery(t, notification, "")
	d.Headers.Set(HeaderAuth, "wrong-code")
	if err := a.Verify(ctx, d); err == nil {
		t.Fatal("a wrong auth code was accepted")
	}
	d2 := testDelivery(t, notification, "")
	d2.Headers.Del(HeaderAuth)
	if err := a.Verify(ctx, d2); err == nil {
		t.Fatal("a missing auth code was accepted")
	}
	d3 := testDelivery(t, notification, "")
	d3.Binding.Secrets.SharedToken = ""
	if err := a.Verify(ctx, d3); err == nil {
		t.Fatal("a binding with no configured code must fail closed")
	}
}

func TestIdempotencyIsStableAcrossMapOrdering(t *testing.T) {
	a := New(&fakeFetcher{items: defaultItems()}, nil)
	body := `{"appId":"APP","merchants":{
	  "M2":[{"objectId":"I:B","type":"UPDATE","ts":2}],
	  "M1":[{"objectId":"I:A","type":"UPDATE","ts":1}]}}`
	first := a.IdempotencyParts(testDelivery(t, body, ""))
	// Go randomises map iteration; without sorting, the same redelivery would
	// hash differently on every attempt and defeat deduplication entirely.
	for i := 0; i < 50; i++ {
		again := a.IdempotencyParts(testDelivery(t, body, ""))
		if len(again) != len(first) {
			t.Fatalf("unstable length: %v vs %v", again, first)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("unstable identity on attempt %d: %v vs %v", i, again, first)
			}
		}
	}
	if len(first) != 3 || first[0] != "APP" {
		t.Fatalf("parts = %v", first)
	}
	if !strings.HasPrefix(first[1], "M1|") || !strings.HasPrefix(first[2], "M2|") {
		t.Errorf("merchants are not in sorted order: %v", first)
	}
}

func TestUpstreamOutageIsRetryableNotAPartialPriceBook(t *testing.T) {
	f := &fakeFetcher{items: defaultItems(), fail: errors.New("connection refused")}
	a := New(f, reliability.NewBreakerSet(reliability.BreakerConfig{FailureThreshold: 100}))
	changes, err := a.Ingest(context.Background(), testDelivery(t, notification, ""))

	// A Clover outage is the platform's problem, not the retailer's: the whole
	// delivery is refused with a retryable classification so Clover redelivers,
	// rather than half a price book being published.
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	cls := adapter.Classify(err)
	if cls.Kind != adapter.FailureUnavailable {
		t.Fatalf("kind = %s, want unavailable", cls.Kind)
	}
	if cls.Kind.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", cls.Kind.HTTPStatus())
	}
}

func TestCircuitBreakerStopsHammeringADeadDependency(t *testing.T) {
	f := &fakeFetcher{items: defaultItems(), fail: errors.New("connection refused")}
	breakers := reliability.NewBreakerSet(reliability.BreakerConfig{
		FailureThreshold: 2, Cooldown: time.Hour, HalfOpenProbes: 1,
	})
	a := New(f, breakers)
	ctx := context.Background()

	// The first delivery burns the breaker's budget.
	if _, err := a.Ingest(ctx, testDelivery(t, notification, "")); err == nil {
		t.Fatal("expected an outage error")
	}
	callsAfterFirst := f.calls.Load()
	if callsAfterFirst == 0 {
		t.Fatal("the fetcher was never called")
	}
	if breakers.Get(Name+"/MERCH_ONE").State() != reliability.StateOpen {
		t.Fatalf("the circuit did not open; state = %s", breakers.Get(Name+"/MERCH_ONE").State())
	}

	// Once open, no further calls are attempted at all: a POS timing out at 30
	// seconds would otherwise consume the gateway's entire 50ms budget on every
	// delivery.
	if _, err := a.Ingest(ctx, testDelivery(t, notification, "")); err == nil {
		t.Fatal("expected the open circuit to fail the delivery")
	}
	if got := f.calls.Load(); got != callsAfterFirst {
		t.Errorf("the fetcher was called %d more times while the circuit was open", got-callsAfterFirst)
	}
	if breakers.Get(Name+"/MERCH_ONE").Rejected() == 0 {
		t.Error("the breaker recorded no rejections")
	}
}

func TestDeletedItemsDoNotOpenTheCircuit(t *testing.T) {
	// A merchant tidying their menu produces a run of 404s. Those are Clover
	// answering correctly and quickly; counting them as failures would open a
	// circuit against a healthy dependency.
	f := &fakeFetcher{items: map[string]*Item{}}
	breakers := reliability.NewBreakerSet(reliability.BreakerConfig{FailureThreshold: 2, Cooldown: time.Hour})
	a := New(f, breakers)
	for i := 0; i < 5; i++ {
		_, err := a.Ingest(context.Background(), testDelivery(t, notification, ""))
		if _, ok := adapter.IsPartial(err); !ok {
			t.Fatalf("attempt %d err = %v, want per-item failures", i, err)
		}
	}
	if st := breakers.Get(Name + "/MERCH_ONE").State(); st != reliability.StateClosed {
		t.Fatalf("deleted items opened the circuit; state = %s", st)
	}
}

func TestHTTPFetcherAgainstARecordedShapeServer(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/items/ITEM_ESP"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ITEM_ESP","name":"Espresso 1kg","sku":"ESP-1KG",
			  "price":1299,"priceType":"FIXED","modifiedTime":1756563731000}`))
		case strings.HasSuffix(r.URL.Path, "/items/ITEM_GONE"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.Client())
	base, _ := url.Parse(srv.URL)
	ctx := context.Background()

	item, err := f.FetchItem(ctx, base, "MERCH_ONE", "ITEM_ESP", bearer)
	if err != nil {
		t.Fatalf("FetchItem: %v", err)
	}
	if item.SKU != "ESP-1KG" || item.Price.String() != "1299" {
		t.Fatalf("item = %+v", item)
	}
	if gotAuth != "Bearer "+bearer {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v3/merchants/MERCH_ONE/items/ITEM_ESP" {
		t.Errorf("path = %q", gotPath)
	}

	// A 404 must be a per-item condition, never an outage.
	if _, err := f.FetchItem(ctx, base, "MERCH_ONE", "ITEM_GONE", bearer); !errors.Is(err, ErrItemGone) {
		t.Errorf("404 err = %v, want ErrItemGone", err)
	}
	// A 500 is an outage.
	if _, err := f.FetchItem(ctx, base, "MERCH_ONE", "ITEM_OTHER", bearer); !errors.Is(err, ErrUpstream) {
		t.Errorf("500 err = %v, want ErrUpstream", err)
	}
	// A missing token fails with an actionable message rather than producing a
	// stream of 401s that look like a Clover outage.
	if _, err := f.FetchItem(ctx, base, "MERCH_ONE", "ITEM_ESP", ""); err == nil ||
		!strings.Contains(err.Error(), "bearer token") {
		t.Errorf("missing-token err = %v", err)
	}
}

func TestEndToEndThroughTheHTTPFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"X","name":"Thing","sku":"SKU-X","price":250,"priceType":"FIXED"}`))
	}))
	defer srv.Close()

	a := New(NewHTTPFetcher(srv.Client()), nil)
	d := testDelivery(t, `{"appId":"A","merchants":{"M":[{"objectId":"I:X","type":"UPDATE","ts":1}]}}`,
		`{"base_url":"`+srv.URL+`","fetch_timeout":"2s"}`)
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 1 || changes[0].SKU != "SKU-X" || changes[0].Price.Amount != 250 {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestCompileOptionsRequiresABaseURL(t *testing.T) {
	a := New(nil, nil)
	for _, bad := range []string{``, `{}`, `{"base_url":"not a url"}`, `{"base_url":"https://x","fetch_timeout":"soon"}`, `{"base_url":"https://x","mystery":1}`} {
		var raw json.RawMessage
		if bad != "" {
			raw = json.RawMessage(bad)
		}
		if _, err := a.CompileOptions(raw); err == nil {
			t.Errorf("options %q were accepted; pointing a live merchant at the wrong host 404s every item", bad)
		}
	}
	if _, err := a.CompileOptions(json.RawMessage(`{"base_url":"https://api.clover.com","cooldown":"30s"}`)); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
}

func TestMalformedNotifications(t *testing.T) {
	a := New(&fakeFetcher{items: defaultItems()}, nil)
	for name, body := range map[string]string{
		"not json":     `nope`,
		"no merchants": `{"appId":"A","merchants":{}}`,
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
		if cls := adapter.Classify(err); cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s", name, cls.Kind)
		}
	}
}

func TestUnusableItemFieldsAreRowFailures(t *testing.T) {
	f := &fakeFetcher{items: map[string]*Item{
		"ITEM_ESP":  {ID: "ITEM_ESP", SKU: "OK", Price: json.Number("100"), PriceType: "FIXED"},
		"ITEM_TEA":  {ID: "ITEM_TEA", SKU: "BAD", Price: json.Number("12.50"), PriceType: "FIXED"},
		"ITEM_GONE": {ID: "ITEM_GONE", SKU: "VAR", PriceType: "VARIABLE"},
	}}
	a := New(f, nil)
	changes, err := a.Ingest(context.Background(), testDelivery(t, notification, ""))
	if len(changes) != 1 || changes[0].SKU != "OK" {
		t.Fatalf("changes = %+v", changes)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 1 || partial.Failures[0].Reason != "price_unusable" {
		t.Fatalf("err = %v", err)
	}
}
