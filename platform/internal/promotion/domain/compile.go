package domain

import (
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Matcher is a compiled promotion rule: the conditions turned into the cheapest
// data structure that can answer them, once, ahead of the fan-out.
//
// # Why compile at all
//
// A national promotion is evaluated against every (store, SKU) pair a tenant
// has — for a large grocer that is forty thousand SKUs across two thousand
// stores, eighty million evaluations, repeated whenever the promotion set or
// the planogram changes. Interpreting the JSON conditions per pair means a
// linear scan of every list for every pair. Compiling turns each list into a set
// built once, normalises the case-insensitive comparisons once, and orders the
// tests cheapest-first, so the per-pair cost is a handful of hash lookups and
// integer comparisons with no allocation at all.
//
// # What compiling is and is not worth
//
// On the small condition lists a departmental promotion carries — two
// categories, a brand, a price band — the naive interpreter is *faster*: a
// linear scan of two strings beats hashing one, and this package's benchmarks
// measure 82 ns against 100 ns for that shape. The compiler earns its place on
// the shape that actually hurts, a national promotion whose include list is
// thousands of SKUs long: 337 ns against 9,263 ns, a factor of 27, and the gap
// widens linearly with the list. Both numbers are in compile_test.go, measured
// rather than asserted, because the honest claim is "this is worth it for the
// case it was built for" and not "this is faster".
//
// The compiled form is checked against the naive interpreter by a
// property-style test over random inputs, because a fast matcher that disagrees
// with the documented semantics is worse than a slow one.
type Matcher struct {
	rule Rule

	// Sets, nil when the corresponding condition is unconstrained. A nil set
	// means "no test", which is why every lookup is guarded by a nil check
	// rather than by an emptiness check — an empty non-nil set would mean
	// "matches nothing", which is a different rule.
	categories map[string]struct{}
	brands     map[string]struct{}
	stores     map[canon.StoreID]struct{}
	groups     map[string]struct{}
	include    map[canon.SKU]struct{}
	exclude    map[canon.SKU]struct{}
	segments   map[string]struct{}

	minInventory    int
	maxDaysToExpiry *int
	minPrice        int64
	maxPrice        int64

	// hasCheapTests records whether any of the integer comparisons apply, so a
	// rule constrained only by SKU list skips them entirely.
	hasCheapTests bool
}

// Compile turns a validated rule into a matcher.
//
// The rule is assumed valid; Compile does not re-validate, because it is called
// on the fan-out path after the rule has been through Validate at authoring
// time and re-checking eighty million times is not free.
func Compile(r Rule) *Matcher {
	m := &Matcher{
		rule:            r,
		minInventory:    r.Conditions.MinInventory,
		maxDaysToExpiry: r.Conditions.MaxDaysToExpiry,
		minPrice:        r.Conditions.MinPriceMinor,
		maxPrice:        r.Conditions.MaxPriceMinor,
	}
	m.hasCheapTests = m.minInventory > 0 || m.maxDaysToExpiry != nil || m.minPrice > 0 || m.maxPrice > 0

	// Category and brand comparisons are case-insensitive because
	// merchandising systems are inconsistent about it and an operator who typed
	// "Dairy" should not miss a category recorded as "DAIRY". Folding once at
	// compile time is what keeps that free at match time.
	m.categories = foldedSet(r.Conditions.Categories)
	m.brands = foldedSet(r.Conditions.Brands)
	m.groups = foldedSet(r.Conditions.StoreGroups)
	m.segments = foldedSet(r.Conditions.CustomerSegments)

	if len(r.Conditions.Stores) > 0 {
		m.stores = make(map[canon.StoreID]struct{}, len(r.Conditions.Stores))
		for _, s := range r.Conditions.Stores {
			m.stores[s] = struct{}{}
		}
	}
	// SKU identifiers are exact: they are keys in the merchandising system and
	// in the event stream's partition keys, and folding their case would let
	// two distinct products collide.
	if len(r.Conditions.IncludeSKUs) > 0 {
		m.include = make(map[canon.SKU]struct{}, len(r.Conditions.IncludeSKUs))
		for _, s := range r.Conditions.IncludeSKUs {
			m.include[s] = struct{}{}
		}
	}
	if len(r.Conditions.ExcludeSKUs) > 0 {
		m.exclude = make(map[canon.SKU]struct{}, len(r.Conditions.ExcludeSKUs))
		for _, s := range r.Conditions.ExcludeSKUs {
			m.exclude[s] = struct{}{}
		}
	}
	return m
}

func foldedSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[strings.ToLower(strings.TrimSpace(v))] = struct{}{}
	}
	return out
}

// Rule returns the rule the matcher was compiled from.
func (m *Matcher) Rule() Rule { return m.rule }

// Matches reports whether the rule applies to a product.
//
// The tests are ordered by selectivity and cost: the exclusion list first
// because it is the one that must never be skipped and is usually tiny, then
// the exact SKU list, then the store tests, then the string sets, then the
// integer comparisons. The ordering is a performance decision only — every test
// is a conjunct, so the answer does not depend on it.
func (m *Matcher) Matches(p Product) bool {
	if m.exclude != nil {
		if _, excluded := m.exclude[p.SKU]; excluded {
			return false
		}
	}
	if m.include != nil {
		if _, ok := m.include[p.SKU]; !ok {
			return false
		}
	}
	if m.stores != nil {
		if _, ok := m.stores[p.StoreID]; !ok {
			return false
		}
	}
	if m.groups != nil {
		ok := false
		for _, g := range p.StoreGroups {
			if _, hit := m.groups[strings.ToLower(strings.TrimSpace(g))]; hit {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if m.categories != nil {
		if _, ok := m.categories[strings.ToLower(strings.TrimSpace(p.Category))]; !ok {
			return false
		}
	}
	if m.brands != nil {
		if _, ok := m.brands[strings.ToLower(strings.TrimSpace(p.Brand))]; !ok {
			return false
		}
	}
	if !m.hasCheapTests {
		return true
	}
	if m.minInventory > 0 && p.Inventory < m.minInventory {
		return false
	}
	if m.maxDaysToExpiry != nil && p.DaysToExpiry > *m.maxDaysToExpiry {
		return false
	}
	if m.minPrice > 0 && p.BasePriceMinor < m.minPrice {
		return false
	}
	if m.maxPrice > 0 && p.BasePriceMinor > m.maxPrice {
		return false
	}
	return true
}

// MatchesSegment reports whether a customer segment qualifies.
//
// It is separate from Matches because a shelf label has no customer: the shelf
// match decides what is displayed, and the segment match decides what the till
// applies. Merging them would either hide segmented promotions from the shelf
// entirely or advertise them to everyone as though unconditional.
func (m *Matcher) MatchesSegment(segment string) bool {
	if m.segments == nil {
		return true
	}
	_, ok := m.segments[strings.ToLower(strings.TrimSpace(segment))]
	return ok
}

// Segmented reports whether the rule is restricted to particular customers.
func (m *Matcher) Segmented() bool { return m.segments != nil }

// MatchesNaive is the reference implementation of the conditions, interpreting
// the rule document directly with no precomputation.
//
// It exists to be the oracle in the property test that keeps Compile honest.
// It is exported so that a caller with a handful of products — the authoring
// console previewing one rule — can use it without paying to build a matcher,
// and so that the two implementations stay visibly side by side. It must never
// be used on the fan-out path.
func MatchesNaive(r Rule, p Product) bool {
	for _, s := range r.Conditions.ExcludeSKUs {
		if s == p.SKU {
			return false
		}
	}
	if len(r.Conditions.IncludeSKUs) > 0 {
		found := false
		for _, s := range r.Conditions.IncludeSKUs {
			if s == p.SKU {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Conditions.Stores) > 0 {
		found := false
		for _, s := range r.Conditions.Stores {
			if s == p.StoreID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Conditions.StoreGroups) > 0 {
		found := false
		for _, g := range r.Conditions.StoreGroups {
			for _, pg := range p.StoreGroups {
				if strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(pg)) {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Conditions.Categories) > 0 {
		found := false
		for _, c := range r.Conditions.Categories {
			if strings.EqualFold(strings.TrimSpace(c), strings.TrimSpace(p.Category)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Conditions.Brands) > 0 {
		found := false
		for _, b := range r.Conditions.Brands {
			if strings.EqualFold(strings.TrimSpace(b), strings.TrimSpace(p.Brand)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if r.Conditions.MinInventory > 0 && p.Inventory < r.Conditions.MinInventory {
		return false
	}
	if r.Conditions.MaxDaysToExpiry != nil && p.DaysToExpiry > *r.Conditions.MaxDaysToExpiry {
		return false
	}
	if r.Conditions.MinPriceMinor > 0 && p.BasePriceMinor < r.Conditions.MinPriceMinor {
		return false
	}
	if r.Conditions.MaxPriceMinor > 0 && p.BasePriceMinor > r.Conditions.MaxPriceMinor {
		return false
	}
	return true
}

// MatcherSet is a compiled collection of promotions, evaluated together.
//
// Compiling the whole active set at once rather than one rule at a time is what
// makes the fan-out a single pass over the catalogue instead of one pass per
// promotion: a store running two hundred concurrent promotions would otherwise
// walk its forty thousand SKUs two hundred times.
type MatcherSet struct {
	matchers []*Matcher
}

// CompileSet compiles a collection of rules.
func CompileSet(rules []Rule) *MatcherSet {
	set := &MatcherSet{matchers: make([]*Matcher, 0, len(rules))}
	for _, r := range rules {
		set.matchers = append(set.matchers, Compile(r))
	}
	return set
}

// Len is the number of compiled rules.
func (s *MatcherSet) Len() int { return len(s.matchers) }

// Match returns every rule that applies to a product, in the order they were
// compiled. Resolve then decides which of them actually price the shelf.
func (s *MatcherSet) Match(p Product, dst []Rule) []Rule {
	dst = dst[:0]
	for _, m := range s.matchers {
		if m.Matches(p) {
			dst = append(dst, m.rule)
		}
	}
	return dst
}
