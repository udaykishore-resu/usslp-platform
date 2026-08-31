package domain

import (
	"fmt"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Product is what the evaluator needs to know about one (store, SKU).
//
// It is a projection, not the merchandising system's product record: exactly
// the fields the conditions can test plus the base price. Keeping it narrow is
// what makes the compiled matcher's per-SKU cost a handful of comparisons, and
// it keeps the promotion service from growing an opinion about product data it
// does not own.
type Product struct {
	SKU      canon.SKU     `json:"sku"`
	StoreID  canon.StoreID `json:"store_id"`
	Category string        `json:"category,omitempty"`
	Brand    string        `json:"brand,omitempty"`
	// BasePriceMinor is the everyday price the promotion discounts from.
	BasePriceMinor int64 `json:"base_price_minor"`
	// Currency is the store's trading currency.
	Currency string `json:"currency"`
	// UnitCostMinor is used only by the simulator's cost estimate.
	UnitCostMinor int64 `json:"unit_cost_minor,omitempty"`
	// Inventory is units on hand.
	Inventory int `json:"inventory,omitempty"`
	// DaysToExpiry is shelf life remaining; negative is past date.
	DaysToExpiry int `json:"days_to_expiry,omitempty"`
	// StoreGroups are the clusters this store belongs to.
	StoreGroups []string `json:"store_groups,omitempty"`
	// Velocity is recent units per day, used by the simulator.
	Velocity float64 `json:"velocity,omitempty"`
}

// PricedPromotion is one promotion applied to one product.
type PricedPromotion struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	Type        Type              `json:"type"`
	// BaseMinor is the price before this promotion.
	BaseMinor int64 `json:"base_minor"`
	// PriceMinor is the shelf price after it. For a mechanic the shelf cannot
	// price (THRESHOLD) it equals BaseMinor.
	PriceMinor int64 `json:"price_minor"`
	// DiscountMinor is BaseMinor - PriceMinor.
	DiscountMinor int64 `json:"discount_minor"`
	// UnitPriceMinor is the effective price per unit across the qualifying
	// quantity, which for a multi-buy differs from the shelf price and is what
	// unit-pricing regulation requires be displayed.
	UnitPriceMinor int64 `json:"unit_price_minor"`
	// QualifyingQty is how many units the price applies across.
	QualifyingQty int `json:"qualifying_qty"`
	// ShelfPriced is false when the mechanic cannot produce a definite shelf
	// price and the label keeps showing the base price with a badge.
	ShelfPriced bool `json:"shelf_priced"`
	// Capped records that MaxDiscountMinor or FloorPriceMinor bound the result.
	Capped bool `json:"capped,omitempty"`
	// Notes explains anything an operator would otherwise have to work out.
	Notes string `json:"notes,omitempty"`
}

// Money renders the priced result in the product's currency.
func (p PricedPromotion) Money(currency string) canon.Money {
	return canon.NewMoney(p.PriceMinor, currency)
}

// Apply computes the shelf price for one product under one promotion.
//
// It does not check applicability — the matcher does that — because the two are
// separated on purpose: the matcher runs against millions of pairs and must be
// cheap, and pricing runs only against the pairs that matched.
//
// All arithmetic is in integer minor units, rounding half away from zero
// through canon.Money, so the cloud, the gateway and the till agree to the
// penny. A promotional price computed in floating point is a price that differs
// by a cent between tiers, and in most of the platform's markets that is a
// weights-and-measures violation rather than a rounding curiosity.
func Apply(r Rule, p Product) (PricedPromotion, error) {
	if p.BasePriceMinor < 0 {
		return PricedPromotion{}, fmt.Errorf("%w: negative base price for %s", ErrInvalidRule, p.SKU)
	}
	if r.Params.Currency != "" && p.Currency != "" && r.Params.Currency != p.Currency {
		return PricedPromotion{}, fmt.Errorf("%w: rule is in %s, %s trades in %s",
			canon.ErrCurrencyMismatch, r.Params.Currency, p.StoreID, p.Currency)
	}

	out := PricedPromotion{
		PromotionID: r.ID, Type: r.Type,
		BaseMinor: p.BasePriceMinor, PriceMinor: p.BasePriceMinor,
		QualifyingQty: 1, ShelfPriced: r.Type.ShelfPriceable(),
	}
	base := canon.NewMoney(p.BasePriceMinor, p.Currency)

	switch r.Type {
	case TypePercentageOff:
		out.PriceMinor = base.PercentOff(r.Params.PercentOff).Amount

	case TypeAmountOff:
		out.PriceMinor = p.BasePriceMinor - r.Params.AmountOffMinor
		if out.PriceMinor < 0 {
			// An amount-off larger than the price gives the product away rather
			// than paying the customer to take it.
			out.PriceMinor = 0
		}

	case TypeFixedPrice:
		out.PriceMinor = r.Params.FixedPriceMinor

	case TypeBuyXGetY:
		// The customer pays for BuyQty units in full and GetQty units at a
		// discount, across a qualifying quantity of BuyQty+GetQty. The shelf
		// price stays the single-unit price — one unit still costs one unit —
		// and the *unit* price is what the promotion actually changes.
		qty := r.Params.BuyQty + r.Params.GetQty
		full := int64(r.Params.BuyQty) * p.BasePriceMinor
		discounted := int64(r.Params.GetQty) * canon.NewMoney(p.BasePriceMinor, p.Currency).PercentOff(r.Params.GetPercentOff).Amount
		total := full + discounted
		out.QualifyingQty = qty
		out.PriceMinor = p.BasePriceMinor
		out.UnitPriceMinor = roundDiv(total, int64(qty))
		out.Notes = fmt.Sprintf("buy %d get %d at %.0f%% off: %d units for %d",
			r.Params.BuyQty, r.Params.GetQty, r.Params.GetPercentOff, qty, total)

	case TypeBundle:
		// The bundle price is the whole-set price. The shelf label for one
		// member shows its own price unchanged and advertises the bundle,
		// because a customer buying one member does not get the bundle rate.
		qty := 0
		for i := range r.Params.BundleSKUs {
			n := 1
			if i < len(r.Params.BundleQty) {
				n = r.Params.BundleQty[i]
			}
			qty += n
		}
		if qty == 0 {
			qty = len(r.Params.BundleSKUs)
		}
		out.QualifyingQty = qty
		out.PriceMinor = p.BasePriceMinor
		out.UnitPriceMinor = roundDiv(r.Params.FixedPriceMinor, int64(qty))
		out.Notes = fmt.Sprintf("%d items for %d", qty, r.Params.FixedPriceMinor)

	case TypeThreshold:
		// The shelf cannot know the basket, so it cannot price this. The label
		// shows the base price and the badge advertises the offer, which is the
		// only display that will match the till.
		out.ShelfPriced = false
		if r.Params.ThresholdSpendMinor > 0 {
			out.Notes = fmt.Sprintf("applies once the basket reaches %d", r.Params.ThresholdSpendMinor)
		} else {
			out.Notes = fmt.Sprintf("applies from %d qualifying items", r.Params.ThresholdQty)
		}

	default:
		return PricedPromotion{}, fmt.Errorf("%w: unknown type %q", ErrInvalidRule, r.Type)
	}

	// The guards apply to whichever price the mechanic produced.
	if r.Params.MaxDiscountMinor > 0 {
		if d := p.BasePriceMinor - out.PriceMinor; d > r.Params.MaxDiscountMinor {
			out.PriceMinor = p.BasePriceMinor - r.Params.MaxDiscountMinor
			out.Capped = true
		}
	}
	if r.Params.FloorPriceMinor > 0 && out.PriceMinor < r.Params.FloorPriceMinor {
		out.PriceMinor = r.Params.FloorPriceMinor
		out.Capped = true
	}
	if out.PriceMinor < 0 {
		out.PriceMinor = 0
	}

	out.DiscountMinor = out.BaseMinor - out.PriceMinor
	if out.UnitPriceMinor == 0 {
		out.UnitPriceMinor = out.PriceMinor
	}
	return out, nil
}

// roundDiv divides rounding half away from zero, matching canon.Money's rule so
// that a multi-buy unit price computed here and a discount computed there round
// the same way.
func roundDiv(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	if (num < 0) != (den < 0) {
		return (num - den/2) / den
	}
	return (num + den/2) / den
}
