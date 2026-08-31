package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

const (
	tenantA = canon.TenantID("acme")
	tenantB = canon.TenantID("bodega")
	store01 = canon.StoreID("store-01")
)

type fixture struct {
	deps    Deps
	repo    *memRepo
	dir     *memDirectory
	device  *memDevice
	streams *memStreams
	state   *memState
	handler *UpdatePriceHandler
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	f := &fixture{
		repo: newMemRepo(), dir: newMemDirectory(),
		device: newMemDevice(), streams: newMemStreams(), state: newMemState(),
		now: now,
	}
	f.deps = Deps{
		Repo: f.repo, Directory: f.dir, Attestor: newMemAttestor(),
		Device: f.device, Streams: f.streams, State: f.state,
		Clock: fixedClock{at: now}, Metrics: NewMetrics(nil),
	}
	handler, err := NewUpdatePriceHandler(f.deps, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	f.handler = handler
	return f
}

// seed provisions and assigns a label in both the write side and the directory.
func (f *fixture) seed(t *testing.T, tenant canon.TenantID, id canon.LabelID, sku canon.SKU) {
	t.Helper()
	ctx := context.Background()
	agg, err := f.repo.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	events, err := agg.Provision(domain.Provision{
		TenantID: tenant, StoreID: store01, Region: "us-east-1", SECID: "sec-01",
		Currency: "USD", Now: f.now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := agg.Replay(events...); err != nil {
		t.Fatalf("replay provisioning: %v", err)
	}
	assigned, err := agg.Assign(domain.Assign{SKU: sku, Now: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	events = append(events, assigned...)
	if _, err := f.repo.Append(ctx, id, 0, events, ports.AppendMeta{TenantID: tenant}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.dir.Upsert(ctx, ports.Placement{
		LabelID: id, SECID: "sec-01", TenantID: tenant, StoreID: store01,
		Region: "us-east-1", SKU: sku,
	}); err != nil {
		t.Fatalf("directory: %v", err)
	}
}

func (f *fixture) batch(t *testing.T, limiter ports.RateLimiter, cfg BatchConfig) *BatchUpdater {
	t.Helper()
	b, err := NewBatchUpdater(f.handler, f.deps, limiter, cfg)
	if err != nil {
		t.Fatalf("batch updater: %v", err)
	}
	return b
}

// TestBatchReportsPerLabelFailures covers the partial-failure contract: one
// unreachable controller must not discard the other thirty-nine thousand
// correct price changes.
func TestBatchReportsPerLabelFailures(t *testing.T) {
	f := newFixture(t)
	const n = 40
	for i := 0; i < n; i++ {
		f.seed(t, tenantA, canon.LabelID(fmt.Sprintf("lbl-%03d", i)), "sku-milk")
	}
	f.device.failFor["lbl-007"] = true
	f.device.failWith = errors.New("controller sec-01 is unreachable")

	updater := f.batch(t, nil, BatchConfig{Workers: 8})
	report, err := updater.BatchUpdatePrices(context.Background(), BatchRequest{
		TenantID: tenantA,
		Items: []BatchItem{{
			StoreID: store01, SKU: "sku-milk",
			Price: canon.NewMoney(399, "USD"), EffectiveAt: f.now,
		}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if report.Resolved != n {
		t.Fatalf("resolved = %d, want %d", report.Resolved, n)
	}
	if report.Applied != n-1 {
		t.Fatalf("applied = %d, want %d", report.Applied, n-1)
	}
	if report.Failed != 1 {
		t.Fatalf("failed = %d, want 1", report.Failed)
	}
	if !report.Partial {
		t.Fatalf("a batch with a failure must report Partial")
	}

	var failure PriceResult
	for _, r := range report.Results {
		if r.Outcome == OutcomeError {
			failure = r
		}
	}
	if failure.LabelID != "lbl-007" {
		t.Fatalf("the failure was attributed to %q", failure.LabelID)
	}
	if failure.Detail == "" {
		t.Fatalf("a failed result must explain itself")
	}
	// The other labels really were published, not merely counted.
	for i := 0; i < n; i++ {
		id := canon.LabelID(fmt.Sprintf("lbl-%03d", i))
		want := 1
		if id == "lbl-007" {
			want = 0
		}
		if got := f.device.count(id); got != want {
			t.Fatalf("label %s received %d publishes, want %d", id, got, want)
		}
	}
	// Results are ordered by label id so two runs of one batch are comparable.
	for i := 1; i < len(report.Results); i++ {
		if report.Results[i-1].LabelID > report.Results[i].LabelID {
			t.Fatalf("results are not ordered by label id")
		}
	}
}

// TestBatchIsIdempotentOnRetry covers the safe-retry property the report's
// Partial flag depends on: a caller that retries a partially failed batch must
// not re-publish to the labels that already succeeded.
func TestBatchIsIdempotentOnRetry(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.seed(t, tenantA, canon.LabelID(fmt.Sprintf("lbl-%03d", i)), "sku-milk")
	}
	updater := f.batch(t, nil, BatchConfig{Workers: 4})
	req := BatchRequest{
		TenantID: tenantA,
		Items: []BatchItem{{
			StoreID: store01, SKU: "sku-milk", Price: canon.NewMoney(399, "USD"),
			EffectiveAt: f.now, IdempotencyKey: "promo-2026-03-14",
		}},
	}
	ctx := context.Background()
	if _, err := updater.BatchUpdatePrices(ctx, req); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := updater.BatchUpdatePrices(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Applied != 0 || second.Stale != 5 {
		t.Fatalf("retry applied=%d stale=%d, want 0/5", second.Applied, second.Stale)
	}
	for i := 0; i < 5; i++ {
		id := canon.LabelID(fmt.Sprintf("lbl-%03d", i))
		if got := f.device.count(id); got != 1 {
			t.Fatalf("label %s received %d publishes across two runs, want 1", id, got)
		}
	}
}

// TestBatchChargesTheRateLimiterPerTenant covers the fairness mechanism: each
// tenant's fan-out is charged to its own bucket, in chunks, so one retailer's
// bulk repricing cannot occupy the pool to the exclusion of another's single
// urgent change.
func TestBatchChargesTheRateLimiterPerTenant(t *testing.T) {
	f := newFixture(t)
	const n = 300
	for i := 0; i < n; i++ {
		f.seed(t, tenantA, canon.LabelID(fmt.Sprintf("a-%03d", i)), "sku-milk")
	}
	f.seed(t, tenantB, "b-000", "sku-bread")

	limiter := newCountingLimiter()
	updater := f.batch(t, limiter, BatchConfig{Workers: 8, StoreChunk: 64})
	ctx := context.Background()
	if _, err := updater.BatchUpdatePrices(ctx, BatchRequest{
		TenantID: tenantA,
		Items: []BatchItem{{
			StoreID: store01, SKU: "sku-milk", Price: canon.NewMoney(399, "USD"), EffectiveAt: f.now,
		}},
	}); err != nil {
		t.Fatalf("tenant A batch: %v", err)
	}
	if _, err := updater.BatchUpdatePrices(ctx, BatchRequest{
		TenantID: tenantB,
		Items: []BatchItem{{
			StoreID: store01, SKU: "sku-bread", Price: canon.NewMoney(199, "USD"), EffectiveAt: f.now,
		}},
	}); err != nil {
		t.Fatalf("tenant B batch: %v", err)
	}

	if got := limiter.total(tenantA); got != n {
		t.Fatalf("tenant A was charged %d, want %d", got, n)
	}
	if got := limiter.total(tenantB); got != 1 {
		t.Fatalf("tenant B was charged %d, want 1", got)
	}
}

// TestBatchStopsOnCancellation covers the cancellation contract: a caller that
// hangs up stops the pipeline rather than leaving it repricing a store nobody
// is waiting for.
func TestBatchStopsOnCancellation(t *testing.T) {
	f := newFixture(t)
	const n = 200
	for i := 0; i < n; i++ {
		f.seed(t, tenantA, canon.LabelID(fmt.Sprintf("lbl-%03d", i)), "sku-milk")
	}
	// A limiter that stalls gives the cancellation somewhere to land.
	limiter := newCountingLimiter()
	limiter.delay = 50 * time.Millisecond
	updater := f.batch(t, limiter, BatchConfig{Workers: 4, StoreChunk: 8})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	report, err := updater.BatchUpdatePrices(ctx, BatchRequest{
		TenantID: tenantA,
		Items: []BatchItem{{
			StoreID: store01, SKU: "sku-milk", Price: canon.NewMoney(399, "USD"), EffectiveAt: f.now,
		}},
	})
	if err == nil && report.Applied == n {
		t.Fatalf("a cancelled batch ran to completion")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation surfaced as %v", err)
	}
}

// TestBatchRejectsATenantlessRequest covers the tenancy invariant: a batch
// never spans tenants, because the rate limiter, the guard-rail policy and the
// topic namespace are all tenant scoped.
func TestBatchRejectsATenantlessRequest(t *testing.T) {
	f := newFixture(t)
	updater := f.batch(t, nil, BatchConfig{})
	_, err := updater.BatchUpdatePrices(context.Background(), BatchRequest{
		Items: []BatchItem{{StoreID: store01, SKU: "sku-milk", Price: canon.NewMoney(399, "USD")}},
	})
	if !errors.Is(err, domain.ErrInvalidCommand) {
		t.Fatalf("error = %v, want ErrInvalidCommand", err)
	}
}

// TestPriceHandlerRepublishesAfterABrokerFailure covers the recovery path: the
// events are durable before anything is published, so a broker failure between
// the append and the publish must be recoverable without re-deciding.
func TestPriceHandlerRepublishesAfterABrokerFailure(t *testing.T) {
	f := newFixture(t)
	f.seed(t, tenantA, "lbl-001", "sku-milk")
	ctx := context.Background()
	placement, err := f.dir.Lookup(ctx, "lbl-001")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	cmd := PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: "sku-milk", Price: canon.NewMoney(399, "USD"),
			EffectiveAt: f.now, OccurredAt: f.now, Now: f.now,
			SourceEventID: canon.NewEventID(),
		},
	}

	f.device.failFor["lbl-001"] = true
	if _, err := f.handler.Apply(ctx, cmd); err == nil {
		t.Fatalf("a failed broker publish must be reported")
	}
	// The decision is durable even though the publish failed: that is what
	// makes the price explainable from the event store either way.
	agg, err := f.repo.Load(ctx, "lbl-001")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Sequence != 1 || agg.Pending == nil {
		t.Fatalf("the decision was not persisted: %+v", agg)
	}

	// The consumer retries the same record once the broker is back.
	delete(f.device.failFor, "lbl-001")
	res, err := f.handler.Apply(ctx, cmd)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Outcome != OutcomeRepublished {
		t.Fatalf("outcome = %s, want republished", res.Outcome)
	}
	if res.Sequence != 1 {
		t.Fatalf("republished under sequence %d; the label must not see two renderings of one sequence", res.Sequence)
	}
	if got := f.device.count("lbl-001"); got != 1 {
		t.Fatalf("label received %d publishes, want 1", got)
	}
	// And no second event was appended for the same decision.
	agg, err = f.repo.Load(ctx, "lbl-001")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if agg.Sequence != 1 {
		t.Fatalf("the retry allocated a second sequence: %d", agg.Sequence)
	}
}
