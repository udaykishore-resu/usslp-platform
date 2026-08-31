package ml

import (
	"math"
	"strings"
	"testing"
)

// syntheticDemand generates price/quantity pairs from a constant-elasticity
// demand curve with a known coefficient and multiplicative log-normal noise.
//
// Multiplicative rather than additive noise, because that is what the log-log
// model assumes: e is additive in logs. Generating additive noise and then
// fitting a log-log model would be testing the wrong estimator against the
// wrong data, and it would make the recovered coefficient biased in a way that
// says nothing about whether the code is right.
func syntheticDemand(n int, trueElasticity, noiseSD float64, priceLevels []float64, seed uint64) []Observation {
	rng := newSynth(seed)
	const refPrice, refQuantity = 200.0, 50.0
	obs := make([]Observation, 0, n)
	for i := 0; i < n; i++ {
		p := priceLevels[i%len(priceLevels)]
		q := refQuantity * math.Pow(p/refPrice, trueElasticity) * math.Exp(noiseSD*rng.normal())
		obs = append(obs, Observation{PriceMinor: p, Quantity: q})
	}
	return obs
}

// TestElasticityRecoveryOnSyntheticData asserts that the estimator recovers an
// elasticity the test itself chose. All data here is synthetic.
func TestElasticityRecoveryOnSyntheticData(t *testing.T) {
	prices := []float64{170, 180, 190, 200, 210, 220, 230}
	for _, truth := range []float64{-0.6, -1.4, -2.5} {
		obs := syntheticDemand(180, truth, 0.06, prices, 4242)
		e, err := EstimateElasticity(obs, DefaultElasticityPolicy())
		if err != nil {
			t.Fatalf("estimate: %v", err)
		}
		if !e.Usable {
			t.Fatalf("elasticity %.2f: estimate marked unusable: %s", truth, e.Reason)
		}
		if math.Abs(e.Coefficient-truth) > 0.15 {
			t.Errorf("recovered %.4f, want %.2f within 0.15", e.Coefficient, truth)
		}
		if e.Low > truth || e.High < truth {
			t.Errorf("the 95%% interval [%.3f, %.3f] does not contain the true %.2f", e.Low, e.High, truth)
		}
		t.Logf("synthetic truth %.2f -> recovered %.4f, 95%% CI [%.3f, %.3f], se %.4f, R2 %.3f, n=%d",
			truth, e.Coefficient, e.Low, e.High, e.StdErr, e.R2, e.Observations)
	}
}

// TestElasticityRefusesRatherThanGuesses is the property that matters most: too
// little evidence must produce a refusal with a reason, not a number.
func TestElasticityRefusesRatherThanGuesses(t *testing.T) {
	tests := []struct {
		name       string
		obs        []Observation
		wantPhrase string
	}{
		{
			name:       "too few observations",
			obs:        syntheticDemand(5, -1.5, 0.05, []float64{180, 200, 220}, 7),
			wantPhrase: "below the 12 required",
		},
		{
			name:       "a single price point identifies no slope",
			obs:        syntheticDemand(60, -1.5, 0.05, []float64{200}, 7),
			wantPhrase: "no price variation",
		},
		{
			name:       "two price points is below the evidence bar",
			obs:        syntheticDemand(60, -1.5, 0.05, []float64{195, 205}, 7),
			wantPhrase: "distinct price points is below",
		},
		{
			name: "noise so large the interval is uninformative",
			// A 0.9 log-standard-deviation over a narrow price range leaves the
			// slope essentially unidentified.
			obs:        syntheticDemand(20, -1.5, 0.9, []float64{198, 200, 202}, 7),
			wantPhrase: "wide",
		},
		{
			name:       "a positive elasticity is refused as probable endogeneity",
			obs:        syntheticDemand(120, +1.2, 0.05, []float64{170, 190, 210, 230}, 7),
			wantPhrase: "includes zero or positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := EstimateElasticity(tt.obs, DefaultElasticityPolicy())
			if err != nil {
				t.Fatalf("estimate: %v", err)
			}
			if e.Usable {
				t.Fatalf("estimate was marked usable: %+v", e)
			}
			if !strings.Contains(e.Reason, tt.wantPhrase) {
				t.Errorf("reason %q does not mention %q", e.Reason, tt.wantPhrase)
			}
		})
	}
}

func TestElasticityDropsUnloggableObservations(t *testing.T) {
	obs := syntheticDemand(60, -1.5, 0.05, []float64{180, 200, 220}, 11)
	obs = append(obs,
		Observation{PriceMinor: 200, Quantity: 0}, // a sold-out day
		Observation{PriceMinor: 0, Quantity: 10},  // a data error
		Observation{PriceMinor: -5, Quantity: 10}, // another
	)
	e, err := EstimateElasticity(obs, DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if e.Observations != 60 {
		t.Errorf("used %d observations, want 60 (three unloggable rows dropped)", e.Observations)
	}
	if !e.Usable {
		t.Errorf("estimate should still be usable: %s", e.Reason)
	}
}

// TestElasticityIntervalWidensWithNoise checks the standard error responds to
// the data rather than being a decoration.
func TestElasticityIntervalWidensWithNoise(t *testing.T) {
	prices := []float64{170, 190, 210, 230}
	tight, err := EstimateElasticity(syntheticDemand(120, -1.5, 0.03, prices, 3), DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("tight: %v", err)
	}
	loose, err := EstimateElasticity(syntheticDemand(120, -1.5, 0.30, prices, 3), DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("loose: %v", err)
	}
	tw, lw := tight.High-tight.Low, loose.High-loose.Low
	if lw <= tw {
		t.Errorf("noisier data gave a narrower interval: %.4f vs %.4f", lw, tw)
	}
	t.Logf("synthetic: CI width %.4f at noise sd 0.03, %.4f at 0.30", tw, lw)
}

func TestDemandAtFollowsTheFittedCurve(t *testing.T) {
	e := Elasticity{Coefficient: -2}
	// Halving the price of a good with elasticity -2 quadruples demand.
	if got := e.DemandAt(200, 10, 100); math.Abs(got-40) > 1e-9 {
		t.Errorf("DemandAt = %v, want 40", got)
	}
	// Doubling it quarters demand.
	if got := e.DemandAt(200, 10, 400); math.Abs(got-2.5) > 1e-9 {
		t.Errorf("DemandAt = %v, want 2.5", got)
	}
	// Degenerate inputs give zero rather than a NaN that propagates into an
	// optimiser.
	if got := e.DemandAt(0, 10, 100); got != 0 {
		t.Errorf("DemandAt with a zero reference price = %v, want 0", got)
	}
}

func TestBoundsAreOrdered(t *testing.T) {
	e := Elasticity{Coefficient: -1.5, Low: -2.5, High: -0.5}
	for _, p := range []float64{100, 200, 400} {
		lo, hi := e.Bounds(200, 10, p)
		if lo > hi {
			t.Errorf("at price %v the bounds are inverted: %v > %v", p, lo, hi)
		}
		mid := e.DemandAt(200, 10, p)
		if mid < lo-1e-9 || mid > hi+1e-9 {
			t.Errorf("at price %v the point estimate %v is outside [%v, %v]", p, mid, lo, hi)
		}
	}
}

func TestStudentTCriticalTable(t *testing.T) {
	// Spot-check against published values.
	cases := []struct {
		df    int
		level float64
		want  float64
	}{{1, 0.95, 12.706}, {10, 0.95, 2.228}, {30, 0.95, 2.042}, {1000, 0.95, 1.96}, {5, 0.99, 4.032}}
	for _, c := range cases {
		if got := studentTCritical(c.df, c.level); math.Abs(got-c.want) > 1e-3 {
			t.Errorf("t(%d, %v) = %v, want %v", c.df, c.level, got, c.want)
		}
	}
}
