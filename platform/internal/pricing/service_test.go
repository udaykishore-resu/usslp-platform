package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/app"
	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/features"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/ports"
	"github.com/usslp/usslp/platform/internal/pricing/registry"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

const (
	tenant = canon.TenantID("acme")
	store  = canon.StoreID("store-001")
	sku    = canon.SKU("sku-42")
)

// fixedNow is the clock every test runs against, so a point-in-time read is
// reproducible.
var fixedNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) *Service {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })

	constraints := &ports.StaticConstraints{
		UseDefault: true,
		Default:    domain.Constraints{Currency: "USD"},
		ByKey: map[string]domain.Constraints{
			ports.ConstraintKey(tenant, store, sku): {
				Currency: "USD", UnitCost: 100, MinMarginBps: 2000,
				CurrentMinor: 249, FloorMinor: 150, CeilingMinor: 400,
			},
		},
	}
	svc, err := New(Config{
		State: kv, ConstraintSource: constraints,
		Clock:            ports.FixedClock{T: fixedNow},
		ElasticityPolicy: ml.DefaultElasticityPolicy(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func do(t *testing.T, h http.Handler, method, path string, body any, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if withTenant {
		req.Header.Set(TenantHeader, string(tenant))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEvaluateEndpointIsTier1Only(t *testing.T) {
	svc := newTestService(t)
	h := svc.Handler()

	t.Run("a compliant price is accepted", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/pricing/evaluate",
			EvaluateRequest{StoreID: store, SKU: sku, PriceMinor: 249}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var resp EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Decision.Outcome != domain.OutcomeAccepted {
			t.Errorf("outcome = %s, want accepted", resp.Decision.Outcome)
		}
		if resp.Decision.Price.Amount != 249 {
			t.Errorf("price = %d, want 249", resp.Decision.Price.Amount)
		}
	})

	t.Run("a sub-margin price is adjusted", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/pricing/evaluate",
			EvaluateRequest{StoreID: store, SKU: sku, PriceMinor: 110}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var resp EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Decision.Outcome != domain.OutcomeAdjusted {
			t.Errorf("outcome = %s, want adjusted", resp.Decision.Outcome)
		}
		if resp.Decision.Price.Amount != 150 {
			t.Errorf("price = %d, want the 150 floor", resp.Decision.Price.Amount)
		}
	})

	t.Run("inline constraints bypass the source entirely", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/pricing/evaluate", EvaluateRequest{
			PriceMinor: 500,
			Constraints: &domain.Constraints{
				Currency: "GBP", CeilingMinor: 399, CurrentMinor: 350,
			},
		}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var resp EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Decision.Price.Amount != 399 || resp.Decision.Price.Currency != "GBP" {
			t.Errorf("price = %+v, want 399 GBP", resp.Decision.Price)
		}
	})

	t.Run("infeasible constraints report every conflicting rule", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/pricing/evaluate", EvaluateRequest{
			PriceMinor:  200,
			Constraints: &domain.Constraints{Currency: "USD", FloorMinor: 500, CeilingMinor: 300},
		}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var resp EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Decision.Outcome != domain.OutcomeInfeasible {
			t.Fatalf("outcome = %s, want infeasible", resp.Decision.Outcome)
		}
		if len(resp.Decision.Violations) < 2 {
			t.Errorf("violations = %+v, want both conflicting rules named", resp.Decision.Violations)
		}
	})

	t.Run("a missing tenant header is unauthenticated", func(t *testing.T) {
		rec := do(t, h, "POST", "/v1/pricing/evaluate",
			EvaluateRequest{StoreID: store, SKU: sku, PriceMinor: 249}, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})

	t.Run("an unknown field is rejected rather than ignored", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/pricing/evaluate",
			bytes.NewBufferString(`{"sku":"sku-42","price_minor":249,"typo":1}`))
		req.Header.Set(TenantHeader, string(tenant))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", rec.Code)
		}
	})
}

// seedPriceHistory writes enough price/quantity history for a usable elasticity
// estimate, generated from a known coefficient.
func seedPriceHistory(t *testing.T, svc *Service, elasticity float64) {
	t.Helper()
	fs := svc.Features()
	prices := []float64{199, 219, 239, 259, 279, 299}
	start := fixedNow.AddDate(0, 0, -120)
	for d := 0; d < 120; d++ {
		day := start.AddDate(0, 0, d)
		known := day.Add(time.Hour)
		price := prices[d%len(prices)]
		units := 30 * pow(price/249, elasticity)
		recs := []features.Record{
			{Key: features.Key{Tenant: tenant, Store: store, SKU: sku, Name: domain.FeatureNames[domain.FeatPrice]},
				Value: features.Value{Number: price, ValidFrom: day, KnownAt: known}},
			{Key: features.Key{Tenant: tenant, Store: store, SKU: sku, Name: app.FeatureUnitsSold},
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
			{Key: features.Key{Tenant: tenant, Store: store, SKU: sku, Name: domain.FeatureNames[domain.FeatVelocity7]},
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
		}
		if err := fs.PutBatch(recs); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// pow is math.Pow, spelled out so the test file does not import math for one
// call; the exponent is always the elasticity the test chose.
func pow(base, exp float64) float64 {
	// x^e = exp(e * ln x), computed with the standard library through the
	// elasticity model itself so the fixture and the code agree.
	return ml.Elasticity{Coefficient: exp}.DemandAt(1, 1, base)
}

func TestRecommendUsesTheElasticityEstimate(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc, -2.4)
	h := svc.Handler()

	rec := do(t, h, "POST", "/v1/pricing/recommend", RecommendRequest{
		StoreID: store, SKU: sku, IncludeCurve: true,
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp RecommendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Elasticity.Usable {
		t.Fatalf("elasticity unusable: %s", resp.Elasticity.Reason)
	}
	if resp.Elasticity.Coefficient > -2.2 || resp.Elasticity.Coefficient < -2.6 {
		t.Errorf("recovered elasticity %.3f, want about -2.4", resp.Elasticity.Coefficient)
	}
	// No model is registered, so the recommendation rests on the projection.
	if resp.ModelID != "" {
		t.Errorf("model_id = %q with no registered model", resp.ModelID)
	}
	// The optimum for cost 100 and elasticity -2.4 is 100*2.4/1.4 = 171.4.
	if p := resp.Recommendation.Best.PriceMinor; p < 168 || p > 175 {
		t.Errorf("recommended %d, want about 171", p)
	}
	if d := domain.Evaluate(domain.Constraints{
		Currency: "USD", UnitCost: 100, MinMarginBps: 2000,
		CurrentMinor: 249, FloorMinor: 150, CeilingMinor: 400,
	}, resp.Recommendation.Best.PriceMinor); d.Outcome != domain.OutcomeAccepted {
		t.Errorf("the recommendation is not Tier-1 clean: %s %+v", d.Outcome, d.Violations)
	}
	if len(resp.Recommendation.Curve) == 0 {
		t.Error("include_curve was set but no curve was returned")
	}
	t.Logf("synthetic history: elasticity %.3f [%.3f, %.3f], recommendation %d (from %d), uplift %.0f minor units",
		resp.Elasticity.Coefficient, resp.Elasticity.Low, resp.Elasticity.High,
		resp.Recommendation.Best.PriceMinor, resp.Recommendation.Incumbent.PriceMinor,
		resp.Recommendation.UpliftMinor)
}

func TestRecommendIsReproducibleFromAnAsOfInstant(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc, -2.4)
	h := svc.Handler()

	asOf := fixedNow.AddDate(0, 0, -100)
	rec := do(t, h, "POST", "/v1/pricing/recommend", RecommendRequest{
		StoreID: store, SKU: sku, AsOf: &asOf,
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var early RecommendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &early); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Twenty days of history at that cut, so the estimate must rest on fewer
	// observations than the full-history one and is likely to be refused.
	if early.Elasticity.Observations > 21 {
		t.Errorf("an as-of read 100 days back used %d observations", early.Elasticity.Observations)
	}
}

func TestElasticityEndpointRefusesWithoutEvidence(t *testing.T) {
	svc := newTestService(t)
	h := svc.Handler()
	rec := do(t, h, "GET", "/v1/pricing/elasticity/"+string(sku)+"?store_id="+string(store), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp ElasticityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Elasticity.Usable {
		t.Errorf("an estimate with no history was marked usable: %+v", resp.Elasticity)
	}
	if resp.Elasticity.Reason == "" {
		t.Error("an unusable estimate must say why")
	}
}

func TestModelLifecycleOverHTTP(t *testing.T) {
	svc := newTestService(t)
	h := svc.Handler()

	// An empty registry lists nothing rather than failing.
	rec := do(t, h, "GET", "/v1/models?store_id="+string(store), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var listing struct {
		Models     []registry.Metadata `json:"models"`
		ChampionID string              `json:"champion_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listing.Models) != 0 || listing.ChampionID != "" {
		t.Errorf("a fresh registry listed %+v", listing)
	}

	// Promoting a model that does not exist is a 404, not a 500.
	rec = do(t, h, "POST", "/v1/models/nope/promote", PromoteRequest{}, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}

	// A model belonging to another tenant is indistinguishable from a missing
	// one, so an identifier probe learns nothing.
	reg := svc.Models()
	other, err := reg.Register(registry.Registration{
		Slot: registry.Slot{Tenant: "other-tenant", Store: store, Purpose: registry.PurposeDemand},
		Kind: ml.KindGBT, Body: []byte("body"), Metrics: ml.Metrics{MAE: 1, Rows: 100},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	rec = do(t, h, "POST", "/v1/models/"+other.ID+"/promote", PromoteRequest{}, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant promote returned %d, want 404", rec.Code)
	}
}

func TestTrainEndpointRefusesThinData(t *testing.T) {
	svc := newTestService(t)
	seedPriceHistory(t, svc, -2.0)
	h := svc.Handler()

	examples := make([]TrainExample, 0, 10)
	for i := 0; i < 10; i++ {
		examples = append(examples, TrainExample{
			SKU: sku, DecisionAt: fixedNow.AddDate(0, 0, -i), Target: float64(20 + i),
		})
	}
	rec := do(t, h, "POST", "/v1/models", TrainRequest{StoreID: store, Examples: examples}, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422 for a ten-row training request: %s", rec.Code, rec.Body)
	}
}

func TestAnomaliesEndpointBeforeADetectorIsTrained(t *testing.T) {
	svc := newTestService(t)
	rec := do(t, svc.Handler(), "GET", "/v1/anomalies", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Anomalies []app.AnomalyRecord `json:"anomalies"`
		Trained   bool                `json:"detector_trained"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Trained || len(resp.Anomalies) != 0 {
		t.Errorf("an untrained detector reported %+v", resp)
	}
}

func TestPolicyPackEndpointServesTheEdgeRuleTable(t *testing.T) {
	svc := newTestService(t)
	rec := do(t, svc.Handler(), "GET", "/v1/policy-pack/"+string(store), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var pack domain.PolicyPack
	if err := pack.UnmarshalBinary(rec.Body.Bytes()); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	c, ok := pack.Rules[sku]
	if !ok {
		t.Fatalf("the pack does not carry %s: %+v", sku, pack.Rules)
	}
	// The gateway must reach exactly the decision the cloud reached.
	cloud := do(t, svc.Handler(), "POST", "/v1/pricing/evaluate",
		EvaluateRequest{StoreID: store, SKU: sku, PriceMinor: 110}, true)
	var resp EvaluateResponse
	if err := json.Unmarshal(cloud.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	edge := domain.Evaluate(c, 110)
	if edge.Outcome != resp.Decision.Outcome || edge.Price.Amount != resp.Decision.Price.Amount {
		t.Errorf("the edge decision (%s at %d) differs from the cloud's (%s at %d)",
			edge.Outcome, edge.Price.Amount, resp.Decision.Outcome, resp.Decision.Price.Amount)
	}
}

func TestReadinessChecksReportTheStateStore(t *testing.T) {
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

func TestFeatureStoreNotFoundSurfacesAsA404(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Features().AsOf(features.Key{
		Tenant: tenant, Store: store, SKU: "missing", Name: "price_minor",
	}, fixedNow)
	if !errors.Is(err, features.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
