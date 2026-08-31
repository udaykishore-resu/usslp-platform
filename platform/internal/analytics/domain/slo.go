package domain

import (
	"fmt"
	"math"
	"time"
)

// SLOTarget is one service-level objective.
type SLOTarget struct {
	// Name identifies the objective.
	Name string `json:"name"`
	// Objective is the fraction of events that must be good, e.g. 0.995.
	Objective float64 `json:"objective"`
	// Window is the period the objective and its error budget are measured over.
	Window time.Duration `json:"window"`
	// LatencyBudgetMS applies to the latency objectives: an event is good if it
	// landed inside this many milliseconds.
	LatencyBudgetMS float64 `json:"latency_budget_ms,omitempty"`
	// Description is what an on-call engineer reads at three in the morning.
	Description string `json:"description"`
}

// The platform's SLO catalogue.
//
// The latency target is 3,000 ms because that is the budget the interface
// contract apportions across the nine hops from POS to settled pixels, and it
// is the number in the customer-facing commitment. The 99.5% is not a round
// number chosen for comfort: at 52,000 price updates a second, a 99.9% target
// would give an error budget of 52 events a second and would be burnt through
// by a single store's controller rebooting, which is a normal event and should
// not page anybody.
func DefaultSLOs() []SLOTarget {
	return []SLOTarget{
		{
			Name: "price_latency", Objective: 0.995, Window: 30 * 24 * time.Hour,
			LatencyBudgetMS: 3000,
			Description: "price changes reach the shelf within three seconds of the platform " +
				"accepting them, measured to the moment the pixels settle",
		},
		{
			Name: "delivery_success", Objective: 0.999, Window: 30 * 24 * time.Hour,
			Description: "price updates are confirmed by the label rather than failing after retries",
		},
		{
			Name: "label_availability", Objective: 0.995, Window: 30 * 24 * time.Hour,
			Description: "labels are reporting telemetry, and therefore reachable and showing a verified price",
		},
	}
}

// SLOResult is one objective's status over a window.
type SLOResult struct {
	Target SLOTarget `json:"target"`
	// From and To bracket the measured window.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Total and Good are the event counts.
	Total int64 `json:"total"`
	Good  int64 `json:"good"`
	// Achieved is Good/Total.
	Achieved float64 `json:"achieved"`
	// Met reports whether the objective was reached.
	Met bool `json:"met"`

	// BudgetTotal is the number of bad events the objective permits over the
	// window: (1 - objective) * total.
	BudgetTotal float64 `json:"budget_total"`
	// BudgetSpent is how many bad events actually occurred.
	BudgetSpent float64 `json:"budget_spent"`
	// BudgetRemainingPct is what is left, as a percentage of the budget. It
	// goes negative when the objective has been missed, which is more useful
	// than clamping at zero: "minus 340%" says how badly.
	BudgetRemainingPct float64 `json:"budget_remaining_pct"`
	// BurnRate is the spend rate relative to the rate that would exactly
	// exhaust the budget over the window. One means on track to spend exactly
	// the budget; fourteen means the month's budget goes in two days.
	//
	// This is the number to alert on, not the achieved ratio: a 99.5% objective
	// over a month is still "met" on the morning of the day an outage will
	// consume the rest of it, and the burn rate is what says so in time.
	BurnRate float64 `json:"burn_rate"`
	// ExhaustedIn estimates when the budget runs out at the current burn rate.
	// Absent when the burn rate is at or below one.
	ExhaustedIn *time.Duration `json:"exhausted_in,omitempty"`

	// Percentiles are the latency distribution, present for latency objectives.
	P50 float64 `json:"p50_ms,omitempty"`
	P95 float64 `json:"p95_ms,omitempty"`
	P99 float64 `json:"p99_ms,omitempty"`
	// Approximate marks the percentiles as t-digest estimates.
	Approximate bool `json:"approximate,omitempty"`
}

// FastBurnThreshold is the burn rate that warrants waking somebody.
//
// Fourteen and a bit is the rate at which a thirty-day budget is consumed in
// about two days; it is the standard multi-window burn-rate alert threshold and
// it is here rather than in a dashboard because the number belongs with the
// objective it protects.
const FastBurnThreshold = 14.4

// SlowBurnThreshold is the rate that warrants a ticket rather than a page: the
// budget goes in about ten days.
const SlowBurnThreshold = 3.0

// Severity classifies a burn rate.
func (r SLOResult) Severity() string {
	switch {
	case r.BurnRate >= FastBurnThreshold:
		return "page"
	case r.BurnRate >= SlowBurnThreshold:
		return "ticket"
	case !r.Met:
		return "breached"
	default:
		return "ok"
	}
}

// ComputeSLO turns counts into a budget position.
//
// # Why the burn rate is computed against the elapsed fraction of the window
//
// A budget position quoted at the start of a window is meaningless: every
// objective is met when nothing has happened yet. The burn rate divides the
// fraction of budget spent by the fraction of window elapsed, so it is
// comparable from the first hour of the month to the last, which is what makes
// it alertable.
func ComputeSLO(target SLOTarget, from, to time.Time, total, good int64, elapsed time.Duration) SLOResult {
	r := SLOResult{Target: target, From: from, To: to, Total: total, Good: good}
	if total == 0 {
		// No events is not a breach. A store closed for the night produces no
		// deliveries, and reporting that as a 0% success rate would page
		// somebody every night.
		r.Met = true
		return r
	}
	r.Achieved = float64(good) / float64(total)
	r.Met = r.Achieved >= target.Objective

	bad := float64(total - good)
	r.BudgetTotal = (1 - target.Objective) * float64(total)
	r.BudgetSpent = bad
	if r.BudgetTotal > 0 {
		r.BudgetRemainingPct = 100 * (r.BudgetTotal - r.BudgetSpent) / r.BudgetTotal
	} else if bad > 0 {
		// A 100% objective has no budget at all, so any failure is total
		// exhaustion rather than a division by zero.
		r.BudgetRemainingPct = -100
	} else {
		r.BudgetRemainingPct = 100
	}

	if elapsed <= 0 {
		elapsed = to.Sub(from)
	}
	windowFraction := 1.0
	if target.Window > 0 && elapsed > 0 {
		windowFraction = elapsed.Seconds() / target.Window.Seconds()
	}
	if windowFraction <= 0 {
		windowFraction = 1
	}
	budgetFraction := 0.0
	if r.BudgetTotal > 0 {
		budgetFraction = r.BudgetSpent / r.BudgetTotal
	}
	r.BurnRate = budgetFraction / windowFraction

	if r.BurnRate > 1 && r.BudgetRemainingPct > 0 {
		// At this rate, how long until the remaining budget is gone.
		remainingFraction := r.BudgetRemainingPct / 100
		spendPerSecond := budgetFraction / elapsed.Seconds()
		if spendPerSecond > 0 {
			d := time.Duration(remainingFraction / spendPerSecond * float64(time.Second))
			r.ExhaustedIn = &d
		}
	}
	return r
}

// String renders a one-line summary, which is what a status page and a Slack
// alert both want.
func (r SLOResult) String() string {
	return fmt.Sprintf("%s: %.4f%% against %.4f%% over %d events, %.1f%% of budget left, burn %.2fx (%s)",
		r.Target.Name, 100*r.Achieved, 100*r.Target.Objective, r.Total,
		r.BudgetRemainingPct, r.BurnRate, r.Severity())
}

// AvailabilityInput is what a label-availability calculation needs.
//
// Availability here is "the label reported in the period it was expected to",
// which is the only definition the platform can measure: a label that has
// stopped talking is either flat, out of range, or gone, and from the cloud all
// three look the same and all three mean the shelf may be showing a price
// nobody can verify.
type AvailabilityInput struct {
	// ExpectedReports is how many telemetry reports the fleet should have
	// produced over the window: labels times periods.
	ExpectedReports int64
	// ActualReports is how many arrived.
	ActualReports int64
	// Labels and Periods are carried through for the explanation.
	Labels  int64
	Periods int64
}

// ComputeAvailability turns expected and actual report counts into an SLO
// result.
func ComputeAvailability(target SLOTarget, from, to time.Time, in AvailabilityInput, elapsed time.Duration) SLOResult {
	// A fleet that produced *more* reports than expected — a label with a fast
	// clock, or a period boundary landing awkwardly — is at 100%, not above it.
	good := in.ActualReports
	if good > in.ExpectedReports {
		good = in.ExpectedReports
	}
	return ComputeSLO(target, from, to, in.ExpectedReports, good, elapsed)
}

// LatencyBreakdown is the hop-by-hop view of the three-second budget.
//
// The platform's interface contract apportions the budget across nine hops, and
// the only two the analytics service can measure from delivery confirmations are
// the total and the label's own refresh. Reporting the residual explicitly —
// rather than silently attributing it to the network — is what keeps the
// breakdown honest: it says how much of the budget is accounted for and how much
// is not.
type LatencyBreakdown struct {
	TotalP50 float64 `json:"total_p50_ms"`
	TotalP95 float64 `json:"total_p95_ms"`
	TotalP99 float64 `json:"total_p99_ms"`
	// RefreshP50 is the E-Ink waveform time, which the label reports directly.
	RefreshP50 float64 `json:"refresh_p50_ms"`
	// MeshHopsP50 is the median hop count, the other directly measured
	// component.
	MeshHopsP50 float64 `json:"mesh_hops_p50"`
	// UnattributedP50 is the rest: ingest, the stream, the Label Service, the
	// broker, the bridge and the radio.
	UnattributedP50 float64 `json:"unattributed_p50_ms"`
	// BudgetMS is the objective this is measured against.
	BudgetMS float64 `json:"budget_ms"`
	// WithinBudgetPct is the fraction of confirmations inside it.
	WithinBudgetPct float64 `json:"within_budget_pct"`
}

// Breakdown assembles the latency view.
func Breakdown(budgetMS, totalP50, totalP95, totalP99, refreshP50, hopsP50 float64, within, total int64) LatencyBreakdown {
	b := LatencyBreakdown{
		TotalP50: totalP50, TotalP95: totalP95, TotalP99: totalP99,
		RefreshP50: refreshP50, MeshHopsP50: hopsP50, BudgetMS: budgetMS,
		UnattributedP50: math.Max(0, totalP50-refreshP50),
	}
	if total > 0 {
		b.WithinBudgetPct = 100 * float64(within) / float64(total)
	}
	return b
}
