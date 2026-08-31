package ml

import (
	"math"
	"testing"
)

// TestLSTMGradientsMatchFiniteDifferences is the test that substantiates the
// claim that this package's backpropagation through time is correct.
//
// A hand-written backward pass that is subtly wrong still trains — it descends
// *some* direction — and produces a model that looks plausible and forecasts
// badly. The only way to know the gradients are the gradients of the stated
// loss is to compare them to central finite differences of that loss. Every
// parameter block is checked.
func TestLSTMGradientsMatchFiniteDifferences(t *testing.T) {
	const inputSize, hidden, steps = 3, 4, 6
	n, err := NewLSTM(inputSize, hidden, 12345)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	rng := newSynth(999)
	seq := Sequence{
		Inputs:  make([][]float64, steps),
		Targets: make([]float64, steps),
	}
	for i := 0; i < steps; i++ {
		seq.Inputs[i] = []float64{rng.normal(), rng.normal(), rng.normal()}
		seq.Targets[i] = rng.normal()
	}
	// Non-trivial normalisation, so the check exercises the scaling path too.
	for i := range n.InputMean {
		n.InputMean[i] = 0.1 * float64(i)
		n.InputScale[i] = 1 + 0.2*float64(i)
	}
	n.TargetMean, n.TargetScale = 0.3, 1.7

	// The analytic gradient of the summed loss over the sequence.
	g := newLSTMGrads(n)
	if _, _, err := n.accumulate(seq, 0, g); err != nil {
		t.Fatalf("accumulate: %v", err)
	}

	// lossAt recomputes the same quantity accumulate returns, so the finite
	// difference is of exactly the loss the backward pass claims to
	// differentiate.
	lossAt := func() float64 {
		probe := newLSTMGrads(n)
		l, _, err := n.accumulate(seq, 0, probe)
		if err != nil {
			t.Fatalf("accumulate: %v", err)
		}
		return l
	}

	const eps = 1e-6
	check := func(name string, params, grads []float64) {
		t.Helper()
		worst := 0.0
		for i := range params {
			orig := params[i]
			params[i] = orig + eps
			lp := lossAt()
			params[i] = orig - eps
			lm := lossAt()
			params[i] = orig
			numeric := (lp - lm) / (2 * eps)
			analytic := grads[i]
			denom := math.Max(1, math.Max(math.Abs(numeric), math.Abs(analytic)))
			rel := math.Abs(numeric-analytic) / denom
			if rel > worst {
				worst = rel
			}
			if rel > 1e-5 {
				t.Errorf("%s[%d]: analytic %.10f, numeric %.10f, relative error %.3g",
					name, i, analytic, numeric, rel)
			}
		}
		t.Logf("%s: worst relative gradient error %.3g over %d parameters", name, worst, len(params))
	}

	check("Wx", n.Wx, g.Wx)
	check("Wh", n.Wh, g.Wh)
	check("B", n.B, g.B)
	check("Wy", n.Wy, g.Wy)

	// The scalar read-out bias, checked separately since it is not a slice.
	orig := n.By
	n.By = orig + eps
	lp := lossAt()
	n.By = orig - eps
	lm := lossAt()
	n.By = orig
	numeric := (lp - lm) / (2 * eps)
	if rel := math.Abs(numeric-g.By) / math.Max(1, math.Abs(numeric)); rel > 1e-5 {
		t.Errorf("By: analytic %.10f, numeric %.10f, relative error %.3g", g.By, numeric, rel)
	}
}

// TestLSTMLearnsASeasonalSeries trains the network on a synthetic series with a
// weekly cycle and a trend, and asserts it beats the two baselines that matter:
// predicting the series mean, and predicting yesterday's value.
//
// Beating the mean shows it learned something. Beating the naive lag-1 forecast
// is the harder and more honest bar, because on a smooth series lag-1 is
// already good and a model that cannot beat it is not earning its complexity.
func TestLSTMLearnsASeasonalSeries(t *testing.T) {
	const n = 240
	rng := newSynth(555)
	// units(t) = 40 + 12*sin(2*pi*t/7) + 0.05*t + noise
	series := make([]float64, n)
	for i := range series {
		series[i] = 40 + 12*math.Sin(2*math.Pi*float64(i)/7) + 0.05*float64(i) + 1.5*rng.normal()
	}
	// Per-step features: the day-of-week phase encoded as sine and cosine, plus
	// the previous period's demand. Encoding the phase rather than the raw index
	// is what lets a 6-unit hidden state carry the cycle instead of spending
	// itself counting.
	makeSeq := func(from, to int) Sequence {
		s := Sequence{}
		for i := from; i < to; i++ {
			phase := 2 * math.Pi * float64(i) / 7
			s.Inputs = append(s.Inputs, []float64{math.Sin(phase), math.Cos(phase), series[i-1]})
			s.Targets = append(s.Targets, series[i])
		}
		return s
	}
	train := makeSeq(1, 200)
	test := makeSeq(200, n)

	net, err := NewLSTM(3, 8, 4242)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	losses, err := net.Fit([]Sequence{train}, LSTMTrainParams{Epochs: 400, LearningRate: 0.03, WarmupSteps: 3})
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if losses[len(losses)-1] >= losses[0] {
		t.Errorf("training loss did not fall: %.5f -> %.5f", losses[0], losses[len(losses)-1])
	}

	mse, err := net.Loss(test, 3)
	if err != nil {
		t.Fatalf("loss: %v", err)
	}

	// Baseline 1: the training mean.
	trainMean := mean(train.Targets)
	var meanSSE float64
	// Baseline 2: yesterday's value, which is input feature 2.
	var lagSSE float64
	count := 0
	for i := 3; i < len(test.Targets); i++ {
		d := trainMean - test.Targets[i]
		meanSSE += d * d
		l := test.Inputs[i][2] - test.Targets[i]
		lagSSE += l * l
		count++
	}
	meanMSE, lagMSE := meanSSE/float64(count), lagSSE/float64(count)

	t.Logf("synthetic weekly series: LSTM holdout MSE %.3f, mean-baseline %.3f, lag-1 baseline %.3f "+
		"(irreducible noise variance is 2.25)", mse, meanMSE, lagMSE)
	if mse >= meanMSE {
		t.Errorf("LSTM MSE %.3f does not beat the mean baseline %.3f", mse, meanMSE)
	}
	if mse >= lagMSE {
		t.Errorf("LSTM MSE %.3f does not beat the lag-1 baseline %.3f", mse, lagMSE)
	}
}

func TestLSTMSerialisationRoundTrip(t *testing.T) {
	net, err := NewLSTM(4, 5, 77)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	seq := [][]float64{{1, 2, 3, 4}, {2, 3, 4, 5}, {3, 4, 5, 6}}
	before, err := net.Forward(seq)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	b, err := net.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back LSTM
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	after, err := back.Forward(seq)
	if err != nil {
		t.Fatalf("forward after: %v", err)
	}
	// Weights are stored at single precision, so the round trip is lossy by
	// design; the tolerance is float32's epsilon scaled by the output
	// magnitude, not zero.
	for i := range before {
		if math.Abs(before[i]-after[i]) > 1e-4*math.Max(1, math.Abs(before[i])) {
			t.Errorf("step %d: %v -> %v after a round trip", i, before[i], after[i])
		}
	}
}

func TestLSTMRejectsMalformedContainers(t *testing.T) {
	net, err := NewLSTM(3, 3, 1)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	b, err := net.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Run("corrupted", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[len(bad)/2] ^= 0x5a
		var back LSTM
		if err := back.UnmarshalBinary(bad); err == nil {
			t.Error("corrupted container decoded without error")
		}
	})
	t.Run("wrong kind", func(t *testing.T) {
		var g GBT
		if err := g.UnmarshalBinary(b); err == nil {
			t.Error("an LSTM container decoded as a GBT")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		var back LSTM
		if err := back.UnmarshalBinary(b[:20]); err == nil {
			t.Error("a truncated container decoded without error")
		}
	})
}

func TestSigmoidIsStableAtExtremes(t *testing.T) {
	for _, x := range []float64{-800, -40, 0, 40, 800} {
		v := sigmoid(x)
		if math.IsNaN(v) || v < 0 || v > 1 {
			t.Errorf("sigmoid(%v) = %v", x, v)
		}
	}
	if math.Abs(sigmoid(0)-0.5) > 1e-12 {
		t.Error("sigmoid(0) != 0.5")
	}
}
