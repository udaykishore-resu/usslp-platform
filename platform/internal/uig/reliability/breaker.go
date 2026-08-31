package reliability

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned instead of attempting a call the breaker believes
// will fail. It is deliberately distinguishable from a transport error so the
// caller can report "we did not try" rather than "the POS is down", which are
// different lines in an incident timeline.
var ErrCircuitOpen = errors.New("uig/reliability: circuit open")

// BreakerState is the breaker's position in its cycle.
type BreakerState int

const (
	// StateClosed passes calls through and counts failures.
	StateClosed BreakerState = iota
	// StateOpen rejects calls immediately until the cooldown elapses. This is
	// the state that matters for the latency budget: a POS whose API is timing
	// out at 30 seconds would otherwise burn the entire ingest budget on every
	// delivery, and the breaker turns that into a microsecond.
	StateOpen
	// StateHalfOpen lets a small number of probes through to discover whether
	// the dependency has recovered.
	StateHalfOpen
)

// String renders the state for metrics and logs.
func (s BreakerState) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// BreakerConfig tunes a breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit. Consecutive rather than a ratio, because the UIG's outbound
	// calls are low-volume callbacks: a ratio over a window needs a window's
	// worth of traffic before it says anything, and by then the budget is gone.
	FailureThreshold int
	// Cooldown is how long the circuit stays open before probing.
	Cooldown time.Duration
	// HalfOpenProbes is how many calls are allowed through while probing. Any
	// failure among them re-opens the circuit immediately.
	HalfOpenProbes int
	// SuccessThreshold is how many probe successes close the circuit again.
	SuccessThreshold int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 15 * time.Second
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 1
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = c.HalfOpenProbes
	}
	return c
}

// Breaker guards one outbound dependency.
type Breaker struct {
	name string
	cfg  BreakerConfig
	now  func() time.Time

	mu           sync.Mutex
	state        BreakerState
	failures     int
	successes    int
	probes       int
	openedAt     time.Time
	lastErr      error
	transitions  uint64
	rejectedCall uint64
}

// NewBreaker creates a closed breaker.
func NewBreaker(name string, cfg BreakerConfig) *Breaker {
	return &Breaker{name: name, cfg: cfg.withDefaults(), now: time.Now}
}

// WithClock injects a clock for deterministic tests of the cooldown.
func (b *Breaker) WithClock(now func() time.Time) *Breaker {
	b.mu.Lock()
	b.now = now
	b.mu.Unlock()
	return b
}

// Name returns the breaker's identifier, used as a metric label.
func (b *Breaker) Name() string { return b.name }

// State reports the current state, resolving an elapsed cooldown first so a
// caller polling for metrics sees half-open rather than a stale open.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

// Rejected returns how many calls the breaker refused to attempt.
func (b *Breaker) Rejected() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rejectedCall
}

// Transitions counts state changes. A breaker that is flapping — many
// transitions, none of them lasting — is a different operational problem from
// one that is simply open, and the two need different responses.
func (b *Breaker) Transitions() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transitions
}

// LastError returns the failure that most recently opened the circuit, which is
// what an operator needs in order to know whether to page the POS vendor.
func (b *Breaker) LastError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

func (b *Breaker) maybeHalfOpenLocked() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cfg.Cooldown {
		b.state = StateHalfOpen
		b.probes = 0
		b.successes = 0
		b.transitions++
	}
}

// Do runs fn under the breaker.
//
// A context cancellation is not counted as a dependency failure: the caller
// gave up, which says nothing about the health of the POS, and counting it
// would let a burst of client timeouts open a circuit against a service that is
// perfectly well.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := b.allow(); err != nil {
		return err
	}
	err := fn(ctx)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		b.release()
		return err
	}
	b.record(err)
	return err
}

func (b *Breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	switch b.state {
	case StateOpen:
		b.rejectedCall++
		return ErrCircuitOpen
	case StateHalfOpen:
		if b.probes >= b.cfg.HalfOpenProbes {
			b.rejectedCall++
			return ErrCircuitOpen
		}
		b.probes++
	}
	return nil
}

// release returns a half-open probe slot taken by a call that was abandoned
// rather than answered, so a cancelled probe does not wedge the breaker in
// half-open until the next cooldown.
func (b *Breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateHalfOpen && b.probes > 0 {
		b.probes--
	}
}

func (b *Breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.lastErr = err
		b.failures++
		b.successes = 0
		if b.state == StateHalfOpen || b.failures >= b.cfg.FailureThreshold {
			b.state = StateOpen
			b.openedAt = b.now()
			b.probes = 0
			b.transitions++
		}
		return
	}
	switch b.state {
	case StateHalfOpen:
		b.successes++
		if b.successes >= b.cfg.SuccessThreshold {
			b.state = StateClosed
			b.failures = 0
			b.probes = 0
			b.successes = 0
			b.transitions++
		}
	default:
		b.failures = 0
	}
}

// BreakerSet holds one breaker per outbound dependency, created on demand.
type BreakerSet struct {
	cfg BreakerConfig
	now func() time.Time
	mu  sync.RWMutex
	bs  map[string]*Breaker
}

// NewBreakerSet creates a set that hands out breakers with cfg.
func NewBreakerSet(cfg BreakerConfig) *BreakerSet {
	return &BreakerSet{cfg: cfg.withDefaults(), bs: make(map[string]*Breaker)}
}

// WithClock injects a clock into the set and every breaker it creates.
func (s *BreakerSet) WithClock(now func() time.Time) *BreakerSet {
	s.mu.Lock()
	s.now = now
	for _, b := range s.bs {
		b.WithClock(now)
	}
	s.mu.Unlock()
	return s
}

// Get returns the breaker for name, creating it on first use.
func (s *BreakerSet) Get(name string) *Breaker {
	s.mu.RLock()
	b, ok := s.bs[name]
	s.mu.RUnlock()
	if ok {
		return b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok = s.bs[name]; ok {
		return b
	}
	b = NewBreaker(name, s.cfg)
	if s.now != nil {
		b.WithClock(s.now)
	}
	s.bs[name] = b
	return b
}

// Snapshot returns the current state of every breaker, for the bindings-health
// endpoint and for metrics.
func (s *BreakerSet) Snapshot() map[string]BreakerState {
	s.mu.RLock()
	names := make([]string, 0, len(s.bs))
	brs := make([]*Breaker, 0, len(s.bs))
	for n, b := range s.bs {
		names = append(names, n)
		brs = append(brs, b)
	}
	s.mu.RUnlock()
	out := make(map[string]BreakerState, len(names))
	for i, n := range names {
		out[n] = brs[i].State()
	}
	return out
}
