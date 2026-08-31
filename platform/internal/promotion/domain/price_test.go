package domain

import (
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

func product(priceMinor int64) Product {
	return Product{
		SKU: "sku-1", StoreID: "store-1", Category: "dairy", Brand: "own-label",
		BasePriceMinor: priceMinor, Currency: "GBP", UnitCostMinor: 100,
		Inventory: 40, DaysToExpiry: 5, Velocity: 12,
	}
}

func ruleOf(t Type, params Params) Rule {
	return Rule{
		ID: "p1", TenantID: "acme", Name: "test", Type: t, Params: params,
		Schedule: Schedule{StartLocal: "2026-01-01T00:00", EndLocal: "2026-02-01T00:00"},
	}
}

// TestEveryMechanicComputesTheRightPrice checks each mechanic against a price
// worked out by hand, in minor units, with the rounding the platform documents.
func TestEveryMechanicComputesTheRightPrice(t *testing.T) {
	tests := []struct {
		name          string
		rule          Rule
		base          int64
		wantPrice     int64
		wantUnitPrice int64
		wantQty       int
		wantShelf     bool
	}{
		{
			// 20% off 249 is a 49.8p discount, rounded half away from zero to
			// 50p, leaving 199.
			name:      "percentage off rounds half away from zero",
			rule:      ruleOf(TypePercentageOff, Params{PercentOff: 20, Currency: "GBP"}),
			base:      249,
			wantPrice: 199, wantUnitPrice: 199, wantQty: 1, wantShelf: true,
		},
		{
			// 33% off 100 is 33p exactly.
			name:      "percentage off an exact amount",
			rule:      ruleOf(TypePercentageOff, Params{PercentOff: 33, Currency: "GBP"}),
			base:      100,
			wantPrice: 67, wantUnitPrice: 67, wantQty: 1, wantShelf: true,
		},
		{
			name:      "amount off",
			rule:      ruleOf(TypeAmountOff, Params{AmountOffMinor: 50, Currency: "GBP"}),
			base:      249,
			wantPrice: 199, wantUnitPrice: 199, wantQty: 1, wantShelf: true,
		},
		{
			name:      "amount off cannot go below zero",
			rule:      ruleOf(TypeAmountOff, Params{AmountOffMinor: 400, Currency: "GBP"}),
			base:      249,
			wantPrice: 0, wantUnitPrice: 0, wantQty: 1, wantShelf: true,
		},
		{
			name:      "fixed price",
			rule:      ruleOf(TypeFixedPrice, Params{FixedPriceMinor: 150, Currency: "GBP"}),
			base:      249,
			wantPrice: 150, wantUnitPrice: 150, wantQty: 1, wantShelf: true,
		},
		{
			// Buy 2 get 1 free: three units for the price of two, 498 for 3,
			// so 166 each.
			name: "buy two get one free",
			rule: ruleOf(TypeBuyXGetY, Params{BuyQty: 2, GetQty: 1, GetPercentOff: 100, Currency: "GBP"}),
			base: 249,
			// The shelf price of a single unit is unchanged.
			wantPrice: 249, wantUnitPrice: 166, wantQty: 3, wantShelf: true,
		},
		{
			// Buy one get one half price: 249 + 125 (124.5 rounded up) = 374
			// for two, 187 each.
			name: "buy one get one half price",
			rule: ruleOf(TypeBuyXGetY, Params{BuyQty: 1, GetQty: 1, GetPercentOff: 50, Currency: "GBP"}),
			base: 249,
			// 249 - PercentOff(50) = 249 - 125 = 124; total 373, per unit 187.
			wantPrice: 249, wantUnitPrice: 187, wantQty: 2, wantShelf: true,
		},
		{
			// Three items for 500: 167 each.
			name: "bundle",
			rule: ruleOf(TypeBundle, Params{
				BundleSKUs: []canon.SKU{"a", "b", "c"}, FixedPriceMinor: 500, Currency: "GBP"}),
			base:      249,
			wantPrice: 249, wantUnitPrice: 167, wantQty: 3, wantShelf: true,
		},
		{
			name: "bundle with explicit quantities",
			rule: ruleOf(TypeBundle, Params{
				BundleSKUs: []canon.SKU{"a", "b"}, BundleQty: []int{2, 2},
				FixedPriceMinor: 800, Currency: "GBP"}),
			base:      249,
			wantPrice: 249, wantUnitPrice: 200, wantQty: 4, wantShelf: true,
		},
		{
			// A threshold cannot be priced on a shelf: the label keeps showing
			// the base price and advertises the offer.
			name:      "threshold leaves the shelf price alone",
			rule:      ruleOf(TypeThreshold, Params{ThresholdSpendMinor: 2000, PercentOff: 10, Currency: "GBP"}),
			base:      249,
			wantPrice: 249, wantUnitPrice: 249, wantQty: 1, wantShelf: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply(tt.rule, product(tt.base))
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got.PriceMinor != tt.wantPrice {
				t.Errorf("shelf price = %d, want %d", got.PriceMinor, tt.wantPrice)
			}
			if got.UnitPriceMinor != tt.wantUnitPrice {
				t.Errorf("unit price = %d, want %d", got.UnitPriceMinor, tt.wantUnitPrice)
			}
			if got.QualifyingQty != tt.wantQty {
				t.Errorf("qualifying quantity = %d, want %d", got.QualifyingQty, tt.wantQty)
			}
			if got.ShelfPriced != tt.wantShelf {
				t.Errorf("shelf priced = %v, want %v", got.ShelfPriced, tt.wantShelf)
			}
			if got.DiscountMinor != got.BaseMinor-got.PriceMinor {
				t.Errorf("discount %d does not reconcile with %d - %d",
					got.DiscountMinor, got.BaseMinor, got.PriceMinor)
			}
		})
	}
}

func TestGuardsCapTheDiscount(t *testing.T) {
	t.Run("max discount", func(t *testing.T) {
		r := ruleOf(TypePercentageOff, Params{PercentOff: 50, Currency: "GBP", MaxDiscountMinor: 100})
		got, err := Apply(r, product(1000))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got.PriceMinor != 900 {
			t.Errorf("price = %d, want 900 with the discount capped at 100", got.PriceMinor)
		}
		if !got.Capped {
			t.Error("the result does not say it was capped")
		}
	})
	t.Run("floor price", func(t *testing.T) {
		r := ruleOf(TypePercentageOff, Params{PercentOff: 90, Currency: "GBP", FloorPriceMinor: 150})
		got, err := Apply(r, product(1000))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got.PriceMinor != 150 {
			t.Errorf("price = %d, want the 150 floor", got.PriceMinor)
		}
		if !got.Capped {
			t.Error("the result does not say it was capped")
		}
	})
}

func TestApplyRefusesACurrencyMismatch(t *testing.T) {
	r := ruleOf(TypeFixedPrice, Params{FixedPriceMinor: 199, Currency: "USD"})
	p := product(249) // GBP
	if _, err := Apply(r, p); err == nil {
		t.Fatal("a USD rule priced a GBP shelf")
	}
}

func TestApplyIsExactAcrossManyPrices(t *testing.T) {
	// A percentage promotion must never produce a price that differs from the
	// integer computation by a penny, at any base price. The property is what
	// keeps the shelf and the till in agreement.
	r := ruleOf(TypePercentageOff, Params{PercentOff: 15, Currency: "GBP"})
	for base := int64(1); base <= 5000; base++ {
		got, err := Apply(r, product(base))
		if err != nil {
			t.Fatalf("apply at %d: %v", base, err)
		}
		want := canon.NewMoney(base, "GBP").PercentOff(15).Amount
		if got.PriceMinor != want {
			t.Fatalf("at base %d the promotion priced %d, canon.Money says %d", base, got.PriceMinor, want)
		}
		if got.PriceMinor > base {
			t.Fatalf("at base %d a discount produced a higher price %d", base, got.PriceMinor)
		}
	}
}
