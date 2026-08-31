package app

import (
	"math"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestCrossStoreCorrectsForCannibalisation is the property that justifies Tier 3
// existing at all: with close substitutes, the coordinated answer must be worth
// more than the sum of the independent per-line answers, evaluated under the
// same cannibalisation model.
func TestCrossStoreCorrectsForCannibalisation(t *testing.T) {
	// Three near-identical brands of the same product in one store, each
	// stealing volume from the others.
	mk := func(sku canon.SKU, subs []canon.SKU) SKUState {
		s := SKUState{
			SKU: sku, Store: "store-001",
			Constraints: domain.Constraints{
				Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 150, CeilingMinor: 400, GranularityMinor: 5,
			},
			Elasticity:    usableElasticity(-2.2),
			BaselineUnits: 40,
		}
		for _, o := range subs {
			// A strongly positive cross-elasticity: when a rival brand's price
			// rises, this one's demand rises with it.
			s.Substitutes = append(s.Substitutes, Substitute{SKU: o, CrossElasticity: 1.4})
		}
		return s
	}
	states := []SKUState{
		mk("brand-a", []canon.SKU{"brand-b", "brand-c"}),
		mk("brand-b", []canon.SKU{"brand-a", "brand-c"}),
		mk("brand-c", []canon.SKU{"brand-a", "brand-b"}),
	}

	report, err := OptimiseCrossStore(CrossStoreInput{States: states, Objective: ObjectiveProfit})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	if len(report.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(report.Results))
	}
	if report.OptimisedProfitMinor < report.IndependentProfitMinor {
		t.Errorf("the coordinated answer (%.0f) is worse than the independent one (%.0f)",
			report.OptimisedProfitMinor, report.IndependentProfitMinor)
	}
	if report.OptimisedProfitMinor <= report.BaselineProfitMinor {
		t.Errorf("the coordinated answer (%.0f) does not beat leaving the prices alone (%.0f)",
			report.OptimisedProfitMinor, report.BaselineProfitMinor)
	}
	t.Logf("synthetic category of 3 substitutes: baseline %.0f, independent-per-line %.0f, coordinated %.0f "+
		"(%d rounds, converged=%v)",
		report.BaselineProfitMinor, report.IndependentProfitMinor, report.OptimisedProfitMinor,
		report.Rounds, report.Converged)

	// Every recommended price must still satisfy Tier 1.
	for _, r := range report.Results {
		if r.Decision.Outcome != domain.OutcomeAccepted {
			t.Errorf("%s: recommended %d, which Tier 1 does not accept: %s",
				r.SKU, r.RecommendedMinor, r.Decision.Outcome)
		}
	}
}

// TestCrossStoreMatchesIndependentWithoutSubstitutes is the control: with no substitutes,
// the coordinated answer must equal the per-line one, or the coordination is
// introducing an effect that is not in the data.
func TestCrossStoreMatchesIndependentWithoutSubstitutes(t *testing.T) {
	states := []SKUState{
		{SKU: "a", Store: "s1", Elasticity: usableElasticity(-2.5), BaselineUnits: 30,
			Constraints: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 150, CeilingMinor: 400}},
		{SKU: "b", Store: "s1", Elasticity: usableElasticity(-1.6), BaselineUnits: 25,
			Constraints: domain.Constraints{Currency: "USD", UnitCost: 120, CurrentMinor: 300,
				FloorMinor: 150, CeilingMinor: 500}},
	}
	report, err := OptimiseCrossStore(CrossStoreInput{States: states})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	for _, r := range report.Results {
		if r.RecommendedMinor != r.IndependentMinor {
			t.Errorf("%s: coordinated %d differs from independent %d with no substitutes",
				r.SKU, r.RecommendedMinor, r.IndependentMinor)
		}
		if r.CannibalisationUnits != 0 {
			t.Errorf("%s: reported %v units of cannibalisation with no substitutes",
				r.SKU, r.CannibalisationUnits)
		}
	}
	// Each answer must also match the closed-form single-SKU optimum.
	for i, want := range []float64{100 * -2.5 / -1.5, 120 * -1.6 / -0.6} {
		got := float64(report.Results[i].RecommendedMinor)
		if got < want-1.5 || got > want+1.5 {
			t.Errorf("%s: optimum %v, want %.2f", report.Results[i].SKU, got, want)
		}
	}
}

func TestCrossStoreSkipsInfeasibleSKUsRatherThanFailing(t *testing.T) {
	states := []SKUState{
		{SKU: "good", Store: "s1", Elasticity: usableElasticity(-2), BaselineUnits: 30,
			Constraints: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 150, CeilingMinor: 400}},
		{SKU: "conflicted", Store: "s1", Elasticity: usableElasticity(-2), BaselineUnits: 30,
			Constraints: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 500, CeilingMinor: 300}},
	}
	report, err := OptimiseCrossStore(CrossStoreInput{States: states})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	if len(report.Results) != 1 || report.Results[0].SKU != "good" {
		t.Errorf("results = %+v, want only the feasible SKU", report.Results)
	}
	if report.Skipped["s1/conflicted"] == "" {
		t.Errorf("skipped = %v, want the conflicted SKU with a reason", report.Skipped)
	}
}

func TestCrossStoreKeepsStoresApart(t *testing.T) {
	// The same SKU in two stores, each naming the other's SKU as a substitute.
	// Cannibalisation is a within-store effect, so the cross term must not
	// apply across stores.
	base := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
		FloorMinor: 150, CeilingMinor: 400}
	states := []SKUState{
		{SKU: "a", Store: "s1", Constraints: base, Elasticity: usableElasticity(-2), BaselineUnits: 30,
			Substitutes: []Substitute{{SKU: "a", CrossElasticity: 5}}},
		{SKU: "a", Store: "s2", Constraints: base, Elasticity: usableElasticity(-2), BaselineUnits: 30,
			Substitutes: []Substitute{{SKU: "a", CrossElasticity: 5}}},
	}
	report, err := OptimiseCrossStore(CrossStoreInput{States: states})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	for _, r := range report.Results {
		// Each state names itself as a substitute within its own store, which
		// the index resolves to itself; what must not happen is one store's
		// price moving the other's demand.
		if r.RecommendedMinor != report.Results[0].RecommendedMinor {
			t.Errorf("two identical stores got different answers: %+v", report.Results)
		}
	}
}

func TestForecastIsRecursiveAndNonNegative(t *testing.T) {
	net, err := ml.NewLSTM(2, 4, 11)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	history := [][]float64{{1, 10}, {2, 12}, {3, 11}, {4, 13}}
	out, err := Forecast(net, ForecastInput{History: history, Horizon: 5, LastKnown: 13, LagFeature: 1})
	if err != nil {
		t.Fatalf("forecast: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("got %d steps, want 5", len(out))
	}
	for i, v := range out {
		if v < 0 {
			t.Errorf("step %d forecast negative demand %v", i, v)
		}
	}
}

func TestForecastRejectsEmptyHistory(t *testing.T) {
	net, err := ml.NewLSTM(2, 3, 1)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := Forecast(net, ForecastInput{Horizon: 3, LagFeature: -1}); err == nil {
		t.Error("a forecast with no history was accepted")
	}
	if _, err := Forecast(nil, ForecastInput{History: [][]float64{{1, 2}}}); err == nil {
		t.Error("a forecast with no model was accepted")
	}
}

// TestForecastUnitsRecentresTheTreeModel checks the Tier-3 ensemble: the
// sequence model supplies the level and the tree model supplies the response to
// price, and the combination must honour both.
func TestForecastUnitsRecentresTheTreeModel(t *testing.T) {
	forecast := 80.0
	states := []SKUState{{
		SKU: "a", Store: "s1",
		Constraints: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
			FloorMinor: 150, CeilingMinor: 400},
		Elasticity: usableElasticity(-2.5), BaselineUnits: 40,
		ForecastUnits: &forecast,
	}}
	report, err := OptimiseCrossStore(CrossStoreInput{
		States: states,
		Model:  elasticDemand{refPrice: 250, refUnits: 40, elasticity: -2.5},
	})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	r := report.Results[0]
	// The price is unchanged by a level shift under constant elasticity — the
	// optimum depends on the elasticity and the cost, not the volume — but the
	// expected units must follow the forecast level, doubled from the tree
	// model's own 40.
	want := 80 * math.Pow(float64(r.RecommendedMinor)/250, -2.5)
	if rel := math.Abs(r.ExpectedUnits-want) / want; rel > 1e-9 {
		t.Errorf("expected units %.4f, want %.4f re-centred on the forecast", r.ExpectedUnits, want)
	}
}

func TestTelemetryFeaturesFrom(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	cur := canon.Telemetry{
		LabelID: "lbl-1", BatteryMV: 2900, TemperatureC: 21.5, LQI: 180, RSSI: -60,
		RefreshCount: 3650, UptimeSeconds: 365 * 86400, ReportedAt: now,
	}
	prev := canon.Telemetry{BatteryMV: 2910, ReportedAt: now.Add(-48 * time.Hour)}

	f := TelemetryFeaturesFrom(cur, prev)
	if f.RefreshPerDay < 9.9 || f.RefreshPerDay > 10.1 {
		t.Errorf("refreshes per day = %v, want 10", f.RefreshPerDay)
	}
	if f.BatteryDropMVPerDay < 4.9 || f.BatteryDropMVPerDay > 5.1 {
		t.Errorf("discharge rate = %v mV/day, want 5", f.BatteryDropMVPerDay)
	}

	// A first report has no predecessor, and the rate must be reported as zero
	// rather than invented — otherwise every label in a store rollout is
	// flagged on its first heartbeat.
	f = TelemetryFeaturesFrom(cur, canon.Telemetry{})
	if f.BatteryDropMVPerDay != 0 {
		t.Errorf("a first report produced a discharge rate of %v", f.BatteryDropMVPerDay)
	}
}
