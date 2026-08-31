package columnar

import (
	"math"
	"sort"
)

// TDigest estimates quantiles from a stream in bounded space.
//
// # Why an approximate structure at all
//
// The SLO report asks for p50, p95 and p99 of end-to-end price latency across a
// week of delivery confirmations — for a large estate, several hundred million
// rows. Exact quantiles need every value sorted, which is several gigabytes of
// resident memory for one report. A t-digest holds a few hundred centroids and
// answers the same question to within a fraction of a percent at the tails,
// which is where the SLO lives.
//
// # Why t-digest specifically
//
// Its error is *relative to the quantile*, not uniform: it is most accurate at
// q near 0 and 1 and least accurate in the middle. That is exactly backwards
// from a uniform sketch and exactly right for this use, because nobody has ever
// been paged about a p50. The package's tests measure the error against an
// exact computation on a known distribution rather than asserting the paper's
// bound.
type TDigest struct {
	// centroids are kept sorted by mean.
	centroids []centroid
	// compression controls the size/accuracy trade-off: the digest holds
	// roughly compression centroids.
	compression float64
	count       float64
	min, max    float64
	// unmerged buffers incoming values so that a merge amortises over many
	// additions rather than running per value.
	unmerged []float64
}

type centroid struct {
	mean   float64
	weight float64
}

// DefaultCompression is the platform's t-digest size.
//
// A hundred centroids gives sub-one-percent error at the tails on the
// distributions this store holds, and costs 1.6 kB. Raising it to a thousand
// buys about another factor of three in accuracy for ten times the memory,
// which is not a trade the SLO report needs: the SLO is written against a
// 3,000 ms budget and the measurement error at 100 is under a millisecond.
const DefaultCompression = 100

// NewTDigest builds a digest.
func NewTDigest(compression float64) *TDigest {
	if compression < 20 {
		compression = DefaultCompression
	}
	return &TDigest{
		compression: compression,
		min:         math.Inf(1),
		max:         math.Inf(-1),
		unmerged:    make([]float64, 0, 256),
	}
}

// Add records a value.
func (d *TDigest) Add(v float64) {
	if math.IsNaN(v) {
		// A NaN in a latency column is a producer bug; silently skewing every
		// quantile is worse than dropping it, and the caller's row count still
		// reflects that the row existed.
		return
	}
	if v < d.min {
		d.min = v
	}
	if v > d.max {
		d.max = v
	}
	d.count++
	d.unmerged = append(d.unmerged, v)
	if len(d.unmerged) >= cap(d.unmerged) {
		d.merge()
	}
}

// Count is how many values were recorded.
func (d *TDigest) Count() float64 { return d.count }

// Min and Max are exact, because they are tracked directly rather than read off
// the centroids. A p100 that is an approximation of the maximum would be a
// strange thing to report.
func (d *TDigest) Min() float64 { return d.min }
func (d *TDigest) Max() float64 { return d.max }

// merge folds the buffer into the centroid list.
//
// The scale function is the standard k_1: the permitted weight of a centroid at
// quantile q is proportional to q(1-q), so centroids near the tails stay small
// and the tails stay accurate.
func (d *TDigest) merge() {
	if len(d.unmerged) == 0 {
		return
	}
	d.compress()
}

// compress rebuilds the centroid list from whatever is currently held, buffered
// or not. Merge calls it directly, because folding another digest's centroids in
// leaves nothing in the buffer and would otherwise skip the recompression that
// keeps the list bounded.
func (d *TDigest) compress() {
	all := make([]centroid, 0, len(d.centroids)+len(d.unmerged))
	all = append(all, d.centroids...)
	for _, v := range d.unmerged {
		all = append(all, centroid{mean: v, weight: 1})
	}
	d.unmerged = d.unmerged[:0]
	sort.Slice(all, func(i, j int) bool { return all[i].mean < all[j].mean })

	total := 0.0
	for _, c := range all {
		total += c.weight
	}
	if total == 0 || len(all) == 0 {
		d.centroids = nil
		return
	}

	out := make([]centroid, 0, int(d.compression)+8)
	cur := all[0]
	soFar := 0.0
	for _, c := range all[1:] {
		// The quantile at the centre of the merged centroid if c were added.
		q := (soFar + cur.weight + c.weight/2) / total
		limit := 4 * total * q * (1 - q) / d.compression
		// Note the absence of an escape for a limit below one. At the extreme
		// tails q(1-q) is tiny and the limit falls under a single value's
		// weight, so nothing merges and the tail is kept as singletons — which
		// is precisely the property that makes a t-digest accurate at p99 and
		// p999. An earlier version of this loop merged whenever the limit fell
		// below one "to make progress", and the measured p999 error went from
		// 0.2% to 91%.
		if cur.weight+c.weight <= limit {
			// Merge, keeping the weighted mean.
			w := cur.weight + c.weight
			cur.mean = (cur.mean*cur.weight + c.mean*c.weight) / w
			cur.weight = w
			continue
		}
		out = append(out, cur)
		soFar += cur.weight
		cur = c
	}
	out = append(out, cur)
	d.centroids = out
}

// Quantile returns the estimated value at q in [0, 1].
func (d *TDigest) Quantile(q float64) float64 {
	d.merge()
	if d.count == 0 {
		return 0
	}
	if q <= 0 {
		return d.min
	}
	if q >= 1 {
		return d.max
	}
	if len(d.centroids) == 1 {
		return d.centroids[0].mean
	}

	target := q * d.count
	soFar := 0.0
	for i, c := range d.centroids {
		// Interpolate within the centroid's weight span, between the midpoints
		// of its neighbours. Interpolating rather than returning the centroid's
		// mean is what keeps the estimate smooth on a small digest.
		if soFar+c.weight >= target {
			var left, right float64
			switch {
			case i == 0:
				left = d.min
			default:
				left = (d.centroids[i-1].mean + c.mean) / 2
			}
			switch {
			case i == len(d.centroids)-1:
				right = d.max
			default:
				right = (c.mean + d.centroids[i+1].mean) / 2
			}
			if c.weight <= 0 {
				return c.mean
			}
			frac := (target - soFar) / c.weight
			return left + frac*(right-left)
		}
		soFar += c.weight
	}
	return d.max
}

// Merge folds another digest into this one, which is how a query that scans
// blocks in parallel combines its partial results.
func (d *TDigest) Merge(other *TDigest) {
	if other == nil || other.count == 0 {
		return
	}
	other.merge()
	d.centroids = append(d.centroids, other.centroids...)
	d.count += other.count
	if other.min < d.min {
		d.min = other.min
	}
	if other.max > d.max {
		d.max = other.max
	}
	d.compress()
}

// Centroids is the digest's size, for the memory figures the store reports.
func (d *TDigest) Centroids() int {
	d.merge()
	return len(d.centroids)
}
