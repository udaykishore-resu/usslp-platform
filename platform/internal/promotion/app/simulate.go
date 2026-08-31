package app

import (
	"fmt"
	"math"
	"sort"

	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// SimulationInput asks "what would this promotion affect, and what would it
// cost".
type SimulationInput struct {
	// Rule is the promotion under test. It need not be persisted; the console's
	// preview simulates a draft the operator is still editing.
	Rule domain.Rule
	// Others are the promotions already live, so the simulation reports the
	// *incremental* effect rather than pretending the shelf is empty.
	Others []domain.Rule
	// Catalogue is the (store, SKU) population to evaluate against.
	Catalogue []domain.Product
	// ElasticityOf gives an assumed own-price elasticity per SKU, used for the
	// volume projection. A SKU absent from the map is projected at zero volume
	// response, which is the conservative assumption: the simulation then
	// reports the margin cost of discounting the units the store already sells
	// and claims no extra volume for it.
	ElasticityOf map[canon.SKU]float64
	// MaxExamples bounds the per-SKU detail returned.
	MaxExamples int
}

// SimulationResult is what the promotion would do.
type SimulationResult struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	// MatchedPairs is how many (store, SKU) pairs the rule matches.
	MatchedPairs int `json:"matched_pairs"`
	// MatchedSKUs and MatchedStores are the distinct counts.
	MatchedSKUs   int `json:"matched_skus"`
	MatchedStores int `json:"matched_stores"`
	// AppliedPairs is how many of the matched pairs the rule would actually
	// price, after the precedence policy. The gap between this and
	// MatchedPairs is the promotion being beaten by something already live, and
	// it is the number an operator most often does not expect.
	AppliedPairs int `json:"applied_pairs"`
	// SuppressedPairs is the difference, with the reasons.
	SuppressedPairs int            `json:"suppressed_pairs"`
	SuppressedBy    map[string]int `json:"suppressed_by,omitempty"`

	// AverageDiscountPct is the mean discount across applied pairs.
	AverageDiscountPct float64 `json:"average_discount_pct"`
	// DailyDiscountCostMinor is the projected cost per day: the discount times
	// the projected volume. It is the number the finance team asks for.
	DailyDiscountCostMinor float64 `json:"daily_discount_cost_minor"`
	// DailyMarginBeforeMinor and DailyMarginAfterMinor bracket the margin
	// effect, including any volume the elasticity projects.
	DailyMarginBeforeMinor float64 `json:"daily_margin_before_minor"`
	DailyMarginAfterMinor  float64 `json:"daily_margin_after_minor"`
	// ProjectedMarginChangePct is the margin effect as a percentage of today's
	// margin on the matched pairs, including whatever volume the supplied
	// elasticities project. It is negative for most promotions, which is the
	// point of showing it before activation.
	ProjectedMarginChangePct float64 `json:"projected_margin_change_pct"`
	// BelowCostPairs counts pairs the promotion would price below cost, which
	// is illegal in several of the platform's markets and is always worth an
	// operator's attention before activation rather than after.
	BelowCostPairs int `json:"below_cost_pairs"`
	// BelowCostExamples names a few of them.
	BelowCostExamples []string `json:"below_cost_examples,omitempty"`
	// Examples are a sample of the priced results.
	Examples []domain.PricedPromotion `json:"examples,omitempty"`
	// Warnings are the things an operator should read before activating.
	Warnings []string `json:"warnings,omitempty"`
}

// DefaultSimulationExamples is how many priced examples a simulation returns.
const DefaultSimulationExamples = 20

// Simulate evaluates a promotion against a catalogue without activating it.
//
// # Why the volume projection is deliberately conservative
//
// A simulation that assumes a promotion sells more will always make the
// promotion look better, and the assumption is doing all the work. This one
// projects volume only where the caller supplies an elasticity for the SKU, and
// otherwise reports the discount cost on today's volume with no uplift at all.
// The result is a number that is too pessimistic rather than one that is too
// optimistic, which is the right way round for a decision to spend margin.
func Simulate(in SimulationInput) (SimulationResult, error) {
	if err := in.Rule.Validate(); err != nil {
		return SimulationResult{}, err
	}
	if in.MaxExamples <= 0 {
		in.MaxExamples = DefaultSimulationExamples
	}
	matcher := domain.Compile(in.Rule)
	others := domain.CompileSet(in.Others)

	res := SimulationResult{PromotionID: in.Rule.ID, SuppressedBy: map[string]int{}}
	skus := map[canon.SKU]struct{}{}
	stores := map[canon.StoreID]struct{}{}
	var discountPctSum float64
	scratch := make([]domain.Rule, 0, 8)

	for _, p := range in.Catalogue {
		if !matcher.Matches(p) {
			continue
		}
		res.MatchedPairs++
		skus[p.SKU] = struct{}{}
		stores[p.StoreID] = struct{}{}

		// Resolve against the live set plus this rule, so the answer accounts
		// for whatever is already on the shelf.
		candidates := append(scratch[:0], others.Match(p, make([]domain.Rule, 0, 8))...)
		candidates = append(candidates, in.Rule)
		resolution, err := domain.Resolve(candidates, p)
		if err != nil {
			return SimulationResult{}, err
		}

		applied := false
		var mine domain.PricedPromotion
		for _, a := range resolution.Applied {
			if a.PromotionID == in.Rule.ID {
				applied, mine = true, a
				break
			}
		}
		if !applied {
			res.SuppressedPairs++
			for _, s := range resolution.Suppressed {
				if s.PromotionID == in.Rule.ID {
					res.SuppressedBy[s.Reason]++
				}
			}
			continue
		}
		res.AppliedPairs++

		if mine.BaseMinor > 0 {
			discountPctSum += 100 * float64(mine.DiscountMinor) / float64(mine.BaseMinor)
		}
		volume := p.Velocity
		elasticity, hasElasticity := in.ElasticityOf[p.SKU]
		projected := volume
		if hasElasticity && mine.BaseMinor > 0 && mine.PriceMinor > 0 && volume > 0 {
			projected = volume * powRatio(float64(mine.PriceMinor)/float64(mine.BaseMinor), elasticity)
		}

		res.DailyDiscountCostMinor += float64(mine.DiscountMinor) * projected
		res.DailyMarginBeforeMinor += float64(mine.BaseMinor-p.UnitCostMinor) * volume
		res.DailyMarginAfterMinor += float64(mine.PriceMinor-p.UnitCostMinor) * projected

		if p.UnitCostMinor > 0 && mine.PriceMinor < p.UnitCostMinor {
			res.BelowCostPairs++
			if len(res.BelowCostExamples) < 5 {
				res.BelowCostExamples = append(res.BelowCostExamples,
					fmt.Sprintf("%s/%s at %d against a cost of %d",
						p.StoreID, p.SKU, mine.PriceMinor, p.UnitCostMinor))
			}
		}
		if len(res.Examples) < in.MaxExamples {
			res.Examples = append(res.Examples, mine)
		}
	}

	res.MatchedSKUs = len(skus)
	res.MatchedStores = len(stores)
	if res.AppliedPairs > 0 {
		res.AverageDiscountPct = discountPctSum / float64(res.AppliedPairs)
	}
	if res.DailyMarginBeforeMinor != 0 {
		before, after := res.DailyMarginBeforeMinor, res.DailyMarginAfterMinor
		res.ProjectedMarginChangePct = 100 * (after - before) / before
	}

	if res.MatchedPairs == 0 {
		res.Warnings = append(res.Warnings,
			"this promotion matches nothing in the catalogue supplied; check the conditions")
	}
	if res.BelowCostPairs > 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("%d of %d applied pairs would price below cost, which is unlawful in several markets",
				res.BelowCostPairs, res.AppliedPairs))
	}
	if res.SuppressedPairs > 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("%d of %d matched pairs would be suppressed by promotions already live",
				res.SuppressedPairs, res.MatchedPairs))
	}
	if !in.Rule.Type.ShelfPriceable() {
		res.Warnings = append(res.Warnings,
			"this mechanic depends on the whole basket, so shelf labels will advertise it and keep showing the base price")
	}
	if len(in.ElasticityOf) == 0 {
		res.Warnings = append(res.Warnings,
			"no elasticity estimates were supplied, so the projection assumes no volume response: "+
				"the margin cost shown is the discount on today's volume and nothing else")
	}
	sort.Strings(res.Warnings)
	return res, nil
}

// powRatio raises a price ratio to an elasticity under the constant-elasticity
// demand model, guarding the degenerate inputs that would otherwise put a NaN
// inside a finance report.
func powRatio(ratio, elasticity float64) float64 {
	if ratio <= 0 {
		return 0
	}
	v := math.Pow(ratio, elasticity)
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}
