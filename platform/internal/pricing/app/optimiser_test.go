package app

import (
	"math"
	"testing"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
)

// elasticDemand is a demand model with a known constant elasticity, so the
// optimiser's answer can be checked against the closed-form optimum instead of
// against a remembered number.
//
// For q = k*p^e with e < -1, expected profit (p - c)*q is maximised at
// p* = c*e/(1+e). That identity is the whole point of the fixture: it gives the
// test a right answer computed independently of the code under test.
type elasticDemand struct {
	refPrice, refUnits, elasticity float64
}

func (d elasticDemand) Predict(x []float64) float64 {
	p := x[domain.FeatPrice]
	if p <= 0 {
		return 0
	}
	return d.refUnits * math.Pow(p/d.refPrice, d.elasticity)
}

func usableElasticity(e float64) ml.Elasticity {
	return ml.Elasticity{
		Coefficient: e, Low: e - 0.2, High: e + 0.2, StdErr: 0.1,
		ConfidenceLevel: 0.95, Observations: 60, DistinctPrices: 6, Usable: true,
	}
}

// TestOptimiserFindsTheClosedFormOptimum checks the search against the analytic
// profit-maximising price for a constant-elasticity demand curve.
func TestOptimiserFindsTheClosedFormOptimum(t *testing.T) {
	const cost, elasticity = 100.0, -2.5
	// p* = c*e/(1+e) = 100 * -2.5 / -1.5 = 166.67 -> 166 or 167 on a one-cent
	// lattice.
	want := cost * elasticity / (1 + elasticity)

	c := domain.Constraints{
		Currency: "USD", UnitCost: int64(cost), CurrentMinor: 200,
		FloorMinor: 110, CeilingMinor: 400,
	}
	rec, err := Optimise(OptimisationInput{
		Constraints: c,
		Model:       elasticDemand{refPrice: 200, refUnits: 40, elasticity: elasticity},
		Elasticity:  usableElasticity(elasticity),
		Features:    domain.Features{Velocity7: 40},
		Objective:   ObjectiveProfit,
	})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	if math.Abs(float64(rec.Best.PriceMinor)-want) > 1.5 {
		t.Errorf("optimum = %d, want %.2f within a cent", rec.Best.PriceMinor, want)
	}
	if !rec.Confident {
		t.Error("a usable elasticity should give a confident recommendation")
	}
	if rec.Best.ExpectedProfitMinor <= rec.Incumbent.ExpectedProfitMinor {
		t.Errorf("the chosen price is not better than the incumbent: %.2f vs %.2f",
			rec.Best.ExpectedProfitMinor, rec.Incumbent.ExpectedProfitMinor)
	}
	t.Logf("closed-form optimum %.2f, search found %d over %d candidates, uplift %.0f minor units",
		want, rec.Best.PriceMinor, rec.Evaluated, rec.UpliftMinor)
}

// TestOptimiserNeverLeavesTheTier1FeasibleSet is the safety property: whatever
// the model says, the recommended price must be one Tier 1 would accept.
func TestOptimiserNeverLeavesTheTier1FeasibleSet(t *testing.T) {
	cases := []struct {
		name string
		c    domain.Constraints
	}{
		{
			name: "a tight competitor band",
			c: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				CompetitorMinor: 260, CompetitorBandBps: 200},
		},
		{
			name: "a max-change bound",
			c:    domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250, MaxChangeBps: 500},
		},
		{
			name: "a charm ending inside a narrow band",
			c: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 180, CeilingMinor: 320, Ending: domain.EndingCharm},
		},
		{
			name: "a nickel lattice",
			c: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				FloorMinor: 150, CeilingMinor: 400, Ending: domain.EndingNickel},
		},
		{
			name: "an exhausted change budget",
			c: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
				MaxChangesPerPeriod: 1, ChangesThisPeriod: 1},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := Optimise(OptimisationInput{
				Constraints: tt.c,
				// An elasticity of -3.5 pushes hard towards the floor, so the
				// optimiser will press against whichever bound is binding.
				Model:      elasticDemand{refPrice: 250, refUnits: 30, elasticity: -3.5},
				Elasticity: usableElasticity(-3.5),
			})
			if err != nil {
				t.Fatalf("optimise: %v", err)
			}
			d := domain.Evaluate(tt.c, rec.Best.PriceMinor)
			if d.Outcome != domain.OutcomeAccepted {
				t.Errorf("recommended %d, which Tier 1 does not accept unchanged: %s %+v",
					rec.Best.PriceMinor, d.Outcome, d.Violations)
			}
			if rec.Decision.Outcome != domain.OutcomeAccepted {
				t.Errorf("the returned decision is %s, want accepted", rec.Decision.Outcome)
			}
		})
	}
}

func TestOptimiserRefusesWhenInfeasible(t *testing.T) {
	c := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 200,
		FloorMinor: 400, CeilingMinor: 300}
	_, err := Optimise(OptimisationInput{
		Constraints: c,
		Model:       elasticDemand{refPrice: 200, refUnits: 30, elasticity: -2},
		Elasticity:  usableElasticity(-2),
	})
	if err == nil {
		t.Fatal("an infeasible rule set produced a recommendation")
	}
	if !isInfeasible(err) {
		t.Errorf("err = %v, want ErrInfeasible", err)
	}
}

func isInfeasible(err error) bool {
	for e := err; e != nil; {
		if e == ErrInfeasible {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// TestOptimiserRefusesWithoutEvidence checks that no model and no usable
// elasticity is a refusal, not a guess.
func TestOptimiserRefusesWithoutEvidence(t *testing.T) {
	_, err := Optimise(OptimisationInput{
		Constraints: domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 200,
			FloorMinor: 150, CeilingMinor: 400},
		Elasticity: ml.Elasticity{Usable: false, Reason: "only 4 observations"},
	})
	if err == nil {
		t.Fatal("the optimiser produced a recommendation with no evidence at all")
	}
}

// TestOptimiserFallsBackToTheElasticityProjection covers the SKU with no
// trained model: the recommendation is still made, but it is marked unconfident
// when the elasticity is weak.
func TestOptimiserFallsBackToTheElasticityProjection(t *testing.T) {
	c := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
		FloorMinor: 150, CeilingMinor: 400}
	rec, err := Optimise(OptimisationInput{
		Constraints:    c,
		Elasticity:     usableElasticity(-2.5),
		ReferenceUnits: 30,
	})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	want := 100 * -2.5 / (1 - 2.5)
	if math.Abs(float64(rec.Best.PriceMinor)-want) > 1.5 {
		t.Errorf("optimum = %d, want %.2f", rec.Best.PriceMinor, want)
	}
	if rec.Best.ProfitLowMinor >= rec.Best.ProfitHighMinor {
		t.Errorf("the profit interval is not a band: [%.2f, %.2f]",
			rec.Best.ProfitLowMinor, rec.Best.ProfitHighMinor)
	}
}

func TestOptimiserObjectives(t *testing.T) {
	c := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
		FloorMinor: 120, CeilingMinor: 500}
	model := elasticDemand{refPrice: 250, refUnits: 30, elasticity: -2.5}
	el := usableElasticity(-2.5)

	profit, err := Optimise(OptimisationInput{Constraints: c, Model: model, Elasticity: el, Objective: ObjectiveProfit})
	if err != nil {
		t.Fatalf("profit: %v", err)
	}
	revenue, err := Optimise(OptimisationInput{Constraints: c, Model: model, Elasticity: el, Objective: ObjectiveRevenue})
	if err != nil {
		t.Fatalf("revenue: %v", err)
	}
	units, err := Optimise(OptimisationInput{Constraints: c, Model: model, Elasticity: el, Objective: ObjectiveUnits})
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	// With elasticity below -1, revenue falls monotonically in price, so the
	// revenue and volume objectives both sit at the floor while profit sits
	// above cost. The ordering is what the objectives *mean*, so asserting it
	// catches an objective wired to the wrong field.
	if revenue.Best.PriceMinor != c.FloorMinor {
		t.Errorf("revenue optimum = %d, want the floor %d", revenue.Best.PriceMinor, c.FloorMinor)
	}
	if units.Best.PriceMinor != c.FloorMinor {
		t.Errorf("volume optimum = %d, want the floor %d", units.Best.PriceMinor, c.FloorMinor)
	}
	if profit.Best.PriceMinor <= c.FloorMinor {
		t.Errorf("profit optimum = %d, should be above the floor %d", profit.Best.PriceMinor, c.FloorMinor)
	}
}

func TestOptimiserCurveCoversEveryCandidate(t *testing.T) {
	c := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
		FloorMinor: 200, CeilingMinor: 210}
	rec, err := Optimise(OptimisationInput{
		Constraints: c,
		Model:       elasticDemand{refPrice: 250, refUnits: 30, elasticity: -2},
		Elasticity:  usableElasticity(-2),
	})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	if len(rec.Curve) != 11 { // 200..210 inclusive
		t.Fatalf("curve has %d points, want 11", len(rec.Curve))
	}
	for i := 1; i < len(rec.Curve); i++ {
		if rec.Curve[i].PriceMinor <= rec.Curve[i-1].PriceMinor {
			t.Errorf("curve is not ascending at %d", i)
		}
		// Demand must fall as price rises for a negative elasticity; a curve
		// that does not is a sign the price feature is not reaching the model.
		if rec.Curve[i].ExpectedUnits >= rec.Curve[i-1].ExpectedUnits {
			t.Errorf("demand did not fall between %d and %d",
				rec.Curve[i-1].PriceMinor, rec.Curve[i].PriceMinor)
		}
	}
}

// negativeDemand returns a negative number, which a real regression can do when
// extrapolated. The optimiser must clamp rather than let it make an absurd
// price look profitable.
type negativeDemand struct{}

func (negativeDemand) Predict([]float64) float64 { return -50 }

func TestOptimiserClampsNegativeDemand(t *testing.T) {
	c := domain.Constraints{Currency: "USD", UnitCost: 100, CurrentMinor: 250,
		FloorMinor: 150, CeilingMinor: 400}
	rec, err := Optimise(OptimisationInput{
		Constraints: c, Model: negativeDemand{}, Elasticity: usableElasticity(-2),
	})
	if err != nil {
		t.Fatalf("optimise: %v", err)
	}
	for _, p := range rec.Curve {
		if p.ExpectedUnits < 0 {
			t.Fatalf("candidate %d carries negative demand %v", p.PriceMinor, p.ExpectedUnits)
		}
	}
}
