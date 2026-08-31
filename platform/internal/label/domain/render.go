package domain

import (
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Display templates. The SEC's zone rendering engine owns the pixel geometry;
// the cloud only ever names a template, so a new hardware tier with a different
// resolution ships without a cloud release.
const (
	// TemplateStandard is a plain price.
	TemplateStandard = "standard"
	// TemplatePromo shows the promotional price with the previous one struck
	// through, which most jurisdictions require for a "was/now" claim.
	TemplatePromo = "promo"
	// TemplateUnitPrice adds the per-unit comparison price that EU and UK unit
	// pricing law requires on the shelf edge.
	TemplateUnitPrice = "unit_price"
	// TemplateClearance is the terminal markdown layout.
	TemplateClearance = "clearance"
)

// LED colours. The LED is a battery cost — a lit indicator shortens a 7-year
// cell measurably — so it is only used where a human is expected to act.
const (
	LEDOff   = "OFF"
	LEDRed   = "RED"
	LEDGreen = "GREEN"
	LEDAmber = "AMBER"
)

// RenderInput is everything the render decision is allowed to see. Keeping it
// explicit rather than passing the aggregate makes DecideRender a pure function
// that a test can drive through every branch without constructing a label.
type RenderInput struct {
	// Price is the price about to be displayed.
	Price canon.Money
	// WasPrice, when set, is the pre-promotion price to strike through.
	WasPrice *canon.Money
	// UnitPrice and UnitMeasure drive the unit-pricing template.
	UnitPrice   *canon.Money
	UnitMeasure string
	// PromotionID marks the change as promotional.
	PromotionID canon.PromotionID
	// Reason is the free-text reason from the POS adapter; "clearance" and
	// "markdown" select the clearance template.
	Reason string
	// Locale controls number and symbol formatting on the device.
	Locale string
	// Attributes carries POS-supplied overrides; "badge" and "template" are
	// honoured, and the merchandising attributes are passed through as render
	// fields.
	Attributes map[string]string
	// Previous describes what the glass is showing now. A zero Previous means
	// the label has never displayed a price, which forces a full refresh.
	Previous PreviousRender
}

// PreviousRender is the state of the glass before this update.
type PreviousRender struct {
	// HasPrice is false for a label that has never displayed anything.
	HasPrice bool
	// Price is what is currently shown.
	Price canon.Money
	// Template, Badge, LEDColor and ShowWas describe the current layout.
	Template string
	Badge    string
	LEDColor string
	ShowWas  bool
	// PartialsSinceFull counts consecutive partial refreshes since the last
	// full waveform.
	PartialsSinceFull int
}

// DecideRender chooses the template, badge, LED colour and — the consequential
// one — whether a partial E-Ink refresh is safe.
//
// # Why partial refresh matters
//
// A full E-Ink waveform takes about 1.5 seconds and visibly flashes the panel
// black and white as it drives every particle to a rail. A partial refresh
// takes about 0.3 seconds and rewrites only the pixels that changed. Inside a
// three-second end-to-end budget whose largest single line item is the refresh
// itself, that 1.2 seconds is the difference between meeting the SLO and
// missing it, and on a shelf of forty labels updating together the flash is the
// difference between "the prices changed" and "something is wrong with the
// shelf".
//
// # Why it cannot always be used
//
// A partial waveform is short precisely because it does not drive the particles
// fully to their rails. Residual charge accumulates in the pixels it touches,
// and after enough consecutive partials a faint image of the previous content
// remains — E-Ink ghosting. On a shelf label the ghost is a *previous price*,
// which is not a cosmetic defect but a weights-and-measures one: a shopper can
// read two prices on one label. The only way to clear it is a full waveform,
// which drives the whole panel through its clear sequence.
//
// The rule is therefore:
//
//   - Partial is offered only when the digits changed and the layout did not.
//     A template, badge, LED or was-price change alters regions outside the
//     price field, and the SEC's cached framebuffer for those regions is no
//     longer valid.
//   - A price whose rendered width grows or shrinks — 9.99 to 10.99 — forces a
//     full refresh, because the digit field itself is re-laid out and the
//     partial region the controller would compute no longer covers the change.
//   - Every Policy.FullRefreshEvery-th refresh is full regardless. At the
//     default of 8 a label repriced twice a day is fully cleared every four
//     days, and a promotion-heavy label repriced hourly every eight hours,
//     which is inside the ghosting threshold measured on the platform's panels
//     with a comfortable margin.
func DecideRender(in RenderInput, policy Policy) canon.RenderSpec {
	policy = policy.WithDefaults()
	spec := canon.RenderSpec{
		Template: TemplateStandard,
		LEDColor: LEDOff,
		Locale:   in.Locale,
	}

	clearance := isClearance(in.Reason) || strings.EqualFold(in.Attributes["template"], TemplateClearance)
	promo := in.PromotionID != "" || (in.WasPrice != nil && in.WasPrice.Cmp(in.Price) > 0)
	unit := in.UnitPrice != nil && in.UnitMeasure != ""

	// Precedence is deliberate: clearance beats promo beats unit price. A
	// clearance item is usually also promotional and usually also unit priced,
	// and the shopper-facing claim that matters most is the terminal markdown.
	switch {
	case clearance:
		spec.Template = TemplateClearance
		spec.Badge = "CLEARANCE"
		// Red: a clearance markdown is the one price change that requires a
		// human to walk the aisle and physically move stock.
		spec.LEDColor = LEDRed
		spec.ShowWas = in.WasPrice != nil
	case promo:
		spec.Template = TemplatePromo
		spec.Badge = "SALE"
		// Green: the merchandising sweep that verifies a promotion went live
		// looks for green, and a lit label is one a colleague can find from the
		// end of the aisle.
		spec.LEDColor = LEDGreen
		spec.ShowWas = in.WasPrice != nil
	case unit:
		spec.Template = TemplateUnitPrice
	}

	// An authored display block supersedes the derived one.
	//
	// The derivation above is a sensible default for a price change that says
	// nothing about how it wants to look. A promotion document, by contrast,
	// has an explicit Display block that a merchandiser filled in and that
	// appears in the campaign brief — "2 FOR £3" is a legal claim the retailer
	// made, and a red LED is the colour the aisle-walk was briefed to look for.
	// The platform is not entitled to substitute its own taste for either.
	//
	// Every override is validated against what the firmware can actually drive.
	// These values arrive from a tenant's merchandising system and end up in a
	// message delivered to fifty million battery-powered devices, so an
	// unrecognised colour is ignored rather than forwarded.
	if badge, ok := in.Attributes["badge"]; ok {
		spec.Badge = badge
	}
	if t, ok := in.Attributes["template"]; ok && knownTemplate(t) {
		spec.Template = t
	}
	if c, ok := in.Attributes["led_color"]; ok {
		if c = strings.ToUpper(strings.TrimSpace(c)); knownLED(c) {
			spec.LEDColor = c
		}
	}
	if a, ok := in.Attributes["animation"]; ok {
		// An explicit animation is a decision, not a default, so the
		// refresh-mode heuristic at the end of this function leaves it alone.
		if a = strings.ToUpper(strings.TrimSpace(a)); knownAnimation(a) {
			spec.Animation = a
		}
	}
	if v, ok := in.Attributes["show_was"]; ok {
		// Several jurisdictions require the struck-through original whenever a
		// saving is claimed, and several forbid it where none is. The authored
		// answer is the only one that can be right.
		spec.ShowWas = strings.EqualFold(strings.TrimSpace(v), "true") && in.WasPrice != nil
	}
	if unit {
		spec.Fields = map[string]string{
			"unit_price":   in.UnitPrice.Display(),
			"unit_measure": in.UnitMeasure,
		}
	}
	if in.WasPrice != nil && spec.ShowWas {
		if spec.Fields == nil {
			spec.Fields = map[string]string{}
		}
		spec.Fields["was_price"] = in.WasPrice.Display()
	}
	// The merchandising attributes travel with the render spec because that is
	// where the rest of the platform already looks for them: the analytics
	// ingest reads Render.Fields["category"] off every price update, and a
	// category that reached the Label Service but stopped there would leave
	// every promotion report unable to say what was discounted.
	for _, name := range passthroughFields {
		if v := in.Attributes[name]; v != "" {
			if spec.Fields == nil {
				spec.Fields = map[string]string{}
			}
			spec.Fields[name] = v
		}
	}

	spec.PartialRefresh = partialSafe(in, spec, policy)
	if authored, ok := in.Attributes["animation"]; !ok || !knownAnimation(strings.ToUpper(strings.TrimSpace(authored))) {
		if !spec.PartialRefresh && spec.Template == TemplatePromo {
			// A full refresh already flashes the panel; adding the border pulse
			// costs nothing extra in settle time and is what draws a shopper's
			// eye to a newly live promotion.
			spec.Animation = "PULSE_BORDER"
		} else {
			spec.Animation = "NONE"
		}
	}
	return spec
}

// partialSafe implements the ghosting rule documented on DecideRender.
func partialSafe(in RenderInput, spec canon.RenderSpec, policy Policy) bool {
	prev := in.Previous
	if !prev.HasPrice {
		// Nothing cached on the controller and nothing on the glass: the first
		// price a label shows is always drawn with a full waveform.
		return false
	}
	if prev.PartialsSinceFull >= policy.FullRefreshEvery {
		return false
	}
	if prev.Template != spec.Template || prev.Badge != spec.Badge ||
		prev.LEDColor != spec.LEDColor || prev.ShowWas != spec.ShowWas {
		return false
	}
	if prev.Price.Currency != in.Price.Currency {
		// A currency change moves the symbol and therefore the whole field.
		return false
	}
	if prev.Price.Amount == in.Price.Amount {
		// Nothing in the price field changed. Re-publishing an identical price
		// (a controller replacement, a retained-message rebuild) must redraw
		// from a known state rather than partially rewriting nothing.
		return false
	}
	if len(prev.Price.Display()) != len(in.Price.Display()) {
		return false
	}
	return true
}

// passthroughFields are the POS-supplied attributes carried onto the render
// spec. The list is closed rather than "everything not otherwise consumed": an
// open pass-through would let a tenant's mapping put arbitrary bytes into a
// message that reaches 50 million battery-powered devices.
var passthroughFields = []string{"category", "brand"}

func isClearance(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	return r == "clearance" || r == "markdown" || r == "final_markdown"
}

// knownLED and knownAnimation bound what an authored display block may ask the
// firmware for. They mirror the promotion DSL's own validation, which rejects
// these at authoring time; repeating the check here is defence against a rule
// that reached the stream some other way.
func knownLED(c string) bool {
	switch c {
	case LEDOff, LEDRed, LEDGreen, LEDAmber, "BLUE", "WHITE":
		return true
	}
	return false
}

func knownAnimation(a string) bool {
	switch a {
	case "NONE", "PULSE_BORDER", "FLASH", "BLINK_LED":
		return true
	}
	return false
}

func knownTemplate(t string) bool {
	switch t {
	case TemplateStandard, TemplatePromo, TemplateUnitPrice, TemplateClearance:
		return true
	}
	return false
}
