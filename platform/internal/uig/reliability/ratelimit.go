// Package reliability holds the two protections the UIG puts either side of a
// POS integration: an inbound rate limiter and an outbound circuit breaker.
//
// They guard opposite directions of the same blast radius. The limiter stops
// one retailer's misconfigured price-book publish — a genuine incident shape,
// where an ERP job re-publishes the entire catalogue in a loop — from consuming
// the ingress capacity every other tenant is relying on. The breaker stops the
// UIG from hammering a POS API that is already down, and from spending the
// gateway's 50ms latency slice waiting for connections that will time out.
//
// Both are deliberately simple, in-process and clock-injectable. A distributed
// rate limiter would be more accurate and would also put a network round trip
// on the hot path of every delivery; the per-replica approximation costs a
// factor of the replica count in headroom and costs nothing in latency, which
// is the right trade for a component with a 50ms budget.
package reliability

import (
	"math"
	"sync"
	"time"
)

// DefaultRate is the sustained per-binding ingress rate applied when a binding
// does not set one. It is sized to be generous for a normal store's price
// traffic and still low enough that a runaway publisher is throttled within a
// second rather than after it has filled a partition.
const DefaultRate = 200.0

// DefaultBurst is the default bucket depth. POS traffic is bursty by nature —
// a price-book publish arrives as thousands of webhooks in a few seconds — so
// the burst is an order of magnitude above the rate, and it is the sustained
// rate that does the actual limiting.
const DefaultBurst = 2000

// Limiter is a set of token buckets keyed by tenant and adapter.
//
// Keying by both is what makes the limit meaningful: a tenant running Shopify
// and a nightly SAP feed has two very different traffic shapes, and a single
// shared bucket would let the file drop starve the webhooks it shares a tenant
// with.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	// idle is how long an unused bucket is kept. Buckets are cheap, but a
	// gateway serving 50,000 bindings over a week would otherwise accumulate
	// one per binding that ever sent a single request.
	idle time.Duration
}

type bucket struct {
	tokens   float64
	rate     float64
	burst    float64
	last     time.Time
	lastSeen time.Time
}

// NewLimiter creates a limiter.
func NewLimiter() *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		now:     time.Now,
		idle:    30 * time.Minute,
	}
}

// WithClock injects a clock so tests can advance time deterministically instead
// of sleeping, which is the difference between a rate-limit test that takes a
// microsecond and one that takes a second and is flaky anyway.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
	return l
}

// Allow consumes one token for key, reporting whether the delivery may proceed
// and, when it may not, how long the caller should wait before retrying. The
// wait is returned so the gateway can send a Retry-After: a 429 without one
// tells a POS to guess, and POS systems guess badly.
func (l *Limiter) Allow(key string, rate float64, burst int) (bool, time.Duration) {
	if rate <= 0 {
		rate = DefaultRate
	}
	if burst <= 0 {
		burst = DefaultBurst
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), rate: rate, burst: float64(burst), last: now}
		l.buckets[key] = b
		l.sweepLocked(now)
	}
	// Re-reading rate and burst on every call means a configuration change to a
	// binding takes effect on the next delivery rather than on the next restart.
	if b.rate != rate || b.burst != float64(burst) {
		b.rate, b.burst = rate, float64(burst)
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
		b.last = now
	}
	b.lastSeen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	wait := time.Duration(deficit / b.rate * float64(time.Second))
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, wait
}

// sweepLocked drops buckets nothing has touched for the idle window. It runs on
// creation rather than on a timer so the limiter owns no goroutine, which keeps
// it usable from a test without a shutdown dance.
func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.idle {
			delete(l.buckets, k)
		}
	}
}

// Len reports how many buckets are live, for the gateway's own metrics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
