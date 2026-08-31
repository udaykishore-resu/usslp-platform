package apigw

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestTokenBucketBurstsThenThrottles(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{
			Tenant:     Limit{Rate: 1000, Burst: 1000},
			Credential: Limit{Rate: 2, Burst: 5},
			Expensive:  Limit{Rate: 1000, Burst: 1000},
		}
	})
	key := h.issueKey("acme", []Role{RoleReadOnly})

	// The burst goes through: a bucket exists precisely so a legitimate spike
	// is not punished.
	for i := 0; i < 5; i++ {
		res := h.do(http.MethodGet, "/v1/me", key, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d of the burst: got %d, want 200", i+1, res.StatusCode)
		}
		if got := res.Header.Get(HeaderRateLimit); got != "5" {
			t.Fatalf("X-RateLimit-Limit = %q, want 5", got)
		}
		wantRemaining := strconv.Itoa(4 - i)
		if got := res.Header.Get(HeaderRateRemaining); got != wantRemaining {
			t.Fatalf("request %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}

	// The sixth is refused, with everything a client needs to back off.
	res := h.do(http.MethodGet, "/v1/me", key, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after the burst: got %d, want 429", res.StatusCode)
	}
	if got := res.Header.Get(HeaderRateRemaining); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if got := res.Header.Get(HeaderRateBucket); got != "credential" {
		t.Fatalf("X-RateLimit-Bucket = %q, want credential", got)
	}
	retry := res.Header.Get(HeaderRetryAfter)
	seconds, err := strconv.Atoi(retry)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q; it must be a whole number of seconds and never zero", retry)
	}
	if got := res.Header.Get(HeaderRateReset); got == "" || got == "0" {
		t.Fatalf("X-RateLimit-Reset = %q, want the seconds until the bucket refills", got)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "rate_limited" {
		t.Fatalf("error code %q, want rate_limited", body.Code)
	}

	// Obeying Retry-After works: after the advertised wait, a token exists.
	h.clock.Advance(time.Duration(seconds) * time.Second)
	if got := h.do(http.MethodGet, "/v1/me", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("after honouring Retry-After: got %d, want 200", got)
	}
}

func TestRateLimitIsPerTenantAndPerCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{
			Tenant:     Limit{Rate: 1000, Burst: 1000},
			Credential: Limit{Rate: 1, Burst: 2},
			Expensive:  Limit{Rate: 1000, Burst: 1000},
		}
	})
	noisy := h.issueKey("acme", []Role{RoleReadOnly})
	quiet := h.issueKey("acme", []Role{RoleReadOnly})
	otherTenant := h.issueKey("beta", []Role{RoleReadOnly})

	// Exhaust one credential.
	for i := 0; i < 2; i++ {
		if got := h.do(http.MethodGet, "/v1/me", noisy, nil).StatusCode; got != http.StatusOK {
			t.Fatalf("burst request %d: %d", i, got)
		}
	}
	if got := h.do(http.MethodGet, "/v1/me", noisy, nil).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("the noisy credential was not throttled: %d", got)
	}
	// A second credential in the same tenant is unaffected: one runaway
	// nightly import must not lock out the store manager holding a browser.
	if got := h.do(http.MethodGet, "/v1/me", quiet, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("a sibling credential was throttled by its neighbour: %d", got)
	}
	if got := h.do(http.MethodGet, "/v1/me", otherTenant, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("another tenant was throttled: %d", got)
	}
}

func TestTenantBucketConstrainsEveryCredentialTogether(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{
			Tenant:     Limit{Rate: 1, Burst: 3},
			Credential: Limit{Rate: 1000, Burst: 1000},
			Expensive:  Limit{Rate: 1000, Burst: 1000},
		}
	})
	first := h.issueKey("acme", []Role{RoleReadOnly})
	second := h.issueKey("acme", []Role{RoleReadOnly})
	other := h.issueKey("beta", []Role{RoleReadOnly})

	// Three requests across two credentials drain the shared tenant bucket.
	for i, key := range []string{first, second, first} {
		if got := h.do(http.MethodGet, "/v1/me", key, nil).StatusCode; got != http.StatusOK {
			t.Fatalf("tenant burst request %d: %d", i, got)
		}
	}
	res := h.do(http.MethodGet, "/v1/me", second, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the tenant ceiling was not enforced: %d", res.StatusCode)
	}
	if got := res.Header.Get(HeaderRateBucket); got != "tenant" {
		t.Fatalf("bucket %q, want tenant — a client needs to know which limit is binding", got)
	}
	if got := h.do(http.MethodGet, "/v1/me", other, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("one tenant's ceiling throttled another: %d", got)
	}
}

func TestExpensiveRoutesDrawOnATighterBucket(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{
			Tenant:     Limit{Rate: 1000, Burst: 1000},
			Credential: Limit{Rate: 1000, Burst: 1000},
			Expensive:  Limit{Rate: 1, Burst: 2},
		}
	})
	key := h.issueKey("acme", []Role{RoleOwner})
	batch := map[string]any{"items": []map[string]any{
		{"store_id": "store-1", "sku": "sku-1", "price": map[string]any{"amount": 199, "currency": "GBP"}},
	}}

	for i := 0; i < 2; i++ {
		if got := h.do(http.MethodPost, "/v1/prices:batch", key, batch).StatusCode; got == http.StatusTooManyRequests {
			t.Fatalf("expensive burst request %d was throttled early", i)
		}
	}
	res := h.do(http.MethodPost, "/v1/prices:batch", key, batch)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the expensive bucket did not throttle a third batch: %d", res.StatusCode)
	}
	if got := res.Header.Get(HeaderRateBucket); got != "expensive" {
		t.Fatalf("bucket %q, want expensive", got)
	}

	// A cheap route is untouched: the whole point of the second bucket is that
	// an expensive endpoint being paced does not stop the console refreshing.
	if got := h.do(http.MethodGet, "/v1/me", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("a cheap route was throttled by the expensive bucket: %d", got)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()
	clock := newClock()
	limiter := NewLimiter("test", Limit{Rate: 10, Burst: 10}, clock.Now)

	for i := 0; i < 10; i++ {
		if d := limiter.Allow("k"); !d.Allowed {
			t.Fatalf("token %d of the burst was refused", i)
		}
	}
	if d := limiter.Allow("k"); d.Allowed {
		t.Fatal("an eleventh token came out of a bucket of ten")
	}

	// Half a second at ten per second is five tokens.
	clock.Advance(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if d := limiter.Allow("k"); !d.Allowed {
			t.Fatalf("refilled token %d was refused", i)
		}
	}
	if d := limiter.Allow("k"); d.Allowed {
		t.Fatal("the bucket refilled by more than the elapsed time allows")
	}

	// And it never overfills: an hour of idleness still leaves burst tokens.
	clock.Advance(time.Hour)
	count := 0
	for limiter.Allow("k").Allowed {
		count++
		if count > 100 {
			t.Fatal("the bucket overfilled past its burst")
		}
	}
	if count != 10 {
		t.Fatalf("after a long idle period the bucket held %d tokens, want the burst of 10", count)
	}
}

func TestIdleBucketsAreSwept(t *testing.T) {
	t.Parallel()
	clock := newClock()
	limiter := NewLimiter("test", Limit{Rate: 10, Burst: 10}, clock.Now)

	for i := 0; i < 50; i++ {
		limiter.Allow("client-" + strconv.Itoa(i))
	}
	if limiter.Len() != 50 {
		t.Fatalf("%d buckets, want 50", limiter.Len())
	}
	// Not yet idle: sweeping must not forgive a client that is still limited.
	if removed := limiter.Sweep(time.Minute); removed != 0 {
		t.Fatalf("swept %d buckets that were neither full nor idle", removed)
	}
	clock.Advance(10 * time.Minute)
	if removed := limiter.Sweep(time.Minute); removed != 50 {
		t.Fatalf("swept %d buckets, want 50", removed)
	}
	if limiter.Len() != 0 {
		t.Fatalf("%d buckets survived the sweep", limiter.Len())
	}
}

func TestDisabledLimiterAllowsEverythingAndEmitsNoHeaders(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{} // every tier zero: limiting off
	})
	key := h.issueKey("acme", []Role{RoleReadOnly})
	for i := 0; i < 25; i++ {
		res := h.do(http.MethodGet, "/v1/me", key, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d refused with limiting disabled: %d", i, res.StatusCode)
		}
		if got := res.Header.Get(HeaderRateLimit); got != "" {
			t.Fatalf("a disabled limiter advertised a limit of %q", got)
		}
	}
}

func TestUnauthenticatedRequestsAreNotChargedToATenant(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.rateLimit = RateLimitConfig{
			Tenant:     Limit{Rate: 1, Burst: 1},
			Credential: Limit{Rate: 1, Burst: 1},
			Expensive:  Limit{Rate: 1, Burst: 1},
		}
	})
	// A flood of anonymous requests is rejected by authentication, the
	// cheapest check, and must not consume a real tenant's tokens.
	for i := 0; i < 20; i++ {
		if got := h.do(http.MethodGet, "/v1/me", "", nil).StatusCode; got != http.StatusUnauthorized {
			t.Fatalf("anonymous request %d: got %d, want 401", i, got)
		}
	}
	key := h.issueKey("acme", []Role{RoleReadOnly})
	if got := h.do(http.MethodGet, "/v1/me", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("an anonymous flood consumed the tenant's budget: %d", got)
	}
}
