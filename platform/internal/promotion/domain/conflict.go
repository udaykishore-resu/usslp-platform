package domain

import (
	"fmt"
	"sort"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Resolution is the outcome of applying a set of matching promotions to one
// product.
type Resolution struct {
	Product Product `json:"product"`
	// Applied are the promotions that actually price the shelf, in the order
	// they were applied.
	Applied []PricedPromotion `json:"applied"`
	// Suppressed are the promotions that matched but did not apply, each with
	// the reason. An operator's first question when a promotion does not show
	// on a shelf is "why", and this is the answer.
	Suppressed []Suppression `json:"suppressed,omitempty"`
	// FinalPriceMinor is the shelf price after every applied promotion.
	FinalPriceMinor int64 `json:"final_price_minor"`
	// TotalDiscountMinor is the base price minus the final price.
	TotalDiscountMinor int64 `json:"total_discount_minor"`
	// Winner is the promotion that owns the label's badge and colour. With
	// stacked promotions several apply but only one can drive the display, and
	// it is the highest-priority applied one.
	Winner canon.PromotionID `json:"winner,omitempty"`
	// Display is the render spec the label should use.
	Display canon.RenderSpec `json:"display"`
}

// Suppression records a promotion that matched but did not apply.
type Suppression struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	Reason      string            `json:"reason"`
	// BeatenBy names the promotion that won, where one did.
	BeatenBy canon.PromotionID `json:"beaten_by,omitempty"`
}

// # Precedence policy
//
// When several promotions match one product, the platform applies this order,
// and it is a documented contract rather than an implementation detail because
// a retailer's finance team will reconcile against it:
//
//  1. **Priority, descending.** An operator who has thought about a clash sets
//     a priority, and nothing else overrides that judgement.
//  2. **Best for the customer.** Between equal priorities, the promotion that
//     produces the lower shelf price wins. This is the rule that keeps the
//     platform out of trouble: a customer who sees two offers and gets the
//     worse one has a complaint that is usually also a regulatory one, whereas
//     giving the better one is at worst a margin decision.
//  3. **Most specific.** Between equal priorities and equal prices, the
//     promotion with the narrower reach wins — an explicit SKU list beats a
//     brand, a brand beats a category, a category beats "everything". A
//     specific rule was authored deliberately; a broad one is a backdrop.
//  4. **Promotion identifier, ascending.** A final, arbitrary but *stable*
//     tie-break. It exists so that two nodes evaluating the same inputs reach
//     the same answer, which map iteration order would otherwise deny them.
//
// Stacking is orthogonal to all of it. The winner is chosen by the order above;
// then, while the current winner is stackable, the next promotion in the order
// is applied on top of the price the previous one produced. A non-stackable
// promotion ends the chain, whether it is first or fifth. Two promotions in the
// same exclusive group never both apply, however stackable they are.

// Resolve applies a set of matching rules to a product under the precedence
// policy documented above.
func Resolve(rules []Rule, p Product) (Resolution, error) {
	res := Resolution{Product: p, FinalPriceMinor: p.BasePriceMinor}
	if len(rules) == 0 {
		return res, nil
	}

	// Price every candidate against the *base* price first, because the
	// best-for-customer comparison must rank them on equal terms. Ranking them
	// on already-stacked prices would make the order depend on itself.
	type candidate struct {
		rule  Rule
		price PricedPromotion
	}
	cands := make([]candidate, 0, len(rules))
	for _, r := range rules {
		pp, err := Apply(r, p)
		if err != nil {
			return Resolution{}, err
		}
		cands = append(cands, candidate{rule: r, price: pp})
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.rule.Priority != b.rule.Priority {
			return a.rule.Priority > b.rule.Priority
		}
		if a.price.PriceMinor != b.price.PriceMinor {
			return a.price.PriceMinor < b.price.PriceMinor
		}
		if as, bs := Specificity(a.rule), Specificity(b.rule); as != bs {
			return as > bs
		}
		return a.rule.ID < b.rule.ID
	})

	current := p.BasePriceMinor
	usedGroups := map[string]canon.PromotionID{}
	var winner *Rule
	chainOpen := true

	for i := range cands {
		c := cands[i]
		if !chainOpen {
			res.Suppressed = append(res.Suppressed, Suppression{
				PromotionID: c.rule.ID,
				Reason:      "a higher-precedence non-stackable promotion already applies",
				BeatenBy:    winnerID(winner),
			})
			continue
		}
		if g := c.rule.ExclusiveGroup; g != "" {
			if holder, taken := usedGroups[g]; taken {
				res.Suppressed = append(res.Suppressed, Suppression{
					PromotionID: c.rule.ID,
					Reason:      fmt.Sprintf("exclusive group %q is already held", g),
					BeatenBy:    holder,
				})
				continue
			}
		}

		// Re-price against the running price so that a stacked percentage
		// applies to what is actually on the shelf, not to the base. Applying
		// every stacked discount to the base instead would make "20% off then
		// 10% off" a 30% discount, which is neither what a retailer means nor
		// what a till computes.
		stepProduct := p
		stepProduct.BasePriceMinor = current
		stepped, err := Apply(c.rule, stepProduct)
		if err != nil {
			return Resolution{}, err
		}
		// The reported base is the true base for the first promotion applied
		// and the running price thereafter, which is what makes the applied
		// list read as a sequence.
		if stepped.ShelfPriced {
			current = stepped.PriceMinor
		}
		res.Applied = append(res.Applied, stepped)
		if g := c.rule.ExclusiveGroup; g != "" {
			usedGroups[g] = c.rule.ID
		}
		if winner == nil {
			r := c.rule
			winner = &r
		}
		if !c.rule.Stackable {
			chainOpen = false
		}
	}

	res.FinalPriceMinor = current
	res.TotalDiscountMinor = p.BasePriceMinor - current
	if winner != nil {
		res.Winner = winner.ID
		res.Display = winner.RenderSpec(res.TotalDiscountMinor > 0)
	}
	return res, nil
}

func winnerID(r *Rule) canon.PromotionID {
	if r == nil {
		return ""
	}
	return r.ID
}

// Specificity scores how narrowly a rule is targeted. Higher is narrower.
//
// The weights encode the ordering an operator expects rather than a measurement
// of reach: an explicit SKU list is the most deliberate statement a rule can
// make, a store list is next, then brand, then category, then a store group,
// and the numeric conditions are the weakest signal because they usually
// accompany a broader rule rather than defining it. A rule with no conditions
// at all scores zero and always loses this tie-break, which is the desired
// treatment for a chain-wide backdrop promotion.
func Specificity(r Rule) int {
	score := 0
	if len(r.Conditions.IncludeSKUs) > 0 {
		score += 1000
	}
	if len(r.Conditions.Stores) > 0 {
		score += 200
	}
	if len(r.Conditions.Brands) > 0 {
		score += 100
	}
	if len(r.Conditions.Categories) > 0 {
		score += 50
	}
	if len(r.Conditions.StoreGroups) > 0 {
		score += 25
	}
	if len(r.Conditions.CustomerSegments) > 0 {
		score += 20
	}
	if r.Conditions.MinInventory > 0 {
		score += 5
	}
	if r.Conditions.MaxDaysToExpiry != nil {
		score += 5
	}
	if r.Conditions.MinPriceMinor > 0 || r.Conditions.MaxPriceMinor > 0 {
		score += 5
	}
	return score
}

// Conflict is a pair of promotions that can match the same product.
type Conflict struct {
	A canon.PromotionID `json:"promotion_a"`
	B canon.PromotionID `json:"promotion_b"`
	// Severity is how much an operator should care.
	Severity Severity `json:"severity"`
	// Reason explains the overlap in the operator's own terms.
	Reason string `json:"reason"`
	// SampleSKUs are up to a handful of products both rules match, so the
	// operator can look at a real example rather than reasoning about set
	// algebra.
	SampleSKUs []canon.SKU `json:"sample_skus,omitempty"`
	// Overlap is how many of the examined products both rules match.
	Overlap int `json:"overlap"`
	// Resolution says what the precedence policy would do, so the operator can
	// see whether the conflict is already handled.
	Resolution string `json:"resolution"`
}

// Severity grades a conflict.
type Severity string

// The severities.
const (
	// SeverityInfo is an overlap the precedence policy resolves cleanly and
	// deliberately: different priorities, or both stackable.
	SeverityInfo Severity = "info"
	// SeverityWarn is an overlap resolved by a tie-break the operator probably
	// did not intend — equal priorities decided by price or specificity.
	SeverityWarn Severity = "warn"
	// SeverityError is an overlap resolved by the arbitrary identifier
	// tie-break, meaning nothing about the two rules distinguishes them. Which
	// promotion a customer sees is then decided by a UUID, which is never what
	// anybody wanted.
	SeverityError Severity = "error"
)

// DetectConflicts finds pairs of rules that can match the same product,
// evaluated against a sample of the catalogue.
//
// # Why a sample rather than set algebra
//
// Deciding whether two condition sets can intersect is answerable in the
// abstract, but the answer is almost always "yes, in principle" — two category
// rules overlap if any product is in both categories, which depends on the
// catalogue, not on the rules. Evaluating against real products gives an
// operator something they can act on: not "these rules might overlap" but
// "these 340 SKUs match both, here are five of them".
func DetectConflicts(rules []Rule, catalogue []Product) []Conflict {
	if len(rules) < 2 {
		return nil
	}
	matchers := make([]*Matcher, len(rules))
	for i, r := range rules {
		matchers[i] = Compile(r)
	}
	// Which products each rule matches, as a list of catalogue indices.
	hits := make([][]int, len(rules))
	for i, m := range matchers {
		for j := range catalogue {
			if m.Matches(catalogue[j]) {
				hits[i] = append(hits[i], j)
			}
		}
	}

	var out []Conflict
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			shared := intersect(hits[i], hits[j])
			if len(shared) == 0 {
				continue
			}
			c := Conflict{A: rules[i].ID, B: rules[j].ID, Overlap: len(shared)}
			for _, idx := range shared {
				if len(c.SampleSKUs) >= 5 {
					break
				}
				c.SampleSKUs = append(c.SampleSKUs, catalogue[idx].SKU)
			}
			c.Severity, c.Reason, c.Resolution = grade(rules[i], rules[j], catalogue[shared[0]])
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return severityRank(out[i].Severity) > severityRank(out[j].Severity)
		}
		if out[i].Overlap != out[j].Overlap {
			return out[i].Overlap > out[j].Overlap
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 3
	case SeverityWarn:
		return 2
	default:
		return 1
	}
}

// grade decides how serious an overlap is, and what the policy will do about it.
func grade(a, b Rule, sample Product) (Severity, string, string) {
	if a.Stackable && b.Stackable {
		if a.ExclusiveGroup != "" && a.ExclusiveGroup == b.ExclusiveGroup {
			return SeverityWarn,
				fmt.Sprintf("both promotions are stackable but share exclusive group %q, so only one will apply",
					a.ExclusiveGroup),
				fmt.Sprintf("%s applies first by precedence; %s is suppressed", higher(a, b), lower(a, b))
		}
		return SeverityInfo,
			"both promotions are stackable, so both will apply and the discounts compound",
			fmt.Sprintf("%s applies first, then %s on the reduced price", higher(a, b), lower(a, b))
	}
	if a.Priority != b.Priority {
		return SeverityInfo,
			fmt.Sprintf("the promotions overlap but have different priorities (%d and %d)", a.Priority, b.Priority),
			fmt.Sprintf("%s wins on priority", higher(a, b))
	}

	pa, errA := Apply(a, sample)
	pb, errB := Apply(b, sample)
	if errA == nil && errB == nil && pa.PriceMinor != pb.PriceMinor {
		return SeverityWarn,
			fmt.Sprintf("the promotions overlap at the same priority %d and produce different prices "+
				"(%d and %d on %s)", a.Priority, pa.PriceMinor, pb.PriceMinor, sample.SKU),
			"the lower price wins on the best-for-customer rule; set an explicit priority if that is not what you want"
	}
	if sa, sb := Specificity(a), Specificity(b); sa != sb {
		return SeverityWarn,
			fmt.Sprintf("the promotions overlap at the same priority %d and the same price, "+
				"so the more specific one wins", a.Priority),
			fmt.Sprintf("%s wins on specificity (%d against %d)", moreSpecific(a, b), max(sa, sb), min(sa, sb))
	}
	return SeverityError,
		fmt.Sprintf("the promotions overlap at priority %d with the same price and the same specificity: "+
			"nothing distinguishes them", a.Priority),
		"the winner is decided by promotion identifier, which is arbitrary; set a priority before this activates"
}

func higher(a, b Rule) canon.PromotionID {
	if a.Priority >= b.Priority {
		return a.ID
	}
	return b.ID
}

func lower(a, b Rule) canon.PromotionID {
	if a.Priority >= b.Priority {
		return b.ID
	}
	return a.ID
}

func moreSpecific(a, b Rule) canon.PromotionID {
	if Specificity(a) >= Specificity(b) {
		return a.ID
	}
	return b.ID
}

// intersect returns the common members of two ascending index lists.
func intersect(a, b []int) []int {
	var out []int
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}
