// Package ml is USSLP's from-scratch machine learning: gradient-boosted
// regression trees, log-log elasticity estimation, an LSTM, an isolation
// forest, int8 quantisation and a compact model serialisation format.
//
// # Why these are hand-written
//
// The models in this package run on a Store Gateway Unit: a fanless x86 box in
// a stock room with 2 GB of RAM, no Python runtime, no BLAS, and no reliable
// WAN. Tier-2 inference has an 8-15 millisecond budget and must hold when the
// cloud is unreachable, which rules out calling a hosted model. Everything here
// is therefore plain Go over flat float64 slices, with a serialisation format
// the gateway can load in one pass and an int8 quantisation path for the
// smallest gateway tier.
//
// The consequence is that this code owns its own correctness. Every model here
// is tested against synthetic data generated from a known ground truth, so that
// the tests assert recovery of a parameter the test itself chose rather than
// asserting that today's output equals yesterday's.
package ml

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrTraining marks a training run that cannot proceed. Training failures are
// errors rather than degraded models: a silently under-trained pricing model is
// worse than no model, because the platform will act on it.
var ErrTraining = errors.New("ml: training failed")

// GBTParams configures gradient boosting.
type GBTParams struct {
	// Rounds is the number of boosting iterations (one tree each).
	Rounds int
	// MaxDepth bounds tree depth. Retail demand surfaces are smooth and
	// low-order; depth beyond about six buys variance, not signal.
	MaxDepth int
	// MinSamplesLeaf refuses splits that would leave a leaf estimated from too
	// few rows. This is the main guard against a tree memorising a single
	// unusual trading day.
	MinSamplesLeaf int
	// LearningRate shrinks each tree's contribution.
	LearningRate float64
	// L2 is the ridge term in the leaf-value denominator. It shrinks leaves
	// estimated from few rows towards zero, which is a cheaper and more stable
	// regulariser than post-hoc pruning.
	L2 float64
	// Bins is the number of histogram bins per feature. Histogram splitting is
	// what makes training tractable in pure Go: an exact split search is
	// O(rows x features x log rows) per node, while binning makes it O(rows x
	// features) once plus O(bins x features) per node.
	Bins int
	// Subsample is the per-round row sample fraction in (0, 1]. Values below 1
	// add the stochastic in stochastic gradient boosting.
	Subsample float64
	// Seed makes subsampling reproducible. Two training runs with the same seed
	// and data must produce byte-identical models, or the model registry cannot
	// attribute a metric change to a data change.
	Seed uint64
}

// DefaultGBTParams are the platform's starting point for a per-store demand
// model, sized against a store's data volume: a year of daily observations for
// one SKU is a few hundred rows, so the model is deliberately small.
func DefaultGBTParams() GBTParams {
	return GBTParams{
		Rounds: 120, MaxDepth: 5, MinSamplesLeaf: 8,
		LearningRate: 0.08, L2: 1.0, Bins: 64, Subsample: 0.9, Seed: 1,
	}
}

func (p *GBTParams) applyDefaults() {
	d := DefaultGBTParams()
	if p.Rounds <= 0 {
		p.Rounds = d.Rounds
	}
	if p.MaxDepth <= 0 {
		p.MaxDepth = d.MaxDepth
	}
	if p.MinSamplesLeaf <= 0 {
		p.MinSamplesLeaf = d.MinSamplesLeaf
	}
	if p.LearningRate <= 0 {
		p.LearningRate = d.LearningRate
	}
	if p.L2 < 0 {
		p.L2 = d.L2
	}
	if p.Bins <= 1 {
		p.Bins = d.Bins
	}
	if p.Bins > 255 {
		// Bin indices are stored as uint8 so the binned matrix for a year of
		// store data fits in a few megabytes.
		p.Bins = 255
	}
	if p.Subsample <= 0 || p.Subsample > 1 {
		p.Subsample = d.Subsample
	}
	if p.Seed == 0 {
		p.Seed = d.Seed
	}
}

// TreeNode is one node of a regression tree, in a flat array.
//
// The flat layout is not premature optimisation: inference walks one node per
// level, and a pointer-chasing tree of 120 x 31 nodes scattered across the heap
// costs a cache miss per level on a gateway with a small L2. A contiguous array
// keeps a whole model in cache.
type TreeNode struct {
	// Feature is the split feature, or -1 for a leaf.
	Feature int32 `json:"feature"`
	// Threshold sends x[Feature] <= Threshold left.
	Threshold float64 `json:"threshold"`
	// Left and Right are node indices; -1 when absent.
	Left  int32 `json:"left"`
	Right int32 `json:"right"`
	// Value is the leaf prediction contribution.
	Value float64 `json:"value"`
}

// Tree is a regression tree.
type Tree struct {
	Nodes []TreeNode `json:"nodes"`
}

// Predict walks the tree.
func (t *Tree) Predict(x []float64) float64 {
	i := int32(0)
	for {
		n := &t.Nodes[i]
		if n.Feature < 0 {
			return n.Value
		}
		if x[n.Feature] <= n.Threshold {
			i = n.Left
		} else {
			i = n.Right
		}
	}
}

// GBT is a trained gradient-boosted regression ensemble.
type GBT struct {
	// Base is the constant initial prediction (the training mean under squared
	// loss).
	Base float64 `json:"base"`
	// LearningRate is folded into the leaf values at training time, so
	// inference is a plain sum. It is retained for metadata only.
	LearningRate float64 `json:"learning_rate"`
	// NumFeatures is the expected input width, checked at inference because a
	// width mismatch is otherwise a silent wrong answer.
	NumFeatures int `json:"num_features"`
	// Trees are applied in order.
	Trees []Tree `json:"trees"`
}

// Predict returns the ensemble's estimate.
func (m *GBT) Predict(x []float64) float64 {
	if len(x) < m.NumFeatures {
		// Refusing loudly beats returning a number computed from whatever
		// happened to be adjacent in memory.
		panic(fmt.Sprintf("ml: GBT expects %d features, got %d", m.NumFeatures, len(x)))
	}
	sum := m.Base
	for i := range m.Trees {
		sum += m.Trees[i].Predict(x)
	}
	return sum
}

// PredictBatch fills out with one prediction per row of X.
func (m *GBT) PredictBatch(X [][]float64, out []float64) {
	for i, x := range X {
		out[i] = m.Predict(x)
	}
}

// NodeCount is the total node count, the model's size metric.
func (m *GBT) NodeCount() int {
	n := 0
	for i := range m.Trees {
		n += len(m.Trees[i].Nodes)
	}
	return n
}

// FeatureImportance returns the total squared-error reduction attributed to
// each feature, normalised to sum to one.
//
// Gain-based importance rather than split-count: a feature used once at the
// root of every tree matters more than one used six times in the leaves, and
// split counts say the opposite.
func (m *GBT) FeatureImportance() []float64 {
	imp := make([]float64, m.NumFeatures)
	for i := range m.Trees {
		for _, n := range m.Trees[i].Nodes {
			if n.Feature >= 0 && int(n.Feature) < len(imp) {
				// Gain was folded into Value at build time via gainOf; the
				// stored per-node gain lives in the split node's Value field,
				// which is unused for internal nodes.
				imp[n.Feature] += n.Value
			}
		}
	}
	total := 0.0
	for _, v := range imp {
		total += v
	}
	if total <= 0 {
		return imp
	}
	for i := range imp {
		imp[i] /= total
	}
	return imp
}

// TrainGBT fits a gradient-boosted regression ensemble under squared loss.
//
// Squared loss (rather than Poisson, the textbook choice for count data) is
// deliberate: the target is units sold per period, which at a store-SKU level
// is often fractional after aggregation, and the optimiser downstream needs a
// conditional mean. Poisson would give a better-calibrated model for raw
// integer counts, and the harness measures MAE either way, so the choice is
// recorded here rather than hidden.
func TrainGBT(X [][]float64, y []float64, p GBTParams) (*GBT, error) {
	p.applyDefaults()
	if len(X) == 0 || len(X) != len(y) {
		return nil, fmt.Errorf("%w: %d rows of features against %d targets", ErrTraining, len(X), len(y))
	}
	nf := len(X[0])
	if nf == 0 {
		return nil, fmt.Errorf("%w: zero-width feature vectors", ErrTraining)
	}
	for i, row := range X {
		if len(row) != nf {
			return nil, fmt.Errorf("%w: row %d has width %d, expected %d", ErrTraining, i, len(row), nf)
		}
		for j, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("%w: row %d feature %d is not finite", ErrTraining, i, j)
			}
		}
	}
	for i, v := range y {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%w: target %d is not finite", ErrTraining, i)
		}
	}
	if len(X) < 2*p.MinSamplesLeaf {
		return nil, fmt.Errorf("%w: %d rows cannot support a minimum leaf of %d", ErrTraining, len(X), p.MinSamplesLeaf)
	}

	binner := newBinner(X, p.Bins)
	binned := binner.transform(X)

	base := mean(y)
	m := &GBT{Base: base, LearningRate: p.LearningRate, NumFeatures: nf, Trees: make([]Tree, 0, p.Rounds)}

	pred := make([]float64, len(y))
	for i := range pred {
		pred[i] = base
	}
	grad := make([]float64, len(y))
	rng := newSplitMix(p.Seed)
	rows := make([]int32, 0, len(y))
	builder := newTreeBuilder(binner, binned, p)

	for round := 0; round < p.Rounds; round++ {
		// Under squared loss the negative gradient is the residual, and the
		// Newton step denominator is the row count.
		for i := range y {
			grad[i] = y[i] - pred[i]
		}
		rows = rows[:0]
		if p.Subsample >= 1 {
			for i := range y {
				rows = append(rows, int32(i))
			}
		} else {
			threshold := uint64(p.Subsample * float64(math.MaxUint64))
			for i := range y {
				if rng.next() < threshold {
					rows = append(rows, int32(i))
				}
			}
			if len(rows) < 2*p.MinSamplesLeaf {
				// The sample was too thin this round; fall back to the full set
				// rather than emit a tree fitted to a handful of rows.
				rows = rows[:0]
				for i := range y {
					rows = append(rows, int32(i))
				}
			}
		}

		tree := builder.build(rows, grad, p.LearningRate)
		if len(tree.Nodes) == 1 && tree.Nodes[0].Value == 0 {
			// No split improved the loss and the leaf contributes nothing:
			// further rounds cannot help either, so stop rather than pad the
			// model with no-op trees.
			break
		}
		for i := range pred {
			pred[i] += tree.Predict(X[i])
		}
		m.Trees = append(m.Trees, tree)
	}
	if len(m.Trees) == 0 {
		return nil, fmt.Errorf("%w: no tree improved on the constant model", ErrTraining)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Histogram binning
// ---------------------------------------------------------------------------

// binner maps raw feature values onto bin indices and remembers the value at
// each bin's upper edge, so that a split found in bin space can be expressed as
// a threshold on the original scale. The model that ships to the edge therefore
// contains no binning table: inference is plain float comparisons.
type binner struct {
	// edges[f] is the ascending upper edge of each bin for feature f.
	edges [][]float64
}

func newBinner(X [][]float64, bins int) *binner {
	nf := len(X[0])
	b := &binner{edges: make([][]float64, nf)}
	col := make([]float64, len(X))
	for f := 0; f < nf; f++ {
		for i := range X {
			col[i] = X[i][f]
		}
		b.edges[f] = quantileEdges(col, bins)
	}
	return b
}

// quantileEdges chooses bin boundaries at equal-count quantiles of the observed
// values, de-duplicated.
//
// Quantile bins rather than equal-width: retail features are heavily skewed
// (inventory, velocity, days of supply all have long right tails) and
// equal-width bins put 95% of the rows in the first bin, which throws away
// almost all of the split candidates that matter.
func quantileEdges(values []float64, bins int) []float64 {
	v := make([]float64, len(values))
	copy(v, values)
	sort.Float64s(v)
	uniq := v[:0]
	for i, x := range v {
		if i == 0 || x != v[i-1] {
			uniq = append(uniq, x)
		}
	}
	if len(uniq) <= 1 {
		// A constant feature: one bin, no split candidates.
		return []float64{math.Inf(1)}
	}
	if len(uniq) <= bins {
		edges := make([]float64, len(uniq))
		copy(edges, uniq)
		edges[len(edges)-1] = math.Inf(1)
		return edges
	}
	edges := make([]float64, 0, bins)
	for i := 1; i <= bins; i++ {
		idx := i*len(uniq)/bins - 1
		if idx < 0 {
			idx = 0
		}
		e := uniq[idx]
		if len(edges) == 0 || e > edges[len(edges)-1] {
			edges = append(edges, e)
		}
	}
	edges[len(edges)-1] = math.Inf(1)
	return edges
}

func (b *binner) transform(X [][]float64) [][]uint8 {
	out := make([][]uint8, len(b.edges))
	for f := range b.edges {
		col := make([]uint8, len(X))
		for i := range X {
			col[i] = uint8(binOf(b.edges[f], X[i][f]))
		}
		out[f] = col
	}
	return out
}

func binOf(edges []float64, v float64) int {
	// Binary search for the first edge >= v. Edges are ascending and the last
	// is +Inf, so this always terminates inside the slice.
	lo, hi := 0, len(edges)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if v <= edges[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// ---------------------------------------------------------------------------
// Tree construction
// ---------------------------------------------------------------------------

// treeBuilder holds the scratch space reused across rounds. Boosting builds
// hundreds of trees; allocating histograms per node per round dominates the
// training time otherwise.
type treeBuilder struct {
	bn     *binner
	binned [][]uint8
	p      GBTParams
	// gradSum and count are per-feature-per-bin accumulators, laid out
	// feature-major so one feature's histogram is contiguous.
	gradSum []float64
	count   []int32
	// scratch holds the row partition during recursion.
	left  []int32
	right []int32
}

func newTreeBuilder(bn *binner, binned [][]uint8, p GBTParams) *treeBuilder {
	nf := len(binned)
	return &treeBuilder{
		bn: bn, binned: binned, p: p,
		gradSum: make([]float64, nf*p.Bins),
		count:   make([]int32, nf*p.Bins),
		left:    make([]int32, 0, len(binned[0])),
		right:   make([]int32, 0, len(binned[0])),
	}
}

// build grows one regression tree on the supplied rows and gradients.
func (b *treeBuilder) build(rows []int32, grad []float64, shrinkage float64) Tree {
	t := Tree{Nodes: make([]TreeNode, 0, 1<<uint(b.p.MaxDepth+1))}
	t.Nodes = append(t.Nodes, TreeNode{Feature: -1, Left: -1, Right: -1})
	b.grow(&t, 0, rows, grad, 0, shrinkage)
	return t
}

// split describes a candidate split in bin space.
type split struct {
	feature  int
	bin      int
	gain     float64
	leftSum  float64
	leftCnt  int32
	rightSum float64
	rightCnt int32
}

func (b *treeBuilder) grow(t *Tree, node int32, rows []int32, grad []float64, depth int, shrinkage float64) {
	sum := 0.0
	for _, r := range rows {
		sum += grad[r]
	}
	leafValue := shrinkage * sum / (float64(len(rows)) + b.p.L2)

	if depth >= b.p.MaxDepth || len(rows) < 2*b.p.MinSamplesLeaf {
		t.Nodes[node] = TreeNode{Feature: -1, Left: -1, Right: -1, Value: leafValue}
		return
	}

	s, ok := b.bestSplit(rows, grad, sum)
	if !ok {
		t.Nodes[node] = TreeNode{Feature: -1, Left: -1, Right: -1, Value: leafValue}
		return
	}

	// Partition. The two child slices are allocated per node because the
	// recursion holds both alive simultaneously; the builder's scratch buffers
	// serve the histogram pass, which is the part that would otherwise allocate
	// once per node per round.
	left := make([]int32, 0, s.leftCnt)
	right := make([]int32, 0, s.rightCnt)
	col := b.binned[s.feature]
	for _, r := range rows {
		if int(col[r]) <= s.bin {
			left = append(left, r)
		} else {
			right = append(right, r)
		}
	}

	threshold := b.bn.edges[s.feature][s.bin]
	li := int32(len(t.Nodes))
	t.Nodes = append(t.Nodes, TreeNode{Feature: -1, Left: -1, Right: -1})
	ri := int32(len(t.Nodes))
	t.Nodes = append(t.Nodes, TreeNode{Feature: -1, Left: -1, Right: -1})
	// An internal node's Value field is unused by inference (Predict returns
	// only at leaves), so it carries the split gain for feature-importance
	// reporting. This is documented here because it is the one place in the
	// package where a field means two things.
	t.Nodes[node] = TreeNode{Feature: int32(s.feature), Threshold: threshold, Left: li, Right: ri, Value: s.gain}

	b.grow(t, li, left, grad, depth+1, shrinkage)
	b.grow(t, ri, right, grad, depth+1, shrinkage)
}

// bestSplit scans every feature's gradient histogram for the split with the
// greatest squared-error reduction.
func (b *treeBuilder) bestSplit(rows []int32, grad []float64, parentSum float64) (split, bool) {
	nf := len(b.binned)
	for i := range b.gradSum {
		b.gradSum[i] = 0
		b.count[i] = 0
	}
	for f := 0; f < nf; f++ {
		col := b.binned[f]
		off := f * b.p.Bins
		for _, r := range rows {
			bi := off + int(col[r])
			b.gradSum[bi] += grad[r]
			b.count[bi]++
		}
	}

	parentCnt := int32(len(rows))
	// The parent's contribution to the objective. Under squared loss with an L2
	// leaf penalty, a leaf's optimal objective value is -G^2 / (n + lambda),
	// so the gain of a split is the drop in that quantity.
	parentObj := parentSum * parentSum / (float64(parentCnt) + b.p.L2)

	best := split{gain: 0}
	found := false
	for f := 0; f < nf; f++ {
		off := f * b.p.Bins
		var accSum float64
		var accCnt int32
		// The last bin is never a valid split point: everything would go left.
		for bin := 0; bin < b.p.Bins-1; bin++ {
			accSum += b.gradSum[off+bin]
			accCnt += b.count[off+bin]
			if accCnt < int32(b.p.MinSamplesLeaf) {
				continue
			}
			rightCnt := parentCnt - accCnt
			if rightCnt < int32(b.p.MinSamplesLeaf) {
				break
			}
			rightSum := parentSum - accSum
			gain := accSum*accSum/(float64(accCnt)+b.p.L2) +
				rightSum*rightSum/(float64(rightCnt)+b.p.L2) - parentObj
			if gain > best.gain {
				best = split{
					feature: f, bin: bin, gain: gain,
					leftSum: accSum, leftCnt: accCnt,
					rightSum: rightSum, rightCnt: rightCnt,
				}
				found = true
			}
		}
	}
	return best, found
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// splitMix is a deterministic 64-bit generator.
//
// math/rand would do, but a self-contained generator makes a trained model
// reproducible across Go releases: the standard library's global source and its
// algorithms have changed between versions, and a model registry that cannot
// reproduce a champion bit-for-bit cannot prove why a challenger beat it.
type splitMix struct{ state uint64 }

func newSplitMix(seed uint64) *splitMix { return &splitMix{state: seed} }

func (s *splitMix) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// float64 returns a value in [0, 1).
func (s *splitMix) float64() float64 {
	return float64(s.next()>>11) / float64(1<<53)
}
