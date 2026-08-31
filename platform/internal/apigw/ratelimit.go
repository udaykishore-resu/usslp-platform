package apigw

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rate limiting
//
// Three buckets, checked in order, all of them token buckets:
//
//	tenant     one per retailer — the contractual limit
//	credential one per API key or JWT subject — so a runaway nightly import
//	           cannot starve the store manager holding a browser open
//	expensive  a much tighter bucket, per tenant, that only the costly routes
//	           draw from: batch price updates and analytics queries
//
// A token bucket rather than a fixed window because retail traffic is bursty
// by nature. A 40,000-item batch import at 06:00 and a promotion activation at
// noon are both legitimate spikes; a fixed window either rejects them or is set
// so wide that it stops limiting anything. A bucket lets the burst through and
// then holds the sustained rate, which is exactly the contract a retailer
// signs.
// ---------------------------------------------------------------------------

// Limit describes a token bucket.
type Limit struct {
	// Rate is the sustained refill in tokens per second.
	Rate float64
	// Burst is the bucket depth: the largest instantaneous spike allowed.
	Burst float64
}

// Enabled reports whether the limit constrains anything.
func (l Limit) Enabled() bool { return l.Rate > 0 && l.Burst > 0 }

// Default rate limits. They are sized against what the platform is for: a
// tenant repricing an entire estate does it in batches, which is what the
// expensive bucket meters, while the per-credential limit is generous enough
// that a normal integration never notices it and low enough that a runaway
// retry loop is contained within a second.
var (
	// DefaultTenantLimit is the whole-organisation ceiling.
	DefaultTenantLimit = Limit{Rate: 500, Burst: 1000}
	// DefaultCredentialLimit is per API key or token subject.
	DefaultCredentialLimit = Limit{Rate: 100, Burst: 200}
	// DefaultExpensiveLimit governs batch price updates and analytics queries.
	// Ten per second sustained with a burst of twenty is one estate-wide
	// repricing run in flight plus headroom, and it is two orders of magnitude
	// below the general limit because one of these calls can cost an upstream
	// several seconds of CPU.
	DefaultExpensiveLimit = Limit{Rate: 10, Burst: 20}
)

// bucket is one token bucket. Tokens are stored as a float and refilled
// lazily from the elapsed time, so an idle bucket costs nothing and there is
// no timer per client.
type bucket struct {
	tokens float64
	last   time.Time
}

// Decision is the outcome of a limit check.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the bucket depth, reported as X-RateLimit-Limit.
	Limit int
	// Remaining is the whole tokens left after this request.
	Remaining int
	// Reset is how long until the bucket is full again. Reporting refill-to-
	// full rather than an opaque window boundary is the only honest answer for
	// a token bucket, and it is what a client needs in order to schedule a
	// backlog.
	Reset time.Duration
	// RetryAfter is how long until one token is available. Zero when allowed.
	RetryAfter time.Duration
	// Bucket names which tier produced this decision.
	Bucket string
}

// Limiter is a set of named token buckets sharing one rate.
type Limiter struct {
	name  string
	limit Limit

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

// NewLimiter builds a limiter.
func NewLimiter(name string, limit Limit, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{name: name, limit: limit, buckets: make(map[string]*bucket), now: now}
}

// Allow takes one token from a key's bucket.
func (l *Limiter) Allow(key string) Decision {
	if !l.limit.Enabled() {
		return Decision{Allowed: true, Bucket: l.name, Limit: 0, Remaining: 0}
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.limit.Burst, last: now}
		l.buckets[key] = b
	} else {
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = math.Min(l.limit.Burst, b.tokens+elapsed.Seconds()*l.limit.Rate)
		}
	}
	b.last = now

	d := Decision{Bucket: l.name, Limit: int(l.limit.Burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		// Deficit to one whole token, converted to a wait. Rounded up to the
		// millisecond so that a client obeying Retry-After to the letter is
		// never rejected a second time for arriving a microsecond early.
		need := 1 - b.tokens
		d.RetryAfter = time.Duration(math.Ceil(need/l.limit.Rate*1000)) * time.Millisecond
	}
	d.Remaining = int(b.tokens)
	deficit := l.limit.Burst - b.tokens
	d.Reset = time.Duration(math.Ceil(deficit/l.limit.Rate*1000)) * time.Millisecond
	return d
}

// Sweep drops buckets that are full and have been idle, bounding memory.
//
// Without it the map grows one entry per credential ever seen, and the
// cardinality of "every JWT subject in a 50,000-store retailer" is not a
// number to leave unbounded in a process that must not be restarted during a
// promotion. A full bucket carries no state worth keeping: recreating it gives
// exactly the same answer.
func (l *Limiter) Sweep(idle time.Duration) int {
	if !l.limit.Enabled() {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for k, b := range l.buckets {
		refilled := b.tokens + now.Sub(b.last).Seconds()*l.limit.Rate
		if refilled >= l.limit.Burst && now.Sub(b.last) >= idle {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}

// Len reports the number of live buckets, for the gateway's own metrics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// RateLimiter is the three-tier limiter the gateway installs.
type RateLimiter struct {
	tenant     *Limiter
	credential *Limiter
	expensive  *Limiter
}

// RateLimitConfig configures the tiers. A zero Limit disables that tier.
type RateLimitConfig struct {
	Tenant     Limit
	Credential Limit
	Expensive  Limit
	Now        func() time.Time
}

// NewRateLimiter builds the tiered limiter.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		tenant:     NewLimiter("tenant", cfg.Tenant, cfg.Now),
		credential: NewLimiter("credential", cfg.Credential, cfg.Now),
		expensive:  NewLimiter("expensive", cfg.Expensive, cfg.Now),
	}
}

// Allow charges a request against every tier that applies.
//
// The tiers are charged in order and evaluation stops at the first refusal, so
// a request rejected by the tenant bucket has not also consumed a credential
// token. That does mean a request refused by a later tier has already spent an
// earlier tier's token; the over-charge is at most one token per tier per
// rejected request and it errs towards throttling harder under abuse, which is
// the right direction for the failure that matters.
func (rl *RateLimiter) Allow(p Principal, expensive bool) Decision {
	tenantKey := "tenant:" + string(p.TenantID)
	if d := rl.tenant.Allow(tenantKey); !d.Allowed {
		return d
	}
	credKey := p.RateKey
	if credKey == "" {
		credKey = tenantKey
	}
	d := rl.credential.Allow(credKey)
	if !d.Allowed {
		return d
	}
	if expensive {
		// The headers describe the binding constraint. On an expensive route
		// that is the expensive bucket whether or not it refused, because a
		// client whose batch import is being paced needs to see the number
		// that is pacing it, not the general limit it is nowhere near.
		return rl.expensive.Allow(tenantKey)
	}
	return d
}

// Sweep prunes idle buckets across every tier.
func (rl *RateLimiter) Sweep(idle time.Duration) int {
	return rl.tenant.Sweep(idle) + rl.credential.Sweep(idle) + rl.expensive.Sweep(idle)
}

// applyRateHeaders writes the X-RateLimit family.
func applyRateHeaders(w http.ResponseWriter, d Decision) {
	if d.Limit <= 0 {
		return
	}
	h := w.Header()
	h.Set(HeaderRateLimit, strconv.Itoa(d.Limit))
	h.Set(HeaderRateRemaining, strconv.Itoa(max(d.Remaining, 0)))
	h.Set(HeaderRateReset, strconv.Itoa(int(math.Ceil(d.Reset.Seconds()))))
	h.Set(HeaderRateBucket, d.Bucket)
}

// rateLimit is the middleware.
func (g *Gateway) rateLimit(route *Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route.Public || g.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		p, err := principalOf(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		d := g.limiter.Allow(p, route.Expensive)
		applyRateHeaders(w, d)
		if !d.Allowed {
			// Retry-After in seconds, rounded up and never zero: a client told
			// to retry after zero seconds retries immediately, which is the
			// behaviour the limit exists to stop.
			seconds := int(math.Ceil(d.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			g.metrics.RateLimited.With(d.Bucket, route.Operation).Inc()
			e := statusError(http.StatusTooManyRequests, "rate_limited",
				"rate limit exceeded on the %s bucket; retry in %ds", d.Bucket, seconds)
			e.headers = map[string]string{HeaderRetryAfter: strconv.Itoa(seconds)}
			writeError(w, r, e)
			return
		}
		next.ServeHTTP(w, r)
	})
}
