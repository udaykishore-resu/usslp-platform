// Package app is the analytics service's application layer: the named retail
// intelligence reports, the SLO computation, ingest from the event streams, and
// the retention sweeps.
//
// Each report is a small number of structured queries plus the arithmetic that
// turns their output into the answer a retailer asked for. Keeping the queries
// structured rather than templated SQL means a report is readable as a
// composition of two or three scans, and means none of them can be persuaded to
// scan something they were not meant to.
package app

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Tables is the set of stores the reports read.
type Tables map[string]*columnar.Store

// Get returns a table's store.
func (t Tables) Get(name string) (*columnar.Store, error) {
	s, ok := t[name]
	if !ok {
		return nil, fmt.Errorf("analytics: no table %q", name)
	}
	return s, nil
}

// Scope narrows a report.
type Scope struct {
	Tenant canon.TenantID  `json:"tenant_id"`
	Stores []canon.StoreID `json:"stores,omitempty"`
	From   time.Time       `json:"from"`
	To     time.Time       `json:"to"`
	// Zone aligns any time bucketing to local days.
	Zone string `json:"zone,omitempty"`
}

// filters builds the tenant and store predicates every report shares.
func (s Scope) filters(storeColumn string) []columnar.Filter {
	out := []columnar.Filter{{Column: "tenant_id", Op: columnar.OpEq, Value: string(s.Tenant)}}
	if len(s.Stores) > 0 {
		vals := make([]any, 0, len(s.Stores))
		for _, st := range s.Stores {
			vals = append(vals, string(st))
		}
		out = append(out, columnar.Filter{Column: storeColumn, Op: columnar.OpIn, Values: vals})
	}
	return out
}

// ---------------------------------------------------------------------------
// Price elasticity curve
// ---------------------------------------------------------------------------

// ElasticityPoint is one observed price level and what sold at it.
type ElasticityPoint struct {
	PriceMinor   float64 `json:"price_minor"`
	Days         int64   `json:"days"`
	UnitsPerDay  float64 `json:"units_per_day"`
	MarginPerDay float64 `json:"margin_per_day"`
}

// ElasticityCurve is the observed price/quantity relationship for one SKU.
type ElasticityCurve struct {
	SKU    canon.SKU         `json:"sku"`
	Scope  Scope             `json:"scope"`
	Points []ElasticityPoint `json:"points"`
	// DistinctPrices is how many price levels the curve rests on. Two is the
	// arithmetic minimum for a slope and is not enough for anybody to act on;
	// the field is here so a reader can see that before reading the curve.
	DistinctPrices int `json:"distinct_prices"`
	// Caveat states what the curve is not.
	Caveat string              `json:"caveat"`
	Stats  columnar.QueryStats `json:"stats"`
}

// PriceElasticityCurve reads the observed price/quantity pairs for a SKU.
//
// It deliberately does not fit an elasticity. That is the pricing service's job,
// where the fit comes with a confidence interval and a refusal when the evidence
// is thin; a curve returned from here with a slope attached would be the same
// number without the honesty. This report answers "what have we charged and what
// happened", which is the input to that fit and is also the chart a category
// manager actually wants to look at.
func PriceElasticityCurve(tables Tables, scope Scope, sku canon.SKU) (ElasticityCurve, error) {
	store, err := tables.Get(domain.TablePrices)
	if err != nil {
		return ElasticityCurve{}, err
	}
	filters := append(scope.filters("store_id"),
		columnar.Filter{Column: "sku", Op: columnar.OpEq, Value: string(sku)})

	res, err := store.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: filters,
		GroupBy: []string{"price_minor"},
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggSum, Column: "units_sold", As: "units"},
			{Func: columnar.AggAvg, Column: "price_minor", As: "price"},
			{Func: columnar.AggAvg, Column: "unit_cost_minor", As: "cost"},
			{Func: columnar.AggCount, As: "days"},
		},
		OrderBy: "price", Ascending: true,
	})
	if err != nil {
		return ElasticityCurve{}, err
	}

	curve := ElasticityCurve{SKU: sku, Scope: scope, Stats: res.Stats}
	for _, row := range res.Rows {
		days := int64(row.Values["days"])
		if days == 0 {
			continue
		}
		unitsPerDay := row.Values["units"] / float64(days)
		curve.Points = append(curve.Points, ElasticityPoint{
			PriceMinor:   row.Values["price"],
			Days:         days,
			UnitsPerDay:  unitsPerDay,
			MarginPerDay: (row.Values["price"] - row.Values["cost"]) * unitsPerDay,
		})
	}
	curve.DistinctPrices = len(curve.Points)
	sort.Slice(curve.Points, func(i, j int) bool { return curve.Points[i].PriceMinor < curve.Points[j].PriceMinor })

	switch {
	case curve.DistinctPrices < 2:
		curve.Caveat = "only one price level was observed, so no relationship between price and volume " +
			"is visible at all"
	case curve.DistinctPrices < 4:
		curve.Caveat = fmt.Sprintf("%d price levels is enough to draw a line and not enough to trust it; "+
			"the pricing service will refuse to act on a fit this thin", curve.DistinctPrices)
	default:
		curve.Caveat = "observed price and volume, not a causal elasticity: prices were set in response to " +
			"conditions that also moved volume, so the slope here is a correlation"
	}
	return curve, nil
}

// ---------------------------------------------------------------------------
// Promotion lift
// ---------------------------------------------------------------------------

// PromotionLift is the measured effect of a promotion, read from the price
// table's outcome columns.
type PromotionLift struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	Scope       Scope             `json:"scope"`
	// PreUnitsPerDay, DuringUnitsPerDay and PostUnitsPerDay are the three
	// periods' daily rates.
	PreUnitsPerDay    float64 `json:"pre_units_per_day"`
	DuringUnitsPerDay float64 `json:"during_units_per_day"`
	PostUnitsPerDay   float64 `json:"post_units_per_day"`
	// UnitLiftPct and MarginLiftPct are during against pre.
	UnitLiftPct   float64 `json:"unit_lift_pct"`
	MarginLiftPct float64 `json:"margin_lift_pct"`
	// PostDipPct is the pull-forward.
	PostDipPct float64 `json:"post_dip_pct"`
	// IncrementalUnits nets the dip out of the lift.
	IncrementalUnits float64 `json:"incremental_units"`
	// SKUs is how many products the promotion touched.
	SKUs int `json:"skus"`
	// Caveats are the reasons to distrust the numbers.
	Caveats []string            `json:"caveats,omitempty"`
	Stats   columnar.QueryStats `json:"stats"`
}

// PromotionLiftReport measures a promotion from the price table.
//
// The during period is identified by the promotion_id column rather than by a
// date range, because a promotion activates store by store on local clocks and a
// single date range would count a store's pre-period as during in the west and
// during as pre in the east.
func PromotionLiftReport(tables Tables, scope Scope, promo canon.PromotionID,
	start, end time.Time) (PromotionLift, error) {

	store, err := tables.Get(domain.TablePrices)
	if err != nil {
		return PromotionLift{}, err
	}
	span := end.Sub(start)
	if span <= 0 {
		span = 24 * time.Hour
	}

	period := func(from, to time.Time, promoFilter string) (units, margin float64, days int64, skus int, stats columnar.QueryStats, err error) {
		filters := scope.filters("store_id")
		if promoFilter != "" {
			filters = append(filters, columnar.Filter{
				Column: "promotion_id", Op: columnar.OpEq, Value: promoFilter})
		}
		res, qerr := store.Query(columnar.Query{
			From: from, To: to, Filters: filters,
			Bucket: 24 * time.Hour, BucketZone: scope.Zone,
			Aggregates: []columnar.Aggregate{
				{Func: columnar.AggSum, Column: "units_sold", As: "units"},
				{Func: columnar.AggAvg, Column: "price_minor", As: "price"},
				{Func: columnar.AggAvg, Column: "unit_cost_minor", As: "cost"},
				{Func: columnar.AggCountDistinct, Column: "sku", As: "skus"},
			},
		})
		if qerr != nil {
			return 0, 0, 0, 0, columnar.QueryStats{}, qerr
		}
		maxSKUs := 0
		for _, row := range res.Rows {
			units += row.Values["units"]
			margin += (row.Values["price"] - row.Values["cost"]) * row.Values["units"]
			days++
			if int(row.Values["skus"]) > maxSKUs {
				maxSKUs = int(row.Values["skus"])
			}
		}
		return units, margin, days, maxSKUs, res.Stats, nil
	}

	preUnits, preMargin, preDays, _, preStats, err := period(start.Add(-span), start, "")
	if err != nil {
		return PromotionLift{}, err
	}
	durUnits, durMargin, durDays, durSKUs, _, err := period(start, end, string(promo))
	if err != nil {
		return PromotionLift{}, err
	}
	postUnits, _, postDays, _, _, err := period(end, end.Add(span), "")
	if err != nil {
		return PromotionLift{}, err
	}

	out := PromotionLift{PromotionID: promo, Scope: scope, SKUs: durSKUs, Stats: preStats}
	rate := func(total float64, days int64) float64 {
		if days == 0 {
			return 0
		}
		return total / float64(days)
	}
	out.PreUnitsPerDay = rate(preUnits, preDays)
	out.DuringUnitsPerDay = rate(durUnits, durDays)
	out.PostUnitsPerDay = rate(postUnits, postDays)
	out.UnitLiftPct = pctChange(out.PreUnitsPerDay, out.DuringUnitsPerDay)
	out.MarginLiftPct = pctChange(rate(preMargin, preDays), rate(durMargin, durDays))
	out.PostDipPct = pctChange(out.PreUnitsPerDay, out.PostUnitsPerDay)
	out.IncrementalUnits = (out.DuringUnitsPerDay-out.PreUnitsPerDay)*float64(durDays) -
		(out.PreUnitsPerDay-out.PostUnitsPerDay)*float64(postDays)

	if preDays == 0 {
		out.Caveats = append(out.Caveats, "no baseline data: the lift percentages are not meaningful")
	}
	if postDays == 0 {
		out.Caveats = append(out.Caveats,
			"no post-promotion data yet, so the incremental figure does not account for pull-forward "+
				"and overstates the effect")
	}
	out.Caveats = append(out.Caveats,
		"no control group: everything else that moved in this period — weather, seasonality, a "+
			"competitor's promotion — is attributed to this one")
	return out, nil
}

// ---------------------------------------------------------------------------
// Label interaction
// ---------------------------------------------------------------------------

// InteractionRow is one grouped interaction figure.
type InteractionRow struct {
	Group map[string]string `json:"group"`
	// Taps is the number of NFC taps in the period.
	Taps float64 `json:"taps"`
	// Labels is how many labels contributed.
	Labels float64 `json:"labels"`
	// TapsPerLabel normalises for fleet size, which is the only way a big store
	// and a small one are comparable.
	TapsPerLabel float64 `json:"taps_per_label"`
	// BucketStart is present when the report was bucketed.
	BucketStart *time.Time `json:"bucket_start,omitempty"`
}

// InteractionReport is shelf-edge engagement.
type InteractionReport struct {
	Scope Scope            `json:"scope"`
	Rows  []InteractionRow `json:"rows"`
	// Caveat records what a tap does and does not measure.
	Caveat string              `json:"caveat"`
	Stats  columnar.QueryStats `json:"stats"`
}

// LabelInteraction reports NFC tap activity.
//
// # What a tap count actually is
//
// The label reports a cumulative counter, so the taps in a period are the
// *difference* between the first and last reading — max minus min, which is
// exact for a monotonic counter and robust to a missed report. Summing the
// counter instead would report a number tens of thousands of times too large,
// which is the mistake this comment exists to prevent.
func LabelInteraction(tables Tables, scope Scope, groupBy []string, bucket time.Duration) (InteractionReport, error) {
	store, err := tables.Get(domain.TableTelemetry)
	if err != nil {
		return InteractionReport{}, err
	}
	res, err := store.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: scope.filters("store_id"),
		GroupBy: append([]string{"label_id"}, groupBy...),
		Bucket:  bucket, BucketZone: scope.Zone,
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggMin, Column: "nfc_tap_count", As: "first"},
			{Func: columnar.AggMax, Column: "nfc_tap_count", As: "last"},
		},
		Limit: columnar.DefaultQueryLimit,
	})
	if err != nil {
		return InteractionReport{}, err
	}

	// Roll the per-label differences up to the requested grouping.
	type key struct {
		group  string
		bucket string
	}
	agg := map[key]*InteractionRow{}
	order := make([]key, 0, 64)
	for _, row := range res.Rows {
		g := map[string]string{}
		parts := make([]string, 0, len(groupBy))
		for _, name := range groupBy {
			g[name] = row.Group[name]
			parts = append(parts, row.Group[name])
		}
		k := key{group: joinKey(parts)}
		if row.BucketStart != nil {
			k.bucket = row.BucketStart.UTC().Format(time.RFC3339)
		}
		cur, ok := agg[k]
		if !ok {
			cur = &InteractionRow{Group: g, BucketStart: row.BucketStart}
			agg[k] = cur
			order = append(order, k)
		}
		cur.Taps += row.Values["last"] - row.Values["first"]
		cur.Labels++
	}

	out := InteractionReport{Scope: scope, Stats: res.Stats,
		Caveat: "an NFC tap is a shopper deliberately holding a phone to a label. It measures " +
			"engagement with the label, not with the product, and only from shoppers whose " +
			"phone has NFC enabled.",
	}
	for _, k := range order {
		r := agg[k]
		if r.Labels > 0 {
			r.TapsPerLabel = r.Taps / r.Labels
		}
		out.Rows = append(out.Rows, *r)
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].Taps > out.Rows[j].Taps })
	return out, nil
}

func joinKey(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

// ---------------------------------------------------------------------------
// Competitive price position
// ---------------------------------------------------------------------------

// CompetitiveRow is one product's position against the tracked competitor.
type CompetitiveRow struct {
	Group map[string]string `json:"group"`
	// OurPrice and CompetitorPrice are the period averages.
	OurPrice        float64 `json:"our_price_minor"`
	CompetitorPrice float64 `json:"competitor_price_minor"`
	// IndexPct is our price as a percentage of theirs. A hundred is parity,
	// below is cheaper.
	IndexPct float64 `json:"index_pct"`
	// Observations is how many priced days the figures rest on.
	Observations float64 `json:"observations"`
}

// CompetitiveReport is the price position.
type CompetitiveReport struct {
	Scope Scope            `json:"scope"`
	Rows  []CompetitiveRow `json:"rows"`
	// OverallIndexPct is the volume-weighted position across everything in
	// scope. Weighting by volume rather than by SKU count is what stops a
	// thousand slow-moving lines drowning out the twenty that customers price
	// against.
	OverallIndexPct float64             `json:"overall_index_pct"`
	Tracked         int                 `json:"tracked_products"`
	Stats           columnar.QueryStats `json:"stats"`
}

// CompetitivePosition reports where the retailer sits against tracked
// competitor prices.
func CompetitivePosition(tables Tables, scope Scope, groupBy []string) (CompetitiveReport, error) {
	store, err := tables.Get(domain.TablePrices)
	if err != nil {
		return CompetitiveReport{}, err
	}
	filters := append(scope.filters("store_id"),
		// A zero competitor price means untracked, not free. Filtering it out
		// here is what stops an untracked line reporting an index of infinity.
		columnar.Filter{Column: "competitor_price_minor", Op: columnar.OpGt, Value: 0.0})
	if len(groupBy) == 0 {
		groupBy = []string{"sku"}
	}
	res, err := store.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: filters,
		GroupBy: groupBy,
		Aggregates: []columnar.Aggregate{
			{Func: columnar.AggAvg, Column: "price_minor", As: "ours"},
			{Func: columnar.AggAvg, Column: "competitor_price_minor", As: "theirs"},
			{Func: columnar.AggSum, Column: "units_sold", As: "units"},
			{Func: columnar.AggCount, As: "obs"},
		},
	})
	if err != nil {
		return CompetitiveReport{}, err
	}

	out := CompetitiveReport{Scope: scope, Stats: res.Stats, Tracked: len(res.Rows)}
	var weighted, weight float64
	for _, row := range res.Rows {
		theirs := row.Values["theirs"]
		if theirs <= 0 {
			continue
		}
		index := 100 * row.Values["ours"] / theirs
		out.Rows = append(out.Rows, CompetitiveRow{
			Group: row.Group, OurPrice: row.Values["ours"], CompetitorPrice: theirs,
			IndexPct: index, Observations: row.Values["obs"],
		})
		w := row.Values["units"]
		if w <= 0 {
			w = 1
		}
		weighted += index * w
		weight += w
	}
	if weight > 0 {
		out.OverallIndexPct = weighted / weight
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].IndexPct > out.Rows[j].IndexPct })
	return out, nil
}

// ---------------------------------------------------------------------------
// Shrinkage and pricing delay
// ---------------------------------------------------------------------------

// ShrinkageBucket is one band of pricing delay and the waste observed in it.
type ShrinkageBucket struct {
	// DelayFromSeconds and DelayToSeconds bound the band.
	DelayFromSeconds float64 `json:"delay_from_seconds"`
	DelayToSeconds   float64 `json:"delay_to_seconds"`
	// Observations, Units and Waste are the totals in the band.
	Observations float64 `json:"observations"`
	Units        float64 `json:"units"`
	WasteUnits   float64 `json:"waste_units"`
	// WasteRatePct is waste as a percentage of units moved.
	WasteRatePct float64 `json:"waste_rate_pct"`
}

// ShrinkageReport is the waste-against-delay analysis.
type ShrinkageReport struct {
	Scope   Scope             `json:"scope"`
	Buckets []ShrinkageBucket `json:"buckets"`
	// Correlation is the Pearson correlation between the band's midpoint delay
	// and its waste rate, weighted by observations.
	Correlation float64 `json:"correlation"`
	// Interpretation states plainly what the correlation is and is not.
	Interpretation string              `json:"interpretation"`
	Stats          columnar.QueryStats `json:"stats"`
}

// DelayBands are the pricing-delay bands the shrinkage report uses.
//
// They are logarithmic because the interesting variation is at the fast end:
// the difference between a three-second markdown and a three-minute one matters
// far more to a short-dated product than the difference between two hours and
// three. Equal-width bands would put almost every observation in the first one.
var DelayBands = []float64{0, 5, 30, 300, 3600, 86400, math.Inf(1)}

// ShrinkageAgainstPricingDelay is the report the platform's business case rests
// on: does a shelf that repriced faster waste less.
//
// # What it can and cannot show
//
// It is observational. Stores with faster pricing are also stores with newer
// hardware, better connectivity and, quite possibly, better managers, and every
// one of those also reduces waste. The correlation is real and the causal claim
// is not established by it; the Interpretation field says so in the report
// itself rather than in a footnote nobody reads, because this is the number a
// salesperson will quote.
func ShrinkageAgainstPricingDelay(tables Tables, scope Scope) (ShrinkageReport, error) {
	store, err := tables.Get(domain.TablePrices)
	if err != nil {
		return ShrinkageReport{}, err
	}
	out := ShrinkageReport{Scope: scope}

	for i := 0; i+1 < len(DelayBands); i++ {
		lo, hi := DelayBands[i], DelayBands[i+1]
		filters := append(scope.filters("store_id"),
			columnar.Filter{Column: "price_delay_seconds", Op: columnar.OpGte, Value: lo})
		if !math.IsInf(hi, 1) {
			filters = append(filters,
				columnar.Filter{Column: "price_delay_seconds", Op: columnar.OpLt, Value: hi})
		}
		res, err := store.Query(columnar.Query{
			From: scope.From, To: scope.To, Filters: filters,
			Aggregates: []columnar.Aggregate{
				{Func: columnar.AggSum, Column: "units_sold", As: "units"},
				{Func: columnar.AggSum, Column: "waste_units", As: "waste"},
				{Func: columnar.AggCount, As: "obs"},
			},
		})
		if err != nil {
			return ShrinkageReport{}, err
		}
		out.Stats.BlocksScanned += res.Stats.BlocksScanned
		out.Stats.BlocksSkipped += res.Stats.BlocksSkipped
		out.Stats.RowsScanned += res.Stats.RowsScanned
		out.Stats.RowsMatched += res.Stats.RowsMatched
		if len(res.Rows) == 0 {
			continue
		}
		v := res.Rows[0].Values
		b := ShrinkageBucket{
			DelayFromSeconds: lo, DelayToSeconds: hi,
			Observations: v["obs"], Units: v["units"], WasteUnits: v["waste"],
		}
		if b.Units > 0 {
			b.WasteRatePct = 100 * b.WasteUnits / b.Units
		}
		out.Buckets = append(out.Buckets, b)
	}

	out.Correlation = weightedCorrelation(out.Buckets)
	switch {
	case len(out.Buckets) < 3:
		out.Interpretation = "too few delay bands have data to say anything about the relationship"
	case out.Correlation > 0.5:
		out.Interpretation = fmt.Sprintf(
			"waste rises with pricing delay (r = %.2f across %d bands). This is an observational "+
				"correlation, not a causal estimate: stores that reprice faster differ from stores "+
				"that reprice slowly in other ways that also affect waste.", out.Correlation, len(out.Buckets))
	case out.Correlation < -0.5:
		out.Interpretation = fmt.Sprintf(
			"waste falls as pricing delay rises (r = %.2f), which is the opposite of the expected "+
				"direction and usually means the slow-repricing lines are the ambient ones that do "+
				"not spoil", out.Correlation)
	default:
		out.Interpretation = fmt.Sprintf(
			"no clear relationship between pricing delay and waste in this data (r = %.2f)", out.Correlation)
	}
	return out, nil
}

// weightedCorrelation is the Pearson correlation between band midpoint and
// waste rate, weighted by observations.
//
// The midpoint of an unbounded top band would be infinite, so it is replaced by
// twice its lower bound — a choice that matters only for the top band's leverage
// and is documented rather than hidden.
func weightedCorrelation(buckets []ShrinkageBucket) float64 {
	if len(buckets) < 3 {
		return 0
	}
	var sw, sx, sy float64
	xs := make([]float64, len(buckets))
	for i, b := range buckets {
		mid := (b.DelayFromSeconds + b.DelayToSeconds) / 2
		if math.IsInf(b.DelayToSeconds, 1) {
			mid = b.DelayFromSeconds * 2
		}
		// Log delay, because the bands are logarithmic and a correlation on the
		// raw seconds would be dominated by the top band.
		xs[i] = math.Log1p(mid)
		w := b.Observations
		if w <= 0 {
			w = 1
		}
		sw += w
		sx += w * xs[i]
		sy += w * b.WasteRatePct
	}
	if sw == 0 {
		return 0
	}
	mx, my := sx/sw, sy/sw
	var sxy, sxx, syy float64
	for i, b := range buckets {
		w := b.Observations
		if w <= 0 {
			w = 1
		}
		dx, dy := xs[i]-mx, b.WasteRatePct-my
		sxy += w * dx * dy
		sxx += w * dx * dx
		syy += w * dy * dy
	}
	if sxx <= 0 || syy <= 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}

// ---------------------------------------------------------------------------
// Cross-store benchmarking
// ---------------------------------------------------------------------------

// BenchmarkRow is one store against the estate.
type BenchmarkRow struct {
	StoreID canon.StoreID `json:"store_id"`
	// Value is the store's figure for the benchmarked metric.
	Value float64 `json:"value"`
	// Percentile is where it sits in the estate, 0 worst to 100 best. Direction
	// is set by HigherIsBetter.
	Percentile float64 `json:"percentile"`
	// VersusMedianPct is how far it is from the estate median.
	VersusMedianPct float64 `json:"versus_median_pct"`
	Rows            float64 `json:"rows"`
}

// BenchmarkReport ranks stores.
type BenchmarkReport struct {
	Scope  Scope  `json:"scope"`
	Metric string `json:"metric"`
	// HigherIsBetter records the direction, because "worst" means the top of
	// the list for latency and the bottom of it for sales.
	HigherIsBetter bool           `json:"higher_is_better"`
	Median         float64        `json:"median"`
	P10            float64        `json:"p10"`
	P90            float64        `json:"p90"`
	Rows           []BenchmarkRow `json:"rows"`
	// Outliers names the stores beyond the tenth and ninetieth percentiles,
	// which is the list a regional manager actually works from.
	Worst []canon.StoreID     `json:"worst,omitempty"`
	Best  []canon.StoreID     `json:"best,omitempty"`
	Stats columnar.QueryStats `json:"stats"`
}

// CrossStoreBenchmark ranks every store in scope on one aggregate.
func CrossStoreBenchmark(tables Tables, scope Scope, table string,
	agg columnar.Aggregate, higherIsBetter bool) (BenchmarkReport, error) {

	store, err := tables.Get(table)
	if err != nil {
		return BenchmarkReport{}, err
	}
	if agg.As == "" {
		agg.As = "metric"
	}
	res, err := store.Query(columnar.Query{
		From: scope.From, To: scope.To, Filters: scope.filters("store_id"),
		GroupBy:    []string{"store_id"},
		Aggregates: []columnar.Aggregate{agg, {Func: columnar.AggCount, As: "rows"}},
		OrderBy:    agg.As, Ascending: !higherIsBetter,
	})
	if err != nil {
		return BenchmarkReport{}, err
	}

	out := BenchmarkReport{
		Scope: scope, Metric: agg.As, HigherIsBetter: higherIsBetter, Stats: res.Stats,
	}
	values := make([]float64, 0, len(res.Rows))
	for _, row := range res.Rows {
		values = append(values, row.Values[agg.As])
	}
	if len(values) == 0 {
		return out, nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	out.Median = quantileOf(sorted, 0.5)
	out.P10 = quantileOf(sorted, 0.1)
	out.P90 = quantileOf(sorted, 0.9)

	for _, row := range res.Rows {
		v := row.Values[agg.As]
		pct := 100 * float64(sort.SearchFloat64s(sorted, v)) / float64(len(sorted))
		if !higherIsBetter {
			pct = 100 - pct
		}
		br := BenchmarkRow{
			StoreID: canon.StoreID(row.Group["store_id"]), Value: v,
			Percentile: pct, Rows: row.Values["rows"],
		}
		if out.Median != 0 {
			br.VersusMedianPct = 100 * (v - out.Median) / out.Median
		}
		out.Rows = append(out.Rows, br)
		switch {
		case higherIsBetter && v <= out.P10, !higherIsBetter && v >= out.P90:
			out.Worst = append(out.Worst, br.StoreID)
		case higherIsBetter && v >= out.P90, !higherIsBetter && v <= out.P10:
			out.Best = append(out.Best, br.StoreID)
		}
	}
	return out, nil
}

func quantileOf(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// pctChange is the percentage change from a to b, with a zero baseline
// returning zero rather than an infinity that would poison every average
// computed over it.
func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return 100 * (to - from) / from
}
