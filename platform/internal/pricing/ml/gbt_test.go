package ml

import (
	"math"
	"testing"
)

// synthRNG is a deterministic generator for the test data, so a failure is
// always reproducible. Every number in these tests comes from it: the data is
// synthetic and the tests say so, both here and in the assertions, which check
// recovery of a parameter the test itself chose rather than a remembered
// output.
type synthRNG struct{ s *splitMix }

func newSynth(seed uint64) *synthRNG { return &synthRNG{s: newSplitMix(seed)} }

func (r *synthRNG) uniform(lo, hi float64) float64 { return lo + r.s.float64()*(hi-lo) }

// normal returns a standard normal deviate by the Box-Muller transform.
func (r *synthRNG) normal() float64 {
	u1 := r.s.float64()
	if u1 < 1e-12 {
		u1 = 1e-12
	}
	u2 := r.s.float64()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// TestGBTRecoversAKnownFunction trains on data generated from a function the
// test chooses and asserts the model recovers it on held-out rows.
//
// The target is deliberately non-linear and interacting — a step in x0, a
// product of x1 and x2, and a threshold on x3 — because that is the shape a
// tree ensemble should handle and a linear model should not. The comparison
// against a constant predictor is the honest floor: a model that cannot beat
// "always predict the mean" has learned nothing.
func TestGBTRecoversAKnownFunction(t *testing.T) {
	rng := newSynth(20260830)
	const n = 1200
	truth := func(x []float64) float64 {
		v := 0.0
		if x[0] > 0.5 {
			v += 3
		}
		v += 2 * x[1] * x[2]
		if x[3] < -1 {
			v -= 4
		}
		v += 0.5 * x[4]
		return v
	}
	X := make([][]float64, n)
	y := make([]float64, n)
	clean := make([]float64, n)
	for i := range X {
		row := []float64{
			rng.uniform(0, 1), rng.uniform(0, 2), rng.uniform(0, 2),
			rng.normal(), rng.uniform(-3, 3), rng.uniform(0, 1),
		}
		X[i] = row
		clean[i] = truth(row)
		// Additive noise with a standard deviation of 0.3, so no model can do
		// better than an MAE of about 0.24 (the mean absolute deviation of a
		// normal is sigma*sqrt(2/pi)).
		y[i] = clean[i] + 0.3*rng.normal()
	}

	trainX, trainY, testX, testY := X[:1000], y[:1000], X[1000:], y[1000:]
	model, err := TrainGBT(trainX, trainY, GBTParams{Rounds: 300, MaxDepth: 4, LearningRate: 0.06, Seed: 5})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	m, err := Evaluate(model, testX, testY)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// The irreducible noise floor is 0.3*sqrt(2/pi) = 0.239. Anything under
	// 0.45 means the model has recovered most of the structure; the bound is
	// loose enough not to be flaky and tight enough that a broken split search
	// fails it.
	const wantMAE = 0.45
	if m.MAE > wantMAE {
		t.Errorf("holdout MAE = %.4f, want below %.4f (noise floor is 0.239)", m.MAE, wantMAE)
	}
	if m.R2 < 0.9 {
		t.Errorf("holdout R2 = %.4f, want at least 0.90", m.R2)
	}
	t.Logf("synthetic holdout: MAE %.4f RMSE %.4f R2 %.4f bias %+.4f over %d rows, %d trees, %d nodes",
		m.MAE, m.RMSE, m.R2, m.Bias, m.Rows, len(model.Trees), model.NodeCount())

	// And it must beat the constant model by a wide margin.
	constant := constantPredictor(mean(trainY))
	cm, err := Evaluate(constant, testX, testY)
	if err != nil {
		t.Fatalf("evaluate constant: %v", err)
	}
	if m.MAE >= cm.MAE*0.5 {
		t.Errorf("model MAE %.4f is not much better than the constant model's %.4f", m.MAE, cm.MAE)
	}
}

type constantPredictor float64

func (c constantPredictor) Predict([]float64) float64 { return float64(c) }

// TestGBTIsReproducible pins the property the model registry depends on: the
// same data and the same seed produce the same model, so a metric change can be
// attributed to a data change.
func TestGBTIsReproducible(t *testing.T) {
	X, y := smallSynthetic(300, 11)
	p := GBTParams{Rounds: 40, MaxDepth: 3, Seed: 99, Subsample: 0.8}
	a, err := TrainGBT(X, y, p)
	if err != nil {
		t.Fatalf("train a: %v", err)
	}
	b, err := TrainGBT(X, y, p)
	if err != nil {
		t.Fatalf("train b: %v", err)
	}
	ab, err := a.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bb, err := b.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if string(ab) != string(bb) {
		t.Error("two training runs with the same seed produced different models")
	}
}

func TestGBTRejectsBadInput(t *testing.T) {
	X, y := smallSynthetic(300, 3)
	t.Run("mismatched lengths", func(t *testing.T) {
		if _, err := TrainGBT(X, y[:10], GBTParams{}); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("non-finite feature", func(t *testing.T) {
		bad := make([][]float64, len(X))
		copy(bad, X)
		row := append([]float64(nil), bad[5]...)
		row[0] = math.NaN()
		bad[5] = row
		if _, err := TrainGBT(bad, y, GBTParams{}); err == nil {
			t.Error("expected an error on a NaN feature")
		}
	})
	t.Run("non-finite target", func(t *testing.T) {
		bad := append([]float64(nil), y...)
		bad[7] = math.Inf(1)
		if _, err := TrainGBT(X, bad, GBTParams{}); err == nil {
			t.Error("expected an error on an infinite target")
		}
	})
	t.Run("too few rows", func(t *testing.T) {
		if _, err := TrainGBT(X[:5], y[:5], GBTParams{MinSamplesLeaf: 20}); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestGBTPredictRejectsShortVector(t *testing.T) {
	X, y := smallSynthetic(300, 4)
	m, err := TrainGBT(X, y, GBTParams{Rounds: 10})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Predict accepted a short vector")
		}
	}()
	m.Predict([]float64{1})
}

// smallSynthetic generates a simple linear-plus-interaction dataset.
func smallSynthetic(n int, seed uint64) ([][]float64, []float64) {
	rng := newSynth(seed)
	X := make([][]float64, n)
	y := make([]float64, n)
	for i := range X {
		X[i] = []float64{rng.uniform(0, 1), rng.uniform(0, 1), rng.uniform(0, 1)}
		y[i] = 2*X[i][0] + 3*X[i][1]*X[i][2] + 0.1*rng.normal()
	}
	return X, y
}

func TestSplitIsForwardInTime(t *testing.T) {
	X, y := smallSynthetic(100, 3)
	trainX, trainY, testX, testY, err := Split(X, y, 0.2)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(trainX) != 80 || len(testX) != 20 {
		t.Fatalf("split sizes %d/%d, want 80/20", len(trainX), len(testX))
	}
	// The holdout must be the tail, not a random sample.
	if &testX[0][0] != &X[80][0] {
		t.Error("holdout is not the tail of the input")
	}
	if len(trainY) != 80 || len(testY) != 20 {
		t.Error("target split does not match the feature split")
	}
}

func TestFeatureImportanceIsNormalised(t *testing.T) {
	X, y := smallSynthetic(400, 21)
	m, err := TrainGBT(X, y, GBTParams{Rounds: 60, MaxDepth: 3})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	imp := m.FeatureImportance()
	sum := 0.0
	for _, v := range imp {
		if v < 0 {
			t.Errorf("negative importance %v", v)
		}
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("importances sum to %v, want 1", sum)
	}
	// x0 has a coefficient of 2 on its own; it should carry real weight.
	if imp[0] < 0.15 {
		t.Errorf("importance of the dominant feature is %.3f, suspiciously low: %v", imp[0], imp)
	}
}

func TestQuantileMatchesExactOnAKnownSet(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct{ q, want float64 }{
		{0, 1}, {0.5, 5.5}, {1, 10}, {0.25, 3.25},
	}
	for _, c := range cases {
		if got := Quantile(v, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Quantile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
}
