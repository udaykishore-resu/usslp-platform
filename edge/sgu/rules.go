package sgu

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// ---------------------------------------------------------------------------
// Tier-1 pricing guard rails
//
// These are the checks that must happen before a price can reach a label, in
// under ten milliseconds, whether or not anything upstream of the store is
// running. They are not the pricing engine — optimisation, elasticity
// modelling and competitor scraping all live in the cloud — they are the small
// set of rules whose violation is a legal or commercial incident rather than a
// missed opportunity:
//
//   - Minimum margin. Selling below cost is a decision a human makes
//     deliberately, on a specific product, with a reason. A pricing engine that
//     arrives at it by arithmetic is malfunctioning, and a store that displays
//     it has already lost the money.
//   - Regulatory floor and ceiling. Minimum unit pricing on alcohol in
//     Scotland, tobacco price controls, statutory maxima on regulated goods:
//     these are not policies, they are laws, and the penalty for breaching one
//     is not a lost margin point.
//   - Competitor parity limits. A price-match rule that undercuts by more than
//     its mandate, usually because a competitor feed was mis-scraped, empties a
//     shelf in an afternoon.
//
// They run locally because an autonomous store still has to enforce them. A WAN
// outage is precisely when a stale local schedule or a nervous store manager is
// most likely to produce a price nobody would have approved.
// ---------------------------------------------------------------------------

// ErrRuleViolation means a price was refused by the guard rails.
var ErrRuleViolation = errors.New("sgu: price violates a local pricing rule")

// RuleName identifies which guard rail refused a price. It appears in the
// rejection event and in the operator's dashboard, so it is the name a
// merchandiser would use.
type RuleName string

// The Tier-1 rules.
const (
	RuleMinimumMargin    RuleName = "minimum_margin"
	RuleRegulatoryFloor  RuleName = "regulatory_floor"
	RuleRegulatoryCeil   RuleName = "regulatory_ceiling"
	RuleCompetitorParity RuleName = "competitor_parity"
	RuleCurrency         RuleName = "currency_mismatch"
)

// ProductRules is the per-product guard-rail configuration, replicated from the
// cloud and held locally so it survives an outage.
type ProductRules struct {
	SKU      canon.SKU `json:"sku"`
	Currency string    `json:"currency"`
	// CostMinor is the landed cost in minor units. Zero disables the margin rule
	// for this product, which is correct for products whose cost the store does
	// not know rather than a silent pass.
	CostMinor int64 `json:"cost_minor"`
	// MinMarginBps is the minimum gross margin in basis points of the selling
	// price. Basis points rather than a float because this arithmetic has to be
	// reproducible byte for byte on a gateway and in the cloud.
	MinMarginBps int64 `json:"min_margin_bps"`
	// FloorMinor and CeilingMinor are statutory bounds. Zero means no bound.
	FloorMinor   int64 `json:"floor_minor"`
	CeilingMinor int64 `json:"ceiling_minor"`
	// CompetitorMinor is the last known competitor price, and the parity bounds
	// are how far either side of it this store is allowed to go. Zero disables
	// the rule.
	CompetitorMinor int64     `json:"competitor_minor"`
	MaxUndercutBps  int64     `json:"max_undercut_bps"`
	MaxPremiumBps   int64     `json:"max_premium_bps"`
	CompetitorAsOf  time.Time `json:"competitor_as_of"`
}

// Violation is one broken rule.
type Violation struct {
	Rule    RuleName `json:"rule"`
	Detail  string   `json:"detail"`
	Allowed int64    `json:"allowed_minor,omitempty"`
	Got     int64    `json:"got_minor"`
}

// Verdict is the outcome of evaluating one price.
type Verdict struct {
	Allowed    bool        `json:"allowed"`
	Violations []Violation `json:"violations,omitempty"`
	// Elapsed is how long evaluation took. It is measured and reported because
	// the platform commits to under ten milliseconds and a commitment nobody
	// measures is a wish.
	Elapsed time.Duration `json:"elapsed"`
	// Evaluated is false when no rules are configured for the product, which is
	// not the same as passing and should not be reported as though it were.
	Evaluated bool `json:"evaluated"`
}

// Error renders the verdict as a rejection reason.
func (v Verdict) Error() string {
	if v.Allowed {
		return ""
	}
	parts := make([]string, 0, len(v.Violations))
	for _, x := range v.Violations {
		parts = append(parts, string(x.Rule)+": "+x.Detail)
	}
	return fmt.Sprintf("%v", parts)
}

// RulesEngine evaluates the Tier-1 guard rails.
//
// Rules are held in a map in memory and mirrored to the durable store, so
// evaluation is a hash lookup and integer arithmetic with no allocation and no
// I/O. That is what makes the ten-millisecond budget comfortable rather than
// tight: the measured cost is in the hundreds of nanoseconds, and the budget's
// real purpose is to forbid an implementation that consults the cloud.
type RulesEngine struct {
	mu    sync.RWMutex
	rules map[canon.SKU]ProductRules
	store *kvstore.Store
	// evaluations and rejections are counters for the diagnostics page.
	evaluations uint64
	rejections  uint64
	slowest     time.Duration
}

const rulesPrefix = "rules/"

// NewRulesEngine builds an engine, restoring any rules a previous process
// persisted.
func NewRulesEngine(store *kvstore.Store) (*RulesEngine, error) {
	e := &RulesEngine{rules: map[canon.SKU]ProductRules{}, store: store}
	if store == nil {
		return e, nil
	}
	it := store.Scan([]byte(rulesPrefix))
	defer it.Close()
	for it.Next() {
		var r ProductRules
		if err := json.Unmarshal(it.Value(), &r); err != nil {
			continue
		}
		e.rules[r.SKU] = r
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("sgu: restoring pricing rules: %w", err)
	}
	return e, nil
}

// Set installs or replaces the rules for a product, persisting them so they
// survive a restart during an outage.
func (e *RulesEngine) Set(r ProductRules) error {
	if r.SKU == "" {
		return errors.New("sgu: pricing rules need a SKU")
	}
	e.mu.Lock()
	e.rules[r.SKU] = r
	e.mu.Unlock()
	if e.store == nil {
		return nil
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("sgu: encoding rules for %s: %w", r.SKU, err)
	}
	if err := e.store.Put([]byte(rulesPrefix+string(r.SKU)), body); err != nil {
		return fmt.Errorf("sgu: persisting rules for %s: %w", r.SKU, err)
	}
	return nil
}

// Rules returns the configured rules for a product.
func (e *RulesEngine) Rules(sku canon.SKU) (ProductRules, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.rules[sku]
	return r, ok
}

// All returns every configured rule set, sorted, for the diagnostics page.
func (e *RulesEngine) All() []ProductRules {
	e.mu.RLock()
	out := make([]ProductRules, 0, len(e.rules))
	for _, r := range e.rules {
		out = append(out, r)
	}
	e.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out
}

// Evaluate checks a price against the guard rails.
func (e *RulesEngine) Evaluate(sku canon.SKU, price canon.Money) Verdict {
	start := time.Now()
	e.mu.RLock()
	r, ok := e.rules[sku]
	e.mu.RUnlock()

	v := Verdict{Allowed: true, Evaluated: ok}
	if !ok {
		v.Elapsed = time.Since(start)
		e.record(v)
		return v
	}

	add := func(rule RuleName, detail string, allowed, got int64) {
		v.Allowed = false
		v.Violations = append(v.Violations, Violation{Rule: rule, Detail: detail, Allowed: allowed, Got: got})
	}

	if r.Currency != "" && r.Currency != price.Currency {
		add(RuleCurrency, fmt.Sprintf("rules are denominated in %s, the price is in %s", r.Currency, price.Currency),
			0, price.Amount)
		v.Elapsed = time.Since(start)
		e.record(v)
		return v
	}

	if r.FloorMinor > 0 && price.Amount < r.FloorMinor {
		add(RuleRegulatoryFloor,
			fmt.Sprintf("statutory minimum is %s", canon.Money{Amount: r.FloorMinor, Currency: price.Currency}.Display()),
			r.FloorMinor, price.Amount)
	}
	if r.CeilingMinor > 0 && price.Amount > r.CeilingMinor {
		add(RuleRegulatoryCeil,
			fmt.Sprintf("statutory maximum is %s", canon.Money{Amount: r.CeilingMinor, Currency: price.Currency}.Display()),
			r.CeilingMinor, price.Amount)
	}
	if r.CostMinor > 0 && r.MinMarginBps > 0 {
		// Margin as a share of the selling price, which is how retail states it:
		// price * (10000 - minMarginBps) >= cost * 10000, kept in integers so the
		// gateway and the cloud agree to the penny.
		minPrice := ceilDiv(r.CostMinor*10000, 10000-r.MinMarginBps)
		if price.Amount < minPrice {
			add(RuleMinimumMargin,
				fmt.Sprintf("cost is %s and the floor for %.2f%% margin is %s",
					canon.Money{Amount: r.CostMinor, Currency: price.Currency}.Display(),
					float64(r.MinMarginBps)/100,
					canon.Money{Amount: minPrice, Currency: price.Currency}.Display()),
				minPrice, price.Amount)
		}
	}
	if r.CompetitorMinor > 0 {
		if r.MaxUndercutBps > 0 {
			floor := r.CompetitorMinor - r.CompetitorMinor*r.MaxUndercutBps/10000
			if price.Amount < floor {
				add(RuleCompetitorParity,
					fmt.Sprintf("undercuts the competitor price of %s by more than %.2f%%",
						canon.Money{Amount: r.CompetitorMinor, Currency: price.Currency}.Display(),
						float64(r.MaxUndercutBps)/100),
					floor, price.Amount)
			}
		}
		if r.MaxPremiumBps > 0 {
			ceiling := r.CompetitorMinor + r.CompetitorMinor*r.MaxPremiumBps/10000
			if price.Amount > ceiling {
				add(RuleCompetitorParity,
					fmt.Sprintf("exceeds the competitor price of %s by more than %.2f%%",
						canon.Money{Amount: r.CompetitorMinor, Currency: price.Currency}.Display(),
						float64(r.MaxPremiumBps)/100),
					ceiling, price.Amount)
			}
		}
	}

	v.Elapsed = time.Since(start)
	e.record(v)
	return v
}

func (e *RulesEngine) record(v Verdict) {
	e.mu.Lock()
	e.evaluations++
	if !v.Allowed {
		e.rejections++
	}
	if v.Elapsed > e.slowest {
		e.slowest = v.Elapsed
	}
	e.mu.Unlock()
}

// RulesStats is what the diagnostics page shows about the guard rails.
type RulesStats struct {
	Products    int           `json:"products"`
	Evaluations uint64        `json:"evaluations"`
	Rejections  uint64        `json:"rejections"`
	Slowest     time.Duration `json:"slowest_evaluation"`
	Budget      time.Duration `json:"budget"`
}

// EvaluationBudget is the platform's commitment for a Tier-1 rule check.
const EvaluationBudget = 10 * time.Millisecond

// Stats returns the engine's counters.
func (e *RulesEngine) Stats() RulesStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return RulesStats{Products: len(e.rules), Evaluations: e.evaluations,
		Rejections: e.rejections, Slowest: e.slowest, Budget: EvaluationBudget}
}

// ceilDiv divides rounding away from zero, so a margin floor is never a penny
// below what the rule requires.
func ceilDiv(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	q := num / den
	if num%den != 0 {
		q++
	}
	return q
}
