package ml

import (
	"math"
	"strings"
	"testing"
)

// syntheticFleet generates telemetry-shaped rows for a healthy label
// population: battery voltage, temperature, refreshes per day, LQI.
//
// The distributions are chosen to look like the real thing rather than to be
// easy: battery is tightly clustered and slowly declining, temperature is
// bimodal because chilled aisles exist, refresh rate is right-skewed, and LQI
// has a long left tail because some labels are genuinely far from their
// controller. A detector that only works on spherical Gaussian data would pass
// a simpler test and fail in a store.
func syntheticFleet(n int, seed uint64) [][]float64 {
	rng := newSynth(seed)
	rows := make([][]float64, n)
	for i := range rows {
		battery := 3000 + 60*rng.normal()
		temp := 20 + 2*rng.normal()
		if rng.s.float64() < 0.25 {
			temp = 4 + 1.5*rng.normal() // chilled aisle
		}
		refresh := math.Exp(1.2 + 0.5*rng.normal()) // right-skewed, ~3-10/day
		lqi := 200 - math.Abs(35*rng.normal())
		rows[i] = []float64{battery, temp, refresh, lqi}
	}
	return rows
}

// TestIsolationForestCatchesInjectedAnomalies measures both the detection rate
// on deliberately injected faults and the false-positive rate on the healthy
// population. Both numbers are reported, because a detector's detection rate
// alone is meaningless — flagging everything catches everything.
func TestIsolationForestCatchesInjectedAnomalies(t *testing.T) {
	const trainN, normalN = 2000, 2000
	train := syntheticFleet(trainN, 31337)
	forest, err := TrainIsolationForest(train, IsoForestParams{
		FeatureNames: []string{"battery_mv", "temperature_c", "refreshes_per_day", "lqi"},
	})
	if err != nil {
		t.Fatalf("train: %v", err)
	}

	// The threshold is a quantile of the training population's own scores, as
	// the service sets it. Five per cent is the loose end of the platform's
	// operating range; TestIsolationForestDetectionCurve is where the tight
	// operating points are measured. What this test asserts is that each
	// canonical fault mode is flagged at all and is attributed to the right
	// signal, which is the part an operator acts on.
	scores := make([]float64, len(train))
	for i, r := range train {
		scores[i] = forest.AnomalyScore(r)
	}
	threshold := Quantile(scores, 0.95)

	// Four injected fault modes, each a real shelf-label failure.
	faults := []struct {
		name    string
		row     []float64
		wantTop string
	}{
		{"a cell that has collapsed", []float64{2100, 20, 4, 190}, "battery_mv"},
		{"a label baking behind a heat lamp", []float64{3000, 62, 4, 190}, "temperature_c"},
		{"a promotion loop thrashing the display", []float64{3000, 20, 900, 190}, "refreshes_per_day"},
		{"a label that has fallen behind a shelf", []float64{3000, 20, 4, 5}, "lqi"},
	}
	for _, f := range faults {
		a := forest.Evaluate(f.row, threshold)
		if !a.Flagged {
			t.Errorf("%s: score %.4f did not reach the threshold %.4f", f.name, a.Score, threshold)
		}
		if a.TopFeature < 0 || forest.FeatureNames[a.TopFeature] != f.wantTop {
			t.Errorf("%s: blamed %q, want %q", f.name, forest.FeatureNames[a.TopFeature], f.wantTop)
		}
		if a.Reason == "" || !strings.Contains(a.Reason, f.wantTop) {
			t.Errorf("%s: reason %q does not name the driving feature", f.name, a.Reason)
		}
		t.Logf("%s: score %.4f, reason %q", f.name, a.Score, a.Reason)
	}

	// The measured false-positive rate on a fresh healthy sample from the same
	// distribution. It should sit near the contamination the threshold encodes;
	// a large gap either way means the threshold is not doing what it claims.
	fresh := syntheticFleet(normalN, 987654)
	flagged := 0
	for _, r := range fresh {
		if forest.AnomalyScore(r) >= threshold {
			flagged++
		}
	}
	fpr := float64(flagged) / float64(normalN)
	t.Logf("synthetic fleet: threshold %.4f set at the training population's 95th percentile; "+
		"measured false-positive rate on %d fresh healthy rows is %.2f%% (%d flagged)",
		threshold, normalN, 100*fpr, flagged)
	if fpr > 0.08 {
		t.Errorf("false-positive rate %.2f%% is far above the 5%% the threshold encodes", 100*fpr)
	}
}

// injectedFaults generates labels with exactly one signal driven far outside the
// healthy population, in equal proportions. Every other signal is drawn from the
// healthy distribution, so the only thing that makes these rows anomalous is the
// one broken signal — which is the hard case for an isolation forest and the
// common case in a real store.
func injectedFaults(n int, seed uint64) [][]float64 {
	rng := newSynth(seed)
	out := make([][]float64, n)
	for i := range out {
		row := []float64{
			3000 + 60*rng.normal(),
			20 + 2*rng.normal(),
			math.Exp(1.2 + 0.5*rng.normal()),
			200 - math.Abs(35*rng.normal()),
		}
		switch i % 4 {
		case 0:
			row[0] = 1800 + 200*rng.s.float64() // a collapsed cell
		case 1:
			row[1] = 55 + 15*rng.s.float64() // baking behind a heat lamp
		case 2:
			row[2] = 200 + 800*rng.s.float64() // a refresh loop
		case 3:
			row[3] = 5 + 20*rng.s.float64() // fallen behind a shelf
		}
		out[i] = row
	}
	return out
}

// TestIsolationForestDetectionCurve measures the detector's discrimination on
// synthetic data and reports it, rather than asserting a remembered number.
//
// The AUC is the threshold-free measure — the probability that a randomly
// chosen faulty label scores above a randomly chosen healthy one — and the
// operating points below it are what an operator actually lives with. All of
// this data is synthetic; the numbers characterise the algorithm on a
// distribution this test chose, not a real fleet.
func TestIsolationForestDetectionCurve(t *testing.T) {
	train := syntheticFleet(2000, 31337)
	forest, err := TrainIsolationForest(train, IsoForestParams{
		Trees:        200,
		FeatureNames: []string{"battery_mv", "temperature_c", "refreshes_per_day", "lqi"},
	})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	healthy := syntheticFleet(4000, 987654)
	faults := injectedFaults(400, 5150)

	hs := make([]float64, len(healthy))
	for i, r := range healthy {
		hs[i] = forest.AnomalyScore(r)
	}
	fs := make([]float64, len(faults))
	for i, r := range faults {
		fs[i] = forest.AnomalyScore(r)
	}

	// AUC by pairwise rank comparison. Exact rather than trapezoidal, since the
	// sample is small enough to afford the quadratic count.
	auc := 0.0
	for _, a := range fs {
		for _, b := range hs {
			switch {
			case a > b:
				auc++
			case a == b:
				auc += 0.5
			}
		}
	}
	auc /= float64(len(fs) * len(hs))
	t.Logf("synthetic: AUC %.4f over %d injected faults against %d healthy labels", auc, len(fs), len(hs))
	if auc < 0.95 {
		t.Errorf("AUC %.4f is below the 0.95 bar", auc)
	}

	for _, q := range []float64{0.95, 0.99, 0.995} {
		threshold := Quantile(hs, q)
		detected, falsePositives := 0, 0
		for _, s := range fs {
			if s >= threshold {
				detected++
			}
		}
		for _, s := range hs {
			if s >= threshold {
				falsePositives++
			}
		}
		dr := float64(detected) / float64(len(fs))
		fpr := float64(falsePositives) / float64(len(hs))
		t.Logf("synthetic: threshold %.4f (healthy p%.1f) -> detection %.1f%%, false positives %.2f%%",
			threshold, q*100, 100*dr, 100*fpr)
		if q == 0.99 && dr < 0.75 {
			t.Errorf("detection rate at a 1%% false-positive budget is %.1f%%, below the 75%% bar", 100*dr)
		}
	}
}

// TestIsolationScoreOrdersByAbnormality checks the score is monotone in how far
// outside the population a point sits, which is the property every threshold
// choice rests on.
func TestIsolationScoreOrdersByAbnormality(t *testing.T) {
	train := syntheticFleet(1500, 4242)
	forest, err := TrainIsolationForest(train, IsoForestParams{})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	// The three probes differ only in battery voltage: typical, a couple of
	// standard deviations low, and far outside anything the fleet has ever
	// reported.
	typical := forest.AnomalyScore([]float64{3000, 20, 3.3, 190})
	odd := forest.AnomalyScore([]float64{2870, 20, 3.3, 190})
	extreme := forest.AnomalyScore([]float64{1200, 20, 3.3, 190})
	if !(typical < odd && odd < extreme) {
		t.Errorf("scores are not ordered by abnormality: typical %.4f, odd %.4f, extreme %.4f",
			typical, odd, extreme)
	}
	t.Logf("synthetic: typical %.4f < mildly odd %.4f < extreme %.4f", typical, odd, extreme)
}

func TestIsolationForestRefusesTinySamples(t *testing.T) {
	if _, err := TrainIsolationForest(syntheticFleet(4, 1), IsoForestParams{}); err == nil {
		t.Error("expected a refusal on a four-row population")
	}
}

func TestIsolationForestSerialisationRoundTrip(t *testing.T) {
	train := syntheticFleet(600, 8080)
	forest, err := TrainIsolationForest(train, IsoForestParams{
		Trees: 40, FeatureNames: []string{"a", "b", "c", "d"},
	})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	b, err := forest.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back IsolationForest
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	probe := []float64{2100, 60, 500, 10}
	// Split points are stored at single precision, so scores agree to float32
	// resolution rather than exactly.
	if math.Abs(forest.AnomalyScore(probe)-back.AnomalyScore(probe)) > 1e-4 {
		t.Errorf("score changed across a round trip: %.6f -> %.6f",
			forest.AnomalyScore(probe), back.AnomalyScore(probe))
	}
	if len(back.FeatureNames) != 4 || back.FeatureNames[2] != "c" {
		t.Errorf("feature names did not survive: %v", back.FeatureNames)
	}
}

func TestAveragePathLength(t *testing.T) {
	// c(1) = 0 and c(2) = 1 are the definitional base cases; beyond that the
	// harmonic approximation should be increasing and finite.
	if got := averagePathLength(1); got != 0 {
		t.Errorf("c(1) = %v, want 0", got)
	}
	if got := averagePathLength(2); got != 1 {
		t.Errorf("c(2) = %v, want 1", got)
	}
	prev := 1.0
	for _, n := range []float64{4, 16, 256, 4096} {
		got := averagePathLength(n)
		if got <= prev || math.IsInf(got, 0) || math.IsNaN(got) {
			t.Errorf("c(%v) = %v, not increasing and finite", n, got)
		}
		prev = got
	}
}

func TestRobustStatsUseMedianNotMean(t *testing.T) {
	// A population with one huge outlier: the median must be unmoved.
	X := [][]float64{{1}, {2}, {3}, {4}, {5}, {1000000}}
	med, mad := robustStats(X)
	if math.Abs(med[0]-3.5) > 1e-9 {
		t.Errorf("median = %v, want 3.5", med[0])
	}
	if mad[0] <= 0 || mad[0] > 10 {
		t.Errorf("MAD = %v, which the outlier should not have inflated", mad[0])
	}
}
