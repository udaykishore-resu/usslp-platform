// Package domain is the promotion rule DSL, its compiler, its lifecycle and its
// conflict-resolution policy.
//
// # What the DSL is for
//
// A promotion is authored once, in a merchandising system or the platform's own
// console, and then evaluated against every (store, SKU) pair in the estate —
// for a national promotion on a large grocer, tens of millions of pairs, on
// every price change and every planogram update. The DSL is a JSON document
// because that is what an operator's tooling produces; the *compiler* exists
// because re-interpreting that JSON per SKU is the difference between a
// promotion activating in seconds and in hours.
//
// # What is deliberately not in the DSL
//
// No expressions, no arithmetic, no user-supplied code. Every condition is a
// fixed, enumerable predicate. That is a security decision as much as a
// performance one: a promotion document arrives from a tenant's merchandising
// system, and a DSL with an expression evaluator is a DSL with a sandbox escape
// waiting to be found. It is also what makes the compiler possible — a fixed
// predicate set can be reduced to bitmaps and sorted sets, an expression tree
// cannot.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Type is the discount mechanic.
type Type string

// The supported promotion mechanics. The set is closed: a mechanic the platform
// cannot compute a shelf price for is a mechanic a label cannot display, and
// displaying "see till for price" is how a retailer fails a price-marking
// inspection.
const (
	// TypePercentageOff takes a percentage off the base price.
	TypePercentageOff Type = "PERCENTAGE_OFF"
	// TypeAmountOff takes a fixed amount off.
	TypeAmountOff Type = "AMOUNT_OFF"
	// TypeFixedPrice sets an absolute price.
	TypeFixedPrice Type = "FIXED_PRICE"
	// TypeBuyXGetY is a multi-buy: buy X, get Y free or discounted. The shelf
	// shows the effective unit price across the qualifying quantity, which is
	// what unit-pricing regulation requires.
	TypeBuyXGetY Type = "BUY_X_GET_Y"
	// TypeBundle prices a set of SKUs together.
	TypeBundle Type = "BUNDLE"
	// TypeThreshold discounts once basket spend or quantity crosses a bar.
	// The shelf can only advertise it, never price it: whether the threshold is
	// met is a basket fact, not a shelf fact.
	TypeThreshold Type = "THRESHOLD"
)

// Valid reports whether the type is one the platform knows.
func (t Type) Valid() bool {
	switch t {
	case TypePercentageOff, TypeAmountOff, TypeFixedPrice, TypeBuyXGetY, TypeBundle, TypeThreshold:
		return true
	}
	return false
}

// ShelfPriceable reports whether a mechanic produces a definite shelf price.
//
// THRESHOLD does not: the discount depends on the whole basket, which the shelf
// cannot know. Such a promotion still drives a badge and an LED, and the label
// keeps showing the undiscounted price — which is the honest thing to display
// and the only thing that will match the till.
func (t Type) ShelfPriceable() bool { return t != TypeThreshold }

// Errors the DSL returns.
var (
	// ErrInvalidRule marks a document that cannot be used. It is never
	// recoverable by retrying: the author must change it.
	ErrInvalidRule = errors.New("promotion: invalid rule")
	// ErrNotApplicable is returned when a rule is asked to price a SKU it does
	// not match.
	ErrNotApplicable = errors.New("promotion: rule does not apply")
)

// Rule is one promotion document.
type Rule struct {
	// ID is the promotion identifier.
	ID canon.PromotionID `json:"id"`
	// TenantID scopes the rule.
	TenantID canon.TenantID `json:"tenant_id"`
	// Name is the operator-facing label.
	Name string `json:"name"`
	// Type is the mechanic.
	Type Type `json:"type"`
	// Description is free text shown in the console.
	Description string `json:"description,omitempty"`

	// Priority orders overlapping promotions. Higher wins. It is the first and
	// most explicit tie-breaker precisely because an operator who has thought
	// about a clash should be able to say what they decided.
	Priority int `json:"priority"`
	// Stackable allows this promotion to combine with others rather than
	// replacing them. Two stackable promotions both apply, in priority order;
	// a non-stackable one ends the chain.
	Stackable bool `json:"stackable"`
	// ExclusiveGroup names a set within which at most one promotion may apply,
	// even when all of them are stackable. It is how "one voucher per basket"
	// and "manufacturer funding cannot combine with retailer funding" are
	// expressed without turning every rule non-stackable.
	ExclusiveGroup string `json:"exclusive_group,omitempty"`

	// Parameters carry the mechanic's numbers.
	Params Params `json:"params"`
	// Conditions decide which (store, SKU, customer) the rule applies to.
	Conditions Conditions `json:"conditions"`
	// Display drives the label's appearance.
	Display Display `json:"display"`

	// Schedule is the activation window.
	Schedule Schedule `json:"schedule"`
	// Funding records who pays for the discount, which matters for the margin
	// report and for the exclusivity rules above.
	Funding string `json:"funding,omitempty"` // "retailer" | "supplier" | "joint"
	// CreatedBy and CreatedAt are the audit trail.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	// Version increments on every edit, for optimistic concurrency.
	Version int64 `json:"version,omitempty"`
}

// Params are the mechanic's numbers. Which fields are meaningful depends on the
// Type, and Validate enforces exactly that — a PERCENTAGE_OFF rule carrying a
// fixed price is an authoring error worth catching, not a field to ignore.
type Params struct {
	// PercentOff is used by PERCENTAGE_OFF, in percent (12.5 means 12.5%).
	PercentOff float64 `json:"percent_off,omitempty"`
	// AmountOffMinor is used by AMOUNT_OFF.
	AmountOffMinor int64 `json:"amount_off_minor,omitempty"`
	// FixedPriceMinor is used by FIXED_PRICE and BUNDLE.
	FixedPriceMinor int64 `json:"fixed_price_minor,omitempty"`
	// BuyQty and GetQty are used by BUY_X_GET_Y.
	BuyQty int `json:"buy_qty,omitempty"`
	GetQty int `json:"get_qty,omitempty"`
	// GetPercentOff is the discount on the "get" units; 100 means free, which
	// is the common case, and anything less is a "buy one get one half price".
	GetPercentOff float64 `json:"get_percent_off,omitempty"`
	// BundleSKUs are the members of a BUNDLE, and BundleQty how many of each.
	BundleSKUs []canon.SKU `json:"bundle_skus,omitempty"`
	BundleQty  []int       `json:"bundle_qty,omitempty"`
	// ThresholdSpendMinor and ThresholdQty are the THRESHOLD bars; exactly one
	// must be set.
	ThresholdSpendMinor int64 `json:"threshold_spend_minor,omitempty"`
	ThresholdQty        int   `json:"threshold_qty,omitempty"`
	// Currency is the currency every monetary parameter is in.
	Currency string `json:"currency,omitempty"`
	// MaxDiscountMinor caps the discount however it was computed. It exists
	// because a percentage promotion applied to a mis-keyed price is the
	// classic way a retailer sells a television for four pounds.
	MaxDiscountMinor int64 `json:"max_discount_minor,omitempty"`
	// FloorPriceMinor refuses to price below this however the mechanic
	// computes. It is a second, promotion-local guard alongside the pricing
	// service's Tier-1 floor.
	FloorPriceMinor int64 `json:"floor_price_minor,omitempty"`
}

// Conditions decide applicability. Every field is a conjunction: a rule applies
// only where all of its populated conditions hold. Empty means unconstrained.
type Conditions struct {
	// Categories limits the rule to product categories.
	Categories []string `json:"categories,omitempty"`
	// Brands limits it to brands.
	Brands []string `json:"brands,omitempty"`
	// Stores limits it to a set of stores. Empty means every store in the
	// tenant, which is what a national promotion wants.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// StoreGroups limits it to named store clusters (format, region, banner).
	StoreGroups []string `json:"store_groups,omitempty"`
	// IncludeSKUs, when non-empty, restricts the rule to exactly these SKUs.
	IncludeSKUs []canon.SKU `json:"include_skus,omitempty"`
	// ExcludeSKUs removes SKUs the category rule would otherwise catch. It is
	// evaluated after every include and always wins, because the exclusion list
	// is where the alcohol, tobacco and infant-formula lines live and a
	// promotion that accidentally includes them is a regulatory incident.
	ExcludeSKUs []canon.SKU `json:"exclude_skus,omitempty"`
	// MinInventory requires stock on hand. A promotion that drives demand for a
	// product the store does not have is worse than no promotion.
	MinInventory int `json:"min_inventory,omitempty"`
	// MaxDaysToExpiry limits the rule to short-dated stock, which is how a
	// waste-reduction markdown is expressed.
	MaxDaysToExpiry *int `json:"max_days_to_expiry,omitempty"`
	// CustomerSegments limits the rule to loyalty segments. A shelf label
	// cannot know who is standing in front of it, so a segmented promotion is
	// advertised on the shelf and applied at the till; the platform models it
	// so the two agree about who qualifies.
	CustomerSegments []string `json:"customer_segments,omitempty"`
	// PriceRange limits the rule by base price, for "20% off everything under a
	// tenner" mechanics.
	MinPriceMinor int64 `json:"min_price_minor,omitempty"`
	MaxPriceMinor int64 `json:"max_price_minor,omitempty"`
}

// Display drives what the label shows.
type Display struct {
	// LEDColor is one of the label's LED colours.
	LEDColor string `json:"led_color,omitempty"`
	// Badge is the short flash text, e.g. "SALE" or "2 FOR £3".
	Badge string `json:"badge,omitempty"`
	// ShowOriginalPrice draws the struck-through was-price. Several
	// jurisdictions require it whenever a saving is claimed, which is why it is
	// an explicit field rather than an inference from the mechanic.
	ShowOriginalPrice bool `json:"show_original_price"`
	// Animation is the attention effect.
	Animation string `json:"animation,omitempty"`
	// Template overrides the render template.
	Template string `json:"template,omitempty"`
}

// The LED colours a label can drive.
var validLEDColors = map[string]bool{
	"": true, "RED": true, "GREEN": true, "BLUE": true, "AMBER": true, "WHITE": true, "OFF": true,
}

// The animations the firmware implements.
var validAnimations = map[string]bool{
	"": true, "NONE": true, "PULSE_BORDER": true, "FLASH": true, "BLINK_LED": true,
}

// maxBadgeRunes bounds the badge.
//
// Sixteen characters is what the smallest display tier can render at the badge
// font size. Truncating at render time produces "2 FOR £3 WHEN Y" on a shelf,
// so the limit is enforced at authoring time where someone can fix it.
const maxBadgeRunes = 16

// Validate checks that a rule is coherent, complete for its mechanic, and safe
// to hand to a store.
//
// It is deliberately strict. The alternative to rejecting a malformed promotion
// at authoring time is discovering it when forty thousand labels in a store
// show something wrong, and the recovery from that is a manual price audit.
func (r Rule) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if r.ID == "" {
		add("missing id")
	} else if !canon.ValidID(string(r.ID)) {
		add("id %q contains reserved characters", r.ID)
	}
	if r.TenantID == "" {
		add("missing tenant_id")
	}
	if strings.TrimSpace(r.Name) == "" {
		add("missing name")
	}
	if !r.Type.Valid() {
		add("unknown type %q", r.Type)
	}
	if r.Priority < 0 || r.Priority > 1000 {
		add("priority %d is outside 0..1000", r.Priority)
	}

	switch r.Type {
	case TypePercentageOff:
		if r.Params.PercentOff <= 0 || r.Params.PercentOff >= 100 {
			add("percent_off %.2f is outside (0, 100)", r.Params.PercentOff)
		}
		if r.Params.FixedPriceMinor != 0 || r.Params.AmountOffMinor != 0 {
			add("a PERCENTAGE_OFF rule must not carry fixed_price_minor or amount_off_minor")
		}
	case TypeAmountOff:
		if r.Params.AmountOffMinor <= 0 {
			add("amount_off_minor must be positive")
		}
		if r.Params.Currency == "" {
			add("amount_off_minor needs a currency")
		}
	case TypeFixedPrice:
		if r.Params.FixedPriceMinor <= 0 {
			add("fixed_price_minor must be positive")
		}
		if r.Params.Currency == "" {
			add("fixed_price_minor needs a currency")
		}
	case TypeBuyXGetY:
		if r.Params.BuyQty <= 0 {
			add("buy_qty must be positive")
		}
		if r.Params.GetQty <= 0 {
			add("get_qty must be positive")
		}
		if r.Params.GetPercentOff <= 0 || r.Params.GetPercentOff > 100 {
			add("get_percent_off %.2f is outside (0, 100]", r.Params.GetPercentOff)
		}
	case TypeBundle:
		if len(r.Params.BundleSKUs) < 2 {
			add("a bundle needs at least two SKUs")
		}
		if len(r.Params.BundleQty) != 0 && len(r.Params.BundleQty) != len(r.Params.BundleSKUs) {
			add("bundle_qty has %d entries for %d SKUs", len(r.Params.BundleQty), len(r.Params.BundleSKUs))
		}
		for i, q := range r.Params.BundleQty {
			if q <= 0 {
				add("bundle_qty[%d] must be positive", i)
			}
		}
		if r.Params.FixedPriceMinor <= 0 {
			add("a bundle needs a positive fixed_price_minor")
		}
		if r.Params.Currency == "" {
			add("a bundle price needs a currency")
		}
	case TypeThreshold:
		spend, qty := r.Params.ThresholdSpendMinor > 0, r.Params.ThresholdQty > 0
		if spend == qty {
			add("a threshold needs exactly one of threshold_spend_minor and threshold_qty")
		}
		if r.Params.PercentOff <= 0 && r.Params.AmountOffMinor <= 0 {
			add("a threshold needs a percent_off or an amount_off_minor reward")
		}
	}

	if r.Params.Currency != "" && len(r.Params.Currency) != 3 {
		add("currency %q is not an ISO 4217 code", r.Params.Currency)
	}
	if r.Params.MaxDiscountMinor < 0 {
		add("max_discount_minor cannot be negative")
	}
	if r.Params.FloorPriceMinor < 0 {
		add("floor_price_minor cannot be negative")
	}

	if !validLEDColors[strings.ToUpper(r.Display.LEDColor)] {
		add("led_color %q is not a colour the label can drive", r.Display.LEDColor)
	}
	if !validAnimations[strings.ToUpper(r.Display.Animation)] {
		add("animation %q is not implemented by the firmware", r.Display.Animation)
	}
	if n := len([]rune(r.Display.Badge)); n > maxBadgeRunes {
		add("badge is %d characters, above the %d the smallest display can render", n, maxBadgeRunes)
	}

	if r.Conditions.MinInventory < 0 {
		add("min_inventory cannot be negative")
	}
	if r.Conditions.MaxDaysToExpiry != nil && *r.Conditions.MaxDaysToExpiry < 0 {
		add("max_days_to_expiry cannot be negative")
	}
	if r.Conditions.MinPriceMinor < 0 || r.Conditions.MaxPriceMinor < 0 {
		add("price range bounds cannot be negative")
	}
	if r.Conditions.MaxPriceMinor > 0 && r.Conditions.MinPriceMinor > r.Conditions.MaxPriceMinor {
		add("price range [%d, %d] is empty", r.Conditions.MinPriceMinor, r.Conditions.MaxPriceMinor)
	}
	for _, s := range r.Conditions.Stores {
		if !canon.ValidID(string(s)) {
			add("store %q contains reserved characters", s)
		}
	}
	for _, s := range r.Conditions.IncludeSKUs {
		if !canon.ValidID(string(s)) {
			add("include sku %q contains reserved characters", s)
		}
	}
	for _, s := range r.Conditions.ExcludeSKUs {
		if !canon.ValidID(string(s)) {
			add("exclude sku %q contains reserved characters", s)
		}
	}
	// An include list wholly cancelled by the exclude list is a rule that
	// cannot ever fire, which is always an authoring mistake.
	if len(r.Conditions.IncludeSKUs) > 0 {
		excl := make(map[canon.SKU]bool, len(r.Conditions.ExcludeSKUs))
		for _, s := range r.Conditions.ExcludeSKUs {
			excl[s] = true
		}
		remaining := 0
		for _, s := range r.Conditions.IncludeSKUs {
			if !excl[s] {
				remaining++
			}
		}
		if remaining == 0 {
			add("every included SKU is also excluded, so the rule can never apply")
		}
	}

	if err := r.Schedule.Validate(); err != nil {
		add("%v", err)
	}

	switch strings.ToLower(r.Funding) {
	case "", "retailer", "supplier", "joint":
	default:
		add("funding %q is not one of retailer, supplier or joint", r.Funding)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w: %s", ErrInvalidRule, strings.Join(problems, "; "))
	}
	return nil
}

// ParseRule decodes and validates a promotion document.
//
// Unknown fields are rejected rather than ignored. A typo in a condition name
// would otherwise silently widen a promotion — "exclud_skus" becomes no
// exclusion list at all — and the first sign of it is a discounted product
// that should not have been.
func ParseRule(data []byte) (Rule, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var r Rule
	if err := dec.Decode(&r); err != nil {
		return Rule{}, fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	// Normalise the display enums so that "red" and "RED" are the same rule and
	// produce byte-identical render specs on every tier.
	r.Display.LEDColor = strings.ToUpper(r.Display.LEDColor)
	r.Display.Animation = strings.ToUpper(r.Display.Animation)
	r.Params.Currency = strings.ToUpper(r.Params.Currency)
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// ParseRules decodes a batch of documents, reporting every failure rather than
// stopping at the first — an operator importing a hundred promotions wants the
// whole list of problems, not one at a time.
func ParseRules(data []byte) ([]Rule, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw []json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	rules := make([]Rule, 0, len(raw))
	var problems []string
	for i, doc := range raw {
		r, err := ParseRule(doc)
		if err != nil {
			problems = append(problems, fmt.Sprintf("[%d] %v", i, err))
			continue
		}
		rules = append(rules, r)
	}
	if len(problems) > 0 {
		return rules, fmt.Errorf("%w: %s", ErrInvalidRule, strings.Join(problems, "; "))
	}
	return rules, nil
}

// RenderSpec converts the display block into the canonical render instruction
// the Label Service and the Shelf Edge Controller already understand.
func (r Rule) RenderSpec(showWas bool) canon.RenderSpec {
	template := r.Display.Template
	if template == "" {
		template = "promo"
	}
	return canon.RenderSpec{
		Template:  template,
		Badge:     r.Display.Badge,
		LEDColor:  r.Display.LEDColor,
		Animation: r.Display.Animation,
		ShowWas:   showWas && r.Display.ShowOriginalPrice,
		// A promotional change alters the price, the badge and the strike-through
		// at once, which is most of the glass; a partial waveform would leave
		// ghosting across the changed region, so promotions always take a full
		// refresh.
		PartialRefresh: false,
	}
}
