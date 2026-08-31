package domain

import (
	"fmt"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

func pct(id canon.PromotionID, priority int, off float64, stackable bool) Rule {
	r := ruleOf(TypePercentageOff, Params{PercentOff: off, Currency: "GBP"})
	r.ID = id
	r.Priority = priority
	r.Stackable = stackable
	r.Display.Badge = string(id)
	r.Display.LEDColor = "RED"
	return r
}

// TestPrecedenceIsPriorityThenCustomerThenSpecificity walks the documented
// policy one rung at a time, so a change to any rung fails exactly one case.
func TestPrecedenceIsPriorityThenCustomerThenSpecificity(t *testing.T) {
	p := product(1000)

	t.Run("priority wins first, even against a better price", func(t *testing.T) {
		high := pct("high", 200, 10, false) // leaves 900
		low := pct("low", 100, 50, false)   // leaves 500, better for the customer
		res, err := Resolve([]Rule{low, high}, p)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Winner != "high" {
			t.Errorf("winner = %s, want the higher priority", res.Winner)
		}
		if res.FinalPriceMinor != 900 {
			t.Errorf("price = %d, want 900", res.FinalPriceMinor)
		}
		if len(res.Suppressed) != 1 || res.Suppressed[0].PromotionID != "low" {
			t.Errorf("suppressed = %+v", res.Suppressed)
		}
	})

	t.Run("at equal priority the customer gets the better price", func(t *testing.T) {
		a := pct("a", 100, 10, false)
		b := pct("b", 100, 40, false)
		res, err := Resolve([]Rule{a, b}, p)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Winner != "b" || res.FinalPriceMinor != 600 {
			t.Errorf("winner %s at %d, want b at 600", res.Winner, res.FinalPriceMinor)
		}
	})

	t.Run("at equal priority and price the more specific rule wins", func(t *testing.T) {
		broad := pct("broad", 100, 20, false)
		broad.Conditions.Categories = []string{"dairy"}
		narrow := pct("narrow", 100, 20, false)
		narrow.Conditions.IncludeSKUs = []canon.SKU{"sku-1"}
		res, err := Resolve([]Rule{broad, narrow}, p)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Winner != "narrow" {
			t.Errorf("winner = %s, want the SKU-specific rule", res.Winner)
		}
	})

	t.Run("the final tie-break is stable", func(t *testing.T) {
		a := pct("aaa", 100, 20, false)
		b := pct("bbb", 100, 20, false)
		// Both orders must give the same winner, or two nodes evaluating the
		// same inputs would disagree about what a shelf shows.
		first, err := Resolve([]Rule{a, b}, p)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		second, err := Resolve([]Rule{b, a}, p)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if first.Winner != second.Winner {
			t.Errorf("the tie-break is order-dependent: %s then %s", first.Winner, second.Winner)
		}
		if first.Winner != "aaa" {
			t.Errorf("winner = %s, want the lower identifier", first.Winner)
		}
	})
}

func TestStackingCompoundsOnTheRunningPrice(t *testing.T) {
	p := product(1000)
	a := pct("a", 200, 20, true) // 1000 -> 800
	b := pct("b", 100, 10, true) // 800 -> 720, not 700

	res, err := Resolve([]Rule{a, b}, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("applied %d promotions, want both: %+v", len(res.Applied), res.Applied)
	}
	if res.FinalPriceMinor != 720 {
		t.Errorf("price = %d, want 720 (20%% then 10%% of the reduced price)", res.FinalPriceMinor)
	}
	if res.TotalDiscountMinor != 280 {
		t.Errorf("discount = %d, want 280", res.TotalDiscountMinor)
	}
	// The display belongs to the highest-precedence applied promotion.
	if res.Winner != "a" || res.Display.Badge != "a" {
		t.Errorf("display came from %s (%q), want a", res.Winner, res.Display.Badge)
	}
}

func TestANonStackableRuleEndsTheChain(t *testing.T) {
	p := product(1000)
	blocking := pct("blocking", 200, 20, false)
	other := pct("other", 100, 10, true)

	res, err := Resolve([]Rule{blocking, other}, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].PromotionID != "blocking" {
		t.Errorf("applied = %+v, want only the non-stackable rule", res.Applied)
	}
	if res.FinalPriceMinor != 800 {
		t.Errorf("price = %d, want 800", res.FinalPriceMinor)
	}
	if len(res.Suppressed) != 1 || res.Suppressed[0].BeatenBy != "blocking" {
		t.Errorf("suppressed = %+v, want the other rule beaten by the blocking one", res.Suppressed)
	}
}

func TestExclusiveGroupAdmitsOnlyOneEvenWhenStackable(t *testing.T) {
	p := product(1000)
	a := pct("a", 200, 20, true)
	a.ExclusiveGroup = "supplier-funded"
	b := pct("b", 100, 10, true)
	b.ExclusiveGroup = "supplier-funded"
	c := pct("c", 50, 5, true) // a different group entirely

	res, err := Resolve([]Rule{a, b, c}, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ids := map[canon.PromotionID]bool{}
	for _, ap := range res.Applied {
		ids[ap.PromotionID] = true
	}
	if !ids["a"] || ids["b"] || !ids["c"] {
		t.Errorf("applied %v, want a and c but not b", ids)
	}
	var reason string
	for _, s := range res.Suppressed {
		if s.PromotionID == "b" {
			reason = s.Reason
		}
	}
	if reason == "" {
		t.Error("the suppressed promotion carries no reason")
	}
}

func TestThresholdPromotionsDoNotMoveTheShelfPrice(t *testing.T) {
	p := product(1000)
	th := ruleOf(TypeThreshold, Params{ThresholdSpendMinor: 5000, PercentOff: 10, Currency: "GBP"})
	th.ID = "threshold"
	th.Priority = 500
	th.Stackable = true
	th.Display.Badge = "SPEND 50"
	pctRule := pct("pct", 100, 20, true)

	res, err := Resolve([]Rule{th, pctRule}, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The threshold wins the display on priority but leaves the price to the
	// percentage promotion.
	if res.Winner != "threshold" {
		t.Errorf("winner = %s, want the threshold on priority", res.Winner)
	}
	if res.FinalPriceMinor != 800 {
		t.Errorf("price = %d, want 800 from the percentage promotion alone", res.FinalPriceMinor)
	}
}

func TestResolveWithNoRulesLeavesThePriceAlone(t *testing.T) {
	p := product(999)
	res, err := Resolve(nil, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.FinalPriceMinor != 999 || res.TotalDiscountMinor != 0 || res.Winner != "" {
		t.Errorf("an empty rule set produced %+v", res)
	}
}

// TestDetectConflictsGradesOverlaps checks the authoring-time detector puts the
// right severity on each kind of clash.
func TestDetectConflictsGradesOverlaps(t *testing.T) {
	catalogue := []Product{product(1000), product(500)}
	catalogue[1].SKU = "sku-2"

	t.Run("different priorities are informational", func(t *testing.T) {
		got := DetectConflicts([]Rule{pct("a", 200, 20, false), pct("b", 100, 30, false)}, catalogue)
		if len(got) != 1 || got[0].Severity != SeverityInfo {
			t.Fatalf("conflicts = %+v", got)
		}
		if got[0].Overlap != 2 || len(got[0].SampleSKUs) != 2 {
			t.Errorf("overlap = %d with samples %v, want both products",
				got[0].Overlap, got[0].SampleSKUs)
		}
	})

	t.Run("both stackable is informational", func(t *testing.T) {
		got := DetectConflicts([]Rule{pct("a", 100, 20, true), pct("b", 100, 30, true)}, catalogue)
		if len(got) != 1 || got[0].Severity != SeverityInfo {
			t.Fatalf("conflicts = %+v", got)
		}
	})

	t.Run("equal priority with different prices is a warning", func(t *testing.T) {
		got := DetectConflicts([]Rule{pct("a", 100, 20, false), pct("b", 100, 30, false)}, catalogue)
		if len(got) != 1 || got[0].Severity != SeverityWarn {
			t.Fatalf("conflicts = %+v", got)
		}
		if got[0].Resolution == "" {
			t.Error("the conflict does not say what the policy will do")
		}
	})

	t.Run("nothing distinguishing them is an error", func(t *testing.T) {
		got := DetectConflicts([]Rule{pct("a", 100, 20, false), pct("b", 100, 20, false)}, catalogue)
		if len(got) != 1 || got[0].Severity != SeverityError {
			t.Fatalf("conflicts = %+v", got)
		}
	})

	t.Run("rules that cannot both match are not a conflict", func(t *testing.T) {
		a := pct("a", 100, 20, false)
		a.Conditions.IncludeSKUs = []canon.SKU{"sku-1"}
		b := pct("b", 100, 20, false)
		b.Conditions.IncludeSKUs = []canon.SKU{"sku-2"}
		if got := DetectConflicts([]Rule{a, b}, catalogue); len(got) != 0 {
			t.Errorf("disjoint rules reported a conflict: %+v", got)
		}
	})

	t.Run("stackable rules in one exclusive group are a warning", func(t *testing.T) {
		a := pct("a", 100, 20, true)
		a.ExclusiveGroup = "g"
		b := pct("b", 100, 30, true)
		b.ExclusiveGroup = "g"
		got := DetectConflicts([]Rule{a, b}, catalogue)
		if len(got) != 1 || got[0].Severity != SeverityWarn {
			t.Fatalf("conflicts = %+v", got)
		}
	})
}

func TestDetectConflictsOrdersBySeverity(t *testing.T) {
	catalogue := []Product{product(1000)}
	rules := []Rule{
		pct("info-a", 200, 20, false),
		pct("info-b", 300, 20, false),
		pct("err-a", 100, 25, false),
		pct("err-b", 100, 25, false),
	}
	got := DetectConflicts(rules, catalogue)
	if len(got) == 0 {
		t.Fatal("no conflicts")
	}
	if got[0].Severity != SeverityError {
		t.Errorf("the most serious conflict is not first: %+v", got)
	}
	for i := 1; i < len(got); i++ {
		if severityRank(got[i-1].Severity) < severityRank(got[i].Severity) {
			t.Errorf("conflicts are not ordered by severity at %d", i)
		}
	}
}

func TestSpecificityOrdering(t *testing.T) {
	broad := pct("a", 0, 10, false)
	category := pct("b", 0, 10, false)
	category.Conditions.Categories = []string{"dairy"}
	brand := pct("c", 0, 10, false)
	brand.Conditions.Brands = []string{"own-label"}
	sku := pct("d", 0, 10, false)
	sku.Conditions.IncludeSKUs = []canon.SKU{"sku-1"}

	ordered := []Rule{broad, category, brand, sku}
	for i := 1; i < len(ordered); i++ {
		if Specificity(ordered[i]) <= Specificity(ordered[i-1]) {
			t.Errorf("%s (%d) is not more specific than %s (%d)",
				ordered[i].ID, Specificity(ordered[i]),
				ordered[i-1].ID, Specificity(ordered[i-1]))
		}
	}
	if Specificity(broad) != 0 {
		t.Errorf("an unconditional rule scores %d, want 0", Specificity(broad))
	}
}

func TestResolveIsDeterministicAcrossOrderings(t *testing.T) {
	// The property that lets two nodes agree about a shelf. Every permutation of
	// the same rule set must give the same price and the same winner.
	p := product(1234)
	rules := []Rule{
		pct("a", 100, 15, true),
		pct("b", 100, 15, true),
		pct("c", 200, 5, true),
	}
	want, err := Resolve(rules, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		shuffled := make([]Rule, len(rules))
		for i, idx := range perm {
			shuffled[i] = rules[idx]
		}
		got, err := Resolve(shuffled, p)
		if err != nil {
			t.Fatalf("resolve %v: %v", perm, err)
		}
		if got.FinalPriceMinor != want.FinalPriceMinor || got.Winner != want.Winner {
			t.Errorf("permutation %v gave %d/%s, want %d/%s",
				perm, got.FinalPriceMinor, got.Winner, want.FinalPriceMinor, want.Winner)
		}
	}
	_ = fmt.Sprint()
}
