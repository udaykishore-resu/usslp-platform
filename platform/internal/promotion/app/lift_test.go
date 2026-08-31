package app

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

var day0 = time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC) // a Monday

// synthSales builds a flat daily series for one store: the same units every
// day at the same price, so the arithmetic in a lift calculation can be checked
// by hand.
func synthSales(store canon.StoreID, from time.Time, days int, units, priceMinor, costMinor float64) []SalesPoint {
	out := make([]SalesPoint, 0, days)
	for d := 0; d < days; d++ {
		out = append(out, SalesPoint{
			StoreID: store, SKU: "sku-1", Day: from.AddDate(0, 0, d),
			Units: units, RevenueMinor: units * priceMinor, CostMinor: units * costMinor,
		})
	}
	return out
}

// TestLiftMatchesAHandComputedExpectation uses a series simple enough that
// every reported number can be derived on paper.
func TestLiftMatchesAHandComputedExpectation(t *testing.T) {
	// Seven days before at 10 units a day at 200, seven during at 25 a day at
	// 150, seven after at 6 a day at 200. Cost is 100 throughout.
	pre := synthSales("store-1", day0, 7, 10, 200, 100)
	during := synthSales("store-1", day0.AddDate(0, 0, 7), 7, 25, 150, 100)
	post := synthSales("store-1", day0.AddDate(0, 0, 14), 7, 6, 200, 100)
	sales := append(append(append([]SalesPoint{}, pre...), during...), post...)

	w := DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14))
	res := MeasureLift("promo-1", "store-1", sales, w)

	// Units per day: 10 before, 25 during, 6 after.
	if res.Pre.UnitsPerDay != 10 || res.During.UnitsPerDay != 25 || res.Post.UnitsPerDay != 6 {
		t.Fatalf("daily rates = %v / %v / %v, want 10 / 25 / 6",
			res.Pre.UnitsPerDay, res.During.UnitsPerDay, res.Post.UnitsPerDay)
	}
	// Unit lift is (25 - 10) / 10 = 150%.
	if math.Abs(res.UnitLiftPct-150) > 1e-9 {
		t.Errorf("unit lift = %.4f%%, want 150%%", res.UnitLiftPct)
	}
	// Revenue per day: 2000 before, 3750 during, so +87.5%.
	if math.Abs(res.RevenueLiftPct-87.5) > 1e-9 {
		t.Errorf("revenue lift = %.4f%%, want 87.5%%", res.RevenueLiftPct)
	}
	// Margin per day: 10*(200-100) = 1000 before, 25*(150-100) = 1250 during,
	// so +25%. The gap between a 150% unit lift and a 25% margin lift is the
	// whole reason both are reported.
	if math.Abs(res.MarginLiftPct-25) > 1e-9 {
		t.Errorf("margin lift = %.4f%%, want 25%%", res.MarginLiftPct)
	}
	// Post dip: (6 - 10) / 10 = -40%.
	if math.Abs(res.PostDipPct-(-40)) > 1e-9 {
		t.Errorf("post dip = %.4f%%, want -40%%", res.PostDipPct)
	}
	// Incremental units: (25-10)*7 during, minus (10-6)*7 lost after = 105 - 28
	// = 77, against a headline of 105.
	if math.Abs(res.IncrementalUnits-77) > 1e-9 {
		t.Errorf("incremental units = %.4f, want 77", res.IncrementalUnits)
	}
	// Incremental margin: (1250-1000)*7 - (1000-600)*7 = 1750 - 2800 = -1050.
	// The promotion sold more and made less, which is the finding that matters.
	if math.Abs(res.IncrementalMarginMinor-(-1050)) > 1e-9 {
		t.Errorf("incremental margin = %.4f, want -1050", res.IncrementalMarginMinor)
	}
	if res.During.AverageSellingPri != 150 {
		t.Errorf("average selling price during = %v, want 150", res.During.AverageSellingPri)
	}
	t.Logf("synthetic series: units +%.0f%%, revenue +%.1f%%, margin +%.0f%%, post dip %.0f%%, "+
		"incremental %.0f units and %.0f minor units of margin",
		res.UnitLiftPct, res.RevenueLiftPct, res.MarginLiftPct, res.PostDipPct,
		res.IncrementalUnits, res.IncrementalMarginMinor)
}

// TestPullForwardIsNettedOut is the case a two-period comparison gets wrong: a
// promotion that sold a fortnight of stock in a week created nothing.
func TestPullForwardIsNettedOut(t *testing.T) {
	pre := synthSales("s1", day0, 7, 10, 200, 100)
	during := synthSales("s1", day0.AddDate(0, 0, 7), 7, 20, 150, 100)
	// The following week sells nothing: everybody bought during the promotion.
	post := synthSales("s1", day0.AddDate(0, 0, 14), 7, 0, 200, 100)
	sales := append(append(append([]SalesPoint{}, pre...), during...), post...)

	res := MeasureLift("p", "s1", sales, DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
	if res.UnitLiftPct <= 0 {
		t.Fatalf("the headline lift is %.1f%%, expected strongly positive", res.UnitLiftPct)
	}
	// 70 extra units during, 70 lost after: exactly nothing incremental.
	if math.Abs(res.IncrementalUnits) > 1e-9 {
		t.Errorf("incremental units = %.4f, want 0: everything was pulled forward", res.IncrementalUnits)
	}
	t.Logf("synthetic pull-forward: headline lift %.0f%%, incremental units %.0f",
		res.UnitLiftPct, res.IncrementalUnits)
}

func TestClosedDaysDoNotDiluteTheDailyRate(t *testing.T) {
	// A store that traded on five of seven days. Its rate is per trading day,
	// not per calendar day, or the comparison is biased by opening hours.
	sales := synthSales("s1", day0, 5, 10, 200, 100)
	w := LiftWindow{PreStart: day0, PreEnd: day0.AddDate(0, 0, 7)}
	res := MeasureLift("p", "s1", sales, w)
	if res.Pre.Days != 5 {
		t.Errorf("counted %d days, want the 5 with data", res.Pre.Days)
	}
	if res.Pre.UnitsPerDay != 10 {
		t.Errorf("rate = %v, want 10 (50 units over 5 trading days)", res.Pre.UnitsPerDay)
	}
}

// TestControlGroupSeparatesSeasonalityFromThePromotion is the case the
// difference-in-differences estimate exists for.
func TestControlGroupSeparatesSeasonalityFromThePromotion(t *testing.T) {
	// Both groups rise: the control by 50% (weather, a school holiday), the
	// test by 150%. The promotion caused the extra 100 points, not all 150.
	test := append(
		synthSales("t1", day0, 7, 10, 200, 100),
		synthSales("t1", day0.AddDate(0, 0, 7), 7, 25, 150, 100)...)
	control := append(
		synthSales("c1", day0, 7, 10, 200, 100),
		synthSales("c1", day0.AddDate(0, 0, 7), 7, 15, 200, 100)...)
	// Four control stores' worth of data, but one store's rate, so the baseline
	// comparison is per store.
	for _, id := range []canon.StoreID{"c2", "c3", "c4"} {
		control = append(control, synthSales(id, day0, 7, 10, 200, 100)...)
		control = append(control, synthSales(id, day0.AddDate(0, 0, 7), 7, 15, 200, 100)...)
	}

	w := DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14))
	res := MeasureLiftWithControl("p", "all", test, control, 4, w)
	if res.Control == nil {
		t.Fatal("no control comparison")
	}
	if math.Abs(res.UnitLiftPct-150) > 1e-9 {
		t.Errorf("test lift = %.2f%%, want 150%%", res.UnitLiftPct)
	}
	if math.Abs(res.Control.ControlLiftPct-50) > 1e-9 {
		t.Errorf("control lift = %.2f%%, want 50%%", res.Control.ControlLiftPct)
	}
	if math.Abs(res.Control.DiffInDiffPct-100) > 1e-9 {
		t.Errorf("difference in differences = %.2f%%, want 100%%", res.Control.DiffInDiffPct)
	}
	if !res.Control.Trustworthy {
		t.Errorf("the comparison was disowned: %s", res.Control.Reason)
	}
	if !strings.Contains(res.Method, "difference-in-differences") {
		t.Errorf("method = %q", res.Method)
	}
	t.Logf("synthetic: test +%.0f%%, control +%.0f%%, difference-in-differences +%.0f%% "+
		"(baseline divergence %.1f%%)",
		res.UnitLiftPct, res.Control.ControlLiftPct, res.Control.DiffInDiffPct,
		res.Control.BaselineDivergencePct)
}

func TestControlGroupIsDisownedWhenItIsNotComparable(t *testing.T) {
	t.Run("too few control stores", func(t *testing.T) {
		test := synthSales("t1", day0, 14, 10, 200, 100)
		control := synthSales("c1", day0, 14, 10, 200, 100)
		res := MeasureLiftWithControl("p", "all", test, control, 1,
			DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
		if res.Control.Trustworthy {
			t.Error("a one-store control was trusted")
		}
		if !strings.Contains(res.Control.Reason, "too small") {
			t.Errorf("reason = %q", res.Control.Reason)
		}
	})

	t.Run("groups that were never comparable", func(t *testing.T) {
		// The control sells four times as much per store before the promotion:
		// these are different businesses.
		test := append(
			synthSales("t1", day0, 7, 10, 200, 100),
			synthSales("t1", day0.AddDate(0, 0, 7), 7, 25, 150, 100)...)
		var control []SalesPoint
		for _, id := range []canon.StoreID{"c1", "c2", "c3"} {
			control = append(control, synthSales(id, day0, 7, 40, 200, 100)...)
			control = append(control, synthSales(id, day0.AddDate(0, 0, 7), 7, 45, 200, 100)...)
		}
		res := MeasureLiftWithControl("p", "all", test, control, 3,
			DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
		if res.Control.Trustworthy {
			t.Errorf("incomparable groups were trusted (divergence %.1f%%)", res.Control.BaselineDivergencePct)
		}
		if !strings.Contains(res.Control.Reason, "not comparable") {
			t.Errorf("reason = %q", res.Control.Reason)
		}
	})
}

func TestCaveatsAreStatedRatherThanBuried(t *testing.T) {
	t.Run("no post period yet", func(t *testing.T) {
		sales := append(
			synthSales("s1", day0, 7, 10, 200, 100),
			synthSales("s1", day0.AddDate(0, 0, 7), 7, 25, 150, 100)...)
		res := MeasureLift("p", "s1", sales, DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
		found := false
		for _, c := range res.Caveats {
			if strings.Contains(c, "pull-forward") {
				found = true
			}
		}
		if !found {
			t.Errorf("caveats = %v, want a note about the missing post period", res.Caveats)
		}
	})

	t.Run("a very short baseline", func(t *testing.T) {
		sales := append(
			synthSales("s1", day0, 3, 10, 200, 100),
			synthSales("s1", day0.AddDate(0, 0, 3), 3, 25, 150, 100)...)
		res := MeasureLift("p", "s1", sales,
			DefaultLiftWindow(day0.AddDate(0, 0, 3), day0.AddDate(0, 0, 6)))
		found := false
		for _, c := range res.Caveats {
			if strings.Contains(c, "single unusual day") {
				found = true
			}
		}
		if !found {
			t.Errorf("caveats = %v, want a note about the short baseline", res.Caveats)
		}
	})
}

func TestPercentChangeFromZeroIsNotInfinite(t *testing.T) {
	// A SKU that sold nothing before the promotion. An infinite lift in a
	// report poisons every average computed over it.
	sales := synthSales("s1", day0.AddDate(0, 0, 7), 7, 25, 150, 100)
	res := MeasureLift("p", "s1", sales, DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
	if math.IsInf(res.UnitLiftPct, 0) || math.IsNaN(res.UnitLiftPct) {
		t.Errorf("unit lift = %v", res.UnitLiftPct)
	}
	if res.During.Units != 175 {
		t.Errorf("the absolute figures must still be there: %v", res.During.Units)
	}
}

func TestClusterLiftSplitsByStoreGroup(t *testing.T) {
	var sales []SalesPoint
	sales = append(sales, synthSales("conv-1", day0, 7, 10, 200, 100)...)
	sales = append(sales, synthSales("conv-1", day0.AddDate(0, 0, 7), 7, 12, 150, 100)...)
	sales = append(sales, synthSales("super-1", day0, 7, 100, 200, 100)...)
	sales = append(sales, synthSales("super-1", day0.AddDate(0, 0, 7), 7, 300, 150, 100)...)

	clusters := map[canon.StoreID]string{"conv-1": "convenience", "super-1": "superstore"}
	got := ClusterLift("p", sales, clusters, DefaultLiftWindow(day0.AddDate(0, 0, 7), day0.AddDate(0, 0, 14)))
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	// Sorted by name, so convenience comes first.
	if got[0].Scope != "convenience" || got[1].Scope != "superstore" {
		t.Fatalf("scopes = %q, %q", got[0].Scope, got[1].Scope)
	}
	if math.Abs(got[0].UnitLiftPct-20) > 1e-9 {
		t.Errorf("convenience lift = %.2f%%, want 20%%", got[0].UnitLiftPct)
	}
	if math.Abs(got[1].UnitLiftPct-200) > 1e-9 {
		t.Errorf("superstore lift = %.2f%%, want 200%%", got[1].UnitLiftPct)
	}
	t.Logf("synthetic: the same promotion lifted convenience by %.0f%% and superstores by %.0f%%",
		got[0].UnitLiftPct, got[1].UnitLiftPct)
}
