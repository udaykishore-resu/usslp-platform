package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// movingClock is a manually advanced clock, so the limiter's refill behaviour
// can be asserted exactly rather than approximately.
type movingClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *movingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *movingClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func TestTenantLimiterIsolatesTenants(t *testing.T) {
	clock := &movingClock{at: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)}
	l := NewTenantLimiter(TenantLimiterConfig{
		RatePerSecond: 100, Burst: 100, Now: clock.Now,
	})
	ctx := context.Background()

	// One tenant drains its whole bucket.
	if err := l.Wait(ctx, tenantA, 100); err != nil {
		t.Fatalf("draining tenant A: %v", err)
	}
	if got := l.Tokens(tenantA); got > 0.001 {
		t.Fatalf("tenant A has %v tokens left, want 0", got)
	}

	// The other tenant's urgent single change must not wait behind it. Under a
	// deadline shorter than the refill, an exhausted bucket reports
	// ErrRateLimited while a full one returns immediately — and that
	// difference is the whole point of the per-tenant split.
	deadlined, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(deadlined, tenantB, 1); err != nil {
		t.Fatalf("tenant B was starved by tenant A's bulk repricing: %v", err)
	}
	if err := l.Wait(deadlined, tenantA, 100); !errors.Is(err, ports.ErrRateLimited) {
		t.Fatalf("exhausted bucket returned %v, want ErrRateLimited", err)
	}
}

func TestTenantLimiterRefills(t *testing.T) {
	clock := &movingClock{at: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)}
	l := NewTenantLimiter(TenantLimiterConfig{
		RatePerSecond: 1000, Burst: 1000, Now: clock.Now,
	})
	ctx := context.Background()
	if err := l.Wait(ctx, tenantA, 1000); err != nil {
		t.Fatalf("drain: %v", err)
	}
	clock.advance(500 * time.Millisecond)
	if got := l.Tokens(tenantA); got != 0 {
		// Tokens is read-only; the refill is applied on the next reserve.
		t.Fatalf("Tokens must not refill on read: %v", got)
	}
	if err := l.Wait(ctx, tenantA, 500); err != nil {
		t.Fatalf("half a second of refill should cover 500 units: %v", err)
	}
	// And the bucket never exceeds its burst, however long it idles.
	clock.advance(time.Hour)
	if err := l.Wait(ctx, tenantA, 1000); err != nil {
		t.Fatalf("a full bucket after an idle hour: %v", err)
	}
	deadlined, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(deadlined, tenantA, 1000); !errors.Is(err, ports.ErrRateLimited) {
		t.Fatalf("the bucket accumulated beyond its burst: %v", err)
	}
}

func TestTenantLimiterClampsOversizedRequests(t *testing.T) {
	clock := &movingClock{at: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)}
	l := NewTenantLimiter(TenantLimiterConfig{
		RatePerSecond: 100, Burst: 100, Now: clock.Now,
	})
	// A single charge larger than the bucket would otherwise wait forever on
	// arithmetic that can never be satisfied.
	if err := l.Wait(context.Background(), tenantA, 10_000); err != nil {
		t.Fatalf("oversized charge: %v", err)
	}
}

func TestTenantLimiterRespectsCancellation(t *testing.T) {
	l := NewTenantLimiter(TenantLimiterConfig{RatePerSecond: 1, Burst: 1})
	ctx := context.Background()
	if err := l.Wait(ctx, tenantA, 1); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.Wait(cancelled, tenantA, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestTenantLimiterIsSafeUnderConcurrency(t *testing.T) {
	l := NewTenantLimiter(TenantLimiterConfig{RatePerSecond: 1e9, Burst: 1e6})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tenant := canon.TenantID("tenant-" + string(rune('a'+n%4)))
			for j := 0; j < 64; j++ {
				if err := l.Wait(ctx, tenant, 1); err != nil {
					t.Errorf("wait: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestTenantLimiterDefaults(t *testing.T) {
	l := NewTenantLimiter(TenantLimiterConfig{})
	if got := l.Tokens("unseen"); got != DefaultTenantBurst {
		t.Fatalf("a tenant's first sight of the limiter starts at %v, want the full burst %v",
			got, DefaultTenantBurst)
	}
	// One store-wide promotion must not wait at all: that is what the burst is
	// sized for.
	if err := l.Wait(context.Background(), "unseen", 40_000); err != nil {
		t.Fatalf("a 40,000-label store promotion waited: %v", err)
	}
}
