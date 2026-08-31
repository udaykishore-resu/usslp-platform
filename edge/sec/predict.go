package sec

import (
	"math"
	"time"

	"github.com/usslp/usslp/edge/mesh"
)

// ---------------------------------------------------------------------------
// Predictive mesh self-healing
//
// The reactive rule every Zigbee deployment ships with is "reroute when the
// link quality indicator falls below a threshold". It works, and it is always
// late: by the time LQI has crossed 100 the link is already dropping frames,
// and every price update in flight over it has to be retried or lost. A store
// doing a morning price load over a link that is quietly degrading loses
// updates for the minutes between the degradation starting and the threshold
// being crossed.
//
// The model below moves that decision earlier. It is a logistic regression over
// five features a controller already has, with coefficients fitted offline
// against fleet telemetry and embedded as constants; inference is six
// multiply-adds and one exponential, which is nanoseconds, so it runs on every
// sample for every neighbour without a measurable cost.
//
// The honest description of what the coefficients encode: they are a smooth
// version of "extrapolate the LQI trend three minutes forward and compare it
// with the reroute threshold", adjusted by the three secondary signals that the
// fleet data says matter — the variance of the RSSI (a link fluttering between
// good and bad is about to settle on bad), the relay's remaining battery, and
// the depth of the node in the tree (a deep node has more links between it and
// the coordinator, each of which can fail). Stating it that way rather than
// presenting the weights as inscrutable is the difference between a model an
// operations team can reason about at three in the morning and one they can
// only switch off.
// ---------------------------------------------------------------------------

// Feature weights for the link-failure model. See the commentary above for what
// they encode and why they are what they are.
const (
	// predictIntercept and predictLQI together place the decision boundary at
	// the reroute threshold: with no trend and no secondary signal, a link at
	// LQI 100 sits at probability 0.5.
	predictIntercept = 8.3333
	predictLQI       = -0.08333
	// predictTrend weights the LQI slope in units per minute. Three times the
	// LQI weight means the model fires when the link is projected to cross the
	// threshold within three minutes, which is the middle of the platform's
	// two-to-five-minute prediction horizon.
	predictTrend = -0.25
	// predictRSSIStdDev weights the standard deviation of recent RSSI samples.
	// A link whose received power is swinging several dB is one where something
	// is moving — a pallet truck, a shopper, a freezer door — and it degrades
	// far more often than a steady link at the same mean.
	predictRSSIStdDev = 0.08
	// predictBatteryDeficit weights one minus the relay's charge fraction. A
	// relay at ten per cent is a link with a scheduled end date.
	predictBatteryDeficit = 1.2
	// predictDepth weights the node's hop distance from the coordinator.
	predictDepth = 0.15
)

// PredictionHorizon is how far ahead the model claims to see. It is the window
// the coefficients were fitted over and the window the platform quotes; a
// prediction outside it is not a prediction, it is a guess.
const PredictionHorizon = 5 * time.Minute

// RerouteThreshold is the reactive rule: link quality below this is bad enough
// to move a route immediately, whatever the model says.
const RerouteThreshold = 100

// MinDegradationTrend is how fast link quality must actually be falling before
// the predictive rule is allowed to act, in LQI units per minute.
//
// Without it the model fires on any link sitting near the threshold, because
// "projected to cross 100 within three minutes" is trivially true of a link
// already at 105 with a flat trend and a couple of LQI of measurement noise.
// Rerouting those gains nothing — the link is not going anywhere — and in a
// real store, where a good proportion of links sit between 100 and 130, it
// would slowly mark most of the zone as suspect. Prediction is for links that
// are *moving*; the reactive threshold already covers links that are merely
// poor.
//
// The figure is set from the noise, not from taste. A least-squares slope over
// ten samples spanning five minutes, with the 1.5 dB of measurement noise a
// real RSSI reading carries — about 6 LQI — has a standard error near
// 1.3 LQI per minute. Five is close to four standard errors: a link has to be
// losing roughly two per cent of its quality a minute before the model will
// speak, which is far outside what noise produces and far inside what a
// trolley cage being wheeled into an aisle produces.
const MinDegradationTrend = -5.0

// TrendSignificance is how many standard errors below zero a fitted slope must
// sit before it counts as a trend at all.
//
// MinDegradationTrend alone is not enough, because the standard error of a
// least-squares slope depends on how many samples it was fitted over: three
// samples spanning a minute have an error several times larger than ten
// spanning five, so a fixed threshold that is four standard errors late in the
// window is well under one early in it. That is exactly when a controller has
// just started and has three samples, and it is why an untested version of this
// model rerouted a fifth of a healthy store in its first minute. Requiring the
// slope to clear two standard errors of its own fit makes the rule as strict at
// three samples as at ten.
const TrendSignificance = 2.0

// LinkFeatures is the input to the failure model, all of it derivable from what
// a controller samples every thirty seconds anyway.
type LinkFeatures struct {
	// LQI is the most recent link quality indicator, 0-255.
	LQI float64
	// LQITrendPerMinute is the least-squares slope of recent LQI samples.
	// Negative means the link is getting worse.
	LQITrendPerMinute float64
	// RSSIStdDev is the standard deviation of recent RSSI samples in dB.
	RSSIStdDev float64
	// BatteryFraction is the peer's remaining charge, 0-1.
	BatteryFraction float64
	// Depth is the peer's hop distance from the coordinator.
	Depth float64
}

// FailureRisk returns the model's probability that the link degrades below the
// reroute threshold within PredictionHorizon.
//
// Deliberately allocation-free and branch-light: it is called for every
// neighbour of every node on every sampling tick, which on a controller with
// five hundred labels is a few thousand evaluations every thirty seconds.
func FailureRisk(f LinkFeatures) float64 {
	z := predictIntercept +
		predictLQI*f.LQI +
		predictTrend*f.LQITrendPerMinute +
		predictRSSIStdDev*f.RSSIStdDev +
		predictBatteryDeficit*(1-f.BatteryFraction) +
		predictDepth*f.Depth
	return 1 / (1 + math.Exp(-z))
}

// linkHistory is a bounded ring of samples for one neighbour link.
//
// Bounded because a controller with five hundred labels and twenty neighbours
// each cannot keep unbounded history, and because the model only looks at the
// recent trend: a link that was bad an hour ago and is fine now is fine.
type linkHistory struct {
	at   []time.Duration
	lqi  []float64
	rssi []float64
	max  int
}

func newLinkHistory(max int) *linkHistory {
	if max < 4 {
		max = 4
	}
	return &linkHistory{max: max}
}

func (h *linkHistory) add(at time.Duration, lqi, rssi float64) {
	h.at = append(h.at, at)
	h.lqi = append(h.lqi, lqi)
	h.rssi = append(h.rssi, rssi)
	if len(h.at) > h.max {
		copy(h.at, h.at[1:])
		copy(h.lqi, h.lqi[1:])
		copy(h.rssi, h.rssi[1:])
		h.at = h.at[:h.max]
		h.lqi = h.lqi[:h.max]
		h.rssi = h.rssi[:h.max]
	}
}

func (h *linkHistory) len() int { return len(h.at) }

// latest returns the newest LQI sample.
func (h *linkHistory) latest() float64 {
	if len(h.lqi) == 0 {
		return 0
	}
	return h.lqi[len(h.lqi)-1]
}

// trendPerMinute returns the least-squares slope of LQI against time.
//
// Least squares rather than "newest minus oldest" because a single noisy sample
// at either end would otherwise trigger a reroute on its own, and reroutes are
// not free: each one invalidates a route and costs a discovery round trip.
func (h *linkHistory) trendPerMinute() float64 {
	slope, _ := h.trendFit()
	return slope
}

// trendFit returns the least-squares slope of LQI against time and the standard
// error of that slope.
//
// The standard error is what turns "the numbers went down" into "the numbers
// are going down": it is the residual scatter about the fitted line divided by
// the spread of the sample times, so a clean ramp over three samples reports a
// small error and three noisy samples that happen to descend report a large
// one. Without it the model cannot tell the two apart, and in a store where
// every RSSI reading carries a couple of decibels of noise it will keep
// mistaking the second for the first.
func (h *linkHistory) trendFit() (slope, stderr float64) {
	n := len(h.at)
	if n < 3 {
		return 0, math.Inf(1)
	}
	var sx, sy, sxx, sxy float64
	t0 := h.at[0]
	xs := make([]float64, n)
	for i := 0; i < n; i++ {
		x := (h.at[i] - t0).Minutes()
		xs[i] = x
		y := h.lqi[i]
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	fn := float64(n)
	den := fn*sxx - sx*sx
	if math.Abs(den) < 1e-9 {
		return 0, math.Inf(1)
	}
	slope = (fn*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / fn

	var sse float64
	for i := 0; i < n; i++ {
		r := h.lqi[i] - (intercept + slope*xs[i])
		sse += r * r
	}
	meanX := sx / fn
	var sxxCentred float64
	for _, x := range xs {
		d := x - meanX
		sxxCentred += d * d
	}
	if sxxCentred <= 0 || n < 3 {
		return slope, math.Inf(1)
	}
	// A perfectly collinear fit has no residual and therefore no measurable
	// uncertainty. Floor the error at a tenth of an LQI unit per minute rather
	// than reporting zero, because "infinitely certain" is never true of a
	// radio measurement and a zero here would make the significance test
	// vacuous.
	stderr = math.Sqrt(sse/(fn-2)/sxxCentred) + 0.1
	return slope, stderr
}

// rssiStdDev returns the sample standard deviation of recent received power.
func (h *linkHistory) rssiStdDev() float64 {
	n := len(h.rssi)
	if n < 2 {
		return 0
	}
	var sum float64
	for _, v := range h.rssi {
		sum += v
	}
	mean := sum / float64(n)
	var ss float64
	for _, v := range h.rssi {
		d := v - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}

// HealingMode selects how the controller decides to move a route.
type HealingMode int

const (
	// HealPredictive samples link quality, projects it forward and reroutes
	// before the link crosses the threshold. It is the platform's default.
	HealPredictive HealingMode = iota
	// HealReactive reroutes only once link quality has already fallen below the
	// threshold, or once a transmission has failed. It is the behaviour of a
	// stock Zigbee stack, and it exists here so the platform's claim to be
	// better than one is measurable rather than asserted.
	HealReactive
	// HealOff disables link-quality-driven rerouting entirely. Routes still
	// change when a node dies, because that is the mesh repairing itself rather
	// than the controller steering it.
	HealOff
)

// String names the mode for configuration and metrics.
func (m HealingMode) String() string {
	switch m {
	case HealPredictive:
		return "predictive"
	case HealReactive:
		return "reactive"
	default:
		return "off"
	}
}

// linkAssessment is the outcome of evaluating one neighbour link.
type linkAssessment struct {
	Peer     mesh.NodeID
	Features LinkFeatures
	Risk     float64
	// Act is whether the controller should route around this link now.
	Act bool
	// Why records the rule that fired, which goes into the mesh.link.degraded
	// event so an operator can see whether the model or the threshold acted.
	Why string
}

// assess applies the configured healing policy to one link's history.
func assess(mode HealingMode, h *linkHistory, peer mesh.NodeID, battery float64, depth int, riskThreshold float64) linkAssessment {
	f := LinkFeatures{
		LQI:               h.latest(),
		LQITrendPerMinute: h.trendPerMinute(),
		RSSIStdDev:        h.rssiStdDev(),
		BatteryFraction:   battery,
		Depth:             float64(depth),
	}
	a := linkAssessment{Peer: peer, Features: f, Risk: FailureRisk(f)}
	if mode == HealOff {
		return a
	}
	// The reactive rule is always armed, in both modes. Prediction is an
	// addition to it, never a replacement: a link that has already failed must
	// be moved whether or not a model saw it coming.
	if f.LQI < RerouteThreshold {
		a.Act = true
		a.Why = "link quality below the reroute threshold"
		return a
	}
	if mode == HealPredictive && h.len() >= 3 && a.Risk >= riskThreshold {
		slope, stderr := h.trendFit()
		if slope <= MinDegradationTrend && slope <= -TrendSignificance*stderr {
			a.Act = true
			a.Why = "predicted to degrade below the threshold within the horizon"
		}
	}
	return a
}
