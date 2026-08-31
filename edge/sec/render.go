package sec

import (
	"fmt"
	"strings"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// RenderRequest is everything the zone rendering engine needs to produce a
// framebuffer.
//
// It is assembled by the controller from a canon.PriceUpdated, not sent by the
// cloud, and that separation is the point: the cloud declares intent in a
// canon.RenderSpec ("promo template, badge SALE, show the was-price") and the
// controller decides pixels, because only the controller knows which panel is
// clipped to that shelf edge today. Reassigning a 2.9-inch label to a 4.2-inch
// bracket must not require the cloud to know.
type RenderRequest struct {
	Tier        labelsim.DisplayTier
	Spec        canon.RenderSpec
	Price       canon.Money
	WasPrice    *canon.Money
	UnitPrice   *canon.Money
	UnitMeasure string
	SKU         canon.SKU
	LabelID     canon.LabelID
	PromotionID canon.PromotionID
}

// Template codes carried in the air frame for diagnostics, so a support
// engineer reading a label's last frame can tell which layout drew it without
// the image.
const (
	templateStandard  uint8 = 0
	templatePromo     uint8 = 1
	templateUnitPrice uint8 = 2
	templateClearance uint8 = 3
)

// TemplateCode maps a canon template name to its wire code.
func TemplateCode(name string) uint8 {
	switch strings.ToLower(name) {
	case "promo":
		return templatePromo
	case "unit_price":
		return templateUnitPrice
	case "clearance":
		return templateClearance
	default:
		return templateStandard
	}
}

// Render turns a request into a framebuffer for the label's panel.
//
// The layout is computed from the panel's dimensions rather than hard-coded per
// tier, so the same four templates serve all three panels and a fourth panel
// costs a table entry rather than a rewrite. Type sizes are chosen by fitting
// the actual string: "£2.49" and "£129.99" are different widths and a label
// that clipped the second would be a compliance incident.
func Render(req RenderRequest) (*Framebuffer, error) {
	d := labelsim.Display(req.Tier)
	if d.Width <= 0 || d.Height <= 0 {
		return nil, fmt.Errorf("sec: render: panel %v has no geometry", req.Tier)
	}
	if !req.Price.Valid() {
		return nil, fmt.Errorf("sec: render: price currency %q is not a valid ISO 4217 code", req.Price.Currency)
	}
	f := NewFramebuffer(d.Width, d.Height)
	f.Fill(InkWhite)

	// Colour is only available where the panel has it. Asking a two-ink panel
	// for red is not an error worth failing an update over — the price still has
	// to appear — so it falls back to black.
	accent := InkBlack
	if d.Colors >= 3 {
		accent = InkRed
	}

	pad := d.Width / 40
	if pad < 3 {
		pad = 3
	}
	border := 2
	if d.Width > 400 {
		border = 3
	}
	f.StrokeRect(Rect{0, 0, d.Width, d.Height}, border, InkBlack)

	inner := Rect{border + pad, border + pad, d.Width - border - pad, d.Height - border - pad}
	tmpl := strings.ToLower(req.Spec.Template)
	smallScale := scaleFor(d.Width, 1)
	y := inner.Y0

	// --- header band -------------------------------------------------------
	//
	// Promotional and clearance labels carry a full-width banner. It is the
	// single most effective element on a shelf edge and the reason a retailer
	// buys colour panels at all, so it gets its own band rather than sharing
	// space with the product name.
	badge := strings.TrimSpace(req.Spec.Badge)
	if badge == "" && tmpl == "clearance" {
		badge = "CLEARANCE"
	}
	if badge != "" && (tmpl == "promo" || tmpl == "clearance") {
		bandInk := accent
		if tmpl == "clearance" {
			bandInk = InkBlack
		}
		bandH := TextHeight(smallScale*2) + 2*pad
		if bandH > d.Height/4 {
			bandH = d.Height / 4
		}
		band := Rect{inner.X0, y, inner.X1, y + bandH}
		f.FillRect(band, bandInk)
		bs := FitScale(badge, band.X1-band.X0-2*pad, bandH-pad)
		if bs > 0 {
			f.DrawTextCentred(band.X0, band.X1, band.Y0+(bandH-TextHeight(bs))/2, badge, bs, InkWhite)
		}
		y = band.Y1 + pad
	} else {
		name := strings.TrimSpace(req.Spec.Fields["name"])
		if name == "" {
			name = string(req.SKU)
		}
		ns := smallScale
		for ns > 1 && TextWidth(name, ns) > inner.X1-inner.X0 {
			ns--
		}
		if maxRunes := (inner.X1 - inner.X0 + 1) / (glyphAdvance * ns); len([]rune(name)) > maxRunes && maxRunes > 1 {
			name = string([]rune(name)[:maxRunes-1]) + "…"
		}
		f.DrawText(inner.X0, y, name, ns, InkBlack)
		y += TextHeight(ns) + pad
		if badge != "" {
			f.DrawTextRight(inner.X1, inner.Y0, badge, ns, accent)
		}
	}

	// --- footer ------------------------------------------------------------
	//
	// Laid out before the price so the price can be given every pixel that is
	// left, which is the whole visual hierarchy of a shelf edge.
	footerY := inner.Y1
	skuLine := string(req.SKU)
	if req.PromotionID != "" {
		skuLine += "  " + string(req.PromotionID)
	}
	if skuLine != "" {
		footerY -= TextHeight(smallScale)
		f.DrawText(inner.X0, footerY, skuLine, smallScale, InkBlack)
	}
	if req.UnitPrice != nil && req.UnitPrice.Valid() {
		unit := req.UnitPrice.Display()
		if req.UnitMeasure != "" {
			unit += "/" + req.UnitMeasure
		}
		us := smallScale
		if tmpl == "unit_price" {
			us = smallScale * 2
		}
		for us > 1 && TextWidth(unit, us) > inner.X1-inner.X0 {
			us--
		}
		footerY -= TextHeight(us) + pad/2
		f.DrawTextRight(inner.X1, footerY, unit, us, InkBlack)
	}

	// --- was-price ---------------------------------------------------------
	wasY := footerY
	if req.Spec.ShowWas && req.WasPrice != nil && req.WasPrice.Valid() {
		was := req.WasPrice.Display()
		ws := smallScale * 2
		for ws > 1 && TextWidth(was, ws) > (inner.X1-inner.X0)/2 {
			ws--
		}
		wasY -= TextHeight(ws) + pad
		x1 := f.DrawText(inner.X0, wasY, was, ws, InkBlack)
		// Struck through, because "was" next to a smaller number is ambiguous
		// and a line through it is not.
		f.HLine(inner.X0, x1, wasY+TextHeight(ws)/2, maxInt(1, ws/3), accent)
	}

	// --- price -------------------------------------------------------------
	price := req.Price.Display()
	boxW := inner.X1 - inner.X0
	boxH := wasY - y - pad
	if boxH < TextHeight(1) {
		boxH = TextHeight(1)
	}
	ps := FitScale(price, boxW, boxH)
	if ps == 0 {
		// The price does not fit at any scale. Refusing to render is the correct
		// outcome: a truncated price is worse than no update, and the label keeps
		// showing the last one it verified.
		return nil, fmt.Errorf("sec: render: price %q does not fit the %s panel in template %q",
			price, d.Name, req.Spec.Template)
	}
	priceInk := InkBlack
	if tmpl == "promo" || tmpl == "clearance" {
		priceInk = accent
	}
	f.DrawTextCentred(inner.X0, inner.X1, y+(boxH-TextHeight(ps))/2, price, ps, priceInk)

	return f, nil
}

// scaleFor picks a base type size proportional to the panel width, so the same
// template is legible on a 2.9-inch label and on a 5.85-inch one.
func scaleFor(panelWidth, unit int) int {
	s := panelWidth / 148 * unit
	if s < unit {
		return unit
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PartialDecision is the outcome of deciding how to refresh a panel.
type PartialDecision struct {
	// Partial is whether the shortened waveform is safe.
	Partial bool
	// Reason explains the decision in the words an operator would use. It is
	// carried into the delivery record, because "why is this label taking 1.5
	// seconds per update" is a real support question with several answers.
	Reason string
	Diff   DiffResult
}

// PartialThresholds bound when a partial refresh is worth attempting.
type PartialThresholds struct {
	// MaxChangedFraction is the share of pixels that may differ.
	MaxChangedFraction float64
	// MaxWindowFraction is the share of the panel the changed region's bounding
	// box may cover. It is the binding constraint in practice, because a partial
	// waveform drives a rectangle and not a scatter.
	MaxWindowFraction float64
}

// DefaultPartialThresholds are the platform's settings.
//
// A quarter of the pixels and half the panel area: past that the partial
// waveform is driving most of the display anyway, without the full waveform's
// pass through both extremes that clears the residue, so it costs nearly as
// much energy and leaves the panel worse.
func DefaultPartialThresholds() PartialThresholds {
	return PartialThresholds{MaxChangedFraction: 0.25, MaxWindowFraction: 0.5}
}

// DecidePartial compares a new render with the one currently on the glass and
// decides whether the shortened waveform is safe.
//
// The colour rule is the one that surprises people: on a three-ink panel the
// red particles need the full waveform to move, so any change that involves red
// — appearing, disappearing, or turning black — forces a full refresh however
// few pixels it touches. A controller that ignored this would produce labels
// with a pink smear where last week's SALE badge was.
func DecidePartial(next, prev *Framebuffer, tier labelsim.DisplayTier, th PartialThresholds, requested bool) PartialDecision {
	d := labelsim.Display(tier)
	diff := next.Diff(prev)
	switch {
	case !d.SupportsPartial:
		return PartialDecision{Reason: "panel has no partial waveform", Diff: diff}
	case !requested:
		return PartialDecision{Reason: "full refresh requested by the render spec", Diff: diff}
	case prev == nil:
		return PartialDecision{Reason: "no previous image on file for this label", Diff: diff}
	case diff.SizeChanged:
		return PartialDecision{Reason: "panel geometry changed since the last render", Diff: diff}
	case diff.Changed == 0:
		return PartialDecision{Reason: "image is unchanged", Diff: diff}
	case diff.TouchesColour:
		return PartialDecision{Reason: "the change touches the colour plane, which needs the full waveform", Diff: diff}
	case diff.Fraction() > th.MaxChangedFraction:
		return PartialDecision{Reason: fmt.Sprintf("%.0f%% of pixels changed, limit is %.0f%%",
			100*diff.Fraction(), 100*th.MaxChangedFraction), Diff: diff}
	case diff.WindowFraction() > th.MaxWindowFraction:
		return PartialDecision{Reason: fmt.Sprintf("changed region spans %.0f%% of the panel, limit is %.0f%%",
			100*diff.WindowFraction(), 100*th.MaxWindowFraction), Diff: diff}
	}
	return PartialDecision{Partial: true, Diff: diff, Reason: fmt.Sprintf(
		"%.1f%% of pixels changed within %.0f%% of the panel", 100*diff.Fraction(), 100*diff.WindowFraction())}
}
