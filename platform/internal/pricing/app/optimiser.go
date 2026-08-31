// Package app is the pricing service's application layer: the three decision
// tiers, the anomaly detector, and the training jobs that feed them.
//
// The layering is the platform's: domain holds the pure Tier-1 rules, ml holds
// the models, features holds the point-in-time store, and this package
// orchestrates them behind interfaces declared in ports. Nothing here touches
// HTTP or the event bus directly.
package app

import (
	"fmt"
	"math"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
)

// DemandModel is anything that estimates units sold for a feature row. The
// Tier-2 optimiser is written against the interface rather than against *ml.GBT
// so that the same optimiser searches against the float model in the cloud, the
// quantised model on a gateway, and a pure elasticity projection when no model
// has been trained for a SKU yet.
type DemandModel interface {
	Predict(x []float64) float64
}

// Candidate is one evaluated price point.
type Candidate struct {
	// PriceMinor is the price being evaluated.
	PriceMinor int64 `json:"price_minor"`
	// ExpectedUnits is the demand model's estimate at that price.
	ExpectedUnits float64 `json:"expected_units"`
	// ExpectedRevenueMinor and ExpectedProfitMinor follow from it.
	ExpectedRevenueMinor float64 `json:"expected_revenue_minor"`
	ExpectedProfitMinor  float64 `json:"expected_profit_minor"`
	// ProfitLowMinor and ProfitHighMinor bound the profit using the
	// elasticity's confidence interval, where one is available. A candidate
	// whose interval straddles the incumbent's is not really an improvement,
	// and the optimiser says so rather than pretending the point estimate is
	// the whole story.
	ProfitLowMinor  float64 `json:"profit_low_minor"`
	ProfitHighMinor float64 `json:"profit_high_minor"`
}

// OptimisationInput is everything the expected-margin optimiser needs.
type OptimisationInput struct {
	// Constraints are the Tier-1 rules. The optimiser searches only inside
	// their feasible set, so a recommendation can never need a second
	// compliance pass.
	Constraints domain.Constraints
	// Features is the current state of the (store, SKU). The optimiser
	// overwrites the price field for each candidate and leaves the rest alone.
	Features domain.Features
	// Model estimates demand. Nil falls back to the elasticity projection,
	// which requires Elasticity to be usable.
	Model DemandModel
	// Elasticity is the estimated own-price elasticity with its interval. It is
	// used for the fallback projection and, whenever it is usable, for the
	// confidence bounds on every candidate's profit.
	Elasticity ml.Elasticity
	// ReferenceUnits is observed demand at the current price, the anchor for
	// the elasticity projection.
	ReferenceUnits float64
	// Objective selects what is maximised.
	Objective Objective
	// MaxCandidates bounds the search. Zero uses a default sized for a
	// one-cent lattice over a typical feasible range.
	MaxCandidates int
}

// Objective is what the optimiser maximises.
type Objective string

// The supported objectives.
const (
	// ObjectiveProfit maximises expected gross profit. It is the default and
	// the only one that needs a unit cost.
	ObjectiveProfit Objective = "profit"
	// ObjectiveRevenue maximises expected revenue. Retailers use it on traffic
	// drivers where the margin is made on the basket, not the line.
	ObjectiveRevenue Objective = "revenue"
	// ObjectiveUnits maximises volume, subject to the Tier-1 floor. It is what
	// a clearance policy on a short-dated SKU wants: sell the stock before it
	// is written off, and let the margin floor stop the price going silly.
	ObjectiveUnits Objective = "units"
)

// Recommendation is the optimiser's answer.
type Recommendation struct {
	// Best is the chosen candidate.
	Best Candidate `json:"best"`
	// Incumbent is the same evaluation at the price currently on the shelf, so
	// the caller can see what the change is worth rather than only what the new
	// price earns.
	Incumbent Candidate `json:"incumbent"`
	// UpliftMinor is Best minus Incumbent on the chosen objective.
	UpliftMinor float64 `json:"uplift_minor"`
	// Decision is the Tier-1 decision for the chosen price. It is always
	// accepted or adjusted by construction, and it is returned so the caller
	// has the compliance record without a second call.
	Decision domain.Decision `json:"decision"`
	// Evaluated is how many candidates were scored.
	Evaluated int `json:"evaluated"`
	// Curve is the full evaluated set, for the operator UI's price/profit plot.
	Curve []Candidate `json:"curve,omitempty"`
	// Confident reports whether the recommendation rests on an elasticity
	// estimate the platform is willing to act on.
	Confident bool `json:"confident"`
	// Rationale explains the choice, including why it may not be confident.
	Rationale string `json:"rationale"`
}

// ErrInfeasible is returned when the Tier-1 constraints admit no price at all.
// The optimiser refuses rather than relaxing a constraint, because deciding
// which rule to break is a merchandising decision and not a numerical one.
var ErrInfeasible = fmt.Errorf("pricing: no price satisfies the Tier-1 constraints")

// Optimise searches the feasible price set for the best expected outcome.
//
// # Why an exhaustive search rather than a continuous optimiser
//
// The set of prices a shelf may legally display is discrete and small: the
// feasible range is usually a pound or two wide, the lattice is one cent, and
// the ending policy thins it further. A few hundred evaluations of a
// 120-tree ensemble is well inside the Tier-2 budget, and the result is exactly
// optimal over the set that actually exists. A gradient method would find an
// optimum in the continuum, round it to a legal price, and land somewhere that
// is neither the constrained optimum nor obviously wrong — which is worse,
// because it is not detectable.
func Optimise(in OptimisationInput) (Recommendation, error) {
	candidates := in.Constraints.Candidates(in.MaxCandidates)
	if len(candidates) == 0 {
		return Recommendation{}, fmt.Errorf("%w: %s", ErrInfeasible, feasibilitySummary(in.Constraints))
	}
	if in.Objective == "" {
		in.Objective = ObjectiveProfit
	}

	// One buffer, reused across every candidate: the whole point of the
	// FillVector API is that a several-hundred-point search allocates once.
	vec := make([]float64, domain.NumFeatures)
	feats := in.Features
	cost := float64(in.Constraints.UnitCost)

	useModel := in.Model != nil
	if !useModel && !in.Elasticity.Usable {
		return Recommendation{}, fmt.Errorf("pricing: no demand model and no usable elasticity estimate (%s)",
			in.Elasticity.Reason)
	}

	score := func(priceMinor int64) Candidate {
		p := float64(priceMinor)
		var units float64
		if useModel {
			feats.PriceMinor = p
			feats.FillVector(vec)
			units = in.Model.Predict(vec)
		} else {
			units = in.Elasticity.DemandAt(float64(in.Constraints.CurrentMinor), in.ReferenceUnits, p)
		}
		// A demand model is a regression, not a physical law: it can and does
		// return a negative number when extrapolated. Clamping at zero here
		// rather than in the model keeps the model honest and the optimiser
		// sane — negative demand would otherwise make an absurdly high price
		// look profitable.
		if units < 0 || math.IsNaN(units) {
			units = 0
		}
		c := Candidate{
			PriceMinor:           priceMinor,
			ExpectedUnits:        units,
			ExpectedRevenueMinor: p * units,
			ExpectedProfitMinor:  (p - cost) * units,
		}
		if in.Elasticity.Usable && in.ReferenceUnits > 0 && in.Constraints.CurrentMinor > 0 {
			lo, hi := in.Elasticity.Bounds(float64(in.Constraints.CurrentMinor), in.ReferenceUnits, p)
			c.ProfitLowMinor = (p - cost) * lo
			c.ProfitHighMinor = (p - cost) * hi
			if c.ProfitLowMinor > c.ProfitHighMinor {
				c.ProfitLowMinor, c.ProfitHighMinor = c.ProfitHighMinor, c.ProfitLowMinor
			}
		}
		return c
	}

	objective := func(c Candidate) float64 {
		switch in.Objective {
		case ObjectiveRevenue:
			return c.ExpectedRevenueMinor
		case ObjectiveUnits:
			return c.ExpectedUnits
		default:
			return c.ExpectedProfitMinor
		}
	}

	curve := make([]Candidate, 0, len(candidates))
	best := Candidate{}
	bestScore := math.Inf(-1)
	for _, p := range candidates {
		c := score(p)
		curve = append(curve, c)
		if s := objective(c); s > bestScore {
			best, bestScore = c, s
		}
	}

	incumbent := score(in.Constraints.CurrentMinor)
	rec := Recommendation{
		Best: best, Incumbent: incumbent,
		UpliftMinor: objective(best) - objective(incumbent),
		Decision:    domain.Evaluate(in.Constraints, best.PriceMinor),
		Evaluated:   len(candidates),
		Curve:       curve,
		Confident:   in.Elasticity.Usable,
	}
	switch {
	case !in.Elasticity.Usable:
		rec.Rationale = fmt.Sprintf(
			"optimum of %d over %d feasible prices, but the elasticity estimate is not usable (%s); "+
				"treat the size of the move as indicative and the direction as the recommendation",
			best.PriceMinor, len(candidates), in.Elasticity.Reason)
	case best.PriceMinor == in.Constraints.CurrentMinor:
		rec.Rationale = fmt.Sprintf("the current price is already optimal over %d feasible prices", len(candidates))
	default:
		rec.Rationale = fmt.Sprintf(
			"%s improves by %.0f minor units moving from %d to %d, searched over %d feasible prices "+
				"with elasticity %.2f [%.2f, %.2f]",
			in.Objective, rec.UpliftMinor, in.Constraints.CurrentMinor, best.PriceMinor, len(candidates),
			in.Elasticity.Coefficient, in.Elasticity.Low, in.Elasticity.High)
	}
	return rec, nil
}

// feasibilitySummary renders why no price exists, for the error message.
func feasibilitySummary(c domain.Constraints) string {
	d := domain.Evaluate(c, c.CurrentMinor)
	if len(d.Violations) == 0 {
		return "the feasible range is empty"
	}
	s := ""
	for i, v := range d.Violations {
		if i > 0 {
			s += "; "
		}
		s += string(v.Kind) + ": " + v.Detail
	}
	return s
}
