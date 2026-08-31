package analytics

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/app"
	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

const tenant = canon.TenantID("acme")

var base = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(Config{
		DataDir: t.TempDir(),
		// Small blocks and segments so a test can produce several of each
		// without generating a million rows.
		BlockRows: 512, BlocksPerSegment: 2,
		Clock: func() time.Time { return base.Add(24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(func() { _ = svc.Shutdown(t.Context()) })
	return svc
}

func do(t *testing.T, h http.Handler, method, path, body string, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withTenant {
		req.Header.Set(TenantHeader, string(tenant))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// envelopeOf wraps a payload the way the platform's producers do.
func envelopeOf(t *testing.T, eventType, aggregateType, aggregateID string, at time.Time, payload any) canon.Envelope {
	t.Helper()
	env, err := canon.NewEnvelope(eventType, aggregateType, aggregateID, tenant, payload)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.OccurredAt, env.RecordedAt = at, at
	return env
}

// TestIngestFromEventsToQuery is the round trip through the real event
// contracts: platform envelopes in, columnar rows out, answered by a query.
func TestIngestFromEventsToQuery(t *testing.T) {
	svc := newTestService(t)
	in := svc.Ingest()

	// A batched telemetry report, as the device registry produces it.
	batch := []canon.Telemetry{
		{LabelID: "lbl-1", StoreID: "s1", SECID: "sec-1", ReportedAt: base,
			BatteryMV: 2980, BatteryPct: 81, TemperatureC: 4.2, RSSI: -62, LQI: 190,
			MeshHops: 2, FirmwareVer: "1.5.0", RefreshCount: 1200, NFCTapCount: 14, UptimeSeconds: 400000},
		{LabelID: "lbl-2", StoreID: "s1", SECID: "sec-1", ReportedAt: base,
			BatteryMV: 2200, BatteryPct: 16, TemperatureC: 21.0, RSSI: -80, LQI: 120,
			MeshHops: 3, FirmwareVer: "1.4.3", RefreshCount: 9000, NFCTapCount: 3, UptimeSeconds: 400000},
	}
	if err := in.Envelope(envelopeOf(t, canon.EvtDeviceTelemetry, "sec", "sec-1", base, batch)); err != nil {
		t.Fatalf("telemetry: %v", err)
	}

	// A delivery confirmation and a failure.
	if err := in.Envelope(envelopeOf(t, canon.EvtLabelDelivered, "label", "lbl-1", base,
		canon.LabelDelivered{LabelID: "lbl-1", StoreID: "s1", SECID: "sec-1",
			DeliveredAt: base, LatencyMS: 1450, MeshHops: 2, RefreshMS: 1500})); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if err := in.Envelope(envelopeOf(t, canon.EvtLabelDeliveryFailed, "label", "lbl-2", base,
		canon.LabelDeliveryFailed{LabelID: "lbl-2", StoreID: "s1", SECID: "sec-1",
			Reason: "mesh unreachable", Attempts: 3})); err != nil {
		t.Fatalf("failure: %v", err)
	}

	// A price update.
	if err := in.Envelope(envelopeOf(t, canon.EvtPriceUpdated, "label", "lbl-1", base,
		canon.PriceUpdated{LabelID: "lbl-1", SKU: "milk", StoreID: "s1",
			Price: canon.NewMoney(120, "GBP"), EffectiveAt: base})); err != nil {
		t.Fatalf("price: %v", err)
	}

	// A promotion activation, in the shape the promotion service publishes.
	promoPayload := map[string]any{
		"promotion_id": "promo-1", "tenant_id": string(tenant), "state": "active",
		"rule": map[string]any{"type": "PERCENTAGE_OFF", "priority": 100, "stackable": false},
	}
	if err := in.Envelope(envelopeOf(t, canon.EvtPromotionActivated, "promotion", "promo-1", base,
		promoPayload)); err != nil {
		t.Fatalf("promotion: %v", err)
	}

	// An event type this service does not model must be skipped, not fail.
	if err := in.Envelope(envelopeOf(t, canon.EvtOTAJobCreated, "ota", "job-1", base,
		map[string]any{"job_id": "job-1"})); err == nil {
		t.Error("an unmodelled event was accepted")
	} else if err != app.ErrUnroutable {
		t.Errorf("err = %v, want ErrUnroutable", err)
	}

	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	h := svc.Handler()
	t.Run("telemetry lands with its fields intact", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/query", fmt.Sprintf(`{
          "table": %q,
          "group_by": ["label_id"],
          "aggregates": [
            {"func":"max","column":"battery_mv","as":"battery"},
            {"func":"max","column":"nfc_tap_count","as":"taps"},
            {"func":"max","column":"temperature_c","as":"temp"}
          ]}`, domain.TableTelemetry), true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var res columnar.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("got %d label groups, want 2", len(res.Rows))
		}
		byLabel := map[string]map[string]float64{}
		for _, r := range res.Rows {
			byLabel[r.Group["label_id"]] = r.Values
		}
		if byLabel["lbl-1"]["battery"] != 2980 || byLabel["lbl-1"]["taps"] != 14 {
			t.Errorf("lbl-1 = %+v", byLabel["lbl-1"])
		}
		if math.Abs(byLabel["lbl-1"]["temp"]-4.2) > 1e-9 {
			t.Errorf("temperature = %v, want 4.2 exactly through the XOR encoding", byLabel["lbl-1"]["temp"])
		}
	})

	t.Run("a delivery failure is stored with an outcome, not a zero latency", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/query", fmt.Sprintf(`{
          "table": %q, "group_by": ["outcome"],
          "aggregates": [{"func":"count","as":"n"},{"func":"avg","column":"latency_ms","as":"latency"}]}`,
			domain.TableDelivery), true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var res columnar.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("got %d outcome groups, want delivered and failed", len(res.Rows))
		}
		for _, r := range res.Rows {
			if r.Group["outcome"] == "delivered" && r.Values["latency"] != 1450 {
				t.Errorf("delivered latency = %v, want 1450", r.Values["latency"])
			}
		}
	})

	t.Run("a query cannot name another tenant", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/query", fmt.Sprintf(`{
          "table": %q, "filters": [{"column":"tenant_id","op":"eq","value":"rival"}],
          "aggregates":[{"func":"count","as":"n"}]}`, domain.TableTelemetry), true)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403: %s", rec.Code, rec.Body)
		}
	})

	t.Run("another tenant sees nothing", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/query", strings.NewReader(fmt.Sprintf(
			`{"table": %q, "aggregates":[{"func":"count","as":"n"}]}`, domain.TableTelemetry)))
		req.Header.Set(TenantHeader, "rival")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var res columnar.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(res.Rows) != 0 {
			t.Errorf("another tenant saw %d rows", len(res.Rows))
		}
	})

	t.Run("an unknown table is a 400", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/query", `{"table":"nope"}`, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", rec.Code)
		}
	})

	t.Run("a missing tenant header is a 401", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/query", `{"table":"x"}`, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})
}

// TestSLOMatchesAHandComputedExpectation builds a delivery set whose SLO
// arithmetic can be done on paper, and checks every field of the answer.
func TestSLOMatchesAHandComputedExpectation(t *testing.T) {
	svc := newTestService(t)
	in := svc.Ingest()

	// 1,000 confirmations. 990 land at 2,200 ms of which 1,500 is the E-Ink
	// waveform, 5 at 5,000 ms (delivered but outside the three-second budget),
	// and 5 fail outright.
	//
	// Against the price_latency objective of 99.5%:
	//   total = 1000, good = 990, achieved = 99.0%, so the objective is missed.
	//   budget = 0.5% of 1000 = 5 events; spent = 10; remaining = -100%.
	// Against delivery_success at 99.9%:
	//   good = 995, achieved = 99.5%, budget = 1 event, spent = 5, remaining
	//   = -400%.
	for i := 0; i < 990; i++ {
		if err := in.Envelope(envelopeOf(t, canon.EvtLabelDelivered, "label", "l", base.Add(time.Duration(i)*time.Second),
			canon.LabelDelivered{LabelID: canon.LabelID(fmt.Sprintf("lbl-%d", i%50)), StoreID: "s1",
				DeliveredAt: base.Add(time.Duration(i) * time.Second), LatencyMS: 2200,
				MeshHops: 2, RefreshMS: 1500})); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := in.Envelope(envelopeOf(t, canon.EvtLabelDelivered, "label", "l", base.Add(time.Duration(1000+i)*time.Second),
			canon.LabelDelivered{LabelID: "lbl-slow", StoreID: "s2",
				DeliveredAt: base.Add(time.Duration(1000+i) * time.Second), LatencyMS: 5000,
				MeshHops: 3, RefreshMS: 1500})); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := in.Envelope(envelopeOf(t, canon.EvtLabelDeliveryFailed, "label", "l",
			base.Add(time.Duration(2000+i)*time.Second),
			canon.LabelDeliveryFailed{LabelID: "lbl-dead", StoreID: "s3",
				Reason: "unreachable", Attempts: 3})); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	scope := app.Scope{Tenant: tenant, From: base, To: base.Add(24 * time.Hour)}
	report, err := app.ComputeSLOReport(svc.Tables(), scope, domain.DefaultSLOs())
	if err != nil {
		t.Fatalf("slo: %v", err)
	}

	byName := map[string]domain.SLOResult{}
	for _, r := range report.Results {
		byName[r.Target.Name] = r
	}

	latency := byName["price_latency"]
	if latency.Total != 1000 || latency.Good != 990 {
		t.Fatalf("latency counts = %d good of %d, want 990 of 1000", latency.Good, latency.Total)
	}
	if math.Abs(latency.Achieved-0.99) > 1e-9 {
		t.Errorf("achieved = %.6f, want 0.99", latency.Achieved)
	}
	if latency.Met {
		t.Error("a 99.0%% achievement met a 99.5%% objective")
	}
	if math.Abs(latency.BudgetTotal-5) > 1e-9 {
		t.Errorf("budget = %v events, want 5", latency.BudgetTotal)
	}
	if math.Abs(latency.BudgetSpent-10) > 1e-9 {
		t.Errorf("spent = %v events, want 10", latency.BudgetSpent)
	}
	if math.Abs(latency.BudgetRemainingPct-(-100)) > 1e-9 {
		t.Errorf("remaining = %v%%, want -100%%", latency.BudgetRemainingPct)
	}
	// The window is 30 days and one day elapsed, so the elapsed fraction is
	// 1/30 and the budget fraction is 2. Burn rate = 2 / (1/30) = 60.
	if math.Abs(latency.BurnRate-60) > 1e-6 {
		t.Errorf("burn rate = %.4f, want 60", latency.BurnRate)
	}
	if latency.Severity() != "page" {
		t.Errorf("severity = %q at a burn rate of %.1f, want page", latency.Severity(), latency.BurnRate)
	}
	// The p50 is 2,200 ms exactly. The p99 is *also* 2,200, and that is the
	// right answer rather than a bug: exactly ten of the thousand values lie
	// outside the 2,200 ms mass (five zeros from the failures below it and five
	// at 5,000 above it), so the 99th percentile still falls inside it. A test
	// that expected the tail here would be asserting an arithmetic error.
	if math.Abs(latency.P50-2200) > 20 {
		t.Errorf("p50 = %.1f ms, want about 2200", latency.P50)
	}
	if math.Abs(latency.P99-2200) > 40 {
		t.Errorf("p99 = %.1f ms, want about 2200 — only 0.5%% of values are above it", latency.P99)
	}
	if latency.P95 < 2100 || latency.P95 > 2300 {
		t.Errorf("p95 = %.1f ms, want about 2200", latency.P95)
	}
	if !latency.Approximate {
		t.Error("percentiles from a t-digest must be marked approximate")
	}

	success := byName["delivery_success"]
	if success.Good != 995 || success.Total != 1000 {
		t.Errorf("success counts = %d of %d, want 995 of 1000", success.Good, success.Total)
	}
	if math.Abs(success.BudgetRemainingPct-(-400)) > 1e-6 {
		t.Errorf("success budget remaining = %v%%, want -400%%", success.BudgetRemainingPct)
	}

	if report.Latency.WithinBudgetPct < 98.9 || report.Latency.WithinBudgetPct > 99.1 {
		t.Errorf("within budget = %.2f%%, want 99%%", report.Latency.WithinBudgetPct)
	}
	// 2,200 ms total less a 1,500 ms waveform leaves 700 ms spread across
	// ingest, the stream, the Label Service, the broker, the bridge and the
	// radio — the six hops this service cannot measure directly.
	if math.Abs(report.Latency.UnattributedP50-700) > 60 {
		t.Errorf("unattributed p50 = %.1f ms, want about 700", report.Latency.UnattributedP50)
	}

	// Per-store attribution must name the slow and the dead store.
	byStore := map[string]app.StoreSLO{}
	for _, s := range report.ByStore {
		byStore[s.StoreID] = s
	}
	if byStore["s1"].AchievedPct != 100 {
		t.Errorf("the healthy store achieved %.1f%%, want 100%%", byStore["s1"].AchievedPct)
	}
	if byStore["s2"].AchievedPct != 0 || byStore["s3"].AchievedPct != 0 {
		t.Errorf("the slow and dead stores achieved %.1f%% and %.1f%%, want 0%% each",
			byStore["s2"].AchievedPct, byStore["s3"].AchievedPct)
	}
	t.Logf("hand-computed SLO: latency %s", latency.String())
	t.Logf("hand-computed SLO: success %s", success.String())
}

func TestSLOEndpointReportsSeverity(t *testing.T) {
	svc := newTestService(t)
	in := svc.Ingest()
	for i := 0; i < 200; i++ {
		if err := in.Envelope(envelopeOf(t, canon.EvtLabelDelivered, "label", "l",
			base.Add(time.Duration(i)*time.Second),
			canon.LabelDelivered{LabelID: "lbl-1", StoreID: "s1",
				DeliveredAt: base.Add(time.Duration(i) * time.Second),
				LatencyMS:   900, MeshHops: 2, RefreshMS: 1500})); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	from := base.Format(time.RFC3339)
	to := base.Add(24 * time.Hour).Format(time.RFC3339)
	rec := do(t, svc.Handler(), "GET", "/v1/slo?from="+from+"&to="+to, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Report   app.SLOReport `json:"report"`
		Severity string        `json:"severity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Every delivery was inside the budget, so latency and success are met. The
	// availability objective is not, because 200 reports over a day is far
	// fewer than a label reporting every five minutes would produce — and the
	// severity must reflect the worst of the three, not the first.
	byName := map[string]domain.SLOResult{}
	for _, r := range body.Report.Results {
		byName[r.Target.Name] = r
	}
	if !byName["price_latency"].Met || !byName["delivery_success"].Met {
		t.Errorf("a fully healthy delivery set missed an objective: %+v", byName)
	}
	if body.Severity == "" {
		t.Error("no severity reported")
	}
}

// TestSLOWithNoDataIsNotABreach covers the store that was closed.
func TestSLOWithNoDataIsNotABreach(t *testing.T) {
	svc := newTestService(t)
	scope := app.Scope{Tenant: tenant, From: base, To: base.Add(time.Hour)}
	report, err := app.ComputeSLOReport(svc.Tables(), scope, domain.DefaultSLOs())
	if err != nil {
		t.Fatalf("slo: %v", err)
	}
	for _, r := range report.Results {
		if !r.Met {
			t.Errorf("%s reported a breach with no events at all: %+v", r.Target.Name, r)
		}
		if r.Severity() != "ok" {
			t.Errorf("%s severity = %q with no events", r.Target.Name, r.Severity())
		}
	}
}

// seedPriceHistory writes daily price and outcome rows for a SKU across a range
// of price points, so the elasticity curve and the shrinkage report have
// something to read.
func seedPriceHistory(t *testing.T, svc *Service) {
	t.Helper()
	in := svc.Ingest()
	prices := []float64{100, 110, 120, 130, 140}
	var outcomes []app.PriceOutcome
	for d := 0; d < 60; d++ {
		p := prices[d%len(prices)]
		// Demand falls with price; waste rises with the pricing delay, which is
		// itself longer on the days the store was slow.
		units := 500 - 2*p
		delay := float64((d % 6) * 900) // 0 to 4500 seconds
		waste := units * (0.01 + delay/200000)
		outcomes = append(outcomes, app.PriceOutcome{
			Tenant: tenant, Store: "s1", SKU: "milk", Category: "dairy",
			Day: base.AddDate(0, 0, d), PriceMinor: p, UnitCost: 60,
			Competitor: p * 1.1, UnitsSold: units, WasteUnits: waste,
			DelaySeconds: delay, Currency: "GBP",
		})
	}
	if err := in.AppendOutcomes(outcomes...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestElasticityCurveReport(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc)

	from := base.Format(time.RFC3339)
	to := base.AddDate(0, 0, 70).Format(time.RFC3339)
	rec := do(t, svc.Handler(), "GET", "/v1/reports/elasticity/milk?from="+from+"&to="+to, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var curve app.ElasticityCurve
	if err := json.Unmarshal(rec.Body.Bytes(), &curve); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if curve.DistinctPrices != 5 {
		t.Fatalf("got %d price points, want 5: %+v", curve.DistinctPrices, curve.Points)
	}
	// Demand must fall as price rises, which is how the seed was built.
	for i := 1; i < len(curve.Points); i++ {
		if curve.Points[i].UnitsPerDay >= curve.Points[i-1].UnitsPerDay {
			t.Errorf("demand did not fall between %v and %v",
				curve.Points[i-1].PriceMinor, curve.Points[i].PriceMinor)
		}
	}
	if !strings.Contains(curve.Caveat, "correlation") {
		t.Errorf("caveat = %q, want it to say the curve is not causal", curve.Caveat)
	}
	t.Logf("synthetic curve: %d price points from %v to %v, %v to %v units per day",
		curve.DistinctPrices, curve.Points[0].PriceMinor, curve.Points[len(curve.Points)-1].PriceMinor,
		curve.Points[0].UnitsPerDay, curve.Points[len(curve.Points)-1].UnitsPerDay)
}

func TestShrinkageAgainstDelayReport(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc)

	from := base.Format(time.RFC3339)
	to := base.AddDate(0, 0, 70).Format(time.RFC3339)
	rec := do(t, svc.Handler(), "GET", "/v1/reports/shrinkage?from="+from+"&to="+to, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var report app.ShrinkageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(report.Buckets) < 3 {
		t.Fatalf("got %d delay bands with data, want at least 3: %+v", len(report.Buckets), report.Buckets)
	}
	// The seed makes waste rise with delay, so the correlation must be strongly
	// positive.
	if report.Correlation < 0.8 {
		t.Errorf("correlation = %.3f on data built to correlate, want above 0.8", report.Correlation)
	}
	if !strings.Contains(report.Interpretation, "not a causal estimate") {
		t.Errorf("interpretation = %q, want it to disclaim causality", report.Interpretation)
	}
	t.Logf("synthetic shrinkage: r = %.3f across %d delay bands; waste rate %.2f%% in the fastest band "+
		"and %.2f%% in the slowest",
		report.Correlation, len(report.Buckets),
		report.Buckets[0].WasteRatePct, report.Buckets[len(report.Buckets)-1].WasteRatePct)
}

func TestCompetitivePositionReport(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc)
	from := base.Format(time.RFC3339)
	to := base.AddDate(0, 0, 70).Format(time.RFC3339)
	rec := do(t, svc.Handler(), "GET", "/v1/reports/competitive?from="+from+"&to="+to, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var report app.CompetitiveReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("got %d rows, want one SKU", len(report.Rows))
	}
	// Every seeded competitor price is 10% above ours, so the index is 1/1.1.
	want := 100 / 1.1
	if math.Abs(report.Rows[0].IndexPct-want) > 0.5 {
		t.Errorf("index = %.2f, want about %.2f", report.Rows[0].IndexPct, want)
	}
	if math.Abs(report.OverallIndexPct-want) > 0.5 {
		t.Errorf("overall index = %.2f, want about %.2f", report.OverallIndexPct, want)
	}
	t.Logf("synthetic competitive position: index %.1f (100 is parity, below is cheaper)",
		report.OverallIndexPct)
}

func TestInteractionReportUsesCounterDifferences(t *testing.T) {
	svc := newTestService(t)
	in := svc.Ingest()
	// One label whose cumulative tap counter goes from 100 to 140 over the
	// window: 40 taps, not 100+110+...+140.
	for i, count := range []int64{100, 110, 125, 140} {
		batch := []canon.Telemetry{{
			LabelID: "lbl-1", StoreID: "s1", ReportedAt: base.Add(time.Duration(i) * time.Hour),
			NFCTapCount: count, BatteryMV: 3000, FirmwareVer: "1.5.0",
		}}
		if err := in.Envelope(envelopeOf(t, canon.EvtDeviceTelemetry, "sec", "sec-1",
			base.Add(time.Duration(i)*time.Hour), batch)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	report, err := app.LabelInteraction(svc.Tables(),
		app.Scope{Tenant: tenant, From: base, To: base.Add(24 * time.Hour)},
		[]string{"store_id"}, 0)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(report.Rows), report.Rows)
	}
	if report.Rows[0].Taps != 40 {
		t.Errorf("taps = %v, want 40 (the counter difference, not its sum)", report.Rows[0].Taps)
	}
	if report.Rows[0].TapsPerLabel != 40 {
		t.Errorf("taps per label = %v, want 40", report.Rows[0].TapsPerLabel)
	}
	if !strings.Contains(report.Caveat, "not with the product") {
		t.Errorf("caveat = %q", report.Caveat)
	}
}

func TestCrossStoreBenchmarkRanksStores(t *testing.T) {
	svc := newTestService(t)
	in := svc.Ingest()
	// Five stores with deliberately different latency profiles.
	latencies := map[canon.StoreID]int64{"s1": 500, "s2": 900, "s3": 1400, "s4": 2200, "s5": 4000}
	i := 0
	for store, latency := range latencies {
		for n := 0; n < 100; n++ {
			at := base.Add(time.Duration(i) * time.Second)
			if err := in.Envelope(envelopeOf(t, canon.EvtLabelDelivered, "label", "l", at,
				canon.LabelDelivered{LabelID: canon.LabelID(fmt.Sprintf("lbl-%d", n)), StoreID: store,
					DeliveredAt: at, LatencyMS: latency, MeshHops: 2, RefreshMS: 1500})); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			i++
		}
	}
	if err := in.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	from := base.Format(time.RFC3339)
	to := base.Add(24 * time.Hour).Format(time.RFC3339)
	rec := do(t, svc.Handler(), "GET",
		"/v1/reports/benchmark?from="+from+"&to="+to+"&metric=latency_ms&q=0.95", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var report app.BenchmarkReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(report.Rows) != 5 {
		t.Fatalf("got %d stores, want 5", len(report.Rows))
	}
	// Lower is better for latency, so the ordering is ascending and the worst
	// store is the slow one.
	if report.Rows[0].StoreID != "s1" {
		t.Errorf("best store = %s, want s1", report.Rows[0].StoreID)
	}
	if report.Rows[len(report.Rows)-1].StoreID != "s5" {
		t.Errorf("worst store = %s, want s5", report.Rows[len(report.Rows)-1].StoreID)
	}
	if math.Abs(report.Median-1400) > 50 {
		t.Errorf("median = %.1f, want about 1400", report.Median)
	}
	found := false
	for _, s := range report.Worst {
		if s == "s5" {
			found = true
		}
	}
	if !found {
		t.Errorf("worst = %v, want it to name s5", report.Worst)
	}
	t.Logf("synthetic benchmark: median p95 %.0f ms, p10 %.0f, p90 %.0f, worst %v",
		report.Median, report.P10, report.P90, report.Worst)
}

func TestRetentionEndpointAndSweep(t *testing.T) {
	svc := newTestService(t)
	rec := do(t, svc.Handler(), "GET", "/v1/retention", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Policies []struct {
			Table     string `json:"table"`
			HotHuman  string `json:"hot_human"`
			WarmHuman string `json:"warm_human"`
		} `json:"policies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Policies) != len(domain.DefaultRetention()) {
		t.Errorf("got %d policies, want %d", len(body.Policies), len(domain.DefaultRetention()))
	}

	if rec := do(t, svc.Handler(), "POST", "/v1/retention:sweep", "", true); rec.Code != http.StatusOK {
		t.Errorf("sweep: %d %s", rec.Code, rec.Body)
	}
}

func TestRetentionPolicyValidation(t *testing.T) {
	tests := []struct {
		name string
		p    domain.RetentionPolicy
		ok   bool
	}{
		{"sane", domain.RetentionPolicy{Table: "t", Hot: time.Hour, Warm: 2 * time.Hour, Cold: 3 * time.Hour}, true},
		{"zero hot", domain.RetentionPolicy{Table: "t", Hot: 0, Warm: time.Hour}, false},
		{"warm shorter than hot", domain.RetentionPolicy{Table: "t", Hot: 2 * time.Hour, Warm: time.Hour}, false},
		{"cold shorter than warm", domain.RetentionPolicy{Table: "t", Hot: time.Hour, Warm: 2 * time.Hour, Cold: time.Hour}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if tt.ok && err != nil {
				t.Errorf("rejected a sane policy: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("accepted a policy that would delete what it just moved")
			}
		})
	}
}

func TestTablesEndpointDescribesTheSchemas(t *testing.T) {
	svc := newTestService(t)
	rec := do(t, svc.Handler(), "GET", "/v1/tables", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Tables map[string]struct {
			Schema columnar.Schema `json:"schema"`
		} `json:"tables"`
		Operators  []string `json:"operators"`
		Aggregates []string `json:"aggregates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tables) != 4 {
		t.Errorf("described %d tables, want 4", len(body.Tables))
	}
	if len(body.Operators) == 0 || len(body.Aggregates) == 0 {
		t.Error("the catalogue does not list the operators or aggregates a caller may use")
	}
	for name, info := range body.Tables {
		if info.Schema.TimeColumn == "" {
			t.Errorf("table %s has no time column in its published schema", name)
		}
	}
}

func TestReadinessChecksReportTheStore(t *testing.T) {
	svc := newTestService(t)
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
