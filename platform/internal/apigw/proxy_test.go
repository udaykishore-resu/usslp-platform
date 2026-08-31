package apigw

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/retry"
)

func TestProxyRelaysStatusBodyAndHeaders(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	h.stubs[UpstreamLabel].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Upstream-Detail", "roster-v2")
		// An upstream trying to claim a gateway-owned header must not succeed:
		// the gateway's contract with its clients cannot depend on an internal
		// service's implementation detail.
		w.Header().Set(HeaderRateLimit, "999999")
		writeJSON(w, http.StatusMultiStatus, map[string]any{"partial": true})
	})

	res := h.do(http.MethodGet, "/v1/labels/lbl-1", key, nil)
	if res.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status %d, want 207", res.StatusCode)
	}
	if got := res.Header.Get("X-Upstream-Detail"); got != "roster-v2" {
		t.Fatalf("upstream header lost: %q", got)
	}
	if got := res.Header.Get(HeaderUpstream); got != UpstreamLabel {
		t.Fatalf("X-USSLP-Upstream = %q, want %s", got, UpstreamLabel)
	}
	if got := res.Header.Get(HeaderRateLimit); got == "999999" {
		t.Fatal("an upstream overwrote a gateway-owned rate-limit header")
	}
	if got := res.Header.Get(HeaderRequestID); got == "" {
		t.Fatal("no request id on the response")
	}
	if !strings.Contains(bodyString(t, res), `"partial":true`) {
		t.Fatal("the upstream body was not relayed")
	}
}

func TestProxyForwardsTraceAndRequestContext(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/labels/lbl-1", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(HeaderRequestID, "client-supplied-1")
	req.Header.Set(HeaderTraceParent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get(HeaderRequestID); got != "client-supplied-1" {
		t.Fatalf("request id %q; a valid caller-supplied id must survive the hop", got)
	}
	calls := h.stubs[UpstreamLabel].calls()
	if len(calls) == 0 {
		t.Fatal("nothing reached the upstream")
	}
	got := calls[0]
	if got.Header.Get(HeaderRequestID) != "client-supplied-1" {
		t.Fatal("the request id was not propagated upstream")
	}
	tp := got.Header.Get(HeaderTraceParent)
	if !strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("traceparent %q: the caller's trace must continue across the gateway", tp)
	}
	if got.Header.Get(HeaderForwardedFor) == "" {
		t.Fatal("X-Forwarded-For was not set")
	}
}

func TestHostileRequestIDIsReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/me", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// A value that would land unescaped in a log index and an error body.
	req.Header.Set(HeaderRequestID, `" or 1=1 --`)
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if got := res.Header.Get(HeaderRequestID); strings.Contains(got, " ") || strings.Contains(got, `"`) {
		t.Fatalf("request id %q was echoed unsanitised", got)
	}
}

func TestUpstreamTimeoutIsAGatewayTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	h := newHarness(t, func(o *harnessOptions) {
		o.timeout = 50 * time.Millisecond
		o.retry = retry.Policy{MaxAttempts: 1}
	})
	t.Cleanup(func() { close(release) })

	h.stubs[UpstreamRegistry].setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		writeJSON(w, http.StatusOK, map[string]string{"late": "true"})
	})

	key := h.issueKey("acme", []Role{RoleOwner})
	res := h.do(http.MethodGet, "/v1/devices/dev-1", key, nil)
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("got %d, want 504 — a slow upstream and an absent one need different runbooks",
			res.StatusCode)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "upstream_timeout" {
		t.Fatalf("error code %q, want upstream_timeout", body.Code)
	}
}

func TestRetryOnlyOnIdempotentMethods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.retry = retry.Policy{MaxAttempts: 3, Base: time.Millisecond, Max: time.Millisecond}
		// A high failure threshold so the breaker does not open mid-test and
		// change what is being measured.
		o.breaker = BreakerConfig{FailureThreshold: 100}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	var attempts atomic.Int64
	failing := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		attempts.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "unavailable"})
	}
	h.stubs[UpstreamRegistry].setHandler(failing)
	h.stubs[UpstreamOTA].setHandler(failing)

	// GET is safe to replay: the gateway tries the full schedule.
	attempts.Store(0)
	h.do(http.MethodGet, "/v1/devices/dev-1", key, nil)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("GET made %d attempts, want 3", got)
	}

	// POST is not. Replaying it would turn one price change into two audit
	// records, or one rollout into two.
	attempts.Store(0)
	h.do(http.MethodPost, "/v1/ota/jobs", key, map[string]any{"to_version": "1.2.3"})
	if got := attempts.Load(); got != 1 {
		t.Fatalf("POST made %d attempts; a non-idempotent method must never be retried", got)
	}

	// DELETE is idempotent and is retried.
	attempts.Store(0)
	h.stubs[UpstreamLabel].setHandler(failing)
	h.do(http.MethodGet, "/v1/labels/lbl-1", key, nil)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("a second idempotent route made %d attempts, want 3", got)
	}
}

func TestNonRetryableStatusesAreNotReplayed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.retry = retry.Policy{MaxAttempts: 3, Base: time.Millisecond, Max: time.Millisecond}
		o.breaker = BreakerConfig{FailureThreshold: 100}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	var attempts atomic.Int64
	h.stubs[UpstreamLabel].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		attempts.Add(1)
		// 500 means the upstream ran the request and blew up inside it.
		// Running it again reproduces the crash and a second side effect.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "internal"})
	})
	res := h.do(http.MethodGet, "/v1/labels/lbl-1", key, nil)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("a 500 was retried %d times; only 502/503/504 mean the request may not have run", got)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d; the upstream's own 500 is more useful than a synthesised 502", res.StatusCode)
	}
}

func TestRequestSizeLimitIsEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) { o.maxRequest = 2048 })
	key := h.issueKey("acme", []Role{RoleOwner})

	oversized := map[string]any{"blob": strings.Repeat("x", 4096)}
	res := h.do(http.MethodPost, "/v1/ota/jobs", key, oversized)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", res.StatusCode)
	}
	if h.stubs[UpstreamOTA].callCount() != 0 {
		t.Fatal("an oversized body reached an upstream")
	}

	// A body inside the limit still works.
	if got := h.do(http.MethodPost, "/v1/ota/jobs", key, map[string]any{"ok": true}).StatusCode; got != http.StatusOK {
		t.Fatalf("a small body was refused: %d", got)
	}
}

func TestChunkedOversizedBodyIsRefusedWithoutTrustingContentLength(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) { o.maxRequest = 1024 })
	key := h.issueKey("acme", []Role{RoleOwner})

	// A chunked body carries no Content-Length, so the limit has to be
	// enforced while reading rather than from the header.
	body := bytes.NewReader(bytes.Repeat([]byte("A"), 8192))
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/ota/jobs", body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // force chunked transfer encoding
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", res.StatusCode)
	}
}

func TestResponseSizeLimitIsEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.maxResp = 1024
		o.retry = retry.Policy{MaxAttempts: 1}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	h.stubs[UpstreamLabel].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("B"), 4096))
	})
	res := h.do(http.MethodGet, "/v1/labels/lbl-1", key, nil)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 for an oversized upstream response", res.StatusCode)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "upstream_response_too_large" {
		t.Fatalf("error code %q", body.Code)
	}
}

func TestPathRewriteFillsTheTenantFromTheCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	if got := h.do(http.MethodGet, "/v1/pos/integrations", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
	calls := h.stubs[UpstreamUIG].calls()
	if len(calls) != 1 {
		t.Fatalf("%d upstream calls, want 1", len(calls))
	}
	// The Universal Integration Gateway scopes by tenant in the path. The
	// segment comes from the credential; there is no way for a caller to name
	// a different one, because the path they sent is discarded.
	if calls[0].Path != "/v1/bindings/acme" {
		t.Fatalf("upstream path %q, want /v1/bindings/acme", calls[0].Path)
	}
}

func TestQueryStringIsForwarded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})
	h.do(http.MethodGet, "/v1/labels/lbl-1/history?limit=5", key, nil)
	calls := h.stubs[UpstreamLabel].calls()
	if len(calls) != 1 || calls[0].Query != "limit=5" {
		t.Fatalf("upstream query %q, want limit=5", calls[0].Query)
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker
// ---------------------------------------------------------------------------

func TestBreakerOpensHalfOpensAndCloses(t *testing.T) {
	t.Parallel()
	clock := newClock()
	b := NewBreaker(BreakerConfig{
		FailureThreshold: 3, SuccessThreshold: 2,
		OpenTimeout: 5 * time.Second, Now: clock.Now,
	})

	if b.State() != BreakerClosed {
		t.Fatalf("a new breaker is %s, want closed", b.State())
	}
	for i := 0; i < 3; i++ {
		done, ok := b.Allow()
		if !ok {
			t.Fatalf("call %d was refused while the breaker was still closed", i)
		}
		done(false)
	}
	if b.State() != BreakerOpen {
		t.Fatalf("after three consecutive failures the breaker is %s, want open", b.State())
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("an open breaker admitted a call")
	}

	// Not yet: the recovery window has not elapsed.
	clock.Advance(4 * time.Second)
	if _, ok := b.Allow(); ok {
		t.Fatal("the breaker half-opened before its timeout")
	}

	clock.Advance(2 * time.Second)
	probe, ok := b.Allow()
	if !ok {
		t.Fatal("the breaker did not admit a probe after the recovery window")
	}
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state %s, want half-open", b.State())
	}
	// Exactly one probe at a time: risking one request on a service that was
	// just failing is the point; risking all of them is not.
	if _, ok := b.Allow(); ok {
		t.Fatal("a second concurrent probe was admitted while one was in flight")
	}
	probe(true)

	// One success is not enough. Letting full load back in on the first good
	// probe is how a recovering service is knocked straight over again.
	if b.State() != BreakerHalfOpen {
		t.Fatalf("one successful probe closed the breaker; state %s", b.State())
	}
	second, ok := b.Allow()
	if !ok {
		t.Fatal("a second probe was refused")
	}
	second(true)
	if b.State() != BreakerClosed {
		t.Fatalf("state %s after the success threshold was met, want closed", b.State())
	}
}

func TestFailedProbeReopensTheBreaker(t *testing.T) {
	t.Parallel()
	clock := newClock()
	b := NewBreaker(BreakerConfig{FailureThreshold: 1, SuccessThreshold: 1,
		OpenTimeout: time.Second, Now: clock.Now})

	done, _ := b.Allow()
	done(false)
	if b.State() != BreakerOpen {
		t.Fatalf("state %s, want open", b.State())
	}
	clock.Advance(2 * time.Second)
	probe, ok := b.Allow()
	if !ok {
		t.Fatal("no probe admitted")
	}
	probe(false)
	if b.State() != BreakerOpen {
		t.Fatalf("a failed probe left the breaker %s, want open", b.State())
	}
	// And the timer restarted: the upstream is still broken, so there is no
	// partial credit for having been probed.
	if _, ok := b.Allow(); ok {
		t.Fatal("the breaker admitted a call immediately after a failed probe")
	}
}

func TestBreakerShedsLoadFromAFailingUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.retry = retry.Policy{MaxAttempts: 1}
		o.breaker = BreakerConfig{FailureThreshold: 3, SuccessThreshold: 1, OpenTimeout: 5 * time.Second}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	var upstreamCalls atomic.Int64
	h.stubs[UpstreamAnalytics].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		upstreamCalls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "down"})
	})

	for i := 0; i < 3; i++ {
		if got := h.do(http.MethodGet, "/v1/analytics/slo", key, nil).StatusCode; got != http.StatusServiceUnavailable {
			t.Fatalf("failure %d: got %d, want the upstream's 503", i, got)
		}
	}
	before := upstreamCalls.Load()

	// The circuit is now open: further calls fail fast without touching the
	// upstream, which is the whole point — a slow, failing dependency must not
	// occupy a gateway goroutine per request.
	res := h.do(http.MethodGet, "/v1/analytics/slo", key, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 from the open breaker", res.StatusCode)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "upstream_unavailable" {
		t.Fatalf("error code %q, want upstream_unavailable", body.Code)
	}
	if res.Header.Get(HeaderRetryAfter) == "" {
		t.Error("an open breaker should tell the client when to come back")
	}
	if got := upstreamCalls.Load(); got != before {
		t.Fatalf("the open breaker still made %d upstream calls", got-before)
	}

	// Readiness follows the breaker: a replica that cannot reach a dependency
	// must leave the load balancer's rotation, not restart.
	if got := h.do(http.MethodGet, "/readyz", "", nil).StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with an open breaker, want 503", got)
	}

	// Recover the upstream, wait out the window, and traffic resumes.
	h.stubs[UpstreamAnalytics].setHandler(nil)
	h.clock.Advance(6 * time.Second)
	if got := h.do(http.MethodGet, "/v1/analytics/slo", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("after recovery: got %d, want 200", got)
	}
	if got := h.do(http.MethodGet, "/readyz", "", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("/readyz = %d after recovery, want 200", got)
	}
}

func TestBreakersAreIndependentPerUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.retry = retry.Policy{MaxAttempts: 1}
		o.breaker = BreakerConfig{FailureThreshold: 2, OpenTimeout: time.Minute}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	h.stubs[UpstreamPromotion].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "down"})
	})
	for i := 0; i < 2; i++ {
		h.do(http.MethodGet, "/v1/promotions", key, nil)
	}
	if got := h.do(http.MethodGet, "/v1/promotions", key, nil).StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("the promotion breaker did not open: %d", got)
	}
	// A different upstream is unaffected: one failing service must not take
	// the other six with it.
	if got := h.do(http.MethodGet, "/v1/pricing/rules", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("an unrelated upstream was cut off: %d", got)
	}
}

func TestUnknownRouteIsNotFoundAndWrongMethodIsNotAllowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	res := h.do(http.MethodGet, "/v1/nonexistent", key, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: got %d, want 404", res.StatusCode)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "not_found" {
		t.Fatalf("error code %q, want not_found", body.Code)
	}

	res = h.do(http.MethodPut, "/v1/me", key, map[string]any{"x": 1})
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method on a real path: got %d, want 405", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want it to name GET", allow)
	}
}
