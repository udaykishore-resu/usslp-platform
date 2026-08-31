package app

import (
	"fmt"
	"math"
	"sort"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Substitute records that two SKUs compete for the same purchase.
//
// # Why cannibalisation has to be modelled explicitly
//
// Tier 2 optimises one SKU at a time and, given a category of close
// substitutes, will happily recommend discounting all of them — each
// recommendation looks like it wins volume, and collectively they win nothing
// but a lower average selling price. The cross-elasticity term is the only
// thing that makes the category-level answer differ from the sum of the
// line-level answers, which is the entire reason Tier 3 exists as a separate
// pass rather than as Tier 2 run in a loop.
type Substitute struct {
	// SKU is the substitute product.
	SKU canon.SKU `json:"sku"`
	// CrossElasticity is the percentage change in *this* SKU's demand for a one
	// percent change in the substitute's price. It is positive for substitutes
	// (their price up, our demand up) and negative for complements.
	CrossElasticity float64 `json:"cross_elasticity"`
}

// SKUState is one product's input to the cross-store pass.
type SKUState struct {
	SKU         canon.SKU          `json:"sku"`
	Store       canon.StoreID      `json:"store_id"`
	Constraints domain.Constraints `json:"constraints"`
	Features    domain.Features    `json:"features"`
	// Elasticity is the own-price estimate.
	Elasticity ml.Elasticity `json:"elasticity"`
	// BaselineUnits is observed demand at the current price.
	BaselineUnits float64 `json:"baseline_units"`
	// Substitutes are the products this one cannibalises.
	Substitutes []Substitute `json:"substitutes,omitempty"`
	// ForecastUnits is the Tier-3 sequence model's demand forecast for the
	// coming period at the *current* price. When present it replaces the
	// trailing velocity as the baseline, which is the point of running a
	// sequence model at all: a bank-holiday week's baseline should be the
	// bank-holiday forecast, not last week's average.
	ForecastUnits *float64 `json:"forecast_units,omitempty"`
}

// CrossStoreInput is one Tier-3 optimisation pass.
type CrossStoreInput struct {
	// States are every (store, SKU) in scope.
	States []SKUState
	// Model is the shared demand model, applied per state. Nil falls back to
	// the per-state elasticity projection.
	Model DemandModel
	// Objective is what the pass maximises.
	Objective Objective
	// MaxRounds bounds the coordinate-descent iteration.
	MaxRounds int
	// ConvergenceMinor stops the iteration once no price moves by more than
	// this many minor units in a round.
	ConvergenceMinor int64
}

// CrossStoreResult is one SKU's outcome from the Tier-3 pass.
type CrossStoreResult struct {
	SKU   canon.SKU     `json:"sku"`
	Store canon.StoreID `json:"store_id"`
	// CurrentMinor and RecommendedMinor bracket the proposed move.
	CurrentMinor     int64 `json:"current_minor"`
	RecommendedMinor int64 `json:"recommended_minor"`
	// IndependentMinor is what Tier 2 would have recommended for this SKU
	// alone. Reporting it makes the cannibalisation adjustment visible instead
	// of buried, which is what lets a category manager believe the answer.
	IndependentMinor int64 `json:"independent_minor"`
	// ExpectedUnits and ExpectedProfitMinor are at the recommended price,
	// after cannibalisation.
	ExpectedUnits       float64 `json:"expected_units"`
	ExpectedProfitMinor float64 `json:"expected_profit_minor"`
	// CannibalisationUnits is the demand this SKU takes from, or gives to, its
	// substitutes at the recommended price. Negative means it steals volume.
	CannibalisationUnits float64 `json:"cannibalisation_units"`
	// Decision is the Tier-1 record for the recommended price.
	Decision domain.Decision `json:"decision"`
	// Confident mirrors the elasticity's usability.
	Confident bool   `json:"confident"`
	Rationale string `json:"rationale"`
}

// CrossStoreReport is the whole pass.
type CrossStoreReport struct {
	Results []CrossStoreResult `json:"results"`
	// Rounds is how many coordinate-descent sweeps ran.
	Rounds int `json:"rounds"`
	// Converged reports whether the iteration settled before MaxRounds.
	Converged bool `json:"converged"`
	// CategoryProfitMinor is the objective summed across every SKU, before and
	// after. The number that justifies the pass is the difference, and it is
	// smaller than the sum of the per-SKU Tier-2 uplifts — deliberately, because
	// those double-count the volume the SKUs take from each other.
	BaselineProfitMinor  float64 `json:"baseline_profit_minor"`
	OptimisedProfitMinor float64 `json:"optimised_profit_minor"`
	// IndependentProfitMinor is what the naive sum of Tier-2 answers claims,
	// evaluated *with* cannibalisation. It is normally worse than the
	// coordinated answer, and showing both is how the pass earns its keep.
	IndependentProfitMinor float64 `json:"independent_profit_minor"`
	// Skipped names SKUs that could not be optimised, with the reason.
	Skipped map[string]string `json:"skipped,omitempty"`
}

// OptimiseCrossStore runs the Tier-3 pass.
//
// # The algorithm and why it is this one
//
// Joint optimisation over N SKUs with cross-elasticities is a non-convex
// problem on a discrete lattice; solving it exactly is exponential and solving
// it approximately with a general-purpose method needs a library this module
// cannot have. Coordinate descent — repeatedly re-optimise one SKU holding the
// others at their current proposal, until nothing moves — is the standard
// tractable answer, it converges monotonically on this objective because each
// step can only increase it, and it degrades gracefully: stopping after one
// round gives the naive per-SKU answer, and every extra round strictly
// improves on it. The pass is a fifteen-minute batch job, so the iteration
// cost is irrelevant and the guarantee is worth more than the speed.
func OptimiseCrossStore(in CrossStoreInput) (CrossStoreReport, error) {
	if len(in.States) == 0 {
		return CrossStoreReport{}, fmt.Errorf("pricing: cross-store pass with no SKUs")
	}
	if in.Objective == "" {
		in.Objective = ObjectiveProfit
	}
	if in.MaxRounds <= 0 {
		in.MaxRounds = 6
	}
	if in.ConvergenceMinor <= 0 {
		in.ConvergenceMinor = 1
	}

	// Index by SKU within store: cannibalisation is a within-store effect. Two
	// stores fifty miles apart do not compete for the same shopper, and
	// modelling them as if they did would spread one store's promotion across
	// the estate.
	type ref struct{ store, sku string }
	idx := make(map[ref]int, len(in.States))
	for i, s := range in.States {
		idx[ref{string(s.Store), string(s.SKU)}] = i
	}

	prices := make([]int64, len(in.States))
	independent := make([]int64, len(in.States))
	skipped := map[string]string{}
	candidateSets := make([][]int64, len(in.States))

	for i, s := range in.States {
		prices[i] = s.Constraints.CurrentMinor
		independent[i] = s.Constraints.CurrentMinor
		cands := s.Constraints.Candidates(0)
		if len(cands) == 0 {
			skipped[string(s.Store)+"/"+string(s.SKU)] = feasibilitySummary(s.Constraints)
			continue
		}
		candidateSets[i] = cands
	}

	// baseline demand for state i at price p, before cannibalisation.
	vec := make([]float64, domain.NumFeatures)
	ownDemand := func(i int, p int64) float64 {
		s := in.States[i]
		base := s.BaselineUnits
		if s.ForecastUnits != nil {
			base = *s.ForecastUnits
		}
		var units float64
		switch {
		case in.Model != nil:
			f := s.Features
			f.PriceMinor = float64(p)
			f.FillVector(vec)
			units = in.Model.Predict(vec)
			// Ensembling with the sequence forecast: the trees know how demand
			// responds to price, the LSTM knows what the level should be this
			// week. Re-centring the tree's response on the forecast level takes
			// the useful half of each. The multiplicative form is chosen over
			// an additive blend because demand is non-negative and a level
			// shift is proportional.
			if s.ForecastUnits != nil {
				f.PriceMinor = float64(s.Constraints.CurrentMinor)
				f.FillVector(vec)
				atCurrent := in.Model.Predict(vec)
				if atCurrent > 1e-9 {
					units *= *s.ForecastUnits / atCurrent
				}
			}
		case s.Elasticity.Usable && s.Constraints.CurrentMinor > 0:
			units = s.Elasticity.DemandAt(float64(s.Constraints.CurrentMinor), base, float64(p))
		default:
			units = base
		}
		if units < 0 || math.IsNaN(units) {
			units = 0
		}
		return units
	}

	// demandWithCannibalisation adjusts state i's demand for the current prices
	// of its substitutes, using the constant-cross-elasticity form
	//
	//	q_i = q_i^own * prod_j (p_j / p_j^0) ^ crossElasticity_ij
	//
	// which is the same functional family as the own-price model, so the two
	// compose without a units mismatch.
	demand := func(i int, p int64, current []int64) float64 {
		q := ownDemand(i, p)
		for _, sub := range in.States[i].Substitutes {
			j, ok := idx[ref{string(in.States[i].Store), string(sub.SKU)}]
			if !ok {
				continue
			}
			p0 := float64(in.States[j].Constraints.CurrentMinor)
			pj := float64(current[j])
			if p0 <= 0 || pj <= 0 || sub.CrossElasticity == 0 {
				continue
			}
			q *= math.Pow(pj/p0, sub.CrossElasticity)
		}
		if q < 0 || math.IsNaN(q) || math.IsInf(q, 0) {
			return 0
		}
		return q
	}

	value := func(i int, p int64, q float64) float64 {
		cost := float64(in.States[i].Constraints.UnitCost)
		switch in.Objective {
		case ObjectiveRevenue:
			return float64(p) * q
		case ObjectiveUnits:
			return q
		default:
			return (float64(p) - cost) * q
		}
	}

	total := func(current []int64) float64 {
		sum := 0.0
		for i := range in.States {
			sum += value(i, current[i], demand(i, current[i], current))
		}
		return sum
	}

	baselineProfit := total(prices)

	// Round zero: the independent Tier-2 answer, each SKU optimised against the
	// others' *current* prices. It is both the starting point for coordinate
	// descent and the comparison the report shows.
	for i := range in.States {
		if candidateSets[i] == nil {
			continue
		}
		best, bestVal := prices[i], math.Inf(-1)
		for _, p := range candidateSets[i] {
			if v := value(i, p, demand(i, p, prices)); v > bestVal {
				best, bestVal = p, v
			}
		}
		independent[i] = best
	}
	independentProfit := total(independent)

	copy(prices, independent)
	rounds, converged := 0, false
	for round := 0; round < in.MaxRounds; round++ {
		rounds = round + 1
		moved := int64(0)
		for i := range in.States {
			if candidateSets[i] == nil {
				continue
			}
			// Re-optimise i holding every other price at its current proposal.
			// The objective is the *whole category's* value, not just i's,
			// because moving i changes its substitutes' demand too — optimising
			// i's own line alone is exactly the mistake this pass exists to
			// correct.
			best, bestVal := prices[i], math.Inf(-1)
			saved := prices[i]
			for _, p := range candidateSets[i] {
				prices[i] = p
				if v := total(prices); v > bestVal {
					best, bestVal = p, v
				}
			}
			prices[i] = best
			if d := best - saved; d > moved || -d > moved {
				if d < 0 {
					d = -d
				}
				moved = d
			}
		}
		if moved <= in.ConvergenceMinor {
			converged = true
			break
		}
	}

	report := CrossStoreReport{
		Rounds: rounds, Converged: converged,
		BaselineProfitMinor:    baselineProfit,
		OptimisedProfitMinor:   total(prices),
		IndependentProfitMinor: independentProfit,
		Results:                make([]CrossStoreResult, 0, len(in.States)),
	}
	if len(skipped) > 0 {
		report.Skipped = skipped
	}
	for i, s := range in.States {
		if candidateSets[i] == nil {
			continue
		}
		q := demand(i, prices[i], prices)
		own := ownDemand(i, prices[i])
		res := CrossStoreResult{
			SKU: s.SKU, Store: s.Store,
			CurrentMinor:         s.Constraints.CurrentMinor,
			RecommendedMinor:     prices[i],
			IndependentMinor:     independent[i],
			ExpectedUnits:        q,
			ExpectedProfitMinor:  value(i, prices[i], q),
			CannibalisationUnits: q - own,
			Decision:             domain.Evaluate(s.Constraints, prices[i]),
			Confident:            s.Elasticity.Usable,
		}
		switch {
		case res.RecommendedMinor == res.IndependentMinor:
			res.Rationale = "cannibalisation did not change this line's answer"
		default:
			res.Rationale = fmt.Sprintf(
				"coordinated price %d differs from the line-level optimum %d because %d substitute(s) share its demand",
				res.RecommendedMinor, res.IndependentMinor, len(s.Substitutes))
		}
		report.Results = append(report.Results, res)
	}
	sort.Slice(report.Results, func(i, j int) bool {
		if report.Results[i].Store != report.Results[j].Store {
			return report.Results[i].Store < report.Results[j].Store
		}
		return report.Results[i].SKU < report.Results[j].SKU
	})
	return report, nil
}

// ForecastInput drives the Tier-3 sequence forecast.
type ForecastInput struct {
	// History is the per-step feature series, oldest first.
	History [][]float64
	// Horizon is how many steps ahead to forecast.
	Horizon int
	// LastKnown is the most recent observed target, used to seed the recursive
	// forecast's lag feature.
	LastKnown float64
	// LagFeature is the index in the per-step feature vector holding the
	// previous period's demand, or -1 when the caller's features carry no lag.
	// A recursive multi-step forecast has to feed its own output back into that
	// slot; a caller that has no lag feature gets a repeated one-step forecast,
	// which is correct but less useful, and the difference is visible here
	// rather than silently assumed.
	LagFeature int
}

// Forecast produces a multi-step demand forecast from a trained LSTM.
//
// The recursion is deliberately explicit about its own weakness: each step
// after the first is conditioned on the model's own output, so errors compound,
// and a fourteen-step forecast is a much weaker claim than a one-step one. The
// evaluation harness measures the horizon the caller actually uses rather than
// reporting the one-step error and hoping.
func Forecast(n *ml.LSTM, in ForecastInput) ([]float64, error) {
	if n == nil {
		return nil, fmt.Errorf("pricing: no forecast model")
	}
	if len(in.History) == 0 {
		return nil, fmt.Errorf("pricing: forecast needs at least one step of history")
	}
	if in.Horizon <= 0 {
		in.Horizon = 1
	}
	seq := make([][]float64, len(in.History), len(in.History)+in.Horizon)
	copy(seq, in.History)
	out := make([]float64, 0, in.Horizon)
	last := in.LastKnown
	width := len(in.History[0])

	for step := 0; step < in.Horizon; step++ {
		y, err := n.PredictNext(seq)
		if err != nil {
			return nil, err
		}
		if y < 0 {
			// Demand cannot be negative; the model has no such constraint, so
			// the clamp lives here where it is visible.
			y = 0
		}
		out = append(out, y)
		if step == in.Horizon-1 {
			break
		}
		next := make([]float64, width)
		copy(next, seq[len(seq)-1])
		if in.LagFeature >= 0 && in.LagFeature < width {
			next[in.LagFeature] = last
		}
		last = y
		seq = append(seq, next)
	}
	return out, nil
}
