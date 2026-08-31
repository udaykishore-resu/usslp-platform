package app

import (
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
)

// SLOReport is the whole service-level picture for a scope.
type SLOReport struct {
	Scope   Scope                   `json:"scope"`
	Results []domain.SLOResult      `json:"results"`
	Latency domain.LatencyBreakdown `json:"latency"`
	// ByStore ranks stores by their own latency percentile, so a breach can be
	// attributed to a store rather than to the platform.
	ByStore []StoreSLO          `json:"by_store,omitempty"`
	Stats   columnar.QueryStats `json:"stats"`
}

// StoreSLO is one store's contribution.
type StoreSLO struct {
	StoreID string  `json:"store_id"`
	Total   int64   `json:"total"`
	Good    int64   `json:"good"`
	P95     float64 `json:"p95_ms"`
	P99     float64 `json:"p99_ms"`
	// AchievedPct is the store's own latency success rate.
	AchievedPct float64 `json:"achieved_pct"`
}

// ExpectedReportInterval is how often a healthy label reports telemetry.
//
// Five minutes is the firmware's heartbeat period. It is here because the label
// availability SLO is computed as "reports that arrived against reports that
// should have", and that calculation is meaningless without knowing what the
// fleet was asked to do.
const ExpectedReportInterval = 5 * time.Minute

// ComputeSLOReport measures every objective over a scope.
//
// # Why the latency objective is computed with two queries and not one
//
// The success count needs a filter on latency, and the percentiles need the
// whole distribution. Doing both in one pass would mean either computing the
// percentiles over only the successful events — which would hide exactly the
// tail that caused the breach — or counting successes over a filtered
// distribution. Two scans of a column store over a month of one tenant's
// deliveries is a few hundred milliseconds, which is a cheap price for a number
// that is not quietly wrong.
func ComputeSLOReport(tables Tables, scope Scope, targets []domain.SLOTarget) (SLOReport, error) {
	delivery, err := tables.Get(domain.TableDelivery)
	if err != nil {
		return SLOReport{}, err
	}
	if len(targets) == 0 {
		targets = domain.DefaultSLOs()
	}
	out := SLOReport{Scope: scope}
	elapsed := scope.To.Sub(scope.From)
	if elapsed <= 0 {
		elapsed = time.Hour
	}

	// One scan for the totals, the percentiles and the directly measured
	// components of the latency breakdown.
	base, err := delivery.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: scope.filters("store_id"),
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggCount, As: "total"},
			{Func: columnar.AggQuantile, Column: "latency_ms", Q: 0.50, As: "p50"},
			{Func: columnar.AggQuantile, Column: "latency_ms", Q: 0.95, As: "p95"},
			{Func: columnar.AggQuantile, Column: "latency_ms", Q: 0.99, As: "p99"},
			{Func: columnar.AggQuantile, Column: "refresh_ms", Q: 0.50, As: "refresh_p50"},
			{Func: columnar.AggQuantile, Column: "mesh_hops", Q: 0.50, As: "hops_p50"},
			{Func: columnar.AggCountDistinct, Column: "label_id", As: "labels"},
		},
	})
	if err != nil {
		return SLOReport{}, err
	}
	out.Stats = base.Stats
	var total int64
	var p50, p95, p99, refreshP50, hopsP50 float64
	var labels int64
	if len(base.Rows) > 0 {
		v := base.Rows[0].Values
		total = int64(v["total"])
		p50, p95, p99 = v["p50"], v["p95"], v["p99"]
		refreshP50, hopsP50 = v["refresh_p50"], v["hops_p50"]
		labels = int64(v["labels"])
	}

	for _, target := range targets {
		switch target.Name {
		case "price_latency":
			budget := target.LatencyBudgetMS
			within, err := delivery.Query(columnar.Query{
				From: scope.From, To: scope.To,
				Filters: append(scope.filters("store_id"),
					columnar.Filter{Column: "latency_ms", Op: columnar.OpLte, Value: budget},
					columnar.Filter{Column: "outcome", Op: columnar.OpEq, Value: "delivered"}),
				Aggregates: []columnar.Aggregate{{Func: columnar.AggCount, As: "good"}},
			})
			if err != nil {
				return SLOReport{}, err
			}
			good := int64(0)
			if len(within.Rows) > 0 {
				good = int64(within.Rows[0].Values["good"])
			}
			r := domain.ComputeSLO(target, scope.From, scope.To, total, good, elapsed)
			r.P50, r.P95, r.P99 = p50, p95, p99
			r.Approximate = true
			out.Results = append(out.Results, r)
			out.Latency = domain.Breakdown(budget, p50, p95, p99, refreshP50, hopsP50, good, total)

		case "delivery_success":
			ok, err := delivery.Query(columnar.Query{
				From: scope.From, To: scope.To,
				Filters: append(scope.filters("store_id"),
					columnar.Filter{Column: "outcome", Op: columnar.OpEq, Value: "delivered"}),
				Aggregates: []columnar.Aggregate{{Func: columnar.AggCount, As: "good"}},
			})
			if err != nil {
				return SLOReport{}, err
			}
			good := int64(0)
			if len(ok.Rows) > 0 {
				good = int64(ok.Rows[0].Values["good"])
			}
			out.Results = append(out.Results,
				domain.ComputeSLO(target, scope.From, scope.To, total, good, elapsed))

		case "label_availability":
			r, err := availability(tables, scope, target, labels, elapsed)
			if err != nil {
				return SLOReport{}, err
			}
			out.Results = append(out.Results, r)
		}
	}

	// Per-store attribution, so a breach names the stores responsible rather
	// than leaving somebody to find them.
	byStore, err := delivery.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: scope.filters("store_id"),
		GroupBy: []string{"store_id"},
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggCount, As: "total"},
			{Func: columnar.AggQuantile, Column: "latency_ms", Q: 0.95, As: "p95"},
			{Func: columnar.AggQuantile, Column: "latency_ms", Q: 0.99, As: "p99"},
		},
		OrderBy: "p99",
	})
	if err != nil {
		return SLOReport{}, err
	}
	budget := 3000.0
	for _, t := range targets {
		if t.Name == "price_latency" && t.LatencyBudgetMS > 0 {
			budget = t.LatencyBudgetMS
		}
	}
	for _, row := range byStore.Rows {
		store := row.Group["store_id"]
		good, err := delivery.Query(columnar.Query{
			From: scope.From, To: scope.To,
			Filters: append(scope.filters("store_id"),
				columnar.Filter{Column: "store_id", Op: columnar.OpEq, Value: store},
				columnar.Filter{Column: "latency_ms", Op: columnar.OpLte, Value: budget},
				columnar.Filter{Column: "outcome", Op: columnar.OpEq, Value: "delivered"}),
			Aggregates: []columnar.Aggregate{{Func: columnar.AggCount, As: "good"}},
		})
		if err != nil {
			return SLOReport{}, err
		}
		s := StoreSLO{StoreID: store, Total: int64(row.Values["total"]),
			P95: row.Values["p95"], P99: row.Values["p99"]}
		if len(good.Rows) > 0 {
			s.Good = int64(good.Rows[0].Values["good"])
		}
		if s.Total > 0 {
			s.AchievedPct = 100 * float64(s.Good) / float64(s.Total)
		}
		out.ByStore = append(out.ByStore, s)
	}
	return out, nil
}

// availability computes the label-availability objective from telemetry.
//
// The expected report count is the *distinct labels seen in the window* times
// the number of report periods in it. Using labels seen rather than labels
// provisioned is a deliberate choice with a real consequence: a label that has
// been silent for the whole window is invisible to this calculation and does not
// count against availability. That understates outages of a whole store, so the
// report says so, and the device registry's provisioned count is the number a
// stricter version would use.
func availability(tables Tables, scope Scope, target domain.SLOTarget,
	deliveryLabels int64, elapsed time.Duration) (domain.SLOResult, error) {

	telemetry, err := tables.Get(domain.TableTelemetry)
	if err != nil {
		return domain.SLOResult{}, err
	}
	res, err := telemetry.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: scope.filters("store_id"),
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggCount, As: "reports"},
			{Func: columnar.AggCountDistinct, Column: "label_id", As: "labels"},
		},
	})
	if err != nil {
		return domain.SLOResult{}, err
	}
	var reports, labels int64
	if len(res.Rows) > 0 {
		reports = int64(res.Rows[0].Values["reports"])
		labels = int64(res.Rows[0].Values["labels"])
	}
	if labels == 0 {
		labels = deliveryLabels
	}
	periods := int64(elapsed / ExpectedReportInterval)
	if periods < 1 {
		periods = 1
	}
	return domain.ComputeAvailability(target, scope.From, scope.To, domain.AvailabilityInput{
		ExpectedReports: labels * periods, ActualReports: reports,
		Labels: labels, Periods: periods,
	}, elapsed), nil
}
