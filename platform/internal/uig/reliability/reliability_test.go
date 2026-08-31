package reliability

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock lets the limiter and breaker be tested at the microsecond rather
// than by sleeping, which is the difference between a suite that runs in
// milliseconds and one that is both slow and flaky.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestLimiterBurstThenRate(t *testing.T) {
	clk := newClock()
	l := NewLimiter().WithClock(clk.Now)

	// The burst is spent first, at full speed, because a price-book publish is
	// legitimately thousands of webhooks in a few seconds.
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("acme|shopify", 10, 5); !ok {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	ok, wait := l.Allow("acme|shopify", 10, 5)
	if ok {
		t.Fatal("the sixth request should have exhausted the burst")
	}
	// At 10/s the caller must wait about 100ms, and must be told so: a 429 with
	// no Retry-After makes a POS guess, and POS systems guess by retrying at
	// once.
	if wait <= 0 || wait > 200*time.Millisecond {
		t.Fatalf("wait = %s, want roughly 100ms", wait)
	}

	// After a tenth of a second exactly one token has refilled.
	clk.Advance(100 * time.Millisecond)
	if ok, _ := l.Allow("acme|shopify", 10, 5); !ok {
		t.Fatal("a token should have refilled after 100ms")
	}
	if ok, _ := l.Allow("acme|shopify", 10, 5); ok {
		t.Fatal("only one token should have refilled")
	}

	// Refill saturates at the burst rather than accumulating forever.
	clk.Advance(time.Hour)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("acme|shopify", 10, 5); !ok {
			t.Fatalf("refilled request %d refused", i)
		}
	}
	if ok, _ := l.Allow("acme|shopify", 10, 5); ok {
		t.Fatal("the bucket accumulated beyond its burst")
	}
}

func TestLimiterIsolatesKeys(t *testing.T) {
	clk := newClock()
	l := NewLimiter().WithClock(clk.Now)
	for i := 0; i < 3; i++ {
		l.Allow("acme|sap", 1, 3)
	}
	if ok, _ := l.Allow("acme|sap", 1, 3); ok {
		t.Fatal("the sap bucket should be empty")
	}
	// A tenant's nightly SAP drop must not be able to starve its own Shopify
	// webhooks, which is the entire reason the key includes the adapter.
	if ok, _ := l.Allow("acme|shopify", 1, 3); !ok {
		t.Fatal("a separate adapter's bucket was affected")
	}
	if ok, _ := l.Allow("other|sap", 1, 3); !ok {
		t.Fatal("a separate tenant's bucket was affected")
	}
	if l.Len() != 3 {
		t.Errorf("Len = %d, want 3 buckets", l.Len())
	}
}

func TestLimiterPicksUpConfigurationChanges(t *testing.T) {
	clk := newClock()
	l := NewLimiter().WithClock(clk.Now)
	l.Allow("k", 100, 1)
	if ok, _ := l.Allow("k", 100, 1); ok {
		t.Fatal("burst of one should be spent")
	}
	// Raising the binding's burst must take effect on the next delivery rather
	// than on the next restart.
	clk.Advance(time.Second)
	if ok, _ := l.Allow("k", 100, 50); !ok {
		t.Fatal("a raised burst was not picked up")
	}
}

func TestLimiterDefaults(t *testing.T) {
	l := NewLimiter()
	if ok, _ := l.Allow("k", 0, 0); !ok {
		t.Fatal("zero rate and burst must fall back to the defaults, not to zero")
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	clk := newClock()
	b := NewBreaker("pos", BreakerConfig{
		FailureThreshold: 3, Cooldown: 5 * time.Second, HalfOpenProbes: 1, SuccessThreshold: 1,
	}).WithClock(clk.Now)
	ctx := context.Background()
	boom := errors.New("connection refused")
	fail := func(context.Context) error { return boom }
	ok := func(context.Context) error { return nil }

	if b.State() != StateClosed {
		t.Fatal("a new breaker must be closed")
	}
	for i := 0; i < 2; i++ {
		if err := b.Do(ctx, fail); !errors.Is(err, boom) {
			t.Fatalf("attempt %d err = %v", i, err)
		}
		if b.State() != StateClosed {
			t.Fatalf("breaker opened after %d failures, threshold is 3", i+1)
		}
	}
	if err := b.Do(ctx, fail); !errors.Is(err, boom) {
		t.Fatalf("third failure err = %v", err)
	}
	if b.State() != StateOpen {
		t.Fatal("the breaker did not open at its threshold")
	}

	// While open the dependency is not called at all — which is the point: a
	// POS timing out at 30 seconds would otherwise consume the gateway's entire
	// 50ms budget on every delivery.
	var called atomic.Int32
	err := b.Do(ctx, func(context.Context) error { called.Add(1); return nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open circuit err = %v, want ErrCircuitOpen", err)
	}
	if called.Load() != 0 {
		t.Fatal("the breaker called through while open")
	}
	if b.Rejected() != 1 {
		t.Errorf("Rejected = %d, want 1", b.Rejected())
	}
	if !errors.Is(b.LastError(), boom) {
		t.Errorf("LastError = %v", b.LastError())
	}

	// After the cooldown one probe is allowed through.
	clk.Advance(5 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state after cooldown = %s, want half_open", b.State())
	}
	if err := b.Do(ctx, ok); err != nil {
		t.Fatalf("probe err = %v", err)
	}
	if b.State() != StateClosed {
		t.Fatalf("a successful probe should close the circuit, state = %s", b.State())
	}
}

func TestBreakerReopensOnFailedProbe(t *testing.T) {
	clk := newClock()
	b := NewBreaker("pos", BreakerConfig{
		FailureThreshold: 1, Cooldown: time.Second, HalfOpenProbes: 1,
	}).WithClock(clk.Now)
	ctx := context.Background()
	boom := errors.New("still down")
	_ = b.Do(ctx, func(context.Context) error { return boom })
	if b.State() != StateOpen {
		t.Fatal("threshold of one should open immediately")
	}
	clk.Advance(time.Second)
	if b.State() != StateHalfOpen {
		t.Fatal("cooldown did not move the breaker to half-open")
	}
	// A single failed probe re-opens: a dependency that is still down must not
	// be given a second chance in the same cooldown window.
	_ = b.Do(ctx, func(context.Context) error { return boom })
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open after a failed probe", b.State())
	}
	if b.Transitions() < 3 {
		t.Errorf("Transitions = %d, want at least 3 (open, half-open, open)", b.Transitions())
	}
}

func TestBreakerIgnoresContextCancellation(t *testing.T) {
	b := NewBreaker("pos", BreakerConfig{FailureThreshold: 1, Cooldown: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The caller gave up. That says nothing about the health of the POS, and
	// counting it would let a burst of client timeouts open a circuit against a
	// dependency that is perfectly well.
	err := b.Do(ctx, func(ctx context.Context) error { return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if b.State() != StateClosed {
		t.Fatalf("a cancelled call opened the circuit; state = %s", b.State())
	}
}

func TestBreakerSetIsolatesDependencies(t *testing.T) {
	clk := newClock()
	set := NewBreakerSet(BreakerConfig{FailureThreshold: 1, Cooldown: time.Minute}).WithClock(clk.Now)
	ctx := context.Background()
	_ = set.Get("clover/M1").Do(ctx, func(context.Context) error { return errors.New("x") })

	snap := set.Snapshot()
	if snap["clover/M1"] != StateOpen {
		t.Fatalf("M1 state = %v", snap["clover/M1"])
	}
	// One merchant's revoked token must not stop every other merchant's price
	// changes, so breakers are per dependency rather than per adapter.
	if err := set.Get("clover/M2").Do(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("M2 err = %v", err)
	}
	if set.Get("clover/M2").State() != StateClosed {
		t.Error("M2 was affected by M1's failure")
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	l := NewLimiter()
	set := NewBreakerSet(BreakerConfig{FailureThreshold: 3, Cooldown: time.Millisecond})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Allow("tenant|adapter", 1000, 100)
				br := set.Get("dep")
				_ = br.Do(ctx, func(context.Context) error {
					if j%7 == 0 {
						return errors.New("flap")
					}
					return nil
				})
				_ = br.State()
			}
			set.Snapshot()
		}(i)
	}
	wg.Wait()
}
