package domain

import (
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

func TestDecideRenderTemplateSelection(t *testing.T) {
	tests := []struct {
		name      string
		in        RenderInput
		template  string
		badge     string
		led       string
		showWas   bool
		wantField string
	}{
		{
			name:     "a plain price gets the standard template and no LED",
			in:       RenderInput{Price: usd(279)},
			template: TemplateStandard,
			led:      LEDOff,
		},
		{
			name: "a promotion gets the promo template, a SALE badge and a green LED",
			in: RenderInput{
				Price: usd(199), WasPrice: ptr(usd(279)), PromotionID: "promo-1",
			},
			template: TemplatePromo,
			badge:    "SALE",
			led:      LEDGreen,
			showWas:  true,
		},
		{
			name:     "a was-price above the new price is promotional even with no promotion id",
			in:       RenderInput{Price: usd(199), WasPrice: ptr(usd(279))},
			template: TemplatePromo,
			badge:    "SALE",
			led:      LEDGreen,
			showWas:  true,
		},
		{
			name: "clearance beats promotion and lights red",
			in: RenderInput{
				Price: usd(99), WasPrice: ptr(usd(279)), PromotionID: "promo-1",
				Reason: "clearance",
			},
			template: TemplateClearance,
			badge:    "CLEARANCE",
			led:      LEDRed,
			showWas:  true,
		},
		{
			name: "unit pricing selects its template and carries the comparison field",
			in: RenderInput{
				Price: usd(279), UnitPrice: ptr(usd(140)), UnitMeasure: "per litre",
			},
			template:  TemplateUnitPrice,
			led:       LEDOff,
			wantField: "unit_price",
		},
		{
			name: "a POS-supplied badge is passed through verbatim",
			in: RenderInput{
				Price: usd(199), PromotionID: "promo-1",
				Attributes: map[string]string{"badge": "2 FOR £3"},
			},
			template: TemplatePromo,
			badge:    "2 FOR £3",
			led:      LEDGreen,
		},
		{
			name: "an explicit template attribute overrides the derived one",
			in: RenderInput{
				Price: usd(279), Attributes: map[string]string{"template": TemplateClearance},
			},
			template: TemplateClearance,
			badge:    "CLEARANCE",
			led:      LEDRed,
		},
		{
			name: "an unknown template attribute is ignored rather than sent to the glass",
			in: RenderInput{
				Price: usd(279), Attributes: map[string]string{"template": "neon_disco"},
			},
			template: TemplateStandard,
			led:      LEDOff,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := DecideRender(tc.in, DefaultPolicy())
			if spec.Template != tc.template {
				t.Errorf("template = %q, want %q", spec.Template, tc.template)
			}
			if spec.Badge != tc.badge {
				t.Errorf("badge = %q, want %q", spec.Badge, tc.badge)
			}
			if spec.LEDColor != tc.led {
				t.Errorf("led = %q, want %q", spec.LEDColor, tc.led)
			}
			if spec.ShowWas != tc.showWas {
				t.Errorf("show_was = %v, want %v", spec.ShowWas, tc.showWas)
			}
			if tc.wantField != "" {
				if _, ok := spec.Fields[tc.wantField]; !ok {
					t.Errorf("missing render field %q in %v", tc.wantField, spec.Fields)
				}
			}
		})
	}
}

func TestDecideRenderPartialRefreshRules(t *testing.T) {
	// The reference "before" state: an active standard-template label showing
	// $2.49 with no partials taken since its last full waveform.
	base := PreviousRender{
		HasPrice: true, Price: usd(249),
		Template: TemplateStandard, LEDColor: LEDOff,
	}
	tests := []struct {
		name    string
		in      RenderInput
		policy  Policy
		partial bool
		why     string
	}{
		{
			name:    "digits changed and the layout did not",
			in:      RenderInput{Price: usd(279), Previous: base},
			partial: true,
			why:     "only the price field changed and its width is unchanged",
		},
		{
			name:    "the first price a label ever shows is drawn in full",
			in:      RenderInput{Price: usd(279)},
			partial: false,
			why:     "nothing is cached on the controller and nothing is on the glass",
		},
		{
			name: "a template change forces a full refresh",
			in: RenderInput{
				Price: usd(199), WasPrice: ptr(usd(249)), PromotionID: "p",
				Previous: base,
			},
			partial: false,
			why:     "the promo template redraws regions the cached framebuffer does not cover",
		},
		{
			name: "a badge change forces a full refresh",
			in: RenderInput{
				Price:    usd(279),
				Previous: PreviousRender{HasPrice: true, Price: usd(249), Template: TemplateStandard, Badge: "NEW", LEDColor: LEDOff},
			},
			partial: false,
			why:     "the badge region is outside the digit field",
		},
		{
			name: "a widening price forces a full refresh",
			in: RenderInput{
				Price:    usd(1099),
				Previous: PreviousRender{HasPrice: true, Price: usd(999), Template: TemplateStandard, LEDColor: LEDOff},
			},
			partial: false,
			why:     "$9.99 to $10.99 re-lays out the digit field itself",
		},
		{
			name: "a currency change forces a full refresh",
			in: RenderInput{
				Price:    canon.NewMoney(279, "EUR"),
				Previous: base,
			},
			partial: false,
			why:     "the symbol moves, so the whole field moves",
		},
		{
			name:    "an unchanged price is redrawn in full rather than partially rewriting nothing",
			in:      RenderInput{Price: usd(249), Previous: base},
			partial: false,
			why:     "a retained-message rebuild must redraw from a known state",
		},
		{
			name: "the ghosting counter forces a full refresh at the limit",
			in: RenderInput{
				Price:    usd(279),
				Previous: PreviousRender{HasPrice: true, Price: usd(249), Template: TemplateStandard, LEDColor: LEDOff, PartialsSinceFull: DefaultFullRefreshEvery},
			},
			partial: false,
			why:     "residual charge from consecutive partials leaves a readable previous price",
		},
		{
			name: "one below the limit is still partial",
			in: RenderInput{
				Price:    usd(279),
				Previous: PreviousRender{HasPrice: true, Price: usd(249), Template: TemplateStandard, LEDColor: LEDOff, PartialsSinceFull: DefaultFullRefreshEvery - 1},
			},
			partial: true,
			why:     "the counter has not reached the ghosting threshold",
		},
		{
			name: "a tenant may tighten the ghosting interval",
			in: RenderInput{
				Price:    usd(279),
				Previous: PreviousRender{HasPrice: true, Price: usd(249), Template: TemplateStandard, LEDColor: LEDOff, PartialsSinceFull: 2},
			},
			policy:  Policy{FullRefreshEvery: 2},
			partial: false,
			why:     "a shorter interval on a panel generation with worse ghosting",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy.WithDefaults()
			spec := DecideRender(tc.in, policy)
			if spec.PartialRefresh != tc.partial {
				t.Fatalf("partial_refresh = %v, want %v (%s)", spec.PartialRefresh, tc.partial, tc.why)
			}
		})
	}
}

func TestPartialRefreshCounterForcesAFullWaveform(t *testing.T) {
	// Drive a label through more consecutive partial-safe changes than the
	// policy allows and assert that a full refresh is interposed. This is the
	// behaviour that keeps a previous price from remaining faintly readable on
	// the glass, which is a weights-and-measures defect rather than a cosmetic
	// one.
	l := newActiveLabel(t)
	policy := DefaultPolicy()
	prices := []int64{259, 269, 279, 289, 299, 309, 319, 329, 339, 349, 359}
	var fullRefreshes int
	for i, p := range prices {
		at := testNow.Add(time.Duration(i+1) * time.Minute)
		events, err := l.ApplyPriceChange(PriceChange{
			SKU: testSKU, Price: usd(p), EffectiveAt: at, OccurredAt: at, Now: at,
		}, policy)
		if err != nil {
			t.Fatalf("apply %d: %v", p, err)
		}
		applied := events[0].(PriceApplied)
		if !applied.Render.PartialRefresh {
			fullRefreshes++
			if l.PartialsSinceFull != policy.FullRefreshEvery {
				t.Fatalf("full refresh at %d consecutive partials, want %d",
					l.PartialsSinceFull, policy.FullRefreshEvery)
			}
		}
		if err := l.Replay(events...); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if applied.Render.PartialRefresh && l.PartialsSinceFull == 0 {
			t.Fatalf("partial refresh reset the ghosting counter")
		}
		if !applied.Render.PartialRefresh && l.PartialsSinceFull != 0 {
			t.Fatalf("full refresh did not clear the ghosting counter: %d", l.PartialsSinceFull)
		}
	}
	if fullRefreshes == 0 {
		t.Fatalf("no full refresh in %d consecutive changes; ghosting would accumulate", len(prices))
	}
}

func TestDecideRenderHonoursAnAuthoredDisplayBlock(t *testing.T) {
	// A promotion document's Display block is a merchandiser's decision that
	// appears in the campaign brief. The platform's own derivation is a default
	// for price changes that express no opinion, and must not override one that
	// does.
	base := RenderInput{
		Price: usd(199), WasPrice: ptr(usd(249)), PromotionID: "promo-1",
		Previous: PreviousRender{
			HasPrice: true, Price: usd(249), Template: TemplateStandard, LEDColor: LEDOff,
		},
	}
	tests := []struct {
		name  string
		attrs map[string]string
		check func(*testing.T, canon.RenderSpec)
	}{
		{
			name:  "an authored LED colour replaces the derived one",
			attrs: map[string]string{"led_color": "RED"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if s.LEDColor != LEDRed {
					t.Fatalf("led = %q, want RED", s.LEDColor)
				}
			},
		},
		{
			name:  "a colour the firmware cannot drive is ignored, not forwarded",
			attrs: map[string]string{"led_color": "ULTRAVIOLET"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if s.LEDColor != LEDGreen {
					t.Fatalf("led = %q, want the derived GREEN", s.LEDColor)
				}
			},
		},
		{
			name:  "an authored animation survives the refresh heuristic",
			attrs: map[string]string{"animation": "FLASH"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if s.Animation != "FLASH" {
					t.Fatalf("animation = %q, want the authored FLASH", s.Animation)
				}
			},
		},
		{
			name:  "an unimplemented animation falls back to the derivation",
			attrs: map[string]string{"animation": "DISCO"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if s.Animation != "PULSE_BORDER" {
					t.Fatalf("animation = %q, want the derived PULSE_BORDER", s.Animation)
				}
			},
		},
		{
			name:  "show_was false suppresses a strike-through the derivation would add",
			attrs: map[string]string{"show_was": "false"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if s.ShowWas {
					t.Fatalf("a rule that forbids the was-price claim still drew one")
				}
			},
		},
		{
			name:  "show_was true keeps it",
			attrs: map[string]string{"show_was": "true"},
			check: func(t *testing.T, s canon.RenderSpec) {
				if !s.ShowWas {
					t.Fatalf("a rule that requires the was-price claim did not draw one")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Attributes = tc.attrs
			tc.check(t, DecideRender(in, DefaultPolicy()))
		})
	}
}

func TestDecideRenderCarriesMerchandisingAttributes(t *testing.T) {
	// The analytics ingest reads Render.Fields["category"] off every price
	// update; a category that reached the Label Service and stopped there would
	// leave every promotion report unable to say what was discounted.
	spec := DecideRender(RenderInput{
		Price:      usd(279),
		Attributes: map[string]string{"category": "dairy", "brand": "own-label", "unrelated": "x"},
	}, DefaultPolicy())
	if spec.Fields["category"] != "dairy" || spec.Fields["brand"] != "own-label" {
		t.Fatalf("merchandising attributes not carried: %v", spec.Fields)
	}
	if _, leaked := spec.Fields["unrelated"]; leaked {
		t.Fatalf("an arbitrary attribute reached the device payload: %v", spec.Fields)
	}
}
