package ml

import (
	"math"
	"testing"
)

// TestQuantisationDeltaIsMeasured trains a model on a synthetic demand-shaped
// surface, quantises it to int8, and reports the accuracy actually lost.
//
// The point of the test is the measurement, not a threshold: the platform
// refuses to promote a quantised model whose delta exceeds a tenant's
// tolerance, and that decision needs a number produced by this code path on
// this data rather than a rule of thumb.
func TestQuantisationDeltaIsMeasured(t *testing.T) {
	X, y := syntheticDemandSurface(2000, 777)
	trainX, trainY, testX, testY, err := Split(X, y, 0.25)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	model, err := TrainGBT(trainX, trainY, GBTParams{Rounds: 200, MaxDepth: 5, LearningRate: 0.06, Seed: 3})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	q, err := QuantiseGBT(model)
	if err != nil {
		t.Fatalf("quantise: %v", err)
	}
	report, err := QuantisationDelta(model, q, testX, testY)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}

	t.Logf("synthetic holdout (%d rows): float MAE %.5f, int8 MAE %.5f, delta %+.5f (%+.2f%%); "+
		"mean disagreement %.5f, max %.5f; %d bytes -> %d bytes (%.1fx)",
		report.Rows, report.FloatMAE, report.Int8MAE, report.MAEDelta, report.MAEDeltaPct,
		report.MeanDisagreement, report.MaxDisagreement,
		report.FloatBytes, report.Int8Bytes, report.CompressionRatio)

	if report.CompressionRatio < 3 {
		t.Errorf("compression ratio %.2f is below the 3x the packed layout should give", report.CompressionRatio)
	}
	// The delta is measured, not asserted tightly, but a quantisation that
	// doubled the error would mean the scales are wrong rather than that int8
	// is lossy.
	if report.MAEDeltaPct > 25 {
		t.Errorf("quantisation cost %.1f%% of the model's accuracy, which points at a scaling bug",
			report.MAEDeltaPct)
	}
	if report.Int8MAE <= 0 || math.IsNaN(report.Int8MAE) {
		t.Errorf("int8 MAE is %v", report.Int8MAE)
	}
}

// syntheticDemandSurface generates rows shaped like the pricing service's
// feature vector: a price, a couple of seasonality terms, inventory and
// velocity, with demand falling in price and rising in promotion intensity.
func syntheticDemandSurface(n int, seed uint64) ([][]float64, []float64) {
	rng := newSynth(seed)
	X := make([][]float64, n)
	y := make([]float64, n)
	for i := range X {
		price := rng.uniform(150, 350)
		hour := math.Floor(rng.uniform(0, 24))
		dow := math.Floor(rng.uniform(0, 7))
		inventory := rng.uniform(0, 200)
		velocity := rng.uniform(1, 30)
		competitor := price * rng.uniform(0.85, 1.15)
		X[i] = []float64{price, hour, dow, inventory, velocity, competitor}
		// Constant-elasticity demand with a weekday effect and a competitor
		// cross term, plus multiplicative noise.
		base := 40 * math.Pow(price/250, -1.8)
		weekday := 1.0
		if dow >= 5 {
			weekday = 1.35
		}
		cross := math.Pow(competitor/price, 0.6)
		y[i] = base*weekday*cross + 2*rng.normal()
	}
	return X, y
}

func TestQuantisedModelRoundTrips(t *testing.T) {
	X, y := syntheticDemandSurface(800, 4321)
	model, err := TrainGBT(X, y, GBTParams{Rounds: 60, MaxDepth: 4, Seed: 9})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	q, err := QuantiseGBT(model)
	if err != nil {
		t.Fatalf("quantise: %v", err)
	}
	b, err := q.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back QuantisedGBT
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The threshold scales are stored at single precision, so predictions agree
	// to float32 resolution rather than exactly.
	for _, x := range X[:100] {
		a, c := q.Predict(x), back.Predict(x)
		if math.Abs(a-c) > 1e-3*math.Max(1, math.Abs(a)) {
			t.Fatalf("prediction changed across a round trip: %v -> %v", a, c)
		}
	}
	t.Logf("serialised int8 model: %d bytes on the wire for %d trees, %d nodes",
		len(b), len(q.Trees), model.NodeCount())
}

func TestQuantisedModelRejectsCorruption(t *testing.T) {
	X, y := syntheticDemandSurface(600, 55)
	model, err := TrainGBT(X, y, GBTParams{Rounds: 20, Seed: 1})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	q, err := QuantiseGBT(model)
	if err != nil {
		t.Fatalf("quantise: %v", err)
	}
	b, err := q.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bad := append([]byte(nil), b...)
	bad[len(bad)/3] ^= 0x11
	var back QuantisedGBT
	if err := back.UnmarshalBinary(bad); err == nil {
		t.Error("a corrupted int8 container decoded without error")
	}
}

func TestGBTSerialisationIsExact(t *testing.T) {
	X, y := syntheticDemandSurface(800, 246)
	model, err := TrainGBT(X, y, GBTParams{Rounds: 50, MaxDepth: 4, Seed: 17})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	b, err := model.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back GBT
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The float container stores float64s, so the round trip must be bit-exact:
	// the cloud and the gateway must agree on a price to the cent.
	for _, x := range X[:200] {
		if a, c := model.Predict(x), back.Predict(x); a != c {
			t.Fatalf("prediction changed across a round trip: %v -> %v", a, c)
		}
	}
	t.Logf("serialised float model: %d bytes for %d trees, %d nodes",
		len(b), len(model.Trees), model.NodeCount())
}

func TestCompareRequiresAMeaningfulMargin(t *testing.T) {
	X, y := syntheticDemandSurface(1200, 606)
	trainX, trainY, testX, testY, err := Split(X, y, 0.3)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	strong, err := TrainGBT(trainX, trainY, GBTParams{Rounds: 200, MaxDepth: 5, Seed: 2})
	if err != nil {
		t.Fatalf("train strong: %v", err)
	}
	weak := constantPredictor(mean(trainY))

	t.Run("a clearly better challenger is promoted", func(t *testing.T) {
		cc, err := Compare(weak, strong, testX, testY)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if !cc.Promote {
			t.Errorf("verdict = %+v, want a promotion", cc)
		}
		t.Logf("synthetic: champion MAE %.4f, challenger %.4f, %.1f%% better -> %s",
			cc.Champion.MAE, cc.Challenger.MAE, cc.MAEImprovementPct, cc.Rationale)
	})

	t.Run("a worse challenger is refused", func(t *testing.T) {
		cc, err := Compare(strong, weak, testX, testY)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if cc.Promote {
			t.Errorf("a worse challenger was recommended for promotion: %+v", cc)
		}
	})

	t.Run("an identical challenger is inside the noise", func(t *testing.T) {
		cc, err := Compare(strong, strong, testX, testY)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if cc.Promote {
			t.Errorf("an identical model was recommended for promotion: %+v", cc)
		}
		if math.Abs(cc.MAEImprovementPct) > 1e-9 {
			t.Errorf("identical models differ by %.6f%%", cc.MAEImprovementPct)
		}
	})

	t.Run("a tiny holdout refuses to decide", func(t *testing.T) {
		cc, err := Compare(weak, strong, testX[:10], testY[:10])
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if cc.Promote {
			t.Error("a 10-row holdout was allowed to justify a promotion")
		}
	})
}

func TestFitEnsembleWeight(t *testing.T) {
	// Two predictors bracketing the truth: the optimal blend is in between, and
	// the grid search must find it rather than collapsing onto one of them.
	y := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100,
		11, 21, 31, 41, 51, 61, 71, 81, 91, 101}
	a := make([]float64, len(y))
	b := make([]float64, len(y))
	for i := range y {
		a[i] = y[i] - 4
		b[i] = y[i] + 4
	}
	w, err := FitEnsembleWeight(a, b, y)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if math.Abs(w.WeightA-0.5) > 1e-9 {
		t.Errorf("weight = %v, want 0.5", w.WeightA)
	}
	if w.Metrics.MAE > 1e-9 {
		t.Errorf("blended MAE = %v, want 0", w.Metrics.MAE)
	}
}

func TestEvaluateExcludesZeroTargetsFromMAPE(t *testing.T) {
	X := [][]float64{{1}, {2}, {3}, {4}}
	y := []float64{0, 10, 20, 0}
	m, err := Evaluate(constantPredictor(10), X, y)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if m.MAPEExcluded != 2 {
		t.Errorf("excluded %d rows from MAPE, want 2", m.MAPEExcluded)
	}
	// The two scored rows are 0% and 50% off, so MAPE is 25%.
	if math.Abs(m.MAPE-25) > 1e-9 {
		t.Errorf("MAPE = %v, want 25", m.MAPE)
	}
	if m.Rows != 4 {
		t.Errorf("rows = %d, want 4", m.Rows)
	}
}
