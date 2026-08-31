// Package labelsim models a USSLP smart shelf label faithfully enough that the
// timings and battery figures the platform publishes are worth something.
//
// The three things it takes seriously are the ones that dominate a real
// deployment:
//
//   - The display is electrophoretic. A refresh is slow, it is *blocking*, and
//     a label busy driving a waveform has its radio off and cannot receive.
//     Partial refreshes are cheap and fast but accumulate ghosting, so the
//     panel has to be cleared with a full waveform every few updates.
//   - The power budget is a 500 mAh primary cell that has to last the better
//     part of a decade. Almost all of the interesting engineering is in what
//     the radio does while nothing is happening, not in what it costs to change
//     a price.
//   - The radio is a duty-cycled 802.15.4 end device. It is unreachable except
//     in its own receive windows, which is where most of the SEC-to-label
//     latency actually goes.
//
// Labels are plain structs driven by edge/sim, not goroutines: a store of
// 40,000 of them is 40,000 structs and a shared event queue, which is what
// makes a whole-store load test possible in one process.
package labelsim

import (
	"fmt"
	"time"
)

// DisplayTier identifies one of the three panels in the USSLP hardware range.
type DisplayTier int

const (
	// Tier29BWR is the 2.9-inch 296x128 black/white/red panel: the volume
	// product, on the overwhelming majority of shelf edges.
	Tier29BWR DisplayTier = iota
	// Tier42 is the 4.2-inch 400x300 black/white panel used for larger price
	// points and multi-buy messaging.
	Tier42
	// Tier585Color is the 5.85-inch 600x448 seven-colour panel used on promotion
	// ends and fresh counters. Its waveform takes fifteen seconds and it cannot
	// do partial updates at all, which is the whole reason it is not the
	// default.
	Tier585Color
)

// DisplaySpec is the physical behaviour of one panel.
type DisplaySpec struct {
	Name          string
	Width, Height int
	// Colors is the number of ink states the panel can hold.
	Colors int
	// FullRefresh is the complete waveform: several passes driving every pixel
	// to both extremes before settling it, which is what makes the panel
	// bistable and readable for years without power.
	FullRefresh time.Duration
	// PartialRefresh drives only the changed region with a shortened waveform.
	// It is five times faster and leaves residue.
	PartialRefresh time.Duration
	// SupportsPartial is false for the colour panel: its waveform sequences the
	// pigments through the whole stack and there is no shortened form.
	SupportsPartial bool
	// MaxPartials is how many partial refreshes may run before a full one is
	// forced to clear ghosting. Beyond it the residue from previous images is
	// visible enough that a shopper can read the old price behind the new one,
	// which is a weights-and-measures problem, not a cosmetic one.
	MaxPartials int
	// RefreshCurrentMA is the average draw while the waveform runs. The charge
	// pump driving +/-15 V rails into a capacitive panel is the largest single
	// current the device ever draws.
	RefreshCurrentMA float64
	// ImageBytes is the size of an uncompressed framebuffer at one byte per
	// pixel, which is what the controller compresses before transmission.
	ImageBytes int
}

// Displays is the panel catalogue, indexed by tier.
var displays = map[DisplayTier]DisplaySpec{
	Tier29BWR: {
		Name: "2.9in-296x128-BWR", Width: 296, Height: 128, Colors: 3,
		FullRefresh: 1500 * time.Millisecond, PartialRefresh: 300 * time.Millisecond,
		SupportsPartial: true, MaxPartials: 8, RefreshCurrentMA: 26,
		ImageBytes: 296 * 128,
	},
	Tier42: {
		Name: "4.2in-400x300-BW", Width: 400, Height: 300, Colors: 2,
		FullRefresh: 2000 * time.Millisecond, PartialRefresh: 300 * time.Millisecond,
		SupportsPartial: true, MaxPartials: 8, RefreshCurrentMA: 30,
		ImageBytes: 400 * 300,
	},
	Tier585Color: {
		Name: "5.85in-600x448-7C", Width: 600, Height: 448, Colors: 7,
		FullRefresh: 15 * time.Second, PartialRefresh: 15 * time.Second,
		SupportsPartial: false, MaxPartials: 0, RefreshCurrentMA: 35,
		ImageBytes: 600 * 448,
	},
}

// Display returns the panel specification for a tier.
func Display(t DisplayTier) DisplaySpec {
	if d, ok := displays[t]; ok {
		return d
	}
	return displays[Tier29BWR]
}

// String names the tier for configuration and logs.
func (t DisplayTier) String() string { return Display(t).Name }

// ParseTier resolves a tier from configuration. The names are the ones that
// appear in a planogram and on a purchase order, not Go identifiers.
func ParseTier(s string) (DisplayTier, error) {
	switch s {
	case "2.9", "2.9bwr", "tier29", Display(Tier29BWR).Name:
		return Tier29BWR, nil
	case "4.2", "4.2bw", "tier42", Display(Tier42).Name:
		return Tier42, nil
	case "5.85", "5.85color", "tier585", Display(Tier585Color).Name:
		return Tier585Color, nil
	}
	return Tier29BWR, fmt.Errorf("labelsim: unknown display tier %q", s)
}

// RefreshPlan is the decision a label makes about how to draw an update.
type RefreshPlan struct {
	// Partial is true when the shortened waveform will be used.
	Partial bool
	// Duration is how long the panel will be blocked, and how long the radio
	// will be off.
	Duration time.Duration
	// ForcedFull is true when a partial refresh was requested but a full one was
	// run anyway to clear accumulated ghosting. It is surfaced upward because a
	// controller that keeps triggering it has mis-estimated its diff threshold
	// and is spending five times the energy it planned to.
	ForcedFull bool
}

// planRefresh decides between the waveforms.
//
// The rule is deliberately conservative in the panel's favour: a partial
// refresh happens only when the controller asked for one, the panel supports
// one, and the ghosting budget has not been spent. Getting this wrong is not a
// performance problem, it is a legibility problem on a device that a customer
// reads a price off.
func planRefresh(d DisplaySpec, requestPartial bool, partialsSinceFull int) RefreshPlan {
	if !requestPartial || !d.SupportsPartial {
		return RefreshPlan{Partial: false, Duration: d.FullRefresh}
	}
	if partialsSinceFull >= d.MaxPartials {
		return RefreshPlan{Partial: false, Duration: d.FullRefresh, ForcedFull: true}
	}
	return RefreshPlan{Partial: true, Duration: d.PartialRefresh}
}
