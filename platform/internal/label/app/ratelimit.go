package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TenantLimiter is a per-tenant token bucket.
//
// The problem it solves is specific and it is not "protect the service". A
// single tenant's overnight repricing enqueues 40,000 labels per store across
// hundreds of stores, and without a per-tenant bound it will occupy every
// worker in the pool for minutes. During those minutes another tenant's single
// urgent change — a mispriced item a manager is standing next to — waits behind
// it. A global limiter does not fix that: it just means the loudest tenant
// consumes the whole global budget. The bucket has to be per tenant, and each
// tenant's fan-out has to be shaped independently of every other's.
//
// Bursts are allowed because that is the shape of the real traffic: a
// promotion is not a steady stream, it is a cliff. The bucket therefore carries
// capacity for one store's worth of labels and refills at the sustained rate.
//
// A TenantLimiter is safe for concurrent use.
type TenantLimiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[canon.TenantID]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// TenantLimiterConfig configures the limiter.
type TenantLimiterConfig struct {
	// RatePerSecond is the sustained per-tenant label update rate. Zero means
	// DefaultTenantRate.
	RatePerSecond float64
	// Burst is the bucket capacity. Zero means DefaultTenantBurst.
	Burst float64
	// Now injects a clock for tests. Nil means time.Now.
	Now func() time.Time
}

// Per-tenant fan-out budget defaults.
//
// The rate is sized from the capacity model: 52,000 price updates per second is
// the platform's peak across every tenant, and no single tenant may consume
// more than a fifth of it without deliberate configuration. The burst is one
// store-wide promotion, so the common case — a chain pushing one store's
// planogram — never waits at all.
const (
	// DefaultTenantRate is the sustained per-tenant label updates per second.
	DefaultTenantRate = 10000.0
	// DefaultTenantBurst is the per-tenant burst allowance, one store's worth
	// of labels.
	DefaultTenantBurst = 40000.0
)

// NewTenantLimiter builds a limiter.
func NewTenantLimiter(cfg TenantLimiterConfig) *TenantLimiter {
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond = DefaultTenantRate
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultTenantBurst
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TenantLimiter{
		rate: cfg.RatePerSecond, burst: cfg.Burst, now: cfg.Now,
		buckets: make(map[canon.TenantID]*bucket),
	}
}

// Wait blocks until n units of budget are available for the tenant.
//
// It sleeps in one calculated interval rather than spinning, so a batch parked
// behind a full bucket costs nothing while it waits. A context deadline that
// falls before the tokens would arrive returns ErrRateLimited rather than the
// context error, so a caller can tell "your tenant is over budget" from "your
// request was cancelled" — different answers for the client and different
// alerts for the operator.
func (l *TenantLimiter) Wait(ctx context.Context, tenant canon.TenantID, n int) error {
	if n <= 0 {
		return nil
	}
	need := float64(n)
	if need > l.burst {
		// A single request larger than the bucket would wait forever. Clamping
		// to the burst lets a 50,000-label batch through in bucket-sized bites
		// instead of deadlocking on arithmetic.
		need = l.burst
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait := l.reserve(tenant, need)
		if wait <= 0 {
			return nil
		}
		// The deadline comparison uses real time, not the injected clock: a
		// context deadline is a real-world instant set by a caller, and
		// comparing it against a clock the limiter was handed for token
		// accounting would make the answer depend on which of the two was
		// mocked.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return fmt.Errorf("%w: tenant %s needs %s of budget, deadline is sooner",
				ports.ErrRateLimited, tenant, wait)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// reserve takes n tokens if they are available, otherwise reports how long to
// wait before trying again.
func (l *TenantLimiter) reserve(tenant canon.TenantID, n float64) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[tenant]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[tenant] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= n {
		b.tokens -= n
		return 0
	}
	deficit := n - b.tokens
	return time.Duration(deficit / l.rate * float64(time.Second))
}

// Tokens reports a tenant's current budget. It exists for the admin surface and
// for tests; nothing on the hot path reads it.
func (l *TenantLimiter) Tokens(tenant canon.TenantID) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[tenant]
	if !ok {
		return l.burst
	}
	return b.tokens
}

var _ ports.RateLimiter = (*TenantLimiter)(nil)
