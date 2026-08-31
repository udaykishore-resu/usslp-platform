package ml

import (
	"errors"
	"fmt"
	"math"
)

// Observation is one price/quantity pair from a SKU's trading history.
type Observation struct {
	// PriceMinor is the price actually charged, in minor units.
	PriceMinor float64 `json:"price_minor"`
	// Quantity is units sold in the period at that price.
	Quantity float64 `json:"quantity"`
	// Weight lets a caller down-weight an observation — a partial trading day,
	// a period with a stock-out. Zero or negative means one.
	Weight float64 `json:"weight,omitempty"`
}

// Elasticity is an own-price elasticity estimate with its uncertainty.
//
// The uncertainty is the point of the type. An elasticity is a slope estimated
// from a handful of price points, and the difference between "-1.8, tightly
// estimated" and "-1.8, could be anywhere from -6 to +2" is the difference
// between a defensible price change and a guess. Callers are made to look at
// the interval because the struct does not give them a bare coefficient to use
// without one: Usable is false whenever the estimate should not drive a price.
type Elasticity struct {
	// Coefficient is the estimated elasticity: the percentage change in units
	// sold for a one percent change in price. Normal goods give a negative
	// value.
	Coefficient float64 `json:"coefficient"`
	// StdErr is the standard error of the coefficient.
	StdErr float64 `json:"std_err"`
	// Low and High bound the coefficient at the stated confidence level.
	Low  float64 `json:"ci_low"`
	High float64 `json:"ci_high"`
	// ConfidenceLevel is the two-sided level the interval is quoted at.
	ConfidenceLevel float64 `json:"confidence_level"`
	// R2 is the fraction of log-quantity variance the log-price term explains.
	R2 float64 `json:"r_squared"`
	// Observations is the number of usable price/quantity pairs.
	Observations int `json:"observations"`
	// DistinctPrices is how many different price points the estimate rests on.
	// One price point means no slope is identified at all, however many
	// observations there are — the classic way a retail elasticity model
	// produces a confident number from nothing.
	DistinctPrices int `json:"distinct_prices"`
	// Usable reports whether the estimate is fit to drive a price change.
	Usable bool `json:"usable"`
	// Reason explains an unusable estimate, in operator-facing language.
	Reason string `json:"reason,omitempty"`
}

// ElasticityPolicy is the evidence bar an estimate must clear.
//
// The defaults are deliberately conservative. The cost of refusing to optimise
// a SKU is a price that stays where a human put it; the cost of acting on a
// spurious elasticity is a systematic margin error repeated across every store,
// which is discovered in a quarterly review rather than an alert.
type ElasticityPolicy struct {
	// MinObservations is the minimum usable pairs.
	MinObservations int
	// MinDistinctPrices is the minimum number of distinct price points. Two is
	// the arithmetic minimum for a slope; three is the minimum for the residual
	// variance that the standard error is computed from to mean anything.
	MinDistinctPrices int
	// MaxCIWidth is the widest acceptable confidence interval, in elasticity
	// units. An interval wider than this says the data cannot distinguish an
	// elastic SKU from an inelastic one.
	MaxCIWidth float64
	// RequireNegative refuses an interval that includes zero or positive
	// elasticity. A non-negative own-price elasticity is either a Giffen good,
	// a luxury signalling effect, or — overwhelmingly more likely — endogeneity:
	// the retailer raised the price *because* demand was strong.
	RequireNegative bool
	// ConfidenceLevel is the two-sided level, 0.95 by default.
	ConfidenceLevel float64
}

// DefaultElasticityPolicy is the platform default.
func DefaultElasticityPolicy() ElasticityPolicy {
	return ElasticityPolicy{
		MinObservations: 12, MinDistinctPrices: 3,
		MaxCIWidth: 2.0, RequireNegative: true, ConfidenceLevel: 0.95,
	}
}

func (p *ElasticityPolicy) applyDefaults() {
	d := DefaultElasticityPolicy()
	if p.MinObservations <= 0 {
		p.MinObservations = d.MinObservations
	}
	if p.MinDistinctPrices <= 0 {
		p.MinDistinctPrices = d.MinDistinctPrices
	}
	if p.MaxCIWidth <= 0 {
		p.MaxCIWidth = d.MaxCIWidth
	}
	if p.ConfidenceLevel <= 0 || p.ConfidenceLevel >= 1 {
		p.ConfidenceLevel = d.ConfidenceLevel
	}
}

// ErrElasticity marks an estimate that could not be computed at all, as
// distinct from one that was computed and found untrustworthy. The second case
// is an Elasticity with Usable false and is the normal, expected outcome for
// most SKUs on most days.
var ErrElasticity = errors.New("ml: elasticity cannot be estimated")

// EstimateElasticity fits the constant-elasticity demand model
//
//	ln(q) = a + b*ln(p) + e
//
// by weighted least squares and returns b with its confidence interval.
//
// # Why log-log
//
// The coefficient of a log-log regression *is* the elasticity, by construction
// and at every price, which is what makes it the right functional form for an
// optimiser that will evaluate demand at prices the retailer has never charged.
// A linear demand curve's elasticity varies along the curve, so extrapolating
// one from a fitted line means quoting an elasticity that is only valid at the
// mean price.
//
// # What this does not claim
//
// This is a correlational estimate from observational data. Where the retailer
// changed prices in response to demand, the estimate is biased towards zero or
// positive, and the policy's RequireNegative check is the crude guard against
// acting on such a fit. A causal estimate needs randomised price experiments,
// which the promotion service's control-group machinery supports and this
// function does not attempt to fake.
func EstimateElasticity(obs []Observation, policy ElasticityPolicy) (Elasticity, error) {
	policy.applyDefaults()
	e := Elasticity{ConfidenceLevel: policy.ConfidenceLevel}

	// Only strictly positive prices and quantities can be logged. A zero-sales
	// period is real information about demand, but it is not information the
	// log-log form can represent, and quietly adding one unit to every
	// observation to make the logarithm defined would bias the slope towards
	// zero by an amount that depends on the SKU's typical volume.
	type row struct{ lp, lq, w float64 }
	rows := make([]row, 0, len(obs))
	prices := make(map[float64]struct{}, len(obs))
	dropped := 0
	for _, o := range obs {
		if o.PriceMinor <= 0 || o.Quantity <= 0 {
			dropped++
			continue
		}
		w := o.Weight
		if w <= 0 {
			w = 1
		}
		rows = append(rows, row{lp: math.Log(o.PriceMinor), lq: math.Log(o.Quantity), w: w})
		prices[o.PriceMinor] = struct{}{}
	}
	e.Observations = len(rows)
	e.DistinctPrices = len(prices)

	if len(rows) < 3 {
		e.Reason = fmt.Sprintf("only %d usable observations (%d dropped for zero price or zero sales); "+
			"a slope and its residual variance need at least 3", len(rows), dropped)
		return e, nil
	}
	if e.DistinctPrices < 2 {
		e.Reason = "every observation is at the same price: no price variation, so no slope is identified"
		return e, nil
	}

	var sw, swx, swy float64
	for _, r := range rows {
		sw += r.w
		swx += r.w * r.lp
		swy += r.w * r.lq
	}
	mx, my := swx/sw, swy/sw
	var sxx, sxy, syy float64
	for _, r := range rows {
		dx, dy := r.lp-mx, r.lq-my
		sxx += r.w * dx * dx
		sxy += r.w * dx * dy
		syy += r.w * dy * dy
	}
	if sxx <= 0 {
		e.Reason = "log-price has zero variance: no slope is identified"
		return e, nil
	}

	b := sxy / sxx
	e.Coefficient = b

	// Residual variance. Degrees of freedom are n-2 for a two-parameter fit;
	// with n = 3 that is 1, and the resulting interval is enormous, which is
	// the correct and useful answer rather than a failure.
	var rss float64
	for _, r := range rows {
		resid := (r.lq - my) - b*(r.lp-mx)
		rss += r.w * resid * resid
	}
	df := len(rows) - 2
	if df < 1 {
		e.Reason = "too few observations for a residual variance"
		return e, nil
	}
	// Effective sample size for weighted least squares: the weights scale the
	// residual sum of squares, so sigma^2 uses the weighted RSS over df.
	sigma2 := rss / float64(df)
	e.StdErr = math.Sqrt(sigma2 / sxx)
	if syy > 0 {
		e.R2 = 1 - rss/syy
	}

	tcrit := studentTCritical(df, policy.ConfidenceLevel)
	e.Low = b - tcrit*e.StdErr
	e.High = b + tcrit*e.StdErr

	switch {
	case math.IsNaN(b) || math.IsInf(b, 0):
		e.Reason = "the fit did not converge to a finite coefficient"
	case e.Observations < policy.MinObservations:
		e.Reason = fmt.Sprintf("%d observations is below the %d required", e.Observations, policy.MinObservations)
	case e.DistinctPrices < policy.MinDistinctPrices:
		e.Reason = fmt.Sprintf("%d distinct price points is below the %d required",
			e.DistinctPrices, policy.MinDistinctPrices)
	case e.High-e.Low > policy.MaxCIWidth:
		e.Reason = fmt.Sprintf("the %.0f%% interval [%.2f, %.2f] is %.2f wide, above the %.2f limit: "+
			"the data cannot distinguish an elastic SKU from an inelastic one",
			policy.ConfidenceLevel*100, e.Low, e.High, e.High-e.Low, policy.MaxCIWidth)
	case policy.RequireNegative && e.High >= 0:
		e.Reason = fmt.Sprintf("the %.0f%% interval [%.2f, %.2f] includes zero or positive elasticity, "+
			"which usually indicates the price responded to demand rather than the other way round",
			policy.ConfidenceLevel*100, e.Low, e.High)
	default:
		e.Usable = true
	}
	return e, nil
}

// DemandAt projects quantity at a new price under the constant-elasticity model
// anchored at a reference point.
//
//	q(p) = q0 * (p / p0)^b
//
// It is the function the Tier-2 optimiser calls for every candidate price, and
// it is exported so that the same projection is used in the optimiser, in the
// simulator and in the API's elasticity-curve response — three places that must
// not disagree about what the model says.
func (e Elasticity) DemandAt(refPriceMinor, refQuantity, priceMinor float64) float64 {
	if refPriceMinor <= 0 || priceMinor <= 0 || refQuantity <= 0 {
		return 0
	}
	return refQuantity * math.Pow(priceMinor/refPriceMinor, e.Coefficient)
}

// Bounds projects the demand interval implied by the coefficient's confidence
// interval. A price rise makes the *upper* elasticity bound the optimistic case
// and the lower bound the pessimistic one; the function sorts them so callers
// need not reason about the sign flip.
func (e Elasticity) Bounds(refPriceMinor, refQuantity, priceMinor float64) (low, high float64) {
	a := Elasticity{Coefficient: e.Low}.DemandAt(refPriceMinor, refQuantity, priceMinor)
	b := Elasticity{Coefficient: e.High}.DemandAt(refPriceMinor, refQuantity, priceMinor)
	if a > b {
		a, b = b, a
	}
	return a, b
}

// studentTCritical returns the two-sided critical value of Student's t.
//
// A table for the small degrees of freedom that actually occur, and the normal
// approximation beyond, because the difference between t and z at 30 degrees of
// freedom is under two percent and a numerical inverse-beta routine would be a
// hundred lines of code serving no decision this platform makes. The 95% and
// 99% columns are the only ones offered, so the confidence level is snapped to
// whichever is nearer and the chosen level is reported back on the estimate.
func studentTCritical(df int, level float64) float64 {
	if df < 1 {
		return math.Inf(1)
	}
	use99 := math.Abs(level-0.99) < math.Abs(level-0.95)
	var table []float64
	if use99 {
		table = t99
	} else {
		table = t95
	}
	if df <= len(table) {
		return table[df-1]
	}
	if use99 {
		return 2.576
	}
	return 1.96
}

// t95 is the two-sided 95% critical value for df = 1..30.
var t95 = []float64{
	12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
}

// t99 is the two-sided 99% critical value for df = 1..30.
var t99 = []float64{
	63.657, 9.925, 5.841, 4.604, 4.032, 3.707, 3.499, 3.355, 3.250, 3.169,
	3.106, 3.055, 3.012, 2.977, 2.947, 2.921, 2.898, 2.878, 2.861, 2.845,
	2.831, 2.819, 2.807, 2.797, 2.787, 2.779, 2.771, 2.763, 2.756, 2.750,
}
