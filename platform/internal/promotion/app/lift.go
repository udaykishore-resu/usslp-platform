// Package app is the promotion service's application layer: lift measurement,
// simulation, and the fan-out that tells the estate a promotion has started.
package app

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// SalesPoint is one day's trading for one (store, SKU).
type SalesPoint struct {
	StoreID canon.StoreID `json:"store_id"`
	SKU     canon.SKU     `json:"sku"`
	// Day is the local trading day, normalised to midnight.
	Day time.Time `json:"day"`
	// Units sold and revenue taken.
	Units        float64 `json:"units"`
	RevenueMinor float64 `json:"revenue_minor"`
	// CostMinor is the cost of goods sold, for the margin figure.
	CostMinor float64 `json:"cost_minor"`
}

// LiftWindow describes the three periods a lift measurement compares.
type LiftWindow struct {
	// PreStart and PreEnd bracket the baseline period before the promotion.
	PreStart, PreEnd time.Time
	// DuringStart and DuringEnd bracket the promotion itself.
	DuringStart, DuringEnd time.Time
	// PostStart and PostEnd bracket the period after, which is where the
	// pull-forward shows up.
	PostStart, PostEnd time.Time
}

// DefaultLiftWindow builds the standard windows around a promotion: an equal
// number of days before and after.
//
// Equal-length windows matter because the comparison is of daily rates: unequal
// windows would be comparable in principle and are routinely mis-read in
// practice, and a four-week baseline against a three-day promotion invites
// somebody to compare the totals.
func DefaultLiftWindow(start, end time.Time) LiftWindow {
	span := end.Sub(start)
	if span <= 0 {
		span = 24 * time.Hour
	}
	return LiftWindow{
		PreStart: start.Add(-span), PreEnd: start,
		DuringStart: start, DuringEnd: end,
		PostStart: end, PostEnd: end.Add(span),
	}
}

// PeriodStats summarises one window.
type PeriodStats struct {
	Days              int     `json:"days"`
	Units             float64 `json:"units"`
	RevenueMinor      float64 `json:"revenue_minor"`
	MarginMinor       float64 `json:"margin_minor"`
	UnitsPerDay       float64 `json:"units_per_day"`
	RevenuePerDay     float64 `json:"revenue_per_day"`
	MarginPerDay      float64 `json:"margin_per_day"`
	AverageSellingPri float64 `json:"average_selling_price_minor"`
}

// LiftResult is a promotion's measured effect.
type LiftResult struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	// Scope names what was measured: a SKU, a store, or a cluster.
	Scope string `json:"scope"`
	// Pre, During and Post are the three periods.
	Pre    PeriodStats `json:"pre"`
	During PeriodStats `json:"during"`
	Post   PeriodStats `json:"post"`

	// UnitLiftPct is the change in units per day, during against pre.
	UnitLiftPct float64 `json:"unit_lift_pct"`
	// RevenueLiftPct and MarginLiftPct are the same for revenue and margin.
	// Margin is the one that decides whether the promotion was worth running,
	// and it is routinely negative while units are strongly positive.
	RevenueLiftPct float64 `json:"revenue_lift_pct"`
	MarginLiftPct  float64 `json:"margin_lift_pct"`
	// PostDipPct is the change in units per day after against before. A large
	// negative number is pull-forward: the promotion moved sales rather than
	// creating them.
	PostDipPct float64 `json:"post_dip_pct"`
	// IncrementalUnits is the units above baseline during the promotion, net of
	// the post-period dip. It is the honest number, and it is usually a good
	// deal smaller than the headline.
	IncrementalUnits float64 `json:"incremental_units"`
	// IncrementalMarginMinor is the same for margin.
	IncrementalMarginMinor float64 `json:"incremental_margin_minor"`

	// Control is the control-group comparison, present only when a control was
	// supplied.
	Control *ControlComparison `json:"control,omitempty"`
	// Method names how the lift was computed, so a reader knows which number
	// they are looking at.
	Method string `json:"method"`
	// Caveats are the reasons to distrust the result, stated rather than
	// buried.
	Caveats []string `json:"caveats,omitempty"`
}

// ControlComparison is a difference-in-differences estimate against stores that
// did not run the promotion.
type ControlComparison struct {
	// ControlPre and ControlDuring are the control group's periods.
	ControlPre    PeriodStats `json:"control_pre"`
	ControlDuring PeriodStats `json:"control_during"`
	// ControlLiftPct is what the control group did over the same period —
	// seasonality, weather, a competitor's move — and therefore what the test
	// group would have done without the promotion.
	ControlLiftPct float64 `json:"control_lift_pct"`
	// DiffInDiffPct is the test group's lift minus the control's: the estimate
	// of what the promotion itself caused.
	DiffInDiffPct float64 `json:"diff_in_diff_pct"`
	// ControlStores is how many stores formed the control.
	ControlStores int `json:"control_stores"`
	// BaselineDivergencePct is how far apart the two groups' *pre-period* daily
	// rates were, per store. Difference-in-differences assumes the groups would
	// have moved in parallel; a large divergence before the promotion says they
	// were never comparable, and the estimate should not be believed.
	BaselineDivergencePct float64 `json:"baseline_divergence_pct"`
	// Trustworthy is false when the divergence is too large or the control is
	// too small.
	Trustworthy bool   `json:"trustworthy"`
	Reason      string `json:"reason,omitempty"`
}

// MaxBaselineDivergencePct is how far the control and test groups' pre-period
// rates may differ before the difference-in-differences estimate is disowned.
//
// Twenty per cent. Below that, two store groups are plausibly comparable and
// the parallel-trends assumption is defensible; above it they are different
// businesses and subtracting one from the other measures the difference between
// them rather than the effect of the promotion.
const MaxBaselineDivergencePct = 20.0

// MinControlStores is the smallest control group worth computing against.
const MinControlStores = 3

// MeasureLift computes a promotion's effect from daily sales.
//
// # Why three periods and not two
//
// A promotion that sells a fortnight of stock in three days has a spectacular
// during-versus-before number and may have sold nothing extra at all. The post
// period is where that shows up, and the incremental figures net it out. A lift
// report without a post period systematically overstates every promotion, and
// does so most for exactly the deep discounts whose margin cost is highest.
func MeasureLift(promotionID canon.PromotionID, scope string, sales []SalesPoint, w LiftWindow) LiftResult {
	res := LiftResult{
		PromotionID: promotionID, Scope: scope,
		Pre:    summarise(sales, w.PreStart, w.PreEnd),
		During: summarise(sales, w.DuringStart, w.DuringEnd),
		Post:   summarise(sales, w.PostStart, w.PostEnd),
		Method: "pre/during/post daily-rate comparison",
	}
	res.UnitLiftPct = pctChange(res.Pre.UnitsPerDay, res.During.UnitsPerDay)
	res.RevenueLiftPct = pctChange(res.Pre.RevenuePerDay, res.During.RevenuePerDay)
	res.MarginLiftPct = pctChange(res.Pre.MarginPerDay, res.During.MarginPerDay)
	res.PostDipPct = pctChange(res.Pre.UnitsPerDay, res.Post.UnitsPerDay)

	// Incremental = what was sold above baseline during, minus what was lost
	// below baseline after.
	duringExcess := (res.During.UnitsPerDay - res.Pre.UnitsPerDay) * float64(res.During.Days)
	postShortfall := (res.Pre.UnitsPerDay - res.Post.UnitsPerDay) * float64(res.Post.Days)
	res.IncrementalUnits = duringExcess - postShortfall

	duringMargin := (res.During.MarginPerDay - res.Pre.MarginPerDay) * float64(res.During.Days)
	postMargin := (res.Pre.MarginPerDay - res.Post.MarginPerDay) * float64(res.Post.Days)
	res.IncrementalMarginMinor = duringMargin - postMargin

	if res.Pre.Days == 0 {
		res.Caveats = append(res.Caveats, "no baseline data: the lift percentages are not meaningful")
	}
	if res.Post.Days == 0 {
		res.Caveats = append(res.Caveats,
			"no post-promotion data yet: the incremental figures do not account for pull-forward and will overstate the effect")
	}
	if res.Pre.Days > 0 && res.Pre.Days < 7 {
		res.Caveats = append(res.Caveats,
			fmt.Sprintf("the baseline is only %d days, so a single unusual day dominates it", res.Pre.Days))
	}
	if res.Pre.Days > 0 && res.During.Days > 0 && res.Pre.Days != res.During.Days {
		res.Caveats = append(res.Caveats,
			fmt.Sprintf("the baseline (%d days) and promotion (%d days) periods differ in length; "+
				"the comparison is of daily rates, not totals", res.Pre.Days, res.During.Days))
	}
	return res
}

// MeasureLiftWithControl adds a difference-in-differences estimate against
// stores that did not run the promotion.
//
// # Why a control group changes the claim
//
// The pre/during comparison attributes everything that happened to the
// promotion — including the weather, the school holidays, and the fact that
// this is the week everybody buys barbecue charcoal anyway. A control group of
// comparable stores that did not run the promotion moved for all of those
// reasons and not for the promotion, so the difference between the two
// differences is the closest thing to a causal estimate the platform can
// produce without randomising.
func MeasureLiftWithControl(promotionID canon.PromotionID, scope string,
	test, control []SalesPoint, controlStores int, w LiftWindow) LiftResult {

	res := MeasureLift(promotionID, scope, test, w)
	cmp := ControlComparison{
		ControlPre:    summarise(control, w.PreStart, w.PreEnd),
		ControlDuring: summarise(control, w.DuringStart, w.DuringEnd),
		ControlStores: controlStores,
	}
	cmp.ControlLiftPct = pctChange(cmp.ControlPre.UnitsPerDay, cmp.ControlDuring.UnitsPerDay)
	cmp.DiffInDiffPct = res.UnitLiftPct - cmp.ControlLiftPct

	// Per-store daily rates make the two groups comparable in size. Comparing
	// raw group totals would call a 200-store test group and a 5-store control
	// incomparable when they are perfectly comparable per store.
	testStores := countStores(test)
	testRate := perStore(res.Pre.UnitsPerDay, testStores)
	ctrlRate := perStore(cmp.ControlPre.UnitsPerDay, controlStores)
	cmp.BaselineDivergencePct = math.Abs(pctChange(ctrlRate, testRate))

	switch {
	case controlStores < MinControlStores:
		cmp.Reason = fmt.Sprintf("a control of %d stores is too small to average out store-level noise "+
			"(the platform wants at least %d)", controlStores, MinControlStores)
	case cmp.ControlPre.Days == 0 || cmp.ControlDuring.Days == 0:
		cmp.Reason = "the control group has no data in one of the periods"
	case cmp.BaselineDivergencePct > MaxBaselineDivergencePct:
		cmp.Reason = fmt.Sprintf("the control and test groups sold at rates %.1f%% apart before the promotion, "+
			"above the %.0f%% limit: they are not comparable, so the difference between them is not the promotion",
			cmp.BaselineDivergencePct, MaxBaselineDivergencePct)
	default:
		cmp.Trustworthy = true
	}

	res.Control = &cmp
	if cmp.Trustworthy {
		res.Method = "difference-in-differences against a control group"
	} else {
		res.Caveats = append(res.Caveats, "control comparison computed but not trustworthy: "+cmp.Reason)
	}
	return res
}

func perStore(rate float64, stores int) float64 {
	if stores <= 0 {
		return rate
	}
	return rate / float64(stores)
}

func countStores(sales []SalesPoint) int {
	seen := map[canon.StoreID]struct{}{}
	for _, s := range sales {
		seen[s.StoreID] = struct{}{}
	}
	return len(seen)
}

// summarise aggregates the sales points inside [from, to).
//
// The day count is of *distinct days with data*, not of calendar days in the
// window. A store that was closed for two of a seven-day window traded for
// five, and dividing its sales by seven would understate its rate by 29% — and
// would do so differently for the promotion period and the baseline, which is
// how a lift measurement acquires a bias nobody can find.
func summarise(sales []SalesPoint, from, to time.Time) PeriodStats {
	var st PeriodStats
	days := map[int64]struct{}{}
	for _, s := range sales {
		if s.Day.Before(from) || !s.Day.Before(to) {
			continue
		}
		st.Units += s.Units
		st.RevenueMinor += s.RevenueMinor
		st.MarginMinor += s.RevenueMinor - s.CostMinor
		days[s.Day.UTC().Truncate(24*time.Hour).Unix()] = struct{}{}
	}
	st.Days = len(days)
	if st.Days > 0 {
		d := float64(st.Days)
		st.UnitsPerDay = st.Units / d
		st.RevenuePerDay = st.RevenueMinor / d
		st.MarginPerDay = st.MarginMinor / d
	}
	if st.Units > 0 {
		st.AverageSellingPri = st.RevenueMinor / st.Units
	}
	return st
}

// pctChange is the percentage change from a to b.
//
// A zero baseline returns zero rather than infinity. "Sales went from nothing to
// something" is real information, but it is not a percentage, and an infinite
// lift in a report is worse than an absent one because it poisons every average
// computed over it. The absolute figures alongside carry the information.
func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return 100 * (to - from) / from
}

// ClusterLift groups a lift measurement by store cluster.
//
// Retailers do not ask "did this promotion work"; they ask "did it work in the
// convenience estate", because the answer routinely differs by format. The
// clustering is supplied by the caller because store groupings are a
// merchandising concept the promotion service does not own.
func ClusterLift(promotionID canon.PromotionID, sales []SalesPoint,
	clusterOf map[canon.StoreID]string, w LiftWindow) []LiftResult {

	byCluster := map[string][]SalesPoint{}
	for _, s := range sales {
		cluster := clusterOf[s.StoreID]
		if cluster == "" {
			cluster = "unclustered"
		}
		byCluster[cluster] = append(byCluster[cluster], s)
	}
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]LiftResult, 0, len(names))
	for _, name := range names {
		out = append(out, MeasureLift(promotionID, name, byCluster[name], w))
	}
	return out
}
