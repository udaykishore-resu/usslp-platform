package promotion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion/app"
	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/internal/promotion/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

const tenant = canon.TenantID("acme")

// clock is the instant every test runs at: Sunday 1 March 2026, 12:00 UTC —
// before the "starts Monday" promotion opens anywhere.
var clockAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testCatalogue() []domain.Product {
	var out []domain.Product
	for _, store := range []canon.StoreID{"lon-1", "nyc-1", "akl-1"} {
		out = append(out,
			domain.Product{SKU: "milk", StoreID: store, Category: "dairy", Brand: "own-label",
				BasePriceMinor: 120, Currency: "GBP", UnitCostMinor: 80, Inventory: 100, Velocity: 40},
			domain.Product{SKU: "cheese", StoreID: store, Category: "dairy", Brand: "brandco",
				BasePriceMinor: 450, Currency: "GBP", UnitCostMinor: 300, Inventory: 30, Velocity: 8},
			domain.Product{SKU: "bread", StoreID: store, Category: "bakery", Brand: "own-label",
				BasePriceMinor: 110, Currency: "GBP", UnitCostMinor: 40, Inventory: 60, Velocity: 55},
			domain.Product{SKU: "whisky", StoreID: store, Category: "alcohol", Brand: "brandco",
				BasePriceMinor: 2400, Currency: "GBP", UnitCostMinor: 1800, Inventory: 12, Velocity: 2},
		)
	}
	return out
}

func newTestService(t *testing.T, at time.Time) *Service {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })

	svc, err := New(Config{
		State: kv,
		Catalogue: &ports.StaticCatalogue{
			ByTenant: map[canon.TenantID][]domain.Product{tenant: testCatalogue()},
		},
		Directory: &ports.StaticDirectory{
			ZoneOf: map[canon.StoreID]string{
				"lon-1": "Europe/London",
				"nyc-1": "America/New_York",
				"akl-1": "Pacific/Auckland",
			},
			ClusterOf: map[canon.StoreID]string{
				"lon-1": "uk", "nyc-1": "us", "akl-1": "nz",
			},
		},
		Clock: ports.FixedClock{T: at},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func do(t *testing.T, h http.Handler, method, path, body string, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withTenant {
		req.Header.Set(TenantHeader, string(tenant))
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func dairyPromotion(id string) string {
	return fmt.Sprintf(`{
      "id": %q, "tenant_id": "acme", "name": "20%% off dairy", "type": "PERCENTAGE_OFF",
      "priority": 100, "stackable": false,
      "params": {"percent_off": 20, "currency": "GBP"},
      "conditions": {"categories": ["dairy"], "exclude_skus": ["whisky"]},
      "display": {"led_color": "RED", "badge": "20%% OFF", "show_original_price": true},
      "schedule": {"start_local": "2026-03-02T00:00", "end_local": "2026-03-09T00:00"},
      "funding": "retailer"
    }`, id)
}

func TestPromotionCRUDLifecycle(t *testing.T) {
	svc := newTestService(t, clockAt)
	h := svc.Handler()

	rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created app.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.State != domain.StateDraft {
		t.Errorf("a new promotion is %s, want draft", created.State)
	}
	if created.Rule.Version != 1 {
		t.Errorf("version = %d, want 1", created.Rule.Version)
	}

	t.Run("a duplicate id is a conflict", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true)
		if rec.Code != http.StatusConflict {
			t.Errorf("status %d, want 409", rec.Code)
		}
	})

	t.Run("a malformed document is unprocessable", func(t *testing.T) {
		bad := strings.Replace(dairyPromotion("p2"), `"percent_off": 20`, `"percent_off": 900`, 1)
		rec := do(t, h, "POST", "/v1/promotions", bad, true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a document for another tenant is refused", func(t *testing.T) {
		other := strings.Replace(dairyPromotion("p3"), `"tenant_id": "acme"`, `"tenant_id": "rival"`, 1)
		rec := do(t, h, "POST", "/v1/promotions", other, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a stale version loses the race", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/v1/promotions/p1", strings.NewReader(dairyPromotion("p1")))
		req.Header.Set(TenantHeader, string(tenant))
		req.Header.Set("If-Match", "99")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != http.StatusConflict {
			t.Errorf("status %d, want 409: %s", out.Code, out.Body)
		}
	})

	t.Run("activation moves through scheduled", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/promotions/p1/activate", `{"by":"alice"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("activate: %d %s", rec.Code, rec.Body)
		}
		var got app.Record
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.State != domain.StateActive {
			t.Errorf("state = %s, want active", got.State)
		}
		if got.ActivatedAt == nil {
			t.Error("no activation timestamp")
		}
	})

	t.Run("an active promotion cannot be edited", func(t *testing.T) {
		rec := do(t, h, "PUT", "/v1/promotions/p1", dairyPromotion("p1"), true)
		if rec.Code != http.StatusConflict {
			t.Errorf("status %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an active promotion cannot be deleted", func(t *testing.T) {
		rec := do(t, h, "DELETE", "/v1/promotions/p1", "", true)
		if rec.Code != http.StatusConflict {
			t.Errorf("status %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("cancelling records who and why", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/promotions/p1/cancel",
			`{"by":"bob","reason":"supplier funding withdrawn"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
		}
		var got app.Record
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.State != domain.StateCancelled || got.CancelledBy != "bob" ||
			!strings.Contains(got.CancelReason, "supplier") {
			t.Errorf("cancellation record = %+v", got)
		}
	})

	t.Run("a cancelled promotion cannot be reactivated", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/promotions/p1/activate", "", true)
		if rec.Code != http.StatusConflict {
			t.Errorf("status %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a missing tenant header is unauthenticated", func(t *testing.T) {
		rec := do(t, h, "GET", "/v1/promotions", "", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})

	t.Run("an unknown promotion is a 404", func(t *testing.T) {
		rec := do(t, h, "GET", "/v1/promotions/nope", "", true)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})
}

// TestSweepActivatesEachStoreOnItsOwnClock is the end-to-end timezone case: the
// promotion is scheduled, and the sweep activates it once the earliest store's
// local Monday arrives — not at UTC midnight.
func TestSweepActivatesEachStoreOnItsOwnClock(t *testing.T) {
	// Auckland's local Monday midnight is 11:00 UTC on Sunday. At 10:00 UTC
	// nothing should be live anywhere.
	before := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	svc := newTestService(t, before)
	h := svc.Handler()
	ctx := context.Background()

	if rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if _, err := svc.store.SetState(tenant, "p1", domain.StateScheduled, before, "alice", ""); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	activated, _, err := svc.Sweep(ctx, tenant)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if activated != 0 {
		t.Errorf("the sweep activated %d promotions an hour before the earliest local Monday", activated)
	}

	// An hour later, Auckland is in Monday.
	svc.clock = ports.FixedClock{T: before.Add(90 * time.Minute)}
	activated, _, err = svc.Sweep(ctx, tenant)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if activated != 1 {
		t.Fatalf("the sweep activated %d promotions once Auckland reached Monday, want 1", activated)
	}

	// Even though the promotion is now active, a London shelf must not carry it
	// until London reaches Monday.
	rec := do(t, h, "POST", "/v1/promotions:resolve",
		`{"product":{"sku":"milk","store_id":"lon-1","category":"dairy","base_price_minor":120,"currency":"GBP"},
          "zone":"Europe/London"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	var resolved struct {
		Resolution domain.Resolution `json:"resolution"`
		Active     int               `json:"active_promotions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resolved.Active != 0 || resolved.Resolution.FinalPriceMinor != 120 {
		t.Errorf("a London shelf was discounted before local Monday: %+v", resolved)
	}

	// An Auckland shelf, at the same instant, does carry it.
	rec = do(t, h, "POST", "/v1/promotions:resolve",
		`{"product":{"sku":"milk","store_id":"akl-1","category":"dairy","base_price_minor":120,"currency":"GBP"},
          "zone":"Pacific/Auckland"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resolved.Active != 1 || resolved.Resolution.FinalPriceMinor != 96 {
		t.Errorf("an Auckland shelf is not carrying the promotion: %+v", resolved.Resolution)
	}
	t.Logf("at %s UTC: Auckland priced at %d, London still at 120",
		svc.clock.Now().Format(time.RFC3339), resolved.Resolution.FinalPriceMinor)
}

func TestSimulateReportsReachAndCost(t *testing.T) {
	svc := newTestService(t, clockAt)
	h := svc.Handler()
	if rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	rec := do(t, h, "POST", "/v1/promotions/p1/simulate",
		`{"elasticity_of": {"milk": -1.8, "cheese": -2.2}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate: %d %s", rec.Code, rec.Body)
	}
	var res app.SimulationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Milk and cheese in three stores; bread is bakery and whisky is excluded.
	if res.MatchedPairs != 6 || res.MatchedSKUs != 2 || res.MatchedStores != 3 {
		t.Errorf("reach = %d pairs, %d SKUs, %d stores; want 6/2/3",
			res.MatchedPairs, res.MatchedSKUs, res.MatchedStores)
	}
	if res.AppliedPairs != 6 {
		t.Errorf("applied %d pairs with nothing else live, want 6", res.AppliedPairs)
	}
	if res.AverageDiscountPct < 19 || res.AverageDiscountPct > 21 {
		t.Errorf("average discount = %.2f%%, want about 20%%", res.AverageDiscountPct)
	}
	if res.DailyDiscountCostMinor <= 0 {
		t.Errorf("discount cost = %v, want a positive daily cost", res.DailyDiscountCostMinor)
	}
	if res.DailyMarginAfterMinor >= res.DailyMarginBeforeMinor {
		t.Errorf("a 20%% discount did not reduce margin: %v -> %v",
			res.DailyMarginBeforeMinor, res.DailyMarginAfterMinor)
	}
	t.Logf("synthetic catalogue: %d pairs, average discount %.1f%%, daily discount cost %.0f minor units, "+
		"margin %.0f -> %.0f (%.1f%%)",
		res.AppliedPairs, res.AverageDiscountPct, res.DailyDiscountCostMinor,
		res.DailyMarginBeforeMinor, res.DailyMarginAfterMinor, res.ProjectedMarginChangePct)
}

func TestSimulateFlagsBelowCostPricing(t *testing.T) {
	svc := newTestService(t, clockAt)
	h := svc.Handler()
	deep := strings.Replace(dairyPromotion("deep"), `"percent_off": 20`, `"percent_off": 80`, 1)
	if rec := do(t, h, "POST", "/v1/promotions", deep, true); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, "POST", "/v1/promotions/deep/simulate", `{}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate: %d %s", rec.Code, rec.Body)
	}
	var res app.SimulationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.BelowCostPairs == 0 {
		t.Fatalf("an 80%% discount on 80p-cost milk did not flag below-cost pricing: %+v", res)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "below cost") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want a below-cost warning", res.Warnings)
	}
}

func TestConflictsEndpointFindsOverlaps(t *testing.T) {
	svc := newTestService(t, clockAt)
	h := svc.Handler()

	a := dairyPromotion("p1")
	// A second dairy promotion at the same priority and the same discount:
	// nothing distinguishes them, which is the case that should be an error.
	b := strings.Replace(dairyPromotion("p2"), `"20% off dairy"`, `"another dairy offer"`, 1)
	for _, doc := range []string{a, b} {
		if rec := do(t, h, "POST", "/v1/promotions", doc, true); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}

	rec := do(t, h, "GET", "/v1/promotions/conflicts", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("conflicts: %d %s", rec.Code, rec.Body)
	}
	var res struct {
		Conflicts  []domain.Conflict `json:"conflicts"`
		BySeverity map[string]int    `json:"by_severity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(res.Conflicts), res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Severity != domain.SeverityError {
		t.Errorf("severity = %s, want error for two indistinguishable rules", c.Severity)
	}
	if c.Overlap != 6 {
		t.Errorf("overlap = %d pairs, want the 6 dairy pairs", c.Overlap)
	}
	if len(c.SampleSKUs) == 0 || c.Resolution == "" {
		t.Errorf("the conflict carries no examples or resolution: %+v", c)
	}
	if res.BySeverity["error"] != 1 {
		t.Errorf("severity counts = %v", res.BySeverity)
	}
}

func TestPerformanceMeasuresLiftFromTheRealDates(t *testing.T) {
	svc := newTestService(t, clockAt)
	// Wire a sales source with a synthetic series around the promotion.
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	var test []app.SalesPoint
	for d := -7; d < 14; d++ {
		units := 10.0
		price := 120.0
		if d >= 0 && d < 7 {
			units, price = 30, 96
		} else if d >= 7 {
			units = 7
		}
		test = append(test, app.SalesPoint{
			StoreID: "lon-1", SKU: "milk", Day: start.AddDate(0, 0, d),
			Units: units, RevenueMinor: units * price, CostMinor: units * 80,
		})
	}
	svc.cfg.Sales = &ports.StaticSales{Test: test}
	h := svc.Handler()

	if rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, "GET", "/v1/promotions/p1/performance", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("performance: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Lift      app.LiftResult   `json:"lift"`
		ByCluster []app.LiftResult `json:"by_cluster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Lift.UnitLiftPct <= 0 {
		t.Errorf("unit lift = %.2f%%, want positive", body.Lift.UnitLiftPct)
	}
	if body.Lift.PostDipPct >= 0 {
		t.Errorf("post dip = %.2f%%, want negative", body.Lift.PostDipPct)
	}
	found := false
	for _, c := range body.Lift.Caveats {
		if strings.Contains(c, "no control group") {
			found = true
		}
	}
	if !found {
		t.Errorf("caveats = %v, want the missing-control note", body.Lift.Caveats)
	}
	if len(body.ByCluster) == 0 {
		t.Error("no per-cluster breakdown")
	}
	t.Logf("synthetic series: unit lift %.0f%%, margin lift %.0f%%, post dip %.0f%%, incremental %.0f units",
		body.Lift.UnitLiftPct, body.Lift.MarginLiftPct, body.Lift.PostDipPct, body.Lift.IncrementalUnits)
}

func TestPublishedActivationCarriesTheWholeRule(t *testing.T) {
	svc := newTestService(t, clockAt)
	bus := &recordingBus{}
	svc.cfg.Bus = bus
	h := svc.Handler()

	if rec := do(t, h, "POST", "/v1/promotions", dairyPromotion("p1"), true); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/promotions/p1/activate", "", true); rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	if len(bus.published) != 1 {
		t.Fatalf("published %d events, want 1", len(bus.published))
	}
	var env canon.Envelope
	if err := json.Unmarshal(bus.published[0].Value, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.EventType != canon.EvtPromotionActivated {
		t.Errorf("event type = %q", env.EventType)
	}
	if env.IdempotencyKey == "" {
		t.Error("no idempotency key: a republished activation would be applied twice")
	}
	var payload ActivationEvent
	if err := env.Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	// The Label Service must be able to price a shelf from this event alone.
	if payload.Rule.Type != domain.TypePercentageOff || payload.Rule.Params.PercentOff != 20 {
		t.Errorf("the event does not carry the whole rule: %+v", payload.Rule)
	}
	if len(payload.Windows) != 3 {
		t.Errorf("the event carries %d resolved windows, want one per zone", len(payload.Windows))
	}
	if payload.Windows["Europe/London"].Start.Equal(payload.Windows["Pacific/Auckland"].Start) {
		t.Error("two zones resolved to the same instant")
	}
}

// recordingBus is an eventbus.Bus that keeps what was published, so the fan-out
// can be asserted on without standing up a real log.
type recordingBus struct {
	mu        sync.Mutex
	published []eventbus.Message
}

func (b *recordingBus) Publish(_ context.Context, msgs ...eventbus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, msgs...)
	return nil
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) Subscribe(eventbus.SubscribeOptions) (eventbus.Consumer, error) {
	return nil, errors.New("the recording bus does not consume")
}

func (b *recordingBus) EnsureStreams(context.Context, ...canon.Stream) error { return nil }

func TestResolveNeedsASKU(t *testing.T) {
	svc := newTestService(t, clockAt)
	rec := do(t, svc.Handler(), "POST", "/v1/promotions:resolve", `{"product":{}}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

func TestReadinessChecksReportTheStateStore(t *testing.T) {
	svc := newTestService(t, clockAt)
	checks := svc.ReadinessChecks()
	if len(checks) == 0 {
		t.Fatal("no readiness checks registered")
	}
	for name, check := range checks {
		if err := check(t.Context()); err != nil {
			t.Errorf("check %q failed on a healthy service: %v", name, err)
		}
	}
}
