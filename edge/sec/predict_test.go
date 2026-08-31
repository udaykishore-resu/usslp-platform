package sec

import (
	"math"
	"testing"
	"time"
)

// history builds a link history from LQI samples taken every interval, with the
// RSSI implied by the LQI so the two features stay consistent.
func history(interval time.Duration, lqis ...float64) *linkHistory {
	h := newLinkHistory(len(lqis))
	for i, lqi := range lqis {
		rssi := -100 + lqi*65/255
		h.add(time.Duration(i)*interval, lqi, rssi)
	}
	return h
}

func TestTrendFitSeparatesARampFromNoise(t *testing.T) {
	// A clean ramp: the slope is what it looks like, and the fit is tight.
	ramp := history(30*time.Second, 150, 145, 140, 135, 130, 125)
	slope, stderr := ramp.trendFit()
	if math.Abs(slope-(-10)) > 0.5 {
		t.Fatalf("a ramp losing 5 LQI every 30 seconds fitted as %.2f per minute, want -10", slope)
	}
	if slope > -TrendSignificance*stderr {
		t.Fatalf("a perfectly linear ramp is not significant: slope %.2f, standard error %.2f", slope, stderr)
	}

	// The same nominal slope, but scattered: a fit through noise must not be
	// mistaken for a trend, which is the mistake that reroutes a healthy store.
	noisy := history(30*time.Second, 150, 128, 152, 126, 148, 130)
	slope, stderr = noisy.trendFit()
	if slope <= -TrendSignificance*stderr {
		t.Fatalf("noise fitted as a significant trend: slope %.2f, standard error %.2f", slope, stderr)
	}
}

func TestPredictiveRuleIgnoresAnInsignificantTrend(t *testing.T) {
	// Three samples that happen to descend, which is what a controller sees in
	// its first ninety seconds and what an earlier version of this model
	// rerouted a fifth of a store on.
	noisy := history(30*time.Second, 130, 108, 118)
	a := assess(HealPredictive, noisy, "relay-1", 1, 1, 0.5)
	if a.Act {
		t.Fatalf("rerouted on three noisy samples: %s (risk %.2f, trend %.2f)",
			a.Why, a.Risk, a.Features.LQITrendPerMinute)
	}

	// A real degradation over the same three samples: the model must act, and
	// well before the reactive threshold at 100.
	real := history(30*time.Second, 150, 138, 126)
	a = assess(HealPredictive, real, "relay-1", 1, 1, 0.5)
	if !a.Act {
		t.Fatalf("did not act on a link losing 24 LQI a minute at 126: risk %.2f, trend %.2f",
			a.Risk, a.Features.LQITrendPerMinute)
	}
	if a.Features.LQI < RerouteThreshold {
		t.Fatal("the scenario is not a prediction: the link is already below the reactive threshold")
	}

	// The reactive rule is armed in both modes and does not need a trend.
	bad := history(30*time.Second, 96, 95, 94)
	if got := assess(HealReactive, bad, "relay-1", 1, 1, 0.5); !got.Act {
		t.Fatal("a link below the reroute threshold was not acted on in reactive mode")
	}
	if got := assess(HealPredictive, bad, "relay-1", 1, 1, 0.5); !got.Act {
		t.Fatal("a link below the reroute threshold was not acted on in predictive mode")
	}
	if got := assess(HealOff, bad, "relay-1", 1, 1, 0.5); got.Act {
		t.Fatal("healing is off; nothing should be rerouted")
	}
}
