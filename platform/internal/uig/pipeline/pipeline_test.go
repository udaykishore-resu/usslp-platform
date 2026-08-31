package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// capturePublisher records what the pipeline published and can be made to fail,
// which is the only way to exercise the "durability failed" path without
// breaking a real disk.
type capturePublisher struct {
	mu       sync.Mutex
	msgs     []eventbus.Message
	failWith error
	calls    atomic.Int32
}

func (p *capturePublisher) Publish(_ context.Context, msgs ...eventbus.Message) error {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failWith != nil {
		return p.failWith
	}
	p.msgs = append(p.msgs, msgs...)
	return nil
}

func (p *capturePublisher) Close() error { return nil }

func (p *capturePublisher) captured() []eventbus.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]eventbus.Message, len(p.msgs))
	copy(out, p.msgs)
	return out
}

func (p *capturePublisher) reset() {
	p.mu.Lock()
	p.msgs = nil
	p.failWith = nil
	p.mu.Unlock()
}

// testAdapter is a programmable adapter. Using one rather than a real vendor
// adapter keeps these tests about the pipeline's stages, which is what they are
// for; the vendor adapters have their own tests.
type testAdapter struct {
	name    string
	verify  func(*adapter.Delivery) error
	ingest  func(*adapter.Delivery) ([]canon.PriceChangeRequested, error)
	idParts func(*adapter.Delivery) []string
	calls   atomic.Int32
}

func (a *testAdapter) Name() string {
	if a.name == "" {
		return "test"
	}
	return a.name
}

func (a *testAdapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if a.verify != nil {
		return a.verify(d)
	}
	return nil
}

func (a *testAdapter) IdempotencyParts(d *adapter.Delivery) []string {
	if a.idParts != nil {
		return a.idParts(d)
	}
	return []string{d.Header("X-Message-Id")}
}

func (a *testAdapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	a.calls.Add(1)
	if a.ingest != nil {
		return a.ingest(d)
	}
	return []canon.PriceChangeRequested{{
		SKU:     "ESP-1KG",
		StoreID: "0042",
		Price:   canon.Money{Amount: 249, Currency: "USD"},
	}}, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t        *testing.T
	pipe     *Pipeline
	pub      *capturePublisher
	adapter  *testAdapter
	bindings *adapter.BindingStore
	store    *deliveries.Store
	metrics  *Metrics
	guard    *idem.Guard
	clock    *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Advancing by a microsecond per read gives every delivery a strictly
	// increasing receipt time without any test having to sleep.
	c.t = c.t.Add(time.Microsecond)
	return c.t
}

func newHarness(t *testing.T, mutate ...func(*adapter.Binding)) *harness {
	t.Helper()
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatalf("kvstore: %v", err)
	}
	t.Cleanup(func() { kv.Close() })

	backend, err := idem.NewKVBackend(kv, "idem/")
	if err != nil {
		t.Fatal(err)
	}
	guard, err := idem.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	store, err := deliveries.New(kv, deliveries.Options{})
	if err != nil {
		t.Fatal(err)
	}

	ta := &testAdapter{}
	reg := adapter.NewRegistry()
	reg.MustRegister(ta)
	bindings := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "pos", TenantID: "acme", Adapter: ta.Name(),
		POSInstance:     "prod",
		DefaultCurrency: "USD",
		DefaultStore:    "GB-HQ",
		StoreMap:        map[string]canon.StoreID{"0042": "GB-0042"},
		InitiatedBy:     "pos:acme",
	}
	for _, m := range mutate {
		m(b)
	}
	if err := bindings.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}

	pub := &capturePublisher{}
	metrics := NewMetrics(obs.NewRegistry("service", "uig-test"))
	clk := &testClock{t: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
	pipe, err := New(Config{
		Registry:   reg,
		Bindings:   bindings,
		Guard:      guard,
		Bus:        pub,
		Deliveries: store,
		Limiter:    reliability.NewLimiter(),
		Metrics:    metrics,
		Log:        obs.NopLogger(),
		Region:     "eu-west-1",
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	t.Cleanup(func() { pipe.Close() })

	return &harness{
		t: t, pipe: pipe, pub: pub, adapter: ta, bindings: bindings,
		store: store, metrics: metrics, guard: guard, clock: clk,
	}
}

func (h *harness) delivery(msgID string, body string) *adapter.Delivery {
	return &adapter.Delivery{
		TenantID:    "acme",
		BindingID:   "pos",
		Method:      http.MethodPost,
		Path:        "/v1/ingest/acme/pos",
		URL:         "https://uig.example/v1/ingest/acme/pos",
		ContentType: "application/json",
		Headers:     http.Header{"X-Message-Id": []string{msgID}},
		Body:        []byte(body),
	}
}

func (h *harness) flush() {
	h.t.Helper()
	if err := h.pipe.Flush(context.Background()); err != nil {
		h.t.Fatalf("flush: %v", err)
	}
}

// decodeEnvelopes splits the captured messages by topic and decodes them.
func decodeEnvelopes(t *testing.T, msgs []eventbus.Message, topic string) []canon.Envelope {
	t.Helper()
	var out []canon.Envelope
	for _, m := range msgs {
		if m.Topic != topic {
			continue
		}
		var env canon.Envelope
		if err := json.Unmarshal(m.Value, &env); err != nil {
			t.Fatalf("decoding %s envelope: %v", topic, err)
		}
		out = append(out, env)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestIngestPublishesExactCanonicalEvents(t *testing.T) {
	h := newHarness(t)
	h.adapter.ingest = func(d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		d.SourceTime = time.Date(2026, 8, 30, 9, 59, 0, 0, time.UTC)
		return []canon.PriceChangeRequested{
			{SKU: "ESP-1KG", StoreID: "0042", Price: canon.Money{Amount: 249, Currency: "USD"}},
			{SKU: "TEA-500", StoreID: "0042", Price: canon.Money{Amount: 199}},
		}, nil
	}
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"ok":true}`))

	if res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("status = %d (%s: %s)", res.HTTPStatus, res.Reason, res.Detail)
	}
	if res.Status != deliveries.StatusAccepted || res.Emitted != 2 {
		t.Fatalf("result = %+v", res)
	}

	msgs := h.pub.captured()
	prices := decodeEnvelopes(t, msgs, canon.StreamPriceUpdates.Name)
	raw := decodeEnvelopes(t, msgs, canon.StreamPOSIngress.Name)
	if len(prices) != 2 || len(raw) != 1 {
		t.Fatalf("published %d price events and %d raw copies, want 2 and 1", len(prices), len(raw))
	}

	first := prices[0]
	if first.EventType != canon.EvtPriceChangeRequested {
		t.Errorf("event_type = %q", first.EventType)
	}
	if first.TenantID != "acme" || first.StoreID != "GB-0042" {
		t.Errorf("tenancy = %s/%s; the source code 0042 should have been enriched to GB-0042",
			first.TenantID, first.StoreID)
	}
	if first.Region != "eu-west-1" {
		t.Errorf("region = %q", first.Region)
	}
	if first.Source != "uig/test" {
		t.Errorf("source = %q, want uig/test", first.Source)
	}
	if first.SchemaVersion != canon.SchemaVersion {
		t.Errorf("schema_version = %d", first.SchemaVersion)
	}
	// OccurredAt is the POS clock; RecordedAt is when USSLP took responsibility.
	// Analytics depends on being able to tell a backfill from live traffic.
	if !first.OccurredAt.Equal(time.Date(2026, 8, 30, 9, 59, 0, 0, time.UTC)) {
		t.Errorf("occurred_at = %s, want the source clock", first.OccurredAt)
	}
	if !first.RecordedAt.After(first.OccurredAt) {
		t.Errorf("recorded_at %s must be after occurred_at %s", first.RecordedAt, first.OccurredAt)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("published envelope does not validate: %v", err)
	}

	// The partition key is store:sku, so two changes to the same product in the
	// same store are strictly ordered while different products proceed in
	// parallel (contract §2).
	for i, m := range msgs {
		if m.Topic != canon.StreamPriceUpdates.Name {
			continue
		}
		wantKey := "GB-0042:" + []string{"ESP-1KG", "TEA-500"}[i]
		if m.Key != wantKey {
			t.Errorf("price message %d key = %q, want %q", i, m.Key, wantKey)
		}
		if m.Headers[eventbus.HeaderTenantID] != "acme" {
			t.Errorf("message %d missing tenant header", i)
		}
		if m.Headers[eventbus.HeaderIdempotency] == "" {
			t.Errorf("message %d missing idempotency header", i)
		}
	}
	// Idempotency keys must be unique per event, so a delivery carrying 400
	// variants does not collapse to one event on an event-store replay.
	if prices[0].IdempotencyKey == prices[1].IdempotencyKey {
		t.Error("two events in one delivery share an idempotency key")
	}

	var pc canon.PriceChangeRequested
	if err := prices[0].Decode(&pc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if pc.SKU != "ESP-1KG" || pc.Price.Amount != 249 || pc.Price.Currency != "USD" {
		t.Errorf("payload = %+v", pc)
	}
	if pc.InitiatedBy != "pos:acme" {
		t.Errorf("initiated_by = %q, want the binding's value so an auditor sees the integration", pc.InitiatedBy)
	}
	if pc.SourceSystem != "test" {
		t.Errorf("source_system = %q", pc.SourceSystem)
	}
	// The second change carried no currency; the binding's default fills it in.
	var pc2 canon.PriceChangeRequested
	if err := prices[1].Decode(&pc2); err != nil {
		t.Fatal(err)
	}
	if pc2.Price.Currency != "USD" {
		t.Errorf("currency default not applied: %+v", pc2.Price)
	}
	if pc2.EffectiveAt.IsZero() {
		t.Error("a change with no effective_at must default to the receipt time")
	}

	// The raw copy on pos-integration keeps the exact bytes for audit.
	var rawPayload RawDelivery
	if err := raw[0].Decode(&rawPayload); err != nil {
		t.Fatal(err)
	}
	if string(rawPayload.Body) != `{"ok":true}` {
		t.Errorf("raw body = %q", rawPayload.Body)
	}
	if rawPayload.Emitted != 2 || len(rawPayload.Stores) != 1 || rawPayload.Stores[0] != "GB-0042" {
		t.Errorf("raw copy = %+v", rawPayload)
	}
	if rawPayload.BodySHA256 == "" {
		t.Error("the raw copy must carry a digest so a consumer can detect truncation")
	}
	// pos-integration is keyed tenant:store by contract §2.
	for _, m := range msgs {
		if m.Topic == canon.StreamPOSIngress.Name && m.Key != "acme:GB-0042" {
			t.Errorf("pos-integration key = %q, want acme:GB-0042", m.Key)
		}
	}

	if got := h.metrics.ChangesEmitted.With().Value(); got != 2 {
		t.Errorf("changes_emitted = %d, want 2", got)
	}
	if got := h.metrics.IngestTotal.With("test", "acme", "accepted").Value(); got != 1 {
		t.Errorf("ingest_total{accepted} = %d", got)
	}
	if got := h.metrics.IngestDuration.With("test").Count(); got != 1 {
		t.Errorf("duration observations = %d", got)
	}
}

func TestReplayedDeliveryDedupesToZeroNewEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.delivery("m-1", `{"ok":true}`)

	first := h.pipe.Ingest(ctx, d)
	if first.Emitted != 1 || first.Duplicate {
		t.Fatalf("first delivery = %+v", first)
	}
	firstMsgs := len(h.pub.captured())

	// The same message id arriving again — a POS retrying because our
	// acknowledgement was slow — must produce no further events at all.
	second := h.pipe.Ingest(ctx, h.delivery("m-1", `{"ok":true}`))
	if !second.Duplicate {
		t.Fatal("the redelivery was not recognised as a duplicate")
	}
	if second.Emitted != 0 {
		t.Fatalf("a duplicate emitted %d changes", second.Emitted)
	}
	if second.HTTPStatus != http.StatusAccepted {
		t.Errorf("duplicate status = %d, want 202 so the producer stops retrying", second.HTTPStatus)
	}
	if got := len(h.pub.captured()); got != firstMsgs {
		t.Fatalf("published %d messages after the duplicate, want %d", got, firstMsgs)
	}
	if h.adapter.calls.Load() != 1 {
		t.Error("the duplicate was parsed; dedupe must precede parsing")
	}
	if got := h.metrics.DedupeHits.With().Value(); got != 1 {
		t.Errorf("dedupe_hits = %d", got)
	}

	// A different message id is genuinely new work.
	third := h.pipe.Ingest(ctx, h.delivery("m-2", `{"ok":true}`))
	if third.Duplicate || third.Emitted != 1 {
		t.Fatalf("a new message id was suppressed: %+v", third)
	}
}

func TestDedupeFallsBackToBodyDigest(t *testing.T) {
	h := newHarness(t)
	h.adapter.idParts = func(*adapter.Delivery) []string { return nil }
	ctx := context.Background()

	a := h.pipe.Ingest(ctx, h.delivery("", `{"same":1}`))
	b := h.pipe.Ingest(ctx, h.delivery("", `{"same":1}`))
	c := h.pipe.Ingest(ctx, h.delivery("", `{"different":1}`))
	if a.Duplicate || !b.Duplicate || c.Duplicate {
		t.Fatalf("digest fallback wrong: %v %v %v", a.Duplicate, b.Duplicate, c.Duplicate)
	}
}

func TestTamperedSignatureIsRejectedWithoutStoringTheBody(t *testing.T) {
	h := newHarness(t)
	h.adapter.verify = func(*adapter.Delivery) error {
		return adapter.Unauthorized("bad_signature", "signature does not match the request body")
	}
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"tampered":true}`))
	h.flush()

	if res.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.HTTPStatus)
	}
	if res.Status != deliveries.StatusRejected {
		t.Errorf("status = %s, want rejected", res.Status)
	}
	if h.adapter.calls.Load() != 0 {
		t.Error("an unverified delivery was parsed")
	}
	if len(h.pub.captured()) != 0 {
		t.Error("an unverified delivery was published")
	}
	rec, err := h.store.Get("acme", res.DeliveryID)
	if err != nil {
		t.Fatalf("the rejection was not recorded: %v", err)
	}
	// Storing the payloads of unverified callers would make the ingress
	// endpoint free storage for an attacker.
	if len(rec.Body) != 0 {
		t.Error("an unauthenticated caller's body was retained")
	}
	if got := h.metrics.VerifyFailures.With("test", "bad_signature").Value(); got != 1 {
		t.Errorf("verify_failures = %d", got)
	}
}

func TestMalformedBodyIsQuarantinedWith4xx(t *testing.T) {
	h := newHarness(t)
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return nil, adapter.Malformed("json_decode", "the body is not a product object", errors.New("unexpected {"))
	}
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", `not json at all`))

	// Never a 5xx: a 5xx makes the POS retry a message that will never parse.
	if res.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.HTTPStatus)
	}
	if res.HTTPStatus >= 500 {
		t.Fatal("an unparseable body must never be answered with a 5xx")
	}
	if res.Status != deliveries.StatusQuarantined || res.Reason != "json_decode" {
		t.Fatalf("result = %+v", res)
	}
	rec, err := h.store.Get("acme", res.DeliveryID)
	if err != nil {
		t.Fatalf("quarantine record missing: %v", err)
	}
	if string(rec.Body) != "not json at all" {
		t.Errorf("the raw body was not retained for support: %q", rec.Body)
	}
	if rec.HTTPStatus != 422 || rec.Detail == "" {
		t.Errorf("record = %+v", rec)
	}
	if got := h.metrics.Quarantined.With("test", "json_decode").Value(); got != 1 {
		t.Errorf("quarantined = %d", got)
	}
	if got := h.metrics.ParseErrors.With("test", "json_decode").Value(); got != 1 {
		t.Errorf("parse_errors = %d", got)
	}

	// The dedupe key must have been released, so a corrected redelivery under
	// the same message id is processed rather than silently suppressed.
	h.adapter.ingest = nil
	again := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"fixed":true}`))
	if again.Duplicate || again.Emitted != 1 {
		t.Fatalf("a corrected redelivery was suppressed: %+v", again)
	}
}

func TestPartialDeliveryPublishesTheGoodRecords(t *testing.T) {
	h := newHarness(t)
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return []canon.PriceChangeRequested{
				{SKU: "GOOD-1", StoreID: "0042", Price: canon.Money{Amount: 100, Currency: "USD"}},
				{SKU: "GOOD-2", StoreID: "0042", Price: canon.Money{Amount: 200, Currency: "USD"}},
			}, &adapter.PartialError{
				Total:    3,
				Failures: []adapter.RowFailure{{Index: 2, Ref: "BAD-1", Reason: "price_unusable", Detail: "price \"\" is empty"}},
			}
	}
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", "rows"))
	h.flush()

	if res.Status != deliveries.StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	if res.Emitted != 2 {
		t.Fatalf("emitted = %d, want the two good records", res.Emitted)
	}
	if res.HTTPStatus != http.StatusAccepted {
		t.Errorf("status code = %d; a partial success is still a success", res.HTTPStatus)
	}
	if len(res.RowFailures) != 1 || res.RowFailures[0].Ref != "BAD-1" {
		t.Fatalf("row failures = %+v", res.RowFailures)
	}
	rec, err := h.store.Get("acme", res.DeliveryID)
	if err != nil {
		t.Fatalf("partial delivery not stored: %v", err)
	}
	if len(rec.Body) == 0 {
		t.Error("a partial delivery must retain its body so support can find the bad rows")
	}
	if got := h.metrics.RowFailures.With("test", "price_unusable").Value(); got != 1 {
		t.Errorf("row_failures = %d", got)
	}
}

func TestEveryRecordInvalidIsAQuarantineNotAPartialSuccess(t *testing.T) {
	h := newHarness(t)
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		// Parsed fine, but every record fails a canonical invariant.
		return []canon.PriceChangeRequested{
			{SKU: "", StoreID: "0042", Price: canon.Money{Amount: 1, Currency: "USD"}},
			{SKU: "OK", StoreID: "unmapped", Price: canon.Money{Amount: 1, Currency: "USD"}},
		}, nil
	}
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", "x"))
	if res.Status != deliveries.StatusQuarantined {
		t.Fatalf("status = %s, want quarantined", res.Status)
	}
	if res.Emitted != 0 || len(h.pub.captured()) != 0 {
		t.Fatal("nothing should have been published")
	}
	if len(res.RowFailures) != 2 {
		t.Fatalf("row failures = %+v", res.RowFailures)
	}
	if res.RowFailures[0].Reason != "missing_sku" || res.RowFailures[1].Reason != "store_unmapped" {
		t.Errorf("reasons = %q, %q", res.RowFailures[0].Reason, res.RowFailures[1].Reason)
	}
}

func TestUnderstoodButEmptyDeliveryIsAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) { return nil, nil }
	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"topic":"orders/create"}`))
	if res.Status != deliveries.StatusIgnored || res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("result = %+v; an unwanted webhook topic must be acknowledged, not retried", res)
	}
	if len(h.pub.captured()) != 0 {
		t.Error("an ignored delivery published events")
	}
	// It is still deduplicated, so a redelivery is not re-parsed.
	again := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"topic":"orders/create"}`))
	if !again.Duplicate {
		t.Error("an ignored delivery was not recorded in the guard")
	}
}

func TestPublishFailureIsRetryableAndReleasesTheKey(t *testing.T) {
	h := newHarness(t)
	h.pub.failWith = errors.New("broker unavailable")

	res := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"ok":true}`))
	if res.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — this is the one failure a retry can fix", res.HTTPStatus)
	}
	if res.Status != deliveries.StatusRejected || res.Reason != "publish_failed" {
		t.Fatalf("result = %+v", res)
	}
	if got := h.metrics.PublishFailures.With("test").Value(); got != 1 {
		t.Errorf("publish_failures = %d", got)
	}

	// The producer's retry must be treated as first-seen. Holding the key would
	// suppress every retry for 24 hours and lose the price change silently,
	// which is the worst failure mode a pricing system has.
	h.pub.reset()
	again := h.pipe.Ingest(context.Background(), h.delivery("m-1", `{"ok":true}`))
	if again.Duplicate {
		t.Fatal("the failed delivery's key was not released")
	}
	if again.Emitted != 1 {
		t.Fatalf("retry after recovery emitted %d", again.Emitted)
	}
}

func TestRateLimitAnswers429WithRetryAfter(t *testing.T) {
	h := newHarness(t, func(b *adapter.Binding) {
		b.RateLimit = adapter.RateLimitSpec{RatePerSecond: 1, Burst: 2}
	})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if res := h.pipe.Ingest(ctx, h.delivery(fmt.Sprintf("m-%d", i), "{}")); res.HTTPStatus != 202 {
			t.Fatalf("burst request %d = %d", i, res.HTTPStatus)
		}
	}
	res := h.pipe.Ingest(ctx, h.delivery("m-3", "{}"))
	if res.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.HTTPStatus)
	}
	if res.RetryAfter <= 0 {
		t.Error("a 429 with no Retry-After makes a POS guess")
	}
	if got := h.metrics.RateLimited.With("test", "acme").Value(); got != 1 {
		t.Errorf("rate_limited = %d", got)
	}
	if h.adapter.calls.Load() != 2 {
		t.Error("a throttled delivery was parsed")
	}
}

func TestUnknownAndDisabledBindingsAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	missing := h.pipe.Ingest(ctx, &adapter.Delivery{TenantID: "acme", BindingID: "nope", Body: []byte("{}")})
	if missing.HTTPStatus != http.StatusNotFound {
		t.Fatalf("unknown binding = %d", missing.HTTPStatus)
	}
	wrongTenant := h.pipe.Ingest(ctx, &adapter.Delivery{TenantID: "other", BindingID: "pos", Body: []byte("{}")})
	if wrongTenant.HTTPStatus != http.StatusNotFound {
		t.Fatalf("cross-tenant = %d", wrongTenant.HTTPStatus)
	}

	b, _ := h.bindings.Get("acme", "pos")
	disabled := *b
	disabled.Disabled = true
	if err := h.bindings.Put(&disabled); err != nil {
		t.Fatal(err)
	}
	off := h.pipe.Ingest(ctx, h.delivery("m-1", "{}"))
	if off.HTTPStatus != http.StatusNotFound {
		t.Fatalf("disabled binding = %d, want 404 so a decommissioned endpoint does not confirm it existed",
			off.HTTPStatus)
	}
}

func TestCurrencyAndStoreNormalisation(t *testing.T) {
	h := newHarness(t, func(b *adapter.Binding) {
		b.AllowedCurrencies = []string{"USD", "GBP"}
	})
	ctx := context.Background()

	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return []canon.PriceChangeRequested{{SKU: "A", Price: canon.Money{Amount: 1, Currency: "eur"}}}, nil
	}
	res := h.pipe.Ingest(ctx, h.delivery("m-1", "x"))
	if res.Status != deliveries.StatusQuarantined || res.RowFailures[0].Reason != "currency_not_allowed" {
		t.Fatalf("a currency outside the binding's set was accepted: %+v", res)
	}

	// No source store at all falls back to the binding's default store, which
	// is the normal case for a single-site retailer.
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return []canon.PriceChangeRequested{{SKU: "A", Price: canon.Money{Amount: 1, Currency: "gbp"}}}, nil
	}
	res2 := h.pipe.Ingest(ctx, h.delivery("m-2", "x"))
	if res2.Emitted != 1 {
		t.Fatalf("result = %+v", res2)
	}
	env := decodeEnvelopes(t, h.pub.captured(), canon.StreamPriceUpdates.Name)
	var pc canon.PriceChangeRequested
	if err := env[0].Decode(&pc); err != nil {
		t.Fatal(err)
	}
	if pc.StoreID != "GB-HQ" || pc.Price.Currency != "GBP" {
		t.Errorf("normalised = %+v", pc)
	}

	// A SKU containing a namespace separator would let a tenant address topics
	// outside its own namespace, so it is refused.
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return []canon.PriceChangeRequested{{SKU: "bad/sku", Price: canon.Money{Amount: 1, Currency: "USD"}}}, nil
	}
	res3 := h.pipe.Ingest(ctx, h.delivery("m-3", "x"))
	if res3.Status != deliveries.StatusQuarantined || res3.RowFailures[0].Reason != "invalid_sku" {
		t.Fatalf("a SKU with reserved characters was accepted: %+v", res3)
	}
}

func TestReplayAfterAMappingFix(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The mapping is wrong: the delivery is quarantined with a 4xx and its body
	// retained.
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return nil, adapter.Malformed("field_missing", "no priceAmount field", nil)
	}
	bad := h.pipe.Ingest(ctx, h.delivery("m-1", `{"amount":"2.49"}`))
	if bad.Status != deliveries.StatusQuarantined {
		t.Fatalf("expected a quarantine, got %+v", bad)
	}

	// The mapping is corrected and the operator replays the stored bytes.
	h.adapter.ingest = nil
	res, err := h.pipe.Replay(ctx, "acme", bad.DeliveryID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Emitted != 1 || res.Status != deliveries.StatusAccepted {
		t.Fatalf("replay result = %+v", res)
	}
	if res.DeliveryID == bad.DeliveryID {
		t.Error("a replay must be filed under a new delivery id")
	}
	h.flush()
	stored, err := h.store.Get("acme", res.DeliveryID)
	if err != nil {
		t.Fatalf("replay record missing: %v", err)
	}
	if stored.ReplayOf != bad.DeliveryID || stored.ReplayCount != 1 {
		t.Errorf("replay provenance = %+v", stored)
	}
	original, err := h.store.Get("acme", bad.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if original.ReplayCount != 1 {
		t.Errorf("the original's replay count = %d, want 1 so an operator sees it has been replayed",
			original.ReplayCount)
	}

	// A replay is not deduplicated against the original — the whole point is to
	// reprocess something already seen — but a *new* live delivery of the same
	// message still is.
	if got := h.metrics.Replays.With("test", "accepted").Value(); got != 1 {
		t.Errorf("replays = %d", got)
	}

	if _, err := h.pipe.Replay(ctx, "acme", "does-not-exist"); !errors.Is(err, deliveries.ErrNotFound) {
		t.Errorf("replay of a missing delivery err = %v", err)
	}
}

func TestReplayRefusesADeliveryWithNoRetainedBody(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	res := h.pipe.Ingest(ctx, h.delivery("m-1", `{"ok":true}`))
	h.flush()
	// A successful delivery does not retain its body unless the binding asks
	// for it, so replay must refuse with an explanation rather than silently
	// replaying nothing.
	_, err := h.pipe.Replay(ctx, "acme", res.DeliveryID)
	if !errors.Is(err, ErrNotReplayable) {
		t.Fatalf("err = %v, want ErrNotReplayable", err)
	}
	if !strings.Contains(err.Error(), "retain_raw") {
		t.Errorf("the error should tell an operator how to make it replayable: %v", err)
	}
}

func TestRetainRawMakesSuccessfulDeliveriesReplayable(t *testing.T) {
	h := newHarness(t, func(b *adapter.Binding) { b.RetainRaw = true })
	ctx := context.Background()
	res := h.pipe.Ingest(ctx, h.delivery("m-1", `{"ok":true}`))
	if res.Emitted != 1 {
		t.Fatalf("res = %+v", res)
	}
	rec, err := h.store.Get("acme", res.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Body) != `{"ok":true}` {
		t.Fatalf("retain_raw did not keep the body: %q", rec.Body)
	}
	if _, err := h.pipe.Replay(ctx, "acme", res.DeliveryID); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

func TestListDeliveriesForSupportTriage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return nil, adapter.Malformed("bad", "nope", nil)
	}
	for i := 0; i < 3; i++ {
		h.pipe.Ingest(ctx, h.delivery(fmt.Sprintf("m-%d", i), "x"))
	}
	h.adapter.ingest = nil
	h.pipe.Ingest(ctx, h.delivery("m-ok", "x"))
	h.flush()

	q, err := h.pipe.ListDeliveries("acme", deliveries.Query{Status: deliveries.StatusQuarantined})
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 3 {
		t.Fatalf("quarantined = %d, want 3", len(q))
	}
	all, _ := h.pipe.ListDeliveries("acme", deliveries.Query{})
	if len(all) != 4 {
		t.Fatalf("all = %d, want 4", len(all))
	}
}

func TestBindingHealthReflectsTheLastOutcome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if got := h.pipe.Health().Get("acme", "pos").Status; got != "idle" {
		t.Errorf("a binding with no traffic should read idle, got %q", got)
	}

	h.pipe.Ingest(ctx, h.delivery("m-1", "x"))
	hh := h.pipe.Health().Get("acme", "pos")
	if hh.Status != "ok" || hh.Accepted != 1 || hh.Emitted != 1 {
		t.Fatalf("health after success = %+v", hh)
	}

	h.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		return nil, adapter.Malformed("bad", "nope", nil)
	}
	h.pipe.Ingest(ctx, h.delivery("m-2", "x"))
	hh = h.pipe.Health().Get("acme", "pos")
	if hh.Status != "failing" || hh.Quarantined != 1 || hh.LastFailureReason != "bad" {
		t.Fatalf("health after failure = %+v", hh)
	}

	// A duplicate is the producer behaving correctly and must not repaint the
	// binding's status in either direction.
	h.adapter.ingest = nil
	h.pipe.Ingest(ctx, h.delivery("m-3", "x"))
	before := h.pipe.Health().Get("acme", "pos").Status
	h.pipe.Ingest(ctx, h.delivery("m-3", "x"))
	after := h.pipe.Health().Get("acme", "pos")
	if after.Status != before {
		t.Errorf("a duplicate changed the health status from %q to %q", before, after.Status)
	}
	if after.Duplicates != 1 {
		t.Errorf("duplicates = %d", after.Duplicates)
	}
}

func TestCorrelationIDIsTakenFromTheCaller(t *testing.T) {
	h := newHarness(t)
	d := h.delivery("m-1", "x")
	d.Headers.Set("X-Correlation-Id", "retailer-corr-1")
	res := h.pipe.Ingest(context.Background(), d)
	if res.CorrelationID != "retailer-corr-1" {
		t.Fatalf("correlation = %q; a trace that started in the POS must continue into USSLP",
			res.CorrelationID)
	}
	env := decodeEnvelopes(t, h.pub.captured(), canon.StreamPriceUpdates.Name)
	if env[0].CorrelationID != "retailer-corr-1" {
		t.Errorf("envelope correlation = %q", env[0].CorrelationID)
	}
}

func TestLatencyBudgetIsMeasured(t *testing.T) {
	h := newHarness(t)
	h.pipe.Ingest(context.Background(), h.delivery("m-1", "x"))
	// The clock advances a microsecond per read, so a normal delivery is
	// comfortably inside the 50ms slice the gateway owns.
	if got := h.metrics.BudgetExceeded.With("test").Value(); got != 0 {
		t.Errorf("budget_exceeded = %d for a fast delivery", got)
	}
	if got := h.metrics.IngestDuration.With("test").Count(); got != 1 {
		t.Errorf("duration observations = %d", got)
	}

	// A deliberately slow adapter must be counted against the budget.
	slow := newHarness(t)
	slow.clock.mu.Lock()
	slow.clock.mu.Unlock()
	slow.adapter.ingest = func(*adapter.Delivery) ([]canon.PriceChangeRequested, error) {
		slow.clock.mu.Lock()
		slow.clock.t = slow.clock.t.Add(LatencyBudget * 2)
		slow.clock.mu.Unlock()
		return []canon.PriceChangeRequested{{SKU: "A", StoreID: "0042", Price: canon.Money{Amount: 1, Currency: "USD"}}}, nil
	}
	slow.pipe.Ingest(context.Background(), slow.delivery("m-1", "x"))
	if got := slow.metrics.BudgetExceeded.With("test").Value(); got != 1 {
		t.Errorf("budget_exceeded = %d for a slow delivery, want 1", got)
	}
}

func TestConcurrentIngestIsRaceFreeAndDedupesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const workers = 24

	// Every goroutine sends the *same* message id: exactly one must win and
	// emit, which is the property that stops one retailer price decision from
	// becoming two shelf changes.
	var wg sync.WaitGroup
	var accepted, duplicates atomic.Int32
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res := h.pipe.Ingest(ctx, h.delivery("burst-1", `{"ok":true}`))
			if res.Duplicate {
				duplicates.Add(1)
			} else if res.Emitted > 0 {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted.Load() != 1 {
		t.Fatalf("%d goroutines emitted; exactly one must", accepted.Load())
	}
	if duplicates.Load() == 0 {
		t.Error("no goroutine was told it was a duplicate")
	}
	prices := decodeEnvelopes(t, h.pub.captured(), canon.StreamPriceUpdates.Name)
	if len(prices) != 1 {
		t.Fatalf("published %d price events under concurrency, want 1", len(prices))
	}

	// Distinct message ids under concurrency all proceed.
	var wg2 sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			h.pipe.Ingest(ctx, h.delivery(fmt.Sprintf("distinct-%d", i), `{"ok":true}`))
		}(i)
	}
	wg2.Wait()
	h.flush()
	if got := len(decodeEnvelopes(t, h.pub.captured(), canon.StreamPriceUpdates.Name)); got != 1+workers {
		t.Fatalf("published %d price events, want %d", got, 1+workers)
	}
}

// TestDurableThenAcknowledged runs the pipeline against the real embedded event
// log rather than a capture double, because the acknowledgement contract is
// "durable first, ack second" and only a real durable publisher can demonstrate
// it.
func TestDurableThenAcknowledged(t *testing.T) {
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	backend, _ := idem.NewKVBackend(kv, "idem/")
	guard, _ := idem.New(backend)
	store, _ := deliveries.New(kv, deliveries.Options{})

	log, err := eventlog.Open("", eventlog.WithSync(eventlog.SyncAlways))
	if err != nil {
		t.Fatalf("eventlog: %v", err)
	}
	defer log.Close()
	ctx := context.Background()
	if err := log.EnsureStreams(ctx, canon.StreamPriceUpdates, canon.StreamPOSIngress, canon.StreamDLQ); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	ta := &testAdapter{}
	reg := adapter.NewRegistry()
	reg.MustRegister(ta)
	bindings := adapter.NewBindingStore(reg)
	if err := bindings.Put(&adapter.Binding{
		ID: "pos", TenantID: "acme", Adapter: ta.Name(),
		DefaultCurrency: "USD", DefaultStore: "GB-HQ",
		StoreMap: map[string]canon.StoreID{"0042": "GB-0042"},
	}); err != nil {
		t.Fatal(err)
	}
	pipe, err := New(Config{
		Registry: reg, Bindings: bindings, Guard: guard, Bus: log,
		Deliveries: store, Metrics: NewMetrics(obs.NewRegistry()), Log: obs.NopLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()

	res := pipe.Ingest(ctx, &adapter.Delivery{
		TenantID: "acme", BindingID: "pos", Body: []byte(`{"ok":true}`),
		Headers: http.Header{"X-Message-Id": []string{"m-1"}},
	})
	if res.HTTPStatus != http.StatusAccepted || res.Emitted != 1 {
		t.Fatalf("result = %+v", res)
	}

	// By the time the caller was answered the record was already durable, so a
	// consumer starting from the beginning sees it immediately.
	consumer, err := log.Subscribe(eventbus.SubscribeOptions{
		Group: "assert", Topics: []string{canon.StreamPriceUpdates.Name}, FromBeginning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()

	seen := make(chan canon.Envelope, 4)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() {
		_ = consumer.Run(runCtx, func(_ context.Context, m eventbus.Message) error {
			var env canon.Envelope
			if err := json.Unmarshal(m.Value, &env); err == nil {
				select {
				case seen <- env:
				default:
				}
			}
			return nil
		})
	}()
	select {
	case env := <-seen:
		if env.EventType != canon.EvtPriceChangeRequested {
			t.Errorf("consumed event_type = %q", env.EventType)
		}
		if env.StoreID != "GB-0042" {
			t.Errorf("consumed store = %q", env.StoreID)
		}
	case <-runCtx.Done():
		t.Fatal("the acknowledged price change was not durable on the stream")
	}
}

func TestNewValidatesItsDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a pipeline with no dependencies was accepted")
	}
	reg := adapter.NewRegistry()
	if _, err := New(Config{Registry: reg}); err == nil {
		t.Fatal("a pipeline with no binding store was accepted")
	}
}
