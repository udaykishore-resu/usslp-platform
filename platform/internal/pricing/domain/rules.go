// Package domain is the Tier-1 pricing rules engine: the hard, deterministic
// constraints that every price in USSLP must satisfy before it can reach a
// shelf.
//
// # Why this package has no dependencies
//
// Tier 1 runs in three places: in the cloud pricing service, inside the Store
// Gateway Unit's offline brain, and — via the compact policy pack in
// policypack.go — as the SGU's embedded rule table when the WAN is down. It
// therefore imports nothing but the shared kernel (canon) and the standard
// library, allocates nothing on the evaluation path beyond a single small
// violation slice, and contains no clocks, no I/O and no randomness. Given the
// same Constraints and the same requested price it produces the same Decision
// on every tier, forever. That property is not a nicety: a store operating
// autonomously during an outage must reach exactly the decision the cloud would
// have reached, or reconciliation after the outage rewrites prices customers
// already saw.
//
// # Why infeasibility is a first-class result
//
// Constraints in retail pricing routinely conflict. A regulatory floor above a
// competitor-parity ceiling, or a minimum margin above a maximum-change bound,
// is a data problem in the merchandising system, not a pricing problem. The
// engine reports it as such — an infeasible decision naming every binding
// constraint — rather than silently picking whichever bound it happened to
// apply last. Picking one arbitrarily is how a retailer ends up violating a
// price-marking regulation because an unrelated margin rule was edited.
package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ConstraintKind names one hard rule. The names are a contract: they appear in
// operator-facing API responses, in the audit log, and in the SGU's offline
// decision records, so they are only ever added to.
type ConstraintKind string

// The Tier-1 constraint vocabulary.
const (
	// KindMinMargin is the minimum gross margin over unit cost.
	KindMinMargin ConstraintKind = "min_margin"
	// KindRegulatoryFloor is a statutory or contractual minimum price, such as
	// a below-cost-selling prohibition or a minimum advertised price.
	KindRegulatoryFloor ConstraintKind = "regulatory_floor"
	// KindRegulatoryCeiling is a statutory maximum, such as a price freeze on
	// staples or a promised promotional ceiling.
	KindRegulatoryCeiling ConstraintKind = "regulatory_ceiling"
	// KindCompetitorParity keeps the price within a band around a tracked
	// competitor's price.
	KindCompetitorParity ConstraintKind = "competitor_parity"
	// KindMaxChange bounds how far one decision may move the shelf price.
	KindMaxChange ConstraintKind = "max_change"
	// KindChangeFrequency bounds how many times a price may move per period.
	// When the budget is exhausted the only feasible price is the current one.
	KindChangeFrequency ConstraintKind = "change_frequency"
	// KindEndingPolicy is the psychological / round-number ending rule. Unlike
	// the others it is a preference: it snaps a price inside the feasible range
	// and is waived, with a recorded adjustment, when no compliant ending
	// exists in range.
	KindEndingPolicy ConstraintKind = "ending_policy"
	// KindCurrency is the sanity check that the requested price, the cost and
	// every bound are denominated alike.
	KindCurrency ConstraintKind = "currency"
)

// Ending is a price-ending policy. Retailers care about this for real reasons —
// a .99 ending measurably outsells a .00 ending on impulse lines — and the
// platform must produce endings that are stable across tiers, so the policy is
// an enumerated rule rather than free-form rounding.
type Ending uint8

// The supported endings.
const (
	// EndingAny imposes no ending rule.
	EndingAny Ending = iota
	// EndingCharm requires the price to end in .99 of the major unit.
	EndingCharm
	// EndingCharm95 requires the price to end in .95.
	EndingCharm95
	// EndingWhole requires a whole major unit (.00).
	EndingWhole
	// EndingNickel requires a multiple of five minor units, the rule in markets
	// that have withdrawn their one-cent coin.
	EndingNickel
)

// String renders the ending for API responses and audit records.
func (e Ending) String() string {
	switch e {
	case EndingCharm:
		return "charm_99"
	case EndingCharm95:
		return "charm_95"
	case EndingWhole:
		return "whole"
	case EndingNickel:
		return "nickel"
	default:
		return "any"
	}
}

// ParseEnding maps the API spelling back to an Ending.
func ParseEnding(s string) (Ending, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "any", "none":
		return EndingAny, nil
	case "charm_99", "charm", "99":
		return EndingCharm, nil
	case "charm_95", "95":
		return EndingCharm95, nil
	case "whole", "round":
		return EndingWhole, nil
	case "nickel", "five":
		return EndingNickel, nil
	}
	return EndingAny, fmt.Errorf("pricing: unknown ending policy %q", s)
}

// ErrInvalidConstraints marks a Constraints value that cannot be evaluated at
// all, as distinct from one that is merely infeasible.
var ErrInvalidConstraints = errors.New("pricing: invalid constraints")

// Constraints is the complete Tier-1 rule set for one (store, SKU).
//
// Every monetary field is in the same currency, in minor units, because the
// engine compares them with integer arithmetic. A mismatch is an error rather
// than a conversion: an edge device has no exchange-rate feed and must never
// invent one.
type Constraints struct {
	// Currency is the ISO 4217 code all amounts are denominated in.
	Currency string `json:"currency"`
	// UnitCost is the landed cost used for the margin test. Zero disables the
	// margin constraint, which is correct for a SKU whose cost the platform has
	// not been given — refusing to price it would be worse than not enforcing a
	// margin the retailer never configured.
	UnitCost int64 `json:"unit_cost"`
	// MinMarginBps is the minimum gross margin in basis points of the selling
	// price: price >= cost / (1 - margin). Expressed against price rather than
	// cost because that is how retail gross margin is defined and audited.
	MinMarginBps int32 `json:"min_margin_bps"`
	// FloorMinor and CeilingMinor are absolute regulatory bounds. Zero means
	// unset for the floor; zero means unset for the ceiling too, since a
	// ceiling of zero would forbid every price and is always a data error.
	FloorMinor   int64 `json:"floor_minor"`
	CeilingMinor int64 `json:"ceiling_minor"`
	// CompetitorMinor is the tracked competitor price; zero means untracked.
	CompetitorMinor int64 `json:"competitor_minor"`
	// CompetitorBandBps is the permitted deviation either side of the
	// competitor price, in basis points. Zero with a tracked competitor means
	// exact parity, which is a legitimate and occasionally contractual policy.
	CompetitorBandBps int32 `json:"competitor_band_bps"`
	// CurrentMinor is the price presently on the shelf. It anchors the
	// max-change and change-frequency rules.
	CurrentMinor int64 `json:"current_minor"`
	// MaxChangeBps bounds one decision's movement as basis points of the
	// current price. Zero means unbounded. This is the rule that stops a
	// mis-scaled model output from repricing a store by an order of magnitude
	// before a human notices.
	MaxChangeBps int32 `json:"max_change_bps"`
	// MaxChangesPerPeriod and ChangesThisPeriod implement the change-frequency
	// budget. Zero MaxChangesPerPeriod means unlimited. The period itself is
	// the caller's business — the engine only compares the two counters — which
	// keeps the clock out of the rules engine.
	MaxChangesPerPeriod int32 `json:"max_changes_per_period"`
	ChangesThisPeriod   int32 `json:"changes_this_period"`
	// Ending is the price-ending policy applied inside the feasible range.
	Ending Ending `json:"ending"`
	// GranularityMinor forces prices onto a lattice of this many minor units
	// (10 for "whole dimes"). Zero or one means every minor unit is allowed.
	// It is applied before the ending policy; where both are set the ending
	// policy wins, because a .99 ending is itself a granularity statement.
	GranularityMinor int64 `json:"granularity_minor"`
}

// Violation records one constraint that rejected or moved a price.
type Violation struct {
	Kind ConstraintKind `json:"kind"`
	// Detail is human-readable and safe to show an operator.
	Detail string `json:"detail"`
	// BoundMinor is the bound the constraint imposes, where it has one.
	BoundMinor int64 `json:"bound_minor,omitempty"`
}

// Outcome is the disposition of one evaluation.
type Outcome uint8

// The possible outcomes.
const (
	// OutcomeAccepted means the requested price satisfied every rule as-is.
	OutcomeAccepted Outcome = iota
	// OutcomeAdjusted means a compliant price was found near the request. The
	// Decision's Price is that price, and Violations names what moved it.
	OutcomeAdjusted
	// OutcomeInfeasible means the constraints have no solution at all. No price
	// is returned, and Violations names every binding constraint so an operator
	// can see which pair conflicts.
	OutcomeInfeasible
	// OutcomeInvalid means the Constraints themselves are unusable — mismatched
	// currencies, a negative bound. Distinguished from infeasible because the
	// remedy is different: infeasible is a merchandising conflict, invalid is a
	// bug in whatever produced the constraints.
	OutcomeInvalid
)

// String renders the outcome for API responses.
func (o Outcome) String() string {
	switch o {
	case OutcomeAccepted:
		return "accepted"
	case OutcomeAdjusted:
		return "adjusted"
	case OutcomeInfeasible:
		return "infeasible"
	default:
		return "invalid"
	}
}

// Decision is the result of a Tier-1 evaluation.
type Decision struct {
	Outcome Outcome `json:"outcome"`
	// Price is the compliant price. Meaningful only when Outcome is accepted or
	// adjusted.
	Price canon.Money `json:"price"`
	// RequestedMinor echoes the price that was evaluated.
	RequestedMinor int64 `json:"requested_minor"`
	// Feasible is the intersection of every hard bound, before the ending
	// policy. Callers that optimise (Tier 2) search inside it.
	Feasible Range `json:"feasible"`
	// Violations lists every constraint that rejected or moved the price, in a
	// stable order so that two tiers produce byte-identical audit records.
	Violations []Violation `json:"violations,omitempty"`
}

// Range is an inclusive interval of prices in minor units.
type Range struct {
	LowMinor  int64 `json:"low_minor"`
	HighMinor int64 `json:"high_minor"`
	// Empty is true when the hard constraints conflict. It is carried
	// explicitly rather than inferred from Low > High so that a decoded
	// Decision cannot be misread.
	Empty bool `json:"empty"`
	// LowKind and HighKind name the constraints that produced each bound. This
	// is what turns "infeasible" into an actionable message.
	LowKind  ConstraintKind `json:"low_kind,omitempty"`
	HighKind ConstraintKind `json:"high_kind,omitempty"`
}

// maxViolations bounds the violation slice so that one evaluation performs at
// most one allocation of a known size. Tier 1 is on the hot path of a 52,000
// updates-per-second pipeline with a sub-10-millisecond budget; a decision that
// grows a slice is a decision that eventually triggers a GC pause inside the
// budget.
const maxViolations = 8

// Validate reports whether the constraints are internally coherent enough to
// evaluate. It does not check feasibility — that is Evaluate's job and its
// answer is a Decision, not an error.
func (c Constraints) Validate() error {
	if len(c.Currency) != 3 {
		return fmt.Errorf("%w: currency %q is not an ISO 4217 code", ErrInvalidConstraints, c.Currency)
	}
	switch {
	case c.UnitCost < 0:
		return fmt.Errorf("%w: negative unit cost", ErrInvalidConstraints)
	case c.MinMarginBps < 0 || c.MinMarginBps >= 10000:
		return fmt.Errorf("%w: min margin %d bps is outside (0, 10000)", ErrInvalidConstraints, c.MinMarginBps)
	case c.FloorMinor < 0 || c.CeilingMinor < 0 || c.CompetitorMinor < 0 || c.CurrentMinor < 0:
		return fmt.Errorf("%w: negative price bound", ErrInvalidConstraints)
	case c.MaxChangeBps < 0:
		return fmt.Errorf("%w: negative max change", ErrInvalidConstraints)
	case c.CompetitorBandBps < 0:
		return fmt.Errorf("%w: negative competitor band", ErrInvalidConstraints)
	case c.MaxChangesPerPeriod < 0 || c.ChangesThisPeriod < 0:
		return fmt.Errorf("%w: negative change counter", ErrInvalidConstraints)
	case c.GranularityMinor < 0:
		return fmt.Errorf("%w: negative granularity", ErrInvalidConstraints)
	}
	return nil
}

// Feasible computes the intersection of every hard bound.
//
// It is exported because Tier 2's optimiser needs the search space, and because
// an operator UI wants to show "this SKU can be priced between $1.79 and $2.49"
// without proposing a price first.
func (c Constraints) Feasible() Range {
	lo, hi := int64(0), int64(maxPriceMinor)
	loKind, hiKind := ConstraintKind(""), ConstraintKind("")

	raise := func(bound int64, kind ConstraintKind) {
		if bound > lo {
			lo, loKind = bound, kind
		}
	}
	lower := func(bound int64, kind ConstraintKind) {
		if bound < hi {
			hi, hiKind = bound, kind
		}
	}

	// Minimum margin. Gross margin is (price - cost) / price, so the binding
	// price is cost / (1 - m). Computed in integer arithmetic and rounded *up*,
	// because rounding down would produce a price one minor unit below the
	// margin the retailer signed off on.
	if c.UnitCost > 0 && c.MinMarginBps > 0 {
		den := int64(10000 - c.MinMarginBps)
		raise(ceilDiv(c.UnitCost*10000, den), KindMinMargin)
	} else if c.UnitCost > 0 && c.MinMarginBps == 0 {
		// A configured cost with a zero margin still forbids selling below
		// cost, which is the default legal position in most target markets.
		raise(c.UnitCost, KindMinMargin)
	}

	if c.FloorMinor > 0 {
		raise(c.FloorMinor, KindRegulatoryFloor)
	}
	if c.CeilingMinor > 0 {
		lower(c.CeilingMinor, KindRegulatoryCeiling)
	}

	// Competitor parity. A zero band means exact parity.
	if c.CompetitorMinor > 0 {
		delta := mulBps(c.CompetitorMinor, int64(c.CompetitorBandBps))
		raise(c.CompetitorMinor-delta, KindCompetitorParity)
		lower(c.CompetitorMinor+delta, KindCompetitorParity)
	}

	// Maximum movement from the shelf price.
	if c.CurrentMinor > 0 && c.MaxChangeBps > 0 {
		delta := mulBps(c.CurrentMinor, int64(c.MaxChangeBps))
		raise(c.CurrentMinor-delta, KindMaxChange)
		lower(c.CurrentMinor+delta, KindMaxChange)
	}

	// Change-frequency budget. When it is exhausted the feasible set collapses
	// to the single price already on the shelf: not "no price", because leaving
	// the shelf alone is always legal, and a caller re-asserting the current
	// price must succeed rather than be told the SKU is infeasible.
	if c.MaxChangesPerPeriod > 0 && c.ChangesThisPeriod >= c.MaxChangesPerPeriod {
		raise(c.CurrentMinor, KindChangeFrequency)
		lower(c.CurrentMinor, KindChangeFrequency)
	}

	if lo < 0 {
		lo = 0
	}
	return Range{LowMinor: lo, HighMinor: hi, Empty: lo > hi, LowKind: loKind, HighKind: hiKind}
}

// maxPriceMinor is the open upper bound used when no ceiling applies. It is
// large enough for any real retail price in any currency's minor units and
// small enough that arithmetic on it cannot overflow int64 when multiplied by
// 10000 in the margin calculation.
const maxPriceMinor = int64(1) << 40

// Evaluate applies the Tier-1 rules to a requested price.
//
// This is the hot path — `POST /v1/pricing/evaluate` and the SGU's offline
// decision — and it is written to be allocation-light and branch-predictable
// rather than clever. It performs no allocation when the price is accepted
// unchanged, and exactly one small allocation otherwise.
func Evaluate(c Constraints, requestedMinor int64) Decision {
	d := Decision{RequestedMinor: requestedMinor}
	if err := c.Validate(); err != nil {
		d.Outcome = OutcomeInvalid
		d.Violations = []Violation{{Kind: KindCurrency, Detail: err.Error()}}
		return d
	}
	if requestedMinor < 0 {
		d.Outcome = OutcomeInvalid
		d.Violations = []Violation{{Kind: KindCurrency, Detail: "requested price is negative"}}
		return d
	}

	feasible := c.Feasible()
	d.Feasible = feasible
	if feasible.Empty {
		d.Outcome = OutcomeInfeasible
		d.Violations = infeasibilityReport(c, feasible)
		return d
	}

	price := requestedMinor
	// The violation buffer is a stack array, copied to the heap only when a
	// violation is actually recorded. The accepted path — which is the common
	// one, and the one with the sub-10-millisecond budget — therefore performs
	// no allocation at all.
	var buf [maxViolations]Violation
	violations := buf[:0]

	if price < feasible.LowMinor {
		violations = append(violations, Violation{
			Kind:       feasible.LowKind,
			Detail:     fmt.Sprintf("requested %d is below the binding lower bound", requestedMinor),
			BoundMinor: feasible.LowMinor,
		})
		price = feasible.LowMinor
	}
	if price > feasible.HighMinor {
		violations = append(violations, Violation{
			Kind:       feasible.HighKind,
			Detail:     fmt.Sprintf("requested %d is above the binding upper bound", requestedMinor),
			BoundMinor: feasible.HighMinor,
		})
		price = feasible.HighMinor
	}

	// Granularity and ending are preferences applied *inside* the feasible
	// range. Snapping is allowed to move the price, never to leave the range;
	// where no compliant point exists in range the preference is waived and the
	// waiver is recorded, because a hard bound outranks a merchandising habit.
	if snapped, moved, ok := snapGranularity(price, feasible, c.GranularityMinor); ok {
		if moved {
			violations = append(violations, Violation{
				Kind: KindEndingPolicy, Detail: fmt.Sprintf("snapped to a %d-minor-unit lattice", c.GranularityMinor),
				BoundMinor: snapped,
			})
		}
		price = snapped
	} else if c.GranularityMinor > 1 {
		violations = append(violations, Violation{
			Kind: KindEndingPolicy, Detail: "granularity waived: no lattice point inside the feasible range",
		})
	}

	if snapped, moved, ok := snapEnding(price, feasible, c.Ending); ok {
		if moved {
			violations = append(violations, Violation{
				Kind: KindEndingPolicy, Detail: "snapped to the " + c.Ending.String() + " ending policy",
				BoundMinor: snapped,
			})
		}
		price = snapped
	} else if c.Ending != EndingAny {
		violations = append(violations, Violation{
			Kind: KindEndingPolicy,
			Detail: "ending policy " + c.Ending.String() +
				" waived: no compliant ending inside the feasible range",
		})
	}

	d.Price = canon.NewMoney(price, c.Currency)
	if len(violations) == 0 {
		d.Outcome = OutcomeAccepted
		return d
	}
	d.Outcome = OutcomeAdjusted
	d.Violations = append([]Violation(nil), violations...)
	return d
}

// infeasibilityReport names every constraint that participates in the conflict.
//
// It re-derives each bound independently rather than reporting only the two
// that happened to win the intersection, because the operator's question is
// "which of my rules disagree?", and three rules can conflict pairwise in ways
// that a single winning pair hides.
func infeasibilityReport(c Constraints, r Range) []Violation {
	vs := make([]Violation, 0, maxViolations)
	add := func(kind ConstraintKind, detail string, bound int64) {
		vs = append(vs, Violation{Kind: kind, Detail: detail, BoundMinor: bound})
	}

	if c.UnitCost > 0 {
		den := int64(10000 - c.MinMarginBps)
		bound := c.UnitCost
		if c.MinMarginBps > 0 {
			bound = ceilDiv(c.UnitCost*10000, den)
		}
		if bound > r.HighMinor {
			add(KindMinMargin, fmt.Sprintf("requires at least %d, above the highest permitted price %d", bound, r.HighMinor), bound)
		}
	}
	if c.FloorMinor > 0 && c.FloorMinor > r.HighMinor {
		add(KindRegulatoryFloor, fmt.Sprintf("requires at least %d, above the highest permitted price %d", c.FloorMinor, r.HighMinor), c.FloorMinor)
	}
	if c.CeilingMinor > 0 && c.CeilingMinor < r.LowMinor {
		add(KindRegulatoryCeiling, fmt.Sprintf("requires at most %d, below the lowest permitted price %d", c.CeilingMinor, r.LowMinor), c.CeilingMinor)
	}
	if c.CompetitorMinor > 0 {
		delta := mulBps(c.CompetitorMinor, int64(c.CompetitorBandBps))
		lo, hi := c.CompetitorMinor-delta, c.CompetitorMinor+delta
		if lo > r.HighMinor || hi < r.LowMinor {
			add(KindCompetitorParity, fmt.Sprintf("permits only [%d, %d]", lo, hi), c.CompetitorMinor)
		}
	}
	if c.CurrentMinor > 0 && c.MaxChangeBps > 0 {
		delta := mulBps(c.CurrentMinor, int64(c.MaxChangeBps))
		lo, hi := c.CurrentMinor-delta, c.CurrentMinor+delta
		if lo > r.HighMinor || hi < r.LowMinor {
			add(KindMaxChange, fmt.Sprintf("permits only [%d, %d]", lo, hi), c.CurrentMinor)
		}
	}
	if c.MaxChangesPerPeriod > 0 && c.ChangesThisPeriod >= c.MaxChangesPerPeriod {
		if c.CurrentMinor > r.HighMinor || c.CurrentMinor < r.LowMinor {
			add(KindChangeFrequency, fmt.Sprintf("budget of %d changes is exhausted, pinning the price at %d",
				c.MaxChangesPerPeriod, c.CurrentMinor), c.CurrentMinor)
		}
	}

	if len(vs) == 0 {
		// Defensive: the bounds conflict but no single rule looks guilty, which
		// can only happen if two rules produced the same winning bound. Report
		// the winning pair rather than an empty explanation.
		add(r.LowKind, fmt.Sprintf("lower bound %d exceeds upper bound %d", r.LowMinor, r.HighMinor), r.LowMinor)
		add(r.HighKind, fmt.Sprintf("upper bound %d is below lower bound %d", r.HighMinor, r.LowMinor), r.HighMinor)
	}
	// A stable order keeps two tiers' audit records byte-identical.
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].Kind < vs[j].Kind })
	return vs
}

// snapGranularity moves a price onto a lattice, staying inside r.
func snapGranularity(price int64, r Range, granularity int64) (snapped int64, moved, ok bool) {
	if granularity <= 1 {
		return price, false, true
	}
	if price%granularity == 0 {
		return price, false, true
	}
	down := price - price%granularity
	up := down + granularity
	// Prefer the nearer lattice point, then the other, then give up. Ties go
	// downwards: a customer is never harmed by the cheaper of two equally
	// compliant prices.
	first, second := down, up
	if up-price < price-down {
		first, second = up, down
	}
	for _, cand := range [2]int64{first, second} {
		if cand >= r.LowMinor && cand <= r.HighMinor {
			return cand, true, true
		}
	}
	return price, false, false
}

// snapEnding moves a price to the nearest compliant ending inside r.
//
// The search walks outwards from the requested price one major unit at a time
// in both directions and stops at the first candidate inside the range, which
// is at most (range width / major unit) + 1 steps and in practice one or two.
func snapEnding(price int64, r Range, e Ending) (snapped int64, moved, ok bool) {
	if e == EndingAny {
		return price, false, true
	}
	if endingOK(price, e) {
		return price, false, true
	}
	// The lattice for every supported ending repeats every 100 minor units,
	// except the nickel rule which repeats every 5.
	period := int64(100)
	if e == EndingNickel {
		period = 5
	}
	base := price - mod(price, period)
	for step := int64(0); step <= (r.HighMinor-r.LowMinor)/period+2; step++ {
		for _, cand := range [2]int64{base + step*period + endingOffset(e, period), base - step*period + endingOffset(e, period)} {
			if cand < r.LowMinor || cand > r.HighMinor {
				continue
			}
			if !endingOK(cand, e) {
				continue
			}
			return cand, cand != price, true
		}
	}
	return price, false, false
}

// endingOffset is the compliant residue within one period.
func endingOffset(e Ending, period int64) int64 {
	switch e {
	case EndingCharm:
		return 99
	case EndingCharm95:
		return 95
	case EndingWhole:
		return 0
	case EndingNickel:
		return 0
	default:
		_ = period
		return 0
	}
}

// endingOK reports whether a price already satisfies the ending policy.
func endingOK(price int64, e Ending) bool {
	switch e {
	case EndingCharm:
		return mod(price, 100) == 99
	case EndingCharm95:
		return mod(price, 100) == 95
	case EndingWhole:
		return mod(price, 100) == 0
	case EndingNickel:
		return mod(price, 5) == 0
	default:
		return true
	}
}

// mod is a non-negative modulus. Go's % keeps the sign of the dividend, which
// would make a negative price satisfy the wrong ending; prices are non-negative
// here but the helper keeps the property local rather than assumed.
func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// mulBps multiplies by basis points, rounding half away from zero.
func mulBps(v, bps int64) int64 {
	num := v * bps
	if num < 0 {
		return (num - 5000) / 10000
	}
	return (num + 5000) / 10000
}

// ceilDiv divides rounding towards positive infinity.
func ceilDiv(num, den int64) int64 {
	if den <= 0 {
		return num
	}
	q := num / den
	if num%den != 0 {
		q++
	}
	return q
}

// Candidates enumerates every price in the feasible range that satisfies the
// ending and granularity policies, in ascending order.
//
// Tier 2's optimiser searches this set rather than a continuous interval,
// because the set of prices a shelf may legally display is discrete and small —
// typically a few dozen — and searching it exhaustively is both faster and more
// honest than a continuous optimiser whose optimum then has to be rounded to
// something that may not be feasible at all.
//
// The result is capped at limit entries; a limit of zero uses a default that
// covers any realistic retail range at one-cent granularity.
func (c Constraints) Candidates(limit int) []int64 {
	const defaultLimit = 4096
	if limit <= 0 {
		limit = defaultLimit
	}
	r := c.Feasible()
	if r.Empty {
		return nil
	}
	step := c.GranularityMinor
	if step < 1 {
		step = 1
	}
	if c.Ending == EndingNickel && step%5 != 0 {
		step = lcm(step, 5)
	}
	out := make([]int64, 0, 64)
	// Start at the first lattice point at or above the lower bound.
	start := r.LowMinor
	if m := mod(start, step); m != 0 {
		start += step - m
	}
	for p := start; p <= r.HighMinor && len(out) < limit; p += step {
		if endingOK(p, c.Ending) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		// Every candidate was filtered out by the ending policy. The policy is
		// a preference, so fall back to the lattice alone rather than returning
		// an empty search space to the optimiser.
		for p := start; p <= r.HighMinor && len(out) < limit; p += step {
			out = append(out, p)
		}
	}
	return out
}

// lcm is the least common multiple of two positive values.
func lcm(a, b int64) int64 {
	g := a
	h := b
	for h != 0 {
		g, h = h, g%h
	}
	if g == 0 {
		return a
	}
	return a / g * b
}
