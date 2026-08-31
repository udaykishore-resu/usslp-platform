package labelsim

import (
	"math"
	"time"
)

// PowerProfile is the label's energy budget: the current drawn in each state
// and the cell it draws from.
//
// The figures are the ones in the USSLP hardware blueprint. They are held here
// as configuration rather than as constants because the most important thing
// this model has to say is that one of them — the 250 ms beacon interval — does
// not fit in the same sentence as a seven-to-ten-year battery life. See
// Projection and the package tests: the model reports what the numbers give,
// and the numbers were not adjusted to make the target come out.
type PowerProfile struct {
	// DeepSleepUA is the draw with the radio off and the MCU in its lowest
	// retention state. Below a microamp, and almost irrelevant next to
	// everything else.
	DeepSleepUA float64
	// BeaconRXMA and BeaconRXDuration are the cost of one listen window: the
	// radio wakes, listens for its parent's beacon, and goes back to sleep.
	BeaconRXMA       float64
	BeaconRXDuration time.Duration
	// BeaconFast is the listen interval while the label is in its active window
	// — 250 ms, which is what makes a price change reach the glass inside the
	// platform's SEC-to-label budget (INTERFACE-CONTRACTS §4, 400 ms since
	// end-to-end attestation lengthened the frame).
	BeaconFast time.Duration
	// BeaconSlow is the listen interval at rest.
	BeaconSlow time.Duration
	// ActiveWindow is how long the label stays on the fast interval after any
	// activity. This is the duty-cycling that reconciles the latency budget with
	// the battery budget: fast when something is happening, slow the rest of the
	// time.
	ActiveWindow time.Duration
	// DataRXMA is the draw while receiving a data frame.
	DataRXMA float64
	// TXMA is the draw while transmitting.
	TXMA float64
	// NFCMA and NFCTapDuration are the cost of a shopper tapping a phone against
	// the label. The NFC front end is field-powered for reads, but the MCU wakes
	// to serve the dynamic record.
	NFCMA          float64
	NFCTapDuration time.Duration
	// VerifyMA and VerifyDuration are the cost of checking one Ed25519
	// signature end to end: the MCU awake with the radio off, running mbedTLS's
	// software implementation. Thirteen milliseconds at three milliamps is
	// about eleven nanoamp-hours, which is a thousandth of the E-Ink refresh
	// that follows it — the reason canon.AttestationAlg is Ed25519 and not
	// RSA. The figure is the firmware's own measurement point; see
	// firmware/src/crypto/psa_backend.c.
	VerifyMA       float64
	VerifyDuration time.Duration
	// CapacityMAH is the cell. A 500 mAh LiMnO2 coin cell is the standard fit.
	CapacityMAH float64
	// SelfDischargePctPerYear is the cell's own leakage. LiMnO2 is about 1% a
	// year, which over a decade is a tenth of the budget and cannot be ignored
	// in a projection that is trying to distinguish seven years from ten.
	SelfDischargePctPerYear float64
}

// DefaultPower is the blueprint's state budget with the duty cycling that makes
// it survivable. Every current here is the blueprint's figure verbatim; the
// only editorial choice is ActiveWindow, and Projection reports exactly what it
// buys.
func DefaultPower() PowerProfile {
	return PowerProfile{
		DeepSleepUA:             0.8,
		BeaconRXMA:              6.5,
		BeaconRXDuration:        8 * time.Millisecond,
		BeaconFast:              250 * time.Millisecond,
		BeaconSlow:              30 * time.Second,
		ActiveWindow:            60 * time.Second,
		DataRXMA:                12,
		TXMA:                    18,
		NFCMA:                   8,
		NFCTapDuration:          1500 * time.Millisecond,
		VerifyMA:                3,
		VerifyDuration:          13 * time.Millisecond,
		CapacityMAH:             500,
		SelfDischargePctPerYear: 1,
	}
}

// AlwaysFastPower is the blueprint read literally: a 250 ms beacon interval,
// always, with no duty cycling.
//
// It exists so the difference is measurable rather than asserted. It is not a
// shippable configuration, and Projection says so in numbers.
func AlwaysFastPower() PowerProfile {
	p := DefaultPower()
	p.BeaconSlow = p.BeaconFast
	p.ActiveWindow = 0
	return p
}

// chargeMAH converts a current held for a duration into charge.
func chargeMAH(currentMA float64, d time.Duration) float64 {
	return currentMA * d.Hours()
}

// Projection is an analytic battery-life estimate, broken down by what spends
// the charge. The breakdown is the point: an aggregate "eight years" tells an
// engineer nothing, while "83% of it goes on listening for beacons" tells them
// exactly which knob matters.
type Projection struct {
	// Per-component average currents in microamps.
	SleepUA   float64
	BeaconUA  float64
	DataRXUA  float64
	RefreshUA float64
	TXUA      float64
	NFCUA     float64
	// VerifyUA is the cost of end-to-end attestation: one signature check per
	// applied update. Reported separately rather than folded into the update
	// cost because the question "what does end-to-end verification cost the
	// cell" is one a security review asks directly.
	VerifyUA        float64
	SelfDischargeUA float64
	// TotalUA is the average draw.
	TotalUA float64
	// UsableCapacityMAH is the cell derated for temperature.
	UsableCapacityMAH float64
	// Life is the projected time to exhaustion.
	Life time.Duration
	// Years is Life in years, the number a retailer's finance team asks for.
	Years float64
	// MeetsTarget reports whether the projection lands in the 7-to-10-year band
	// the platform commits to.
	MeetsTarget bool
	// FastFraction is the share of wall time spent on the fast beacon interval.
	FastFraction float64
}

// Workload describes what a label is asked to do in a day.
type Workload struct {
	// UpdatesPerDay is price changes reaching the glass. Ten is the platform's
	// planning figure: a morning price load, a couple of promotional changes and
	// margin for markdowns.
	UpdatesPerDay float64
	// PartialFraction is the share of those updates the controller can serve
	// with a partial refresh. It is capped in practice by the panel's ghosting
	// budget, and Project enforces that cap rather than believing the caller.
	PartialFraction float64
	// NFCTapsPerDay is shopper interaction.
	NFCTapsPerDay float64
	// TelemetryPerDay is uplink health reports.
	TelemetryPerDay float64
	// EndToEndAttestation charges one Ed25519 verification per applied update.
	// It is true by default in DefaultWorkload because that is what the fleet
	// ships with; a projection for a deployment running
	// CONFIG_USSLP_REQUIRE_ATTESTATION=n sets it false.
	EndToEndAttestation bool
	// AmbientC is the shelf temperature. A chiller aisle costs real capacity.
	AmbientC float64
}

// DefaultWorkload is the platform's planning workload: the one the
// seven-to-ten-year claim is made against.
func DefaultWorkload() Workload {
	return Workload{
		UpdatesPerDay:   10,
		PartialFraction: 0.875, // seven partials then a forced full, per the panel's budget
		NFCTapsPerDay:   0.5,
		TelemetryPerDay: 288, // one every five minutes
		AmbientC:        20,
		// The shipped posture: the label verifies for itself.
		EndToEndAttestation: true,
	}
}

// CapacityDerating returns the fraction of a LiMnO2 cell's rated capacity
// available at a given temperature.
//
// Primary lithium chemistry holds up far better in the cold than alkaline, but
// it is not immune: internal resistance rises and the pulse the E-Ink charge
// pump demands becomes harder to deliver. A label in a -20 degree freezer case
// gets roughly seventy per cent of its rated capacity, and a fleet plan that
// ignores that will find its freezer aisle dying three years early.
func CapacityDerating(ambientC float64) float64 {
	switch {
	case ambientC >= 20:
		return 1
	case ambientC >= 0:
		return 1 - 0.0075*(20-ambientC) // 1.00 at 20C, 0.85 at 0C
	case ambientC >= -20:
		return 0.85 - 0.0075*(0-ambientC) // 0.85 at 0C, 0.70 at -20C
	default:
		return 0.70
	}
}

// Project computes the analytic battery life for a workload.
//
// It is analytic rather than simulated because the event that dominates the
// budget — a listen window every few seconds — happens tens of millions of
// times over a cell's life, and integrating a duty cycle is exact where
// simulating it would only be slow. The event-driven model in Label accumulates
// the same quantities from actual events, and the package tests check the two
// agree over a simulated year to within a percent; that is what makes it
// legitimate to trust this function for the decade-scale number.
func (p PowerProfile) Project(tier DisplayTier, w Workload) Projection {
	d := Display(tier)
	day := 24 * time.Hour

	// Ghosting caps how many updates can really be partial. Believing a caller
	// who claims every update is partial is how a model produces a battery life
	// the hardware cannot.
	partialFrac := w.PartialFraction
	if !d.SupportsPartial {
		partialFrac = 0
	} else if max := float64(d.MaxPartials) / float64(d.MaxPartials+1); partialFrac > max {
		partialFrac = max
	}

	// Activity keeps the label on the fast beacon interval for a window after
	// each event. Overlapping windows are not double counted: the fraction is
	// clamped at one, which is what happens to a label being hammered.
	events := w.UpdatesPerDay + w.NFCTapsPerDay
	fastSeconds := events * p.ActiveWindow.Seconds()
	fastFrac := fastSeconds / day.Seconds()
	if fastFrac > 1 {
		fastFrac = 1
	}
	if p.BeaconSlow <= p.BeaconFast {
		fastFrac = 1
	}

	beaconCharge := func(interval time.Duration, frac float64) float64 {
		if interval <= 0 {
			return 0
		}
		windows := frac * day.Seconds() / interval.Seconds()
		return windows * chargeMAH(p.BeaconRXMA, p.BeaconRXDuration)
	}
	beaconMAHPerDay := beaconCharge(p.BeaconFast, fastFrac) + beaconCharge(p.BeaconSlow, 1-fastFrac)

	// A price update is received as a compressed image plus, when end-to-end
	// attestation is on, the 138-byte fixed header and the identifiers that
	// carry the signed tuple. At 250 kbps with MAC acknowledgements that is
	// tens of milliseconds of receiver-on time either way; the attested frame
	// costs roughly half as much again.
	updateRXDuration := 40 * time.Millisecond
	if w.EndToEndAttestation {
		updateRXDuration = 60 * time.Millisecond
	}
	const uplinkTXDuration = 8 * time.Millisecond
	rxMAHPerDay := w.UpdatesPerDay * chargeMAH(p.DataRXMA, updateRXDuration)

	avgRefresh := time.Duration(partialFrac*float64(d.PartialRefresh) + (1-partialFrac)*float64(d.FullRefresh))
	refreshMAHPerDay := w.UpdatesPerDay * chargeMAH(d.RefreshCurrentMA, avgRefresh)

	txMAHPerDay := (w.UpdatesPerDay + w.TelemetryPerDay) * chargeMAH(p.TXMA, uplinkTXDuration)
	nfcMAHPerDay := w.NFCTapsPerDay * chargeMAH(p.NFCMA, p.NFCTapDuration)
	// One verification per *applied* update. A duplicate is discarded by the
	// sequence rule before any signature is touched — the free invariant is
	// checked first, on the label as in the controller — so at-least-once
	// redelivery costs nothing here.
	var verifyMAHPerDay float64
	if w.EndToEndAttestation {
		verifyMAHPerDay = w.UpdatesPerDay * chargeMAH(p.VerifyMA, p.VerifyDuration)
	}
	sleepMAHPerDay := chargeMAH(p.DeepSleepUA/1000, day)

	toUA := func(mahPerDay float64) float64 { return mahPerDay / 24 * 1000 }

	usable := p.CapacityMAH * CapacityDerating(w.AmbientC)
	selfUA := usable * (p.SelfDischargePctPerYear / 100) / (365 * 24) * 1000

	pr := Projection{
		SleepUA:           toUA(sleepMAHPerDay),
		BeaconUA:          toUA(beaconMAHPerDay),
		DataRXUA:          toUA(rxMAHPerDay),
		RefreshUA:         toUA(refreshMAHPerDay),
		TXUA:              toUA(txMAHPerDay),
		NFCUA:             toUA(nfcMAHPerDay),
		VerifyUA:          toUA(verifyMAHPerDay),
		SelfDischargeUA:   selfUA,
		UsableCapacityMAH: usable,
		FastFraction:      fastFrac,
	}
	pr.TotalUA = pr.SleepUA + pr.BeaconUA + pr.DataRXUA + pr.RefreshUA +
		pr.TXUA + pr.NFCUA + pr.VerifyUA + pr.SelfDischargeUA
	if pr.TotalUA > 0 {
		hours := usable / (pr.TotalUA / 1000)
		pr.Life = time.Duration(hours * float64(time.Hour))
		pr.Years = hours / (365 * 24)
	}
	pr.MeetsTarget = pr.Years >= 7 && pr.Years <= 10
	return pr
}

// SustainableBeaconInterval returns the slowest-case listen interval at which a
// label just reaches the target life under a workload.
//
// It is the answer to the question the projection provokes: given everything
// else in the budget, how often can this label afford to listen? A negative
// result means the rest of the budget already exceeds the target and no beacon
// interval can save it.
func (p PowerProfile) SustainableBeaconInterval(tier DisplayTier, w Workload, targetYears float64) time.Duration {
	stripped := p
	stripped.BeaconRXMA = 0
	base := stripped.Project(tier, w)
	usable := p.CapacityMAH * CapacityDerating(w.AmbientC)
	budgetUA := usable / (targetYears * 365 * 24) * 1000
	availableUA := budgetUA - base.TotalUA
	if availableUA <= 0 {
		return -1
	}
	// One listen window costs BeaconRXMA for BeaconRXDuration; averaged over an
	// interval I that is a current of BeaconRXMA * BeaconRXDuration / I.
	perWindowMAs := p.BeaconRXMA * p.BeaconRXDuration.Seconds()
	intervalSeconds := perWindowMAs / (availableUA / 1000)
	return time.Duration(intervalSeconds * float64(time.Second))
}

// batteryMillivolts models the discharge curve of a LiMnO2 coin cell.
//
// The curve is flat by design — that is the point of the chemistry — and then
// falls off a cliff. Modelling the cliff matters because the platform raises
// device.battery.critical off the voltage, not off a charge counter the label
// does not keep, and an alert that fires at the wrong point either floods the
// operator or arrives after the label is already blank.
func batteryMillivolts(depthOfDischarge float64) int {
	switch {
	case depthOfDischarge <= 0:
		return 3050
	case depthOfDischarge >= 1:
		return 1800
	case depthOfDischarge < 0.9:
		return int(math.Round(3050 - 250*depthOfDischarge*depthOfDischarge))
	default:
		// The last tenth of the cell falls from 2848 mV to 1800 mV.
		f := (depthOfDischarge - 0.9) / 0.1
		return int(math.Round(2848 - f*(2848-1800)))
	}
}
