package domain

import (
	"fmt"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// rng is a small deterministic generator so a property-test failure is
// reproducible from the seed printed in the log.
type rng struct{ s uint64 }

func newRNG(seed uint64) *rng { return &rng{s: seed} }

func (r *rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *rng) pick(options []string) string { return options[r.intn(len(options))] }

var (
	categories = []string{"dairy", "Dairy", "bakery", "produce", "household", " frozen "}
	brands     = []string{"own-label", "Own-Label", "brandco", "valuebrand", ""}
	skus       = []string{"sku-1", "sku-2", "sku-3", "sku-4", "sku-5", "sku-6"}
	storeIDs   = []string{"store-1", "store-2", "store-3", "store-4"}
	groups     = []string{"convenience", "superstore", "Convenience", "express"}
)

// randomProduct generates a product from the fixed vocabularies above, so
// matches actually happen at a useful rate rather than the sets being disjoint
// by construction.
func randomProduct(r *rng) Product {
	return Product{
		SKU:            canon.SKU(r.pick(skus)),
		StoreID:        canon.StoreID(r.pick(storeIDs)),
		Category:       r.pick(categories),
		Brand:          r.pick(brands),
		BasePriceMinor: int64(r.intn(2000)),
		Currency:       "GBP",
		Inventory:      r.intn(50),
		DaysToExpiry:   r.intn(30) - 5,
		StoreGroups:    []string{r.pick(groups)},
	}
}

// randomConditions generates a condition block, populating each field with
// probability roughly one in three so that both the constrained and the
// unconstrained branch of every test are exercised.
func randomConditions(r *rng) Conditions {
	var c Conditions
	sample := func(pool []string, max int) []string {
		n := r.intn(max) + 1
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, r.pick(pool))
		}
		return out
	}
	if r.intn(3) == 0 {
		c.Categories = sample(categories, 2)
	}
	if r.intn(3) == 0 {
		c.Brands = sample(brands, 2)
	}
	if r.intn(3) == 0 {
		for _, s := range sample(storeIDs, 2) {
			c.Stores = append(c.Stores, canon.StoreID(s))
		}
	}
	if r.intn(3) == 0 {
		c.StoreGroups = sample(groups, 2)
	}
	if r.intn(3) == 0 {
		for _, s := range sample(skus, 3) {
			c.IncludeSKUs = append(c.IncludeSKUs, canon.SKU(s))
		}
	}
	if r.intn(3) == 0 {
		for _, s := range sample(skus, 2) {
			c.ExcludeSKUs = append(c.ExcludeSKUs, canon.SKU(s))
		}
	}
	if r.intn(3) == 0 {
		c.MinInventory = r.intn(40)
	}
	if r.intn(3) == 0 {
		d := r.intn(20)
		c.MaxDaysToExpiry = &d
	}
	if r.intn(3) == 0 {
		c.MinPriceMinor = int64(r.intn(1000))
	}
	if r.intn(3) == 0 {
		c.MaxPriceMinor = int64(r.intn(1000)) + c.MinPriceMinor
	}
	return c
}

// TestCompiledMatcherAgreesWithTheNaiveInterpreter is the property test that
// keeps the compiler honest.
//
// The compiled matcher is what runs against eighty million (store, SKU) pairs;
// the naive interpreter is the readable statement of what the conditions mean.
// If they ever disagree, the fast one is wrong by definition, and the only way
// to know is to check them against each other over inputs nobody chose by hand.
func TestCompiledMatcherAgreesWithTheNaiveInterpreter(t *testing.T) {
	const trials = 20000
	r := newRNG(20260830)
	matches, mismatches := 0, 0

	for i := 0; i < trials; i++ {
		rule := Rule{Conditions: randomConditions(r)}
		p := randomProduct(r)
		compiled := Compile(rule).Matches(p)
		naive := MatchesNaive(rule, p)
		if compiled != naive {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("trial %d: compiled=%v naive=%v\n  conditions %+v\n  product %+v",
					i, compiled, naive, rule.Conditions, p)
			}
			continue
		}
		if compiled {
			matches++
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d trials disagreed", mismatches, trials)
	}
	// A property test where nothing ever matched would pass vacuously. Assert
	// that both branches were genuinely exercised.
	if matches == 0 || matches == trials {
		t.Fatalf("%d of %d trials matched: the generator is not exercising both branches", matches, trials)
	}
	t.Logf("%d random (rule, product) pairs, %d matched, %d disagreements", trials, matches, mismatches)
}

func TestMatcherIsCaseInsensitiveOnDescriptiveFieldsAndExactOnIdentifiers(t *testing.T) {
	r := Rule{Conditions: Conditions{
		Categories:  []string{"DAIRY"},
		Brands:      []string{"Own-Label"},
		StoreGroups: []string{"CONVENIENCE"},
		IncludeSKUs: []canon.SKU{"sku-1"},
	}}
	m := Compile(r)

	p := Product{SKU: "sku-1", Category: " dairy ", Brand: "own-label", StoreGroups: []string{"convenience"}}
	if !m.Matches(p) {
		t.Error("category, brand and group comparisons should fold case and trim space")
	}
	// A SKU differing only in case is a different product, not the same one.
	p.SKU = "SKU-1"
	if m.Matches(p) {
		t.Error("SKU comparison must be exact: identifiers are keys, not descriptions")
	}
}

func TestExclusionAlwaysBeatsInclusion(t *testing.T) {
	r := Rule{Conditions: Conditions{
		Categories:  []string{"dairy"},
		IncludeSKUs: []canon.SKU{"sku-1", "sku-2"},
		ExcludeSKUs: []canon.SKU{"sku-1"},
	}}
	m := Compile(r)
	if m.Matches(Product{SKU: "sku-1", Category: "dairy"}) {
		t.Error("an excluded SKU matched despite being on the include list")
	}
	if !m.Matches(Product{SKU: "sku-2", Category: "dairy"}) {
		t.Error("a non-excluded included SKU did not match")
	}
}

func TestSegmentMatchingIsSeparateFromShelfMatching(t *testing.T) {
	r := Rule{Conditions: Conditions{CustomerSegments: []string{"loyalty-gold"}}}
	m := Compile(r)
	// A shelf label has no customer, so the shelf match must not depend on the
	// segment.
	if !m.Matches(Product{SKU: "sku-1"}) {
		t.Error("a segmented promotion did not match a shelf")
	}
	if !m.Segmented() {
		t.Error("the matcher does not report that the rule is segmented")
	}
	if !m.MatchesSegment("Loyalty-Gold") {
		t.Error("segment matching should fold case")
	}
	if m.MatchesSegment("loyalty-bronze") {
		t.Error("a non-qualifying segment matched")
	}
	// An unsegmented rule qualifies everybody.
	if !Compile(Rule{}).MatchesSegment("anything") {
		t.Error("an unsegmented rule refused a segment")
	}
}

func TestMatcherSetEvaluatesTheWholeCatalogueInOnePass(t *testing.T) {
	rules := []Rule{
		{ID: "dairy", Conditions: Conditions{Categories: []string{"dairy"}}},
		{ID: "cheap", Conditions: Conditions{MaxPriceMinor: 500}},
		{ID: "store-1-only", Conditions: Conditions{Stores: []canon.StoreID{"store-1"}}},
	}
	set := CompileSet(rules)
	if set.Len() != 3 {
		t.Fatalf("set has %d matchers, want 3", set.Len())
	}
	p := Product{SKU: "sku-1", StoreID: "store-1", Category: "dairy", BasePriceMinor: 300}
	got := set.Match(p, nil)
	if len(got) != 3 {
		t.Errorf("matched %d rules, want all three: %+v", len(got), got)
	}
	p.Category = "bakery"
	p.BasePriceMinor = 900
	p.StoreID = "store-2"
	if got := set.Match(p, nil); len(got) != 0 {
		t.Errorf("matched %d rules on a product that satisfies none", len(got))
	}
}

// TestMatchAllocatesOnceForTheWholeFanOut pins the property that makes a
// national activation affordable: reusing the destination slice means the
// per-product cost carries no allocation.
func TestMatchAllocatesOnceForTheWholeFanOut(t *testing.T) {
	set := CompileSet([]Rule{
		{ID: "a", Conditions: Conditions{Categories: []string{"dairy"}}},
		{ID: "b", Conditions: Conditions{MaxPriceMinor: 5000}},
	})
	p := Product{SKU: "sku-1", Category: "dairy", BasePriceMinor: 300}
	dst := make([]Rule, 0, 8)
	allocs := testing.AllocsPerRun(500, func() {
		dst = set.Match(p, dst)
	})
	if allocs > 0 {
		t.Errorf("matching allocated %.1f times per product, want 0 with a reused buffer", allocs)
	}
	if len(dst) != 2 {
		t.Errorf("matched %d rules, want 2", len(dst))
	}
}

func BenchmarkCompiledMatcher(b *testing.B) {
	set := CompileSet([]Rule{
		{ID: "a", Conditions: Conditions{Categories: []string{"dairy"}, MinInventory: 5}},
		{ID: "b", Conditions: Conditions{Brands: []string{"own-label"}, MaxPriceMinor: 1000}},
		{ID: "c", Conditions: Conditions{Stores: []canon.StoreID{"store-1", "store-2"}}},
	})
	p := Product{
		SKU: "sku-1", StoreID: "store-1", Category: "dairy", Brand: "own-label",
		BasePriceMinor: 499, Inventory: 20, StoreGroups: []string{"convenience"},
	}
	dst := make([]Rule, 0, 8)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = set.Match(p, dst)
	}
	_ = fmt.Sprint(len(dst))
}

// largeListRule is the shape compilation exists for: a national promotion whose
// include and exclude lists carry thousands of SKUs.
func largeListRule() Rule {
	const n = 5000
	include := make([]canon.SKU, 0, n)
	exclude := make([]canon.SKU, 0, 200)
	for i := 0; i < n; i++ {
		include = append(include, canon.SKU(fmt.Sprintf("sku-%05d", i)))
	}
	for i := 0; i < 200; i++ {
		exclude = append(exclude, canon.SKU(fmt.Sprintf("restricted-%03d", i)))
	}
	return Rule{ID: "national", Conditions: Conditions{IncludeSKUs: include, ExcludeSKUs: exclude}}
}

// BenchmarkCompiledMatcherLargeLists and its naive counterpart are the pair that
// justifies the compiler. On small condition lists a linear scan beats a hash
// lookup and the naive interpreter is the faster of the two — which is worth
// knowing and worth not hiding. The lists that appear in real national
// promotions are thousands of entries long, and that is where the difference
// stops being a micro-benchmark curiosity.
func BenchmarkCompiledMatcherLargeLists(b *testing.B) {
	m := Compile(largeListRule())
	// A SKU near the end of the include list: the worst case for a linear scan
	// and the same cost as any other for a hash lookup.
	p := Product{SKU: "sku-04999", StoreID: "store-1", BasePriceMinor: 499}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !m.Matches(p) {
			b.Fatal("expected a match")
		}
	}
}

func BenchmarkNaiveMatcherLargeLists(b *testing.B) {
	r := largeListRule()
	p := Product{SKU: "sku-04999", StoreID: "store-1", BasePriceMinor: 499}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !MatchesNaive(r, p) {
			b.Fatal("expected a match")
		}
	}
}

func BenchmarkNaiveMatcher(b *testing.B) {
	rules := []Rule{
		{ID: "a", Conditions: Conditions{Categories: []string{"dairy"}, MinInventory: 5}},
		{ID: "b", Conditions: Conditions{Brands: []string{"own-label"}, MaxPriceMinor: 1000}},
		{ID: "c", Conditions: Conditions{Stores: []canon.StoreID{"store-1", "store-2"}}},
	}
	p := Product{
		SKU: "sku-1", StoreID: "store-1", Category: "dairy", Brand: "own-label",
		BasePriceMinor: 499, Inventory: 20, StoreGroups: []string{"convenience"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, r := range rules {
			_ = MatchesNaive(r, p)
		}
	}
}
