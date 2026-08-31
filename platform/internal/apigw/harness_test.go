package apigw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// ---------------------------------------------------------------------------
// Test harness
//
// Every test gets its own gateway, its own metrics registry (obs.Registry
// panics on duplicate registration, which is the correct constraint and means
// one gateway per registry) and its own set of stub upstreams. Nothing is
// shared, so the suite is safe under -race and under t.Parallel.
// ---------------------------------------------------------------------------

// fakeClock is a manually advanced clock. Rate-limit refill, key expiry and
// the circuit breaker's half-open transition are all time-dependent, and a
// test that proves them by sleeping proves only that the machine was slow.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// recorded is one request a stub upstream received.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// stub is a fake internal service.
type stub struct {
	name string
	srv  *httptest.Server

	mu       sync.Mutex
	requests []recorded
	handler  func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newStub(t *testing.T, name string) *stub {
	t.Helper()
	s := &stub{name: name}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: body,
		})
		h := s.handler
		s.mu.Unlock()
		if h == nil {
			writeJSON(w, http.StatusOK, map[string]string{"upstream": name, "path": r.URL.Path})
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) setHandler(h func(w http.ResponseWriter, r *http.Request, body []byte)) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

func (s *stub) calls() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recorded, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *stub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *stub) reset() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

// harnessOptions tweak the gateway under test.
type harnessOptions struct {
	rateLimit  RateLimitConfig
	stream     StreamConfig
	auth       AuthConfig
	breaker    BreakerConfig
	retry      retry.Policy
	maxRequest int64
	maxResp    int64
	timeout    time.Duration
	source     EventSource
	logs       *bytes.Buffer
}

// harness is a running gateway with stub upstreams in front of it.
type harness struct {
	t        *testing.T
	gw       *Gateway
	server   *httptest.Server
	clock    *fakeClock
	keys     *KeyIssuer
	store    *MemoryKeyStore
	registry *obs.Registry
	stubs    map[string]*stub
}

func newHarness(t *testing.T, opts ...func(*harnessOptions)) *harness {
	t.Helper()

	o := &harnessOptions{
		// Generous defaults so a test that is not about rate limiting never
		// trips one by accident.
		rateLimit: RateLimitConfig{
			Tenant:     Limit{Rate: 10000, Burst: 10000},
			Credential: Limit{Rate: 10000, Burst: 10000},
			Expensive:  Limit{Rate: 10000, Burst: 10000},
		},
	}
	for _, fn := range opts {
		fn(o)
	}

	clock := newClock()
	store := NewMemoryKeyStore()
	// A low iteration count everywhere except the one test that exercises the
	// production default: 4096 rounds of PBKDF2 multiplied by a few hundred
	// authenticated requests would dominate the suite's runtime.
	issuer, err := NewKeyIssuer(KeyIssuerConfig{Store: store, Iterations: 1, Now: clock.Now})
	if err != nil {
		t.Fatalf("key issuer: %v", err)
	}

	names := []string{
		UpstreamLabel, UpstreamRegistry, UpstreamOTA,
		UpstreamPricing, UpstreamPromotion, UpstreamAnalytics, UpstreamUIG,
	}
	stubs := make(map[string]*stub, len(names))
	ups := make([]UpstreamConfig, 0, len(names))
	for _, name := range names {
		s := newStub(t, name)
		stubs[name] = s
		ups = append(ups, UpstreamConfig{
			Name: name, Address: s.srv.URL,
			Timeout:         o.timeout,
			MaxRequestBytes: o.maxRequest, MaxResponseBytes: o.maxResp,
			Retry: o.retry, Breaker: breakerWithClock(o.breaker, clock),
		})
	}

	logOut := io.Discard
	if o.logs != nil {
		logOut = o.logs
	}
	log := obs.NewLogger(obs.LogConfig{Service: "api-gateway-test", Level: "debug", Format: "json", Output: logOut})
	registry := obs.NewRegistry("service", "api-gateway-test")

	auth := o.auth
	if auth.Keys == nil {
		auth.Keys = issuer
	}
	auth.Now = clock.Now

	rl := o.rateLimit
	rl.Now = clock.Now

	// Start-up complete, as obs.Runtime.Ready() would declare in production.
	// Without it every /readyz in the suite would answer 503 for a reason that
	// has nothing to do with what is being tested.
	health := obs.NewHealth()
	health.SetReady(true)

	gw, err := New(Config{
		Service: "api-gateway", Version: "test",
		Log: log, Tracer: obs.NewTracer("api-gateway", 1), Registry: registry,
		Health:    health,
		Auth:      auth,
		Keys:      issuer,
		RateLimit: rl,
		Upstreams: ups,
		Stream:    o.stream,
		Source:    o.source,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("building the gateway: %v", err)
	}

	server := httptest.NewServer(gw.Handler())
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	gw.Start(ctx)
	t.Cleanup(func() {
		shutdownCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = gw.Shutdown(shutdownCtx)
		cancel()
	})

	return &harness{
		t: t, gw: gw, server: server, clock: clock,
		keys: issuer, store: store, registry: registry, stubs: stubs,
	}
}

func breakerWithClock(cfg BreakerConfig, clock *fakeClock) BreakerConfig {
	cfg.Now = clock.Now
	return cfg
}

// issueKey mints a credential directly, bypassing the HTTP endpoint, so that a
// test of some other behaviour does not depend on /v1/keys working.
func (h *harness) issueKey(tenant canon.TenantID, roles []Role, stores ...canon.StoreID) string {
	h.t.Helper()
	_, plaintext, err := h.keys.Issue(context.Background(), IssueRequest{
		TenantID: tenant, Name: "test", Roles: roles, Stores: stores,
	})
	if err != nil {
		h.t.Fatalf("issuing a key: %v", err)
	}
	return plaintext
}

// do performs a request against the gateway.
func (h *harness) do(method, path, credential string, body any) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encoding the request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// decode reads a JSON response body.
func decodeBody(t *testing.T, res *http.Response, dst any) {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decoding %q: %v", string(raw), err)
	}
}

// bodyString reads a response body as text.
func bodyString(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return string(raw)
}

// tenantLabelStore is a stub label-service that partitions by the tenant
// header the gateway stamps, so a cross-tenant request behaves exactly as the
// real service does: the label is simply not there.
type tenantLabelStore struct {
	mu     sync.Mutex
	labels map[canon.TenantID]map[string]map[string]any
}

func newTenantLabelStore() *tenantLabelStore {
	return &tenantLabelStore{labels: map[canon.TenantID]map[string]map[string]any{}}
}

func (s *tenantLabelStore) put(tenant canon.TenantID, id string, doc map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels[tenant] == nil {
		s.labels[tenant] = map[string]map[string]any{}
	}
	s.labels[tenant][id] = doc
}

// install wires the store into a stub as a GET /v1/labels/{id} handler.
func (s *tenantLabelStore) install(st *stub) {
	st.setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		const prefix = "/v1/labels/"
		if r.Method != http.MethodGet || len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
			writeJSON(w, http.StatusNotFound, ErrorBody{Code: "not_found", Message: "no such route"})
			return
		}
		id := r.URL.Path[len(prefix):]
		tenant := canon.TenantID(r.Header.Get(HeaderTenant))
		s.mu.Lock()
		doc, ok := s.labels[tenant][id]
		s.mu.Unlock()
		if !ok {
			// Exactly what label-service does: a label belonging to someone
			// else is reported as absent, never as forbidden.
			writeJSON(w, http.StatusNotFound, ErrorBody{
				Code: "not_found", Message: fmt.Sprintf("label %s has no state", id)})
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

// channelSource is an EventSource driven by a test.
type channelSource struct {
	ch chan canon.Envelope
}

func newChannelSource() *channelSource {
	return &channelSource{ch: make(chan canon.Envelope, 64)}
}

// Run implements EventSource.
func (s *channelSource) Run(ctx context.Context, deliver func(canon.Envelope)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case env := <-s.ch:
			deliver(env)
		}
	}
}

func (s *channelSource) emit(env canon.Envelope) { s.ch <- env }

// substitute fills named wildcards in a route pattern, leaving the rest.
func substitute(pattern string, values map[string]string) string {
	for name, value := range values {
		pattern = strings.ReplaceAll(pattern, "{"+name+"}", value)
	}
	return pattern
}

// concretePath fills any remaining wildcard with a placeholder that satisfies
// canon.ValidID, so a route table walk can actually issue requests.
func concretePath(pattern string) string {
	var b strings.Builder
	rest := pattern
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		shut := strings.IndexByte(rest[open:], '}')
		if shut < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		b.WriteString("probe-" + strings.ToLower(rest[open+1:open+shut]))
		rest = rest[open+shut+1:]
	}
}

// money builds a canon.Money for a test body.
func money(amount int64, currency string) canon.Money {
	return canon.NewMoney(amount, currency)
}

// retryOnce disables retrying, for tests that count upstream attempts.
func retryOnce() retry.Policy { return retry.Policy{MaxAttempts: 1} }

// jsonDecode decodes a JSON payload, used by the WebSocket client.
func jsonDecode(payload []byte, dst any) error { return json.Unmarshal(payload, dst) }

// containsSecret reports whether a response body echoed key material.
func containsSecret(body, secret string) bool {
	return secret != "" && strings.Contains(body, secret)
}

// envelopeFor builds a minimal valid envelope for fan-out tests.
func envelopeFor(tenant canon.TenantID, store canon.StoreID, eventType string, payload any) canon.Envelope {
	env, err := canon.NewEnvelope(eventType, "label", "lbl-1", tenant, payload)
	if err != nil {
		panic(err)
	}
	env.StoreID = store
	return env
}
