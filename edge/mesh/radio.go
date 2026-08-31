// Package mesh is an IEEE 802.15.4 / Zigbee mesh model honest enough that the
// latency and reliability numbers USSLP publishes about its edge tier mean
// something.
//
// It models what actually limits an electronic-shelf-label deployment: one
// 250 kbps channel shared by every node in a controller's zone, a link budget
// that degrades with distance and with the metal shelving between two labels,
// per-hop store-and-forward delay, MAC-layer retries, tree formation and
// re-formation, and multi-hop routing that has to be repaired when a relay
// dies. It also models the failure modes an operator actually sees — a node
// going dark, a link degrading as a display cabinet is moved into the aisle,
// a microwave oven raising the noise floor — because the Shelf Edge
// Controller's predictive self-healing is only worth anything if it can be
// shown to beat reactive rerouting under exactly those conditions.
//
// The model is driven by edge/sim, so a whole store's mesh runs in one
// goroutine, deterministically, as fast as the CPU allows.
package mesh

import (
	"math"
	"time"
)

// Radio and link-budget constants for an IEEE 802.15.4 O-QPSK radio in the
// 2.4 GHz band, which is what every shelf label and controller in the fleet
// uses. They are exported so a deployment with unusual RF — a warehouse with
// steel racking, a store with a licensed sub-GHz backhaul — can be modelled
// without editing this file.
const (
	// DataRateBps is the 802.15.4 2.4 GHz over-the-air rate. Everything about
	// the fan-out cost of a store-wide promotion follows from this number: it
	// is shared by every node in the zone, not per node.
	DataRateBps = 250000

	// PHYOverheadBytes is the preamble, start-of-frame delimiter and PHY header
	// that precede every frame on air.
	PHYOverheadBytes = 6
	// MACOverheadBytes is the MAC, NWK and APS headers plus the message
	// integrity code, with Zigbee network-layer security enabled. Security is
	// not optional in USSLP — an unencrypted mesh would let a shopper with a
	// software-defined radio watch a store's pricing — so its cost is in the
	// baseline rather than in a footnote.
	MACOverheadBytes = 25
	// MaxFrameBytes is the 802.15.4 PHY service data unit limit. A price update
	// with an Ed25519 attestation does not fit in one frame; the fragmentation
	// that follows is a first-class part of the airtime model.
	MaxFrameBytes = 127
	// ACKBytes is an 802.15.4 acknowledgement frame.
	ACKBytes = 11
	// TurnaroundTime is the radio's receive/transmit switch time (aTurnaround-
	// Time, 12 symbol periods).
	TurnaroundTime = 192 * time.Microsecond
	// BackoffPeriod is aUnitBackoffPeriod, the granularity of CSMA-CA backoff.
	BackoffPeriod = 320 * time.Microsecond

	// TxPowerDBm is the label transmit power. A coin cell cannot hold the
	// regulator up through a +3 dBm burst for the whole of a fragmented frame,
	// so shelf labels transmit at 0 dBm and buy their range back with relays.
	TxPowerDBm = 0.0
	// ReferenceLossDB is free-space path loss at one metre at 2.4 GHz.
	ReferenceLossDB = 40.0
	// PathLossExponent is the log-distance exponent. 2.9 is the value measured
	// down supermarket aisles: better than a partitioned office (3.2) because
	// aisles are long and straight, far worse than free space (2.0) because of
	// the steel shelving and the stock on it.
	PathLossExponent = 2.9
	// ShadowSigmaDB is the standard deviation of log-normal shadow fading. Two
	// labels the same distance apart differ by several dB depending on whether
	// a chest freezer sits between them; the deviate is drawn once per link and
	// then held, because that obstruction does not move every packet.
	ShadowSigmaDB = 4.0
	// NoiseFloorDBm is the thermal noise plus typical 2.4 GHz background in a
	// store already saturated with Wi-Fi.
	NoiseFloorDBm = -95.0
	// SensitivityDBm is the *effective* receiver sensitivity at 1% packet error
	// rate. The datasheet figure for an 802.15.4 receiver is around -95 dBm in
	// a conducted test; a shelf label has a printed antenna a few millimetres
	// from a metal rail, in a multipath environment, so the usable figure is
	// roughly ten decibels worse. Using the datasheet number here would make
	// every simulated link look healthy and the whole self-healing story
	// pointless, which is the opposite of what this model is for.
	SensitivityDBm = -85.0
	// MaxLinkRangeM bounds which node pairs are even considered neighbours.
	// Beyond it the link budget is hopeless — below LQI 60, which the routing
	// metric already refuses — and evaluating the pair on every sample wastes
	// time proportional to the square of the fleet size.
	MaxLinkRangeM = 25.0
)

// PathLossDB returns the log-distance path loss between two points, before
// shadow fading.
//
// The one-metre reference is a floor rather than an extrapolation: two labels
// clipped to the same shelf rail are centimetres apart, and the log-distance
// model diverges below its reference distance.
func PathLossDB(distanceM float64) float64 {
	if distanceM < 1 {
		distanceM = 1
	}
	return ReferenceLossDB + 10*PathLossExponent*math.Log10(distanceM)
}

// LQIFromRSSI maps received power to the 0–255 link quality indicator the
// 802.15.4 MAC reports.
//
// The standard leaves the mapping to the implementer, and every silicon vendor
// picks something different; this is the linear map between the receiver's
// noise floor and the point where the AGC saturates, which is what the TI and
// Silicon Labs parts approximately do. The Shelf Edge Controller's reroute
// threshold of 100 therefore lands at about -74.5 dBm, roughly 10 dB above the
// effective sensitivity floor and a packet error rate of about half a percent:
// still working, visibly worse than it should be, and exactly the point at
// which moving the route is cheap and waiting is not.
func LQIFromRSSI(rssiDBm float64) int {
	const floor, ceiling = -100.0, -35.0
	if rssiDBm <= floor {
		return 0
	}
	if rssiDBm >= ceiling {
		return 255
	}
	return int(math.Round(255 * (rssiDBm - floor) / (ceiling - floor)))
}

// PacketErrorRate returns the probability that a frame at the given received
// power fails, given the current noise floor.
//
// A logistic in the signal-to-noise margin above sensitivity reproduces the
// characteristic 802.15.4 waterfall: essentially lossless a few dB above
// sensitivity, unusable a few dB below, with a narrow transition. Modelling it
// as a hard threshold would hide the regime the predictive healer exists to
// handle, which is a link sitting *in* the transition and drifting downward.
func PacketErrorRate(rssiDBm, noiseFloorDBm float64) float64 {
	// Sensitivity is quoted against the nominal noise floor; interference that
	// raises the floor raises the usable sensitivity by the same amount.
	effectiveSensitivity := SensitivityDBm + (noiseFloorDBm - NoiseFloorDBm)
	margin := rssiDBm - effectiveSensitivity
	// The 2.0 dB scale puts the 50% point at the sensitivity figure and gives a
	// transition roughly 8 dB wide, matching published PER-versus-RSSI curves.
	p := 1 / (1 + math.Exp(margin/2.0))
	// A floor of 1e-4 keeps a perfect link from being perfect: real radios drop
	// frames to interference no model captures, and a mesh test that never
	// exercises its retry path is not testing the retry path.
	if p < 1e-4 {
		return 1e-4
	}
	if p > 0.999 {
		return 0.999
	}
	return p
}

// Fragments returns how many 802.15.4 frames a payload of the given size
// occupies. A price update carrying a 64-byte Ed25519 signature takes three.
func Fragments(payloadBytes int) int {
	if payloadBytes <= 0 {
		return 1
	}
	per := MaxFrameBytes - PHYOverheadBytes - MACOverheadBytes
	return (payloadBytes + per - 1) / per
}

// Airtime returns how long a payload occupies the shared channel for one hop,
// including fragmentation, acknowledgements and inter-frame turnaround.
//
// This is the number that makes a store-wide promotion expensive: 250 kbps is
// shared by the whole zone, so the cost of updating every label in it is the
// sum of these, not the maximum.
func Airtime(payloadBytes int) time.Duration {
	frags := Fragments(payloadBytes)
	per := MaxFrameBytes - PHYOverheadBytes - MACOverheadBytes
	remaining := payloadBytes
	var total time.Duration
	for i := 0; i < frags; i++ {
		body := remaining
		if body > per {
			body = per
		}
		if body < 0 {
			body = 0
		}
		remaining -= body
		onAir := PHYOverheadBytes + MACOverheadBytes + body
		total += bytesOnAir(onAir) + TurnaroundTime + bytesOnAir(ACKBytes) + TurnaroundTime
	}
	return total
}

func bytesOnAir(n int) time.Duration {
	return time.Duration(float64(n) * 8 * float64(time.Second) / DataRateBps)
}

// LinkCost returns the Zigbee network-layer cost of a link from its LQI.
//
// The formula is the one in the Zigbee specification: cost is the reciprocal of
// the fourth power of the probability of delivery, clamped to the range 1–7, so
// a marginal link is seven times as expensive as a good one and a route with
// one bad hop loses to a longer route made of good ones. Reproducing the
// specification's own metric rather than inventing a nicer one matters, because
// the point of this model is to predict what real silicon will do.
func LinkCost(lqi int) int {
	if lqi <= 0 {
		return 7
	}
	p := float64(lqi) / 255
	c := int(math.Round(1 / (p * p * p * p)))
	if c < 1 {
		return 1
	}
	if c > 7 {
		return 7
	}
	return c
}
