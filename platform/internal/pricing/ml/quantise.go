package ml

import (
	"fmt"
	"math"
)

// QuantisedGBT is a gradient-boosted ensemble with int8 leaf values and
// per-feature int8 split thresholds, for the smallest Store Gateway Unit tier.
//
// # What is actually saved
//
// A float64 TreeNode is 32 bytes. The quantised form is 6 bytes per node — one
// int8 threshold, one int8 leaf value, one int16 feature, one int16 child
// index. On a per-store model of 120 trees at depth 5 that is roughly 110 kB
// against 590 kB, and more importantly the whole model fits in the gateway's L2
// cache, which is where the inference-latency win comes from. The arithmetic
// win is secondary: the accumulation is int32, so a 15-feature, 120-tree
// prediction is 120 integer comparisons and 120 integer adds.
//
// # What is lost
//
// Precision, in two places. Thresholds are snapped to a 256-level grid per
// feature, which can send a row down the wrong branch when its value sits
// within half a grid step of a split. Leaf values are snapped to a 256-level
// grid over the ensemble's leaf range, which adds a bounded per-tree error.
// Neither is estimated theoretically here: QuantisationDelta measures both, on
// held-out data, and the model registry refuses to promote a quantised model
// whose measured delta exceeds the tenant's tolerance.
type QuantisedGBT struct {
	// Base is the ensemble's constant term, kept in float64 because it is one
	// number and quantising it buys nothing.
	Base float64 `json:"base"`
	// NumFeatures is the expected input width.
	NumFeatures int `json:"num_features"`
	// ThresholdZero and ThresholdScale map feature f's raw value to the int8
	// grid: q = round((v - zero[f]) / scale[f]).
	ThresholdZero  []float64 `json:"threshold_zero"`
	ThresholdScale []float64 `json:"threshold_scale"`
	// LeafScale maps an int8 leaf code back to a contribution:
	// value = code * LeafScale.
	LeafScale float64 `json:"leaf_scale"`
	// Trees are the quantised trees.
	Trees []QuantisedTree `json:"trees"`
}

// QuantisedTree is one tree in the packed representation.
type QuantisedTree struct {
	// Feature is the split feature, or -1 at a leaf.
	Feature []int16 `json:"feature"`
	// Threshold is the quantised split point.
	Threshold []int8 `json:"threshold"`
	// Left and Right are child indices.
	Left  []int16 `json:"left"`
	Right []int16 `json:"right"`
	// Leaf is the quantised leaf contribution.
	Leaf []int8 `json:"leaf"`
}

// QuantiseGBT converts a trained ensemble to int8.
//
// The threshold grid is derived from the *thresholds the trees actually chose*,
// not from the feature's observed range. That matters: a feature whose raw
// range is 0-50,000 (inventory units) may only ever be split between 0 and 40,
// and quantising against the raw range would collapse every one of those splits
// onto the same grid point and destroy the model.
func QuantiseGBT(m *GBT) (*QuantisedGBT, error) {
	if m == nil || len(m.Trees) == 0 {
		return nil, fmt.Errorf("%w: nothing to quantise", ErrTraining)
	}
	nf := m.NumFeatures
	lo := make([]float64, nf)
	hi := make([]float64, nf)
	seen := make([]bool, nf)
	leafAbs := 0.0
	for i := range m.Trees {
		for _, n := range m.Trees[i].Nodes {
			if n.Feature < 0 {
				if a := math.Abs(n.Value); a > leafAbs {
					leafAbs = a
				}
				continue
			}
			f := int(n.Feature)
			if !seen[f] {
				lo[f], hi[f], seen[f] = n.Threshold, n.Threshold, true
				continue
			}
			if n.Threshold < lo[f] {
				lo[f] = n.Threshold
			}
			if n.Threshold > hi[f] {
				hi[f] = n.Threshold
			}
		}
	}

	q := &QuantisedGBT{
		Base: m.Base, NumFeatures: nf,
		ThresholdZero:  make([]float64, nf),
		ThresholdScale: make([]float64, nf),
		Trees:          make([]QuantisedTree, len(m.Trees)),
	}
	// int8 spans -127..127; 127 levels either side of zero. Using 127 rather
	// than 128 keeps the mapping symmetric so that -128 never appears and
	// negation is safe.
	const levels = 127.0
	for f := 0; f < nf; f++ {
		if !seen[f] || hi[f] == lo[f] {
			// Unused or single-valued feature: any scale works, but it must be
			// non-zero so the inverse mapping is defined.
			q.ThresholdZero[f] = lo[f]
			q.ThresholdScale[f] = 1
			continue
		}
		mid := (hi[f] + lo[f]) / 2
		q.ThresholdZero[f] = mid
		q.ThresholdScale[f] = (hi[f] - lo[f]) / (2 * levels)
	}
	if leafAbs == 0 {
		leafAbs = 1
	}
	q.LeafScale = leafAbs / levels

	for i := range m.Trees {
		src := m.Trees[i].Nodes
		if len(src) > math.MaxInt16 {
			return nil, fmt.Errorf("%w: tree %d has %d nodes, above the int16 index limit", ErrTraining, i, len(src))
		}
		t := QuantisedTree{
			Feature:   make([]int16, len(src)),
			Threshold: make([]int8, len(src)),
			Left:      make([]int16, len(src)),
			Right:     make([]int16, len(src)),
			Leaf:      make([]int8, len(src)),
		}
		for j, n := range src {
			if n.Feature < 0 {
				t.Feature[j] = -1
				t.Left[j], t.Right[j] = -1, -1
				t.Leaf[j] = clampInt8(math.Round(n.Value / q.LeafScale))
				continue
			}
			f := int(n.Feature)
			t.Feature[j] = int16(f)
			t.Left[j], t.Right[j] = int16(n.Left), int16(n.Right)
			t.Threshold[j] = clampInt8(math.Round((n.Threshold - q.ThresholdZero[f]) / q.ThresholdScale[f]))
		}
		q.Trees[i] = t
	}
	return q, nil
}

func clampInt8(v float64) int8 {
	switch {
	case v > 127:
		return 127
	case v < -127:
		return -127
	case math.IsNaN(v):
		return 0
	}
	return int8(v)
}

// Predict runs the quantised ensemble.
//
// The input is quantised per feature on the way in, so the comparison at each
// node is int8 against int8 — the same comparison the training-time threshold
// encodes — and the leaf accumulation is int32. The single float multiply at
// the end converts the accumulated code back to the target's units.
func (q *QuantisedGBT) Predict(x []float64) float64 {
	if len(x) < q.NumFeatures {
		panic(fmt.Sprintf("ml: quantised GBT expects %d features, got %d", q.NumFeatures, len(x)))
	}
	// Quantise the row once, not once per tree.
	var buf [64]int8
	var qx []int8
	if q.NumFeatures <= len(buf) {
		qx = buf[:q.NumFeatures]
	} else {
		qx = make([]int8, q.NumFeatures)
	}
	for f := 0; f < q.NumFeatures; f++ {
		qx[f] = clampInt8(math.Round((x[f] - q.ThresholdZero[f]) / q.ThresholdScale[f]))
	}
	acc := int32(0)
	for i := range q.Trees {
		t := &q.Trees[i]
		n := int16(0)
		for t.Feature[n] >= 0 {
			if qx[t.Feature[n]] <= t.Threshold[n] {
				n = t.Left[n]
			} else {
				n = t.Right[n]
			}
		}
		acc += int32(t.Leaf[n])
	}
	return q.Base + float64(acc)*q.LeafScale
}

// SizeBytes is the in-memory footprint of the packed model, the number that
// decides whether it fits the gateway tier it is bound for.
func (q *QuantisedGBT) SizeBytes() int {
	total := 8 + 8 + 16*q.NumFeatures
	for i := range q.Trees {
		// int16 feature + int8 threshold + int16 left + int16 right + int8 leaf.
		total += len(q.Trees[i].Feature) * 8
	}
	return total
}

// QuantisationReport is the measured cost of quantising a model.
//
// Every field is measured on held-out data supplied by the caller. Nothing here
// is an estimate or a rule of thumb: a quantisation delta that is not measured
// on the tenant's own data is not evidence about the tenant's own data.
type QuantisationReport struct {
	// FloatMAE and Int8MAE are the mean absolute errors of the two models
	// against the true target.
	FloatMAE float64 `json:"float_mae"`
	Int8MAE  float64 `json:"int8_mae"`
	// MAEDelta is Int8MAE - FloatMAE: positive means quantisation cost
	// accuracy, which it usually does.
	MAEDelta float64 `json:"mae_delta"`
	// MAEDeltaPct is that delta as a percentage of the float model's MAE.
	MAEDeltaPct float64 `json:"mae_delta_pct"`
	// MaxDisagreement is the largest absolute difference between the two
	// models' predictions on the same row, which bounds how far a quantised
	// decision can diverge from the float one.
	MaxDisagreement float64 `json:"max_disagreement"`
	// MeanDisagreement is the average of that difference.
	MeanDisagreement float64 `json:"mean_disagreement"`
	// FloatBytes and Int8Bytes are the two models' footprints.
	FloatBytes int `json:"float_bytes"`
	Int8Bytes  int `json:"int8_bytes"`
	// CompressionRatio is FloatBytes / Int8Bytes.
	CompressionRatio float64 `json:"compression_ratio"`
	// Rows is the held-out row count the measurement rests on.
	Rows int `json:"rows"`
}

// QuantisationDelta measures the accuracy cost of quantisation on held-out data.
func QuantisationDelta(m *GBT, q *QuantisedGBT, X [][]float64, y []float64) (QuantisationReport, error) {
	if len(X) == 0 || len(X) != len(y) {
		return QuantisationReport{}, fmt.Errorf("%w: %d rows against %d targets", ErrTraining, len(X), len(y))
	}
	var fSum, qSum, dSum, dMax float64
	for i, x := range X {
		fp := m.Predict(x)
		qp := q.Predict(x)
		fSum += math.Abs(fp - y[i])
		qSum += math.Abs(qp - y[i])
		d := math.Abs(fp - qp)
		dSum += d
		if d > dMax {
			dMax = d
		}
	}
	n := float64(len(X))
	r := QuantisationReport{
		FloatMAE: fSum / n, Int8MAE: qSum / n,
		MaxDisagreement: dMax, MeanDisagreement: dSum / n,
		// A float64 TreeNode is int32 + float64 + int32 + int32 + float64 =
		// 32 bytes after alignment.
		FloatBytes: m.NodeCount() * 32,
		Int8Bytes:  q.SizeBytes(),
		Rows:       len(X),
	}
	r.MAEDelta = r.Int8MAE - r.FloatMAE
	if r.FloatMAE > 0 {
		r.MAEDeltaPct = 100 * r.MAEDelta / r.FloatMAE
	}
	if r.Int8Bytes > 0 {
		r.CompressionRatio = float64(r.FloatBytes) / float64(r.Int8Bytes)
	}
	return r, nil
}
