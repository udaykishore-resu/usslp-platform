package apigw

import (
	"errors"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Circuit breaker
//
// One per upstream. The failure it exists for is specific: when label-service
// starts timing out, every gateway replica keeps a connection and a goroutine
// occupied for the full route timeout on every request, and within seconds the
// gateway is out of capacity for the six upstreams that are still healthy. A
// breaker converts "slow and failing" into "fast and failing", which is the
// only version of an outage that stays contained to the service that has it.
//
// The state machine is the classic three-state one, and the details that
// matter are in the half-open transition: exactly one probe at a time, and a
// run of successes before closing. Letting the full load back in on the first
// successful probe is how a recovering service is knocked straight back over.
// ---------------------------------------------------------------------------

// BreakerState is the breaker's current disposition.
type BreakerState string

// The three states.
const (
	// BreakerClosed passes traffic and counts failures.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen refuses traffic without attempting it.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen admits a single probe at a time to test recovery.
	BreakerHalfOpen BreakerState = "half-open"
)

// ErrBreakerOpen is returned when a call is refused without being attempted.
var ErrBreakerOpen = errors.New("apigw: upstream circuit breaker is open")

// BreakerConfig tunes one breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit. Consecutive rather than a ratio because the gateway's traffic
	// per upstream is bursty and low-volume for some routes: a ratio over a
	// window either needs a minimum-request threshold — another knob — or trips
	// on the two requests an hour that a planogram endpoint sees.
	FailureThreshold int
	// SuccessThreshold is the number of consecutive successful probes needed
	// to close a half-open circuit.
	SuccessThreshold int
	// OpenTimeout is how long the circuit stays open before admitting a probe.
	OpenTimeout time.Duration
	// Now supplies the clock, injected so the half-open transition is testable
	// without sleeping.
	Now func() time.Time
	// OnStateChange is called on every transition, used to publish the state
	// gauge and an operator log line.
	OnStateChange func(from, to BreakerState)
}

// Breaker default tuning. Five consecutive failures is past any single
// deployment blip and short of the twenty seconds a rolling restart takes; the
// five-second recovery window is long enough for a pod to finish starting and
// short enough that a transient failure is not punished for a minute.
const (
	DefaultFailureThreshold = 5
	DefaultSuccessThreshold = 2
	DefaultOpenTimeout      = 5 * time.Second
)

// Breaker is a per-upstream circuit breaker.
type Breaker struct {
	cfg BreakerConfig

	mu           sync.Mutex
	state        BreakerState
	failures     int
	successes    int
	openedAt     time.Time
	probeInFlyer bool
}

// NewBreaker builds a closed breaker.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultFailureThreshold
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = DefaultSuccessThreshold
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = DefaultOpenTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Breaker{cfg: cfg, state: BreakerClosed}
}

// State returns the current state, advancing an expired open circuit to
// half-open so that a caller polling the state sees the same thing a caller
// making a request would.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

// Allow asks permission to make a call.
//
// The returned function must be called exactly once with the outcome. It is
// returned rather than having the caller invoke Success/Failure by hand
// because a half-open probe that is never resolved wedges the breaker
// permanently, and `defer done(ok)` is the shape that makes forgetting hard.
func (b *Breaker) Allow() (done func(success bool), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()

	switch b.state {
	case BreakerOpen:
		return nil, false
	case BreakerHalfOpen:
		if b.probeInFlyer {
			// A second concurrent request while a probe is outstanding is
			// refused: the whole point of half-open is to risk one request on
			// an upstream that was just failing, not all of them.
			return nil, false
		}
		b.probeInFlyer = true
		return b.resolver(true), true
	default:
		return b.resolver(false), true
	}
}

func (b *Breaker) resolver(probe bool) func(bool) {
	var once sync.Once
	return func(success bool) {
		once.Do(func() { b.settle(probe, success) })
	}
}

func (b *Breaker) settle(probe, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probeInFlyer = false
	}
	if success {
		switch b.state {
		case BreakerHalfOpen:
			b.successes++
			if b.successes >= b.cfg.SuccessThreshold {
				b.transitionLocked(BreakerClosed)
			}
		default:
			b.failures = 0
		}
		return
	}
	switch b.state {
	case BreakerHalfOpen:
		// A failed probe re-opens immediately and restarts the timer. There is
		// no partial credit: the upstream is still broken.
		b.openedAt = b.cfg.Now()
		b.transitionLocked(BreakerOpen)
	case BreakerClosed:
		b.failures++
		if b.failures >= b.cfg.FailureThreshold {
			b.openedAt = b.cfg.Now()
			b.transitionLocked(BreakerOpen)
		}
	}
}

func (b *Breaker) maybeHalfOpenLocked() {
	if b.state == BreakerOpen && b.cfg.Now().Sub(b.openedAt) >= b.cfg.OpenTimeout {
		b.transitionLocked(BreakerHalfOpen)
	}
}

func (b *Breaker) transitionLocked(to BreakerState) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	b.failures = 0
	b.successes = 0
	if to != BreakerHalfOpen {
		b.probeInFlyer = false
	}
	if b.cfg.OnStateChange != nil {
		// Called with the lock held so that observers see transitions in the
		// order they happened. The callbacks are a gauge write and a log line;
		// neither may re-enter the breaker.
		b.cfg.OnStateChange(from, to)
	}
}

// breakerStateValue renders a state for the Prometheus gauge. Prometheus has
// no string values, so the convention is one gauge per state with a 0/1 value;
// a single gauge with an ordinal would sort meaninglessly in a graph.
func breakerStateValue(current, want BreakerState) float64 {
	if current == want {
		return 1
	}
	return 0
}
