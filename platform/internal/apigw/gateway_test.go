package apigw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

func TestRouteTableIsInternallyConsistent(t *testing.T) {
	t.Parallel()
	// New() runs validateRoutes, so a harness that builds at all has already
	// proved most of this. What is left is the properties a misconfiguration
	// could satisfy while still being wrong.
	seenOps := map[string]bool{}
	for _, rt := range Routes() {
		if seenOps[rt.Operation] {
			t.Errorf("duplicate operation id %q", rt.Operation)
		}
		seenOps[rt.Operation] = true

		if rt.Expensive && rt.Public {
			t.Errorf("%s is public and expensive; an unauthenticated caller cannot be metered", rt.Key())
		}
		if rt.Upstream == "" && rt.Native == "" {
			t.Errorf("%s has no handler", rt.Key())
		}
		if strings.HasPrefix(rt.Pattern, "/v1/") && rt.Public {
			t.Errorf("%s is under /v1 and public; the versioned API is authenticated in its entirety", rt.Key())
		}
		if rt.Method == http.MethodGet && !rt.NoBody {
			t.Errorf("%s is a GET that claims a request body", rt.Key())
		}
	}
}

func TestGatewayRefusesAnInconsistentConfiguration(t *testing.T) {
	t.Parallel()
	// A route names an upstream that is not configured: this must fail at
	// start-up, so the failure is a pod that will not start rather than a
	// route that 500s the first time a customer calls it.
	_, err := New(Config{
		Registry: obs.NewRegistry(),
		Log:      obs.NopLogger(),
		// no upstreams at all
	})
	if err == nil {
		t.Fatal("a gateway with no upstreams was built even though routes name them")
	}
	if !strings.Contains(err.Error(), "unknown upstream") {
		t.Fatalf("error %v, want it to name the missing upstream", err)
	}

	// A metrics registry is mandatory: without one there is no way to see the
	// gateway at all.
	if _, err := New(Config{}); err == nil {
		t.Fatal("a gateway was built with no metrics registry")
	}

	// An upstream address that is not absolute.
	_, err = New(Config{
		Registry:  obs.NewRegistry(),
		Log:       obs.NopLogger(),
		Upstreams: []UpstreamConfig{{Name: UpstreamLabel, Address: "label-service:8082"}},
	})
	if err == nil || !strings.Contains(err.Error(), "absolute URL") {
		t.Fatalf("error %v, want a complaint about a relative upstream address", err)
	}
}

func TestMeDescribesTheCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RolePricingAnalyst}, "store-1", "store-2")

	res := h.do(http.MethodGet, "/v1/me", key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var me MeResponse
	decodeBody(t, res, &me)

	if me.TenantID != "acme" || me.AuthMethod != CredAPIKey {
		t.Fatalf("identity %+v", me)
	}
	if len(me.Stores) != 2 {
		t.Fatalf("stores %v, want both", me.Stores)
	}
	if me.ExpiresAt == nil {
		t.Fatal("no expiry reported; a client cannot know when its credential dies")
	}
	// Permissions are expanded so a console does not have to carry its own
	// copy of the matrix.
	if !contains(me.Permissions, "prices:write") || contains(me.Permissions, "devices:write") {
		t.Fatalf("permissions %v do not match the pricing-analyst role", me.Permissions)
	}
	// And nothing about the credential itself leaks back.
	if strings.Contains(bodyString(t, res), KeyPrefixLive) {
		t.Fatal("/v1/me echoed key material")
	}
}

func TestComposedSinglePriceBecomesAOneItemBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RolePricingAnalyst})

	h.stubs[UpstreamLabel].setHandler(func(w http.ResponseWriter, _ *http.Request, body []byte) {
		var env batchEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorBody{Code: "bad", Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"requested": len(env.Items), "applied": len(env.Items)})
	})

	res := h.do(http.MethodPost, "/v1/prices", key, PriceChangeRequest{
		StoreID: "store-1", SKU: "sku-42",
		Price:  money(299, "GBP"),
		Reason: "weekly promotion",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%s)", res.StatusCode, bodyString(t, res))
	}

	calls := h.stubs[UpstreamLabel].calls()
	if len(calls) != 1 {
		t.Fatalf("%d upstream calls, want 1", len(calls))
	}
	if calls[0].Path != "/v1/prices:batch" {
		t.Fatalf("upstream path %q, want the batch endpoint", calls[0].Path)
	}
	var sent batchEnvelope
	if err := json.Unmarshal(calls[0].Body, &sent); err != nil {
		t.Fatalf("the composed body is not the batch contract: %v", err)
	}
	if len(sent.Items) != 1 {
		t.Fatalf("%d items, want 1", len(sent.Items))
	}
	item := sent.Items[0]
	if item.StoreID != "store-1" || item.SKU != "sku-42" || item.Price.Amount != 299 {
		t.Fatalf("composed item %+v", item)
	}
	if item.EffectiveAt.IsZero() {
		t.Fatal("effective_at was not defaulted; the upstream would reject the item")
	}
	// The audit trail must name the credential, not the caller's claim.
	if !strings.HasPrefix(item.InitiatedBy, "key:") {
		t.Fatalf("initiated_by %q, want the authenticated subject", item.InitiatedBy)
	}
}

func TestComposedPriceValidatesBeforeCallingUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RolePricingAnalyst}, "store-1")

	cases := []struct {
		name string
		body PriceChangeRequest
		want int
	}{
		{"no store", PriceChangeRequest{SKU: "s", Price: money(1, "GBP")}, http.StatusBadRequest},
		{"no sku or label", PriceChangeRequest{StoreID: "store-1", Price: money(1, "GBP")}, http.StatusBadRequest},
		{"bad currency", PriceChangeRequest{StoreID: "store-1", SKU: "s", Price: money(1, "XX")}, http.StatusBadRequest},
		{"out of scope store", PriceChangeRequest{StoreID: "store-9", SKU: "s", Price: money(1, "GBP")}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.stubs[UpstreamLabel].callCount()
			res := h.do(http.MethodPost, "/v1/prices", key, tc.body)
			if res.StatusCode != tc.want {
				t.Fatalf("status %d, want %d (%s)", res.StatusCode, tc.want, bodyString(t, res))
			}
			if h.stubs[UpstreamLabel].callCount() != before {
				t.Fatal("an invalid request reached the upstream")
			}
		})
	}
}

func TestStoreOverviewFansOutAndDegradesGracefully(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleStoreManager}, "store-1")

	h.stubs[UpstreamRegistry].setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, http.StatusOK, map[string]any{"path": r.URL.Path, "labels_online": 812})
	})
	h.stubs[UpstreamLabel].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusOK, map[string]any{"p99_seconds": 1.8})
	})
	// One dependency is down. The console still needs the other three panels.
	h.stubs[UpstreamOTA].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "internal"})
	})

	res := h.do(http.MethodGet, "/v1/stores/store-1/overview", key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 even with a dependency down", res.StatusCode)
	}
	var overview StoreOverview
	decodeBody(t, res, &overview)

	if overview.StoreID != "store-1" {
		t.Fatalf("store %q", overview.StoreID)
	}
	for name, section := range map[string]json.RawMessage{
		"health": overview.Health, "mesh": overview.Mesh, "slo": overview.SLO,
	} {
		if len(section) == 0 {
			t.Errorf("section %q is empty but its upstream answered", name)
		}
	}
	if len(overview.OTA) != 0 {
		t.Error("a failed section was populated")
	}
	if !contains(overview.Degraded, "ota") {
		t.Fatalf("degraded = %v, want it to name ota so the console can say so", overview.Degraded)
	}
	if overview.FetchedAt.IsZero() {
		t.Error("no fetch timestamp; a console cannot tell a stale panel from a fresh one")
	}

	// The fan-out really is a fan-out: three upstreams, four calls.
	if got := h.stubs[UpstreamRegistry].callCount(); got != 2 {
		t.Errorf("registry called %d times, want 2 (health and mesh)", got)
	}
	if got := h.stubs[UpstreamLabel].callCount(); got != 1 {
		t.Errorf("label-service called %d times, want 1", got)
	}
}

func TestStoreOverviewRefusesAnOutOfScopeStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleStoreManager}, "store-1")
	if got := h.do(http.MethodGet, "/v1/stores/store-2/overview", key, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("status %d, want 404", got)
	}
	for _, name := range []string{UpstreamRegistry, UpstreamLabel, UpstreamOTA} {
		if h.stubs[name].callCount() != 0 {
			t.Fatalf("%s was called for an out-of-scope store", name)
		}
	}
}

func TestAccessLogRecordsTheOutcome(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHarness(t, func(o *harnessOptions) { o.logs = &logs })
	key := h.issueKey("acme", []Role{RoleReadOnly})

	h.do(http.MethodGet, "/v1/me", key, nil)

	var found bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" || !strings.Contains(line, "gateway request") {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["operation"] != "getMe" {
			continue
		}
		found = true
		for _, field := range []string{"tenant_id", "latency_ms", "status", "outcome", "request_id", "trace_id", "auth"} {
			if _, ok := entry[field]; !ok {
				t.Errorf("the access log line has no %q", field)
			}
		}
		if entry["tenant_id"] != "acme" {
			t.Errorf("tenant_id = %v, want acme", entry["tenant_id"])
		}
		if entry["outcome"] != "ok" {
			t.Errorf("outcome = %v", entry["outcome"])
		}
	}
	if !found {
		t.Fatalf("no access log line for the request:\n%s", logs.String())
	}
}

func TestMetricsAreRecordedPerRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	h.do(http.MethodGet, "/v1/labels/lbl-1", key, nil)
	h.do(http.MethodGet, "/v1/me", "", nil) // a 401

	m := h.gw.Metrics()
	if got := m.Requests.With("getLabel", http.MethodGet, "200", "ok").Value(); got != 1 {
		t.Errorf("getLabel 200 counter = %d, want 1", got)
	}
	if got := m.Requests.With("getMe", http.MethodGet, "401", "denied").Value(); got != 1 {
		t.Errorf("getMe 401 counter = %d, want 1", got)
	}
	if got := m.Duration.With("getLabel").Count(); got != 1 {
		t.Errorf("getLabel duration observations = %d, want 1", got)
	}
	if got := m.UpstreamDuration.With(UpstreamLabel, "200").Count(); got != 1 {
		t.Errorf("upstream duration observations = %d, want 1", got)
	}
	if got := m.Auth.With(string(CredAPIKey), "accepted").Value(); got == 0 {
		t.Error("no accepted authentications counted")
	}
	if got := m.Auth.With("", "rejected").Value(); got == 0 {
		t.Error("no rejected authentications counted")
	}

	// The exposition renders: a metric that cannot be scraped is not a metric.
	var buf bytes.Buffer
	h.registry.WriteText(&buf)
	for _, want := range []string{
		"usslp_gateway_requests_total", "usslp_gateway_request_duration_seconds",
		"usslp_gateway_upstream_duration_seconds", "usslp_gateway_breaker_state",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the exposition does not contain %s", want)
		}
	}
}

func TestPanicInAHandlerBecomesA500AndKeepsServing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	// A hijacked-looking upstream that closes without a response is the
	// closest a stub can get to a broken handler; the recovery path is
	// exercised directly instead.
	route := &Route{Method: "GET", Pattern: "/panic", Operation: "panicProbe", Public: true, Summary: "x"}
	handler := h.gw.observability(route, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("the shelf caught fire")
	}))
	rec := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/panic", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "shelf caught fire") {
		t.Fatal("the panic message was returned to the client")
	}
	if got := h.gw.Metrics().Panics.With("panicProbe").Value(); got != 1 {
		t.Errorf("panic counter = %d, want 1", got)
	}
	// The process is still serving.
	if got := h.do(http.MethodGet, "/v1/me", key, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the gateway stopped serving after a panic: %d", got)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.gw.Shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	// The harness cleanup calls it again; a second close of the same channel
	// would panic, so this is worth pinning.
	if err := h.gw.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestHealthAndReadinessAreDifferentQuestions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOptions) {
		o.retry = retryOnce()
		o.breaker = BreakerConfig{FailureThreshold: 1, OpenTimeout: time.Minute}
	})
	key := h.issueKey("acme", []Role{RoleOwner})

	h.stubs[UpstreamAnalytics].setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "down"})
	})
	h.do(http.MethodGet, "/v1/analytics/slo", key, nil)

	// Readiness fails: traffic should go elsewhere.
	if got := h.do(http.MethodGet, "/readyz", "", nil).StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with a failing dependency, want 503", got)
	}
	// Liveness does not: restarting a process because a dependency blipped is
	// how a five-second wobble becomes a cluster-wide restart storm.
	res := h.do(http.MethodGet, "/healthz", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d with a failing dependency, want 200", res.StatusCode)
	}
	var body healthResponse
	decodeBody(t, res, &body)
	if body.Status != "alive" {
		t.Fatalf("liveness status %q", body.Status)
	}
	if len(body.Checks) != 0 {
		t.Fatal("liveness runs dependency checks; §7 of the interface contracts says it must not")
	}
}
