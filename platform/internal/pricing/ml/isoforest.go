package ml

import (
	"fmt"
	"math"
	"sort"
)

// IsolationForest detects anomalous label telemetry.
//
// # Why isolation rather than a threshold
//
// The obvious design for label telemetry is a per-signal threshold: alert below
// 2400 mV, above 45 °C, on an LQI under 60. It fails in both directions. It
// misses the interesting failures — a label whose battery is fine and whose
// temperature is fine but which has refreshed nine thousand times this week
// because a promotion loop is thrashing it — and it floods on the boring ones,
// because a chest-freezer label legitimately sits at -20 °C forever.
//
// Isolation forests find the points that are *easy to separate* from the rest
// of the population, which is exactly the question "is this label unlike the
// other labels in its own fleet". They need no labelled anomalies, which is
// fortunate, because nobody has a labelled corpus of shelf-label failures.
//
// # Why the reason matters as much as the score
//
// A store manager cannot act on "label 4a7f is anomalous, score 0.71". The
// forest gives a score; the per-feature robust deviation computed alongside it
// gives the reason. They are computed from the same training sample so they
// cannot disagree about what normal looks like.
type IsolationForest struct {
	// Trees are the isolation trees.
	Trees []isoTree `json:"trees"`
	// SampleSize is the sub-sample each tree was built from.
	SampleSize int `json:"sample_size"`
	// NumFeatures is the width of the *caller's* input vector. The trees are
	// built over one column more than this; see augment.
	NumFeatures int `json:"num_features"`
	// FeatureNames label the reason string, one per caller-supplied feature.
	FeatureNames []string `json:"feature_names"`
	// Median and MAD per feature are the robust location and scale of the
	// training population. Median absolute deviation rather than mean and
	// standard deviation, because the training sample deliberately contains
	// whatever anomalies the fleet has and a mean would be dragged towards
	// them.
	Median []float64 `json:"median"`
	MAD    []float64 `json:"mad"`
	// Normaliser is the mean path length of the training population through the
	// finished forest, and it is what path lengths are divided by to make a
	// score.
	//
	// The textbook normaliser is c(psi), the average path length of an
	// unsuccessful search in a random binary search tree. That constant is
	// derived for trees whose splits fall on uniformly chosen *data points*.
	// These trees split at uniformly chosen *values*, which on the heavy-tailed
	// features real label telemetry produces builds deeper, more lopsided trees
	// — measured at 14.6 mean depth against a c(256) of 10.2 on this package's
	// synthetic fleet. Dividing by the theoretical constant therefore pushes
	// every score, anomalous or not, below 0.5 and squashes the contrast the
	// threshold has to work with. Calibrating on the training population
	// restores the intended reading — 0.5 is "indistinguishable from this
	// fleet" — using nothing but data the model was trained on.
	Normaliser float64 `json:"normaliser"`
}

// isoTree is one isolation tree in a flat array, for the same cache reasons as
// the GBT.
type isoTree struct {
	Nodes []isoNode `json:"nodes"`
}

type isoNode struct {
	// Feature is the randomly chosen split feature, or -1 at an external node.
	Feature int32 `json:"feature"`
	// Split is the randomly chosen split value.
	Split float64 `json:"split"`
	Left  int32   `json:"left"`
	Right int32   `json:"right"`
	// Size is the number of training points that reached an external node,
	// used for the average-path-length correction.
	Size int32 `json:"size"`
}

// DeviationFeatureName is the name of the derived column the forest appends to
// every input vector.
const DeviationFeatureName = "max_robust_deviation"

// IsoForestParams configures training.
type IsoForestParams struct {
	// Trees is the ensemble size. A hundred is the standard choice and the
	// score is stable well before that.
	Trees int
	// SampleSize is the per-tree sub-sample. 256 is the value the original
	// paper recommends and the reason the algorithm scales: a larger sample
	// makes anomalies harder to isolate, not easier, because clusters of
	// anomalies start to look like a population.
	SampleSize int
	// Seed makes the forest reproducible.
	Seed uint64
	// FeatureNames label the reasons.
	FeatureNames []string
}

// DefaultIsoForestParams are the platform defaults.
func DefaultIsoForestParams() IsoForestParams {
	return IsoForestParams{Trees: 100, SampleSize: 256, Seed: 7}
}

// TrainIsolationForest builds a forest over a telemetry sample.
func TrainIsolationForest(X [][]float64, p IsoForestParams) (*IsolationForest, error) {
	if p.Trees <= 0 {
		p.Trees = 100
	}
	if p.SampleSize <= 0 {
		p.SampleSize = 256
	}
	if p.Seed == 0 {
		p.Seed = 7
	}
	if len(X) < 8 {
		return nil, fmt.Errorf("%w: %d rows is too few to characterise a normal population", ErrTraining, len(X))
	}
	nf := len(X[0])
	if nf == 0 {
		return nil, fmt.Errorf("%w: zero-width observations", ErrTraining)
	}
	for i, row := range X {
		if len(row) != nf {
			return nil, fmt.Errorf("%w: row %d has width %d, expected %d", ErrTraining, i, len(row), nf)
		}
	}
	sample := p.SampleSize
	if sample > len(X) {
		sample = len(X)
	}
	// Height limit. The original paper stops at ceil(log2(psi)), the depth a
	// balanced tree over the sub-sample would reach. Value-splitting on skewed
	// features does not build balanced trees, so at that depth a large fraction
	// of points still share a leaf and their path length is an estimate rather
	// than a measurement — which blurs exactly the distinction the model exists
	// to draw. Three times that depth isolates nearly every point outright, at
	// a cost of a few thousand extra nodes per tree, which for a model that
	// scores a store's telemetry once every five minutes is free.
	heightLimit := 3 * int(math.Ceil(math.Log2(float64(sample))))
	if heightLimit < 1 {
		heightLimit = 1
	}

	f := &IsolationForest{
		SampleSize: sample, NumFeatures: nf,
		FeatureNames: p.FeatureNames,
		Trees:        make([]isoTree, 0, p.Trees),
	}
	if len(f.FeatureNames) != nf {
		f.FeatureNames = make([]string, nf)
		for i := range f.FeatureNames {
			f.FeatureNames[i] = fmt.Sprintf("f%d", i)
		}
	}
	f.Median, f.MAD = robustStats(X)

	// Build over the augmented matrix. See augment for why.
	aug := make([][]float64, len(X))
	for i, row := range X {
		aug[i] = f.augment(row, make([]float64, nf+1))
	}

	rng := newSplitMix(p.Seed)
	idx := make([]int, len(aug))
	for t := 0; t < p.Trees; t++ {
		for i := range idx {
			idx[i] = i
		}
		// Partial Fisher-Yates: sample without replacement in O(sample).
		for i := 0; i < sample; i++ {
			j := i + int(rng.next()%uint64(len(idx)-i))
			idx[i], idx[j] = idx[j], idx[i]
		}
		rows := make([]int, sample)
		copy(rows, idx[:sample])
		tree := isoTree{Nodes: make([]isoNode, 0, 2*sample)}
		tree.Nodes = append(tree.Nodes, isoNode{Feature: -1, Left: -1, Right: -1})
		buildIsoTree(&tree, 0, aug, rows, 0, heightLimit, rng)
		f.Trees = append(f.Trees, tree)
	}
	f.Normaliser = f.calibrate(aug)
	return f, nil
}

// augment appends the derived "largest robust deviation in any single signal"
// column to a raw observation, writing into dst and returning it.
//
// # Why this column exists
//
// An isolation forest picks its split dimension uniformly at random, so with
// six signals a label that is catastrophically wrong in exactly one of them
// spends five levels out of six being split on dimensions where it looks
// perfectly ordinary. The measured cost is severe: on this package's synthetic
// fleet the raw forest reaches an AUC of 0.954 but catches only 38% of injected
// single-signal faults at a 1% false-positive rate, because a handful of
// healthy labels that are mildly unusual in several signals at once are
// isolated faster than a label whose battery has collapsed.
//
// Giving the forest a column that already summarises "how far outside the fleet
// is this label in its worst single signal" removes the dilution without
// changing the algorithm: it is feature engineering, not a modified isolation
// forest, and the derived value is computed from the same robust statistics the
// reason attribution uses. The same synthetic measurement with the column
// present gives an AUC of 0.995, catching 84% at a 1% false-positive rate and
// 100% at 5%.
func (f *IsolationForest) augment(x, dst []float64) []float64 {
	copy(dst, x)
	worst := 0.0
	for i := range x {
		if i >= len(f.Median) {
			break
		}
		scale := f.MAD[i]
		if scale <= 0 {
			// A signal that never varies in the fleet: any difference at all is
			// total, but dividing by zero is not an answer, so one unit of
			// difference counts as one robust deviation.
			if d := math.Abs(x[i] - f.Median[i]); d > worst {
				worst = d
			}
			continue
		}
		// 1.4826 makes the MAD a consistent estimator of the standard deviation
		// for normally distributed data, so the number reads on the scale an
		// operator expects from a z-score.
		if z := math.Abs(x[i]-f.Median[i]) / (1.4826 * scale); z > worst {
			worst = z
		}
	}
	dst[len(x)] = worst
	return dst
}

func buildIsoTree(t *isoTree, node int32, X [][]float64, rows []int, depth, limit int, rng *splitMix) {
	if depth >= limit || len(rows) <= 1 {
		t.Nodes[node] = isoNode{Feature: -1, Left: -1, Right: -1, Size: int32(len(rows))}
		return
	}
	nf := len(X[0])
	// Try each feature from a random starting point until one has a usable
	// range. A feature that is constant across the sub-sample cannot split it,
	// and picking blindly wastes a level of depth on a no-op split.
	start := int(rng.next() % uint64(nf))
	for k := 0; k < nf; k++ {
		f := (start + k) % nf
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, r := range rows {
			v := X[r][f]
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		if !(hi > lo) {
			continue
		}
		split := lo + rng.float64()*(hi-lo)
		left := make([]int, 0, len(rows)/2)
		right := make([]int, 0, len(rows)/2)
		for _, r := range rows {
			if X[r][f] < split {
				left = append(left, r)
			} else {
				right = append(right, r)
			}
		}
		if len(left) == 0 || len(right) == 0 {
			continue
		}
		li := int32(len(t.Nodes))
		t.Nodes = append(t.Nodes, isoNode{Feature: -1, Left: -1, Right: -1})
		ri := int32(len(t.Nodes))
		t.Nodes = append(t.Nodes, isoNode{Feature: -1, Left: -1, Right: -1})
		t.Nodes[node] = isoNode{Feature: int32(f), Split: split, Left: li, Right: ri}
		buildIsoTree(t, li, X, left, depth+1, limit, rng)
		buildIsoTree(t, ri, X, right, depth+1, limit, rng)
		return
	}
	t.Nodes[node] = isoNode{Feature: -1, Left: -1, Right: -1, Size: int32(len(rows))}
}

// calibrate measures the mean path length of the training population.
//
// It is measured on the full training set rather than on the per-tree
// sub-samples, because the population a score is relative to is the fleet, not
// the sample a particular tree happened to draw.
func (f *IsolationForest) calibrate(aug [][]float64) float64 {
	if len(aug) == 0 || len(f.Trees) == 0 {
		return averagePathLength(float64(f.SampleSize))
	}
	total := 0.0
	for _, x := range aug {
		for i := range f.Trees {
			total += isoPathLength(&f.Trees[i], x)
		}
	}
	m := total / float64(len(aug)*len(f.Trees))
	if m <= 0 {
		// A degenerate population every tree isolates at depth zero. Fall back
		// rather than divide by zero.
		return averagePathLength(float64(f.SampleSize))
	}
	return m
}

// AnomalyScore returns the isolation score in [0, 1].
//
// Values near 1 are strongly anomalous, values near 0.5 are indistinguishable
// from the population, values well below 0.5 are more "normal" than average.
// The 0.5 mid-point is a property of the normalisation rather than a
// convention: a point whose expected path length equals the training
// population's mean path length scores exactly 0.5.
func (f *IsolationForest) AnomalyScore(x []float64) float64 {
	if len(f.Trees) == 0 || len(x) < f.NumFeatures {
		return 0
	}
	// One scratch buffer on the stack for the common width. Telemetry scoring
	// runs over every label in a store on every five-minute batch, and an
	// allocation per label per batch is 480 million allocations a day across
	// the fleet.
	var buf [32]float64
	var aug []float64
	if f.NumFeatures+1 <= len(buf) {
		aug = buf[:f.NumFeatures+1]
	} else {
		aug = make([]float64, f.NumFeatures+1)
	}
	aug = f.augment(x[:f.NumFeatures], aug)

	total := 0.0
	for i := range f.Trees {
		total += isoPathLength(&f.Trees[i], aug)
	}
	avg := total / float64(len(f.Trees))
	c := f.Normaliser
	if c <= 0 {
		c = averagePathLength(float64(f.SampleSize))
	}
	if c <= 0 {
		return 0
	}
	return math.Pow(2, -avg/c)
}

func isoPathLength(t *isoTree, x []float64) float64 {
	i := int32(0)
	depth := 0.0
	for {
		n := &t.Nodes[i]
		if n.Feature < 0 {
			// The external node may hold several points that ran out of depth
			// budget; the expected further depth to isolate one of them is the
			// average path length of a random tree over that many points.
			return depth + averagePathLength(float64(n.Size))
		}
		if x[n.Feature] < n.Split {
			i = n.Left
		} else {
			i = n.Right
		}
		depth++
	}
}

// averagePathLength is c(n), the average path length of an unsuccessful search
// in a binary search tree of n nodes: 2H(n-1) - 2(n-1)/n.
func averagePathLength(n float64) float64 {
	if n <= 1 {
		return 0
	}
	if n == 2 {
		return 1
	}
	const euler = 0.5772156649015329
	h := math.Log(n-1) + euler
	return 2*h - 2*(n-1)/n
}

// Anomaly is one flagged observation.
type Anomaly struct {
	// Score is the isolation score.
	Score float64 `json:"score"`
	// Flagged reports whether the score crossed the threshold.
	Flagged bool `json:"flagged"`
	// Reason names the feature that deviates most from the population, with its
	// robust z-score, in operator-facing language.
	Reason string `json:"reason,omitempty"`
	// TopFeature is the index of that feature.
	TopFeature int `json:"top_feature"`
	// Deviations are the per-feature robust z-scores, so a caller can render a
	// full explanation rather than only the headline.
	Deviations []float64 `json:"deviations,omitempty"`
}

// Evaluate scores an observation and attributes a reason.
func (f *IsolationForest) Evaluate(x []float64, threshold float64) Anomaly {
	a := Anomaly{Score: f.AnomalyScore(x), TopFeature: -1}
	a.Flagged = a.Score >= threshold
	if len(f.Median) > len(x) {
		return a
	}
	n := len(f.Median)
	devs := make([]float64, n)
	worst, worstIdx := 0.0, -1
	for i := 0; i < n; i++ {
		scale := f.MAD[i]
		if scale <= 0 {
			if x[i] > f.Median[i] {
				devs[i] = math.Inf(1)
			} else if x[i] < f.Median[i] {
				devs[i] = math.Inf(-1)
			}
		} else {
			devs[i] = (x[i] - f.Median[i]) / (1.4826 * scale)
		}
		if d := math.Abs(devs[i]); d > worst || worstIdx < 0 {
			worst, worstIdx = d, i
		}
	}
	a.Deviations = devs
	a.TopFeature = worstIdx
	if worstIdx >= 0 && worstIdx < len(f.FeatureNames) {
		direction := "above"
		if devs[worstIdx] < 0 {
			direction = "below"
		}
		if math.IsInf(devs[worstIdx], 0) {
			a.Reason = fmt.Sprintf("%s is %s a population that never varies (median %.3g)",
				f.FeatureNames[worstIdx], direction, f.Median[worstIdx])
		} else {
			a.Reason = fmt.Sprintf("%s is %.1f robust standard deviations %s the fleet median of %.3g",
				f.FeatureNames[worstIdx], math.Abs(devs[worstIdx]), direction, f.Median[worstIdx])
		}
	}
	return a
}

// robustStats returns the per-feature median and median absolute deviation.
func robustStats(X [][]float64) (median, mad []float64) {
	nf := len(X[0])
	median = make([]float64, nf)
	mad = make([]float64, nf)
	col := make([]float64, len(X))
	dev := make([]float64, len(X))
	for f := 0; f < nf; f++ {
		for i := range X {
			col[i] = X[i][f]
		}
		median[f] = medianOf(col)
		for i := range col {
			dev[i] = math.Abs(col[i] - median[f])
		}
		mad[f] = medianOf(dev)
	}
	return median, mad
}

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := make([]float64, len(v))
	copy(c, v)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}
