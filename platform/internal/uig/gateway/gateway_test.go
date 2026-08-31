package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/adapters/oracle"
	"github.com/usslp/usslp/platform/internal/uig/adapters/shopify"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

const (
	shopifyKey    = "shopify-signing-key"
	operatorToken = "operator-bearer-token"
	oracleUser    = "USSLP_RIB"
	oraclePass    = "rib-password"
)

type capturePublisher struct {
	mu   sync.Mutex
	msgs []eventbus.Message
}

func (p *capturePublisher) Publish(_ context.Context, msgs ...eventbus.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, msgs...)
	return nil
}

func (p *capturePublisher) Close() error { return nil }

func (p *capturePublisher) count(topic string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.msgs {
		if m.Topic == topic {
			n++
		}
	}
	return n
}

type harness struct {
	srv  *httptest.Server
	pipe *pipeline.Pipeline
	pub  *capturePublisher
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kv.Close() })
	backend, _ := idem.NewKVBackend(kv, "idem/")
	guard, _ := idem.New(backend)
	store, _ := deliveries.New(kv, deliveries.Options{})

	reg := adapter.NewRegistry()
	reg.MustRegister(shopify.New())
	reg.MustRegister(oracle.New())
	bindings := adapter.NewBindingStore(reg)
	if err := bindings.Put(&adapter.Binding{
		ID: "shop", TenantID: "acme", Adapter: shopify.Name,
		POSInstance: "shopify-uk", DefaultCurrency: "GBP", DefaultStore: "GB-0001",
		Secrets: adapter.Secrets{HMACKey: shopifyKey},
	}); err != nil {
		t.Fatal(err)
	}
	if err := bindings.Put(&adapter.Binding{
		ID: "rib", TenantID: "acme", Adapter: oracle.Name,
		DefaultCurrency: "USD", DefaultStore: "US-0001",
		Secrets: adapter.Secrets{Username: oracleUser, SharedToken: oraclePass},
	}); err != nil {
		t.Fatal(err)
	}

	pub := &capturePublisher{}
	pipe, err := pipeline.New(pipeline.Config{
		Registry: reg, Bindings: bindings, Guard: guard, Bus: pub,
		Deliveries: store, Metrics: pipeline.NewMetrics(obs.NewRegistry()), Log: obs.NopLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close() })

	gw, err := New(Config{Pipeline: pipe, OperatorToken: operatorToken, Log: obs.NopLogger()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &harness{srv: srv, pipe: pipe, pub: pub}
}

const productBody = `{"id":1,"title":"Espresso","updated_at":"2026-08-30T10:00:00Z","variants":[
  {"id":11,"sku":"ESP-1KG","price":"12.99","compare_at_price":"15.50","updated_at":"2026-08-30T10:00:00Z"}
]}`

func (h *harness) postShopify(t *testing.T, body string, mutate ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/ingest/acme/shop", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(shopify.HeaderTopic, "products/update")
	req.Header.Set(shopify.HeaderShopDomain, "acme-uk.myshopify.com")
	req.Header.Set(shopify.HeaderWebhookID, "wh-1")
	req.Header.Set(shopify.HeaderHMAC,
		adapter.EncodeSignature(adapter.SignHMACSHA256(shopifyKey, []byte(body)), adapter.EncodingBase64))
	for _, m := range mutate {
		m(req)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func TestIngestEndpointAcceptsAValidWebhook(t *testing.T) {
	h := newHarness(t)
	resp := h.postShopify(t, productBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		DeliveryID    string `json:"delivery_id"`
		Status        string `json:"status"`
		Accepted      int    `json:"changes_accepted"`
		CorrelationID string `json:"correlation_id"`
		DurationMS    int64  `json:"duration_ms"`
	}
	decodeBody(t, resp, &body)
	if body.Status != "accepted" || body.Accepted != 1 {
		t.Fatalf("body = %+v", body)
	}
	if body.DeliveryID == "" || body.CorrelationID == "" {
		t.Errorf("the response must carry identifiers support can quote: %+v", body)
	}
	if resp.Header.Get("X-Usslp-Delivery-Id") != body.DeliveryID {
		t.Errorf("delivery id header = %q", resp.Header.Get("X-Usslp-Delivery-Id"))
	}
	// A cached 202 would make a retailer believe a price landed that never did.
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if h.pub.count(canon.StreamPriceUpdates.Name) != 1 {
		t.Errorf("published %d price events", h.pub.count(canon.StreamPriceUpdates.Name))
	}
}

func TestIngestEndpointStatusCodes(t *testing.T) {
	h := newHarness(t)

	// Tampered signature: 401, and nothing published.
	bad := h.postShopify(t, productBody, func(r *http.Request) {
		r.Header.Set(shopify.HeaderHMAC, "AAAA")
	})
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered signature status = %d, want 401", bad.StatusCode)
	}

	// Unparseable body: 422, never 5xx.
	malformed := h.postShopify(t, `not json`)
	defer malformed.Body.Close()
	if malformed.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("malformed status = %d, want 422", malformed.StatusCode)
	}

	// Unknown binding: 404, indistinguishable from a wrong tenant.
	resp, err := h.srv.Client().Post(h.srv.URL+"/v1/ingest/acme/nope", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown binding status = %d", resp.StatusCode)
	}

	// A GET on the ingest path is not routed at all.
	getResp, err := h.srv.Client().Get(h.srv.URL + "/v1/ingest/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode == http.StatusAccepted {
		t.Error("a GET was accepted as an ingest")
	}

	if h.pub.count(canon.StreamPriceUpdates.Name) != 0 {
		t.Error("a refused delivery published events")
	}
}

func TestRedeliveryIsDedupedOverHTTP(t *testing.T) {
	h := newHarness(t)
	first := h.postShopify(t, productBody)
	defer first.Body.Close()
	second := h.postShopify(t, productBody)
	var body struct {
		Duplicate bool `json:"duplicate"`
		Accepted  int  `json:"changes_accepted"`
	}
	decodeBody(t, second, &body)
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate status = %d; a producer must be told to stop", second.StatusCode)
	}
	if !body.Duplicate || body.Accepted != 0 {
		t.Fatalf("duplicate response = %+v", body)
	}
	if h.pub.count(canon.StreamPriceUpdates.Name) != 1 {
		t.Fatalf("published %d events for two deliveries of one message",
			h.pub.count(canon.StreamPriceUpdates.Name))
	}
}

func TestBodyLimitIsEnforcedBeforeParsing(t *testing.T) {
	h := newHarness(t)
	gw, err := New(Config{Pipeline: h.pipe, OperatorToken: operatorToken, MaxBodyBytes: 32, Log: obs.NopLogger()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/v1/ingest/acme/shop", "application/json",
		bytes.NewReader([]byte(strings.Repeat("A", 4096))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

const soapEnvelope = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">
  <soapenv:Header>
    <wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
      <wsse:UsernameToken>
        <wsse:Username>USSLP_RIB</wsse:Username>
        <wsse:Password>rib-password</wsse:Password>
      </wsse:UsernameToken>
    </wsse:Security>
    <rib:RibMessageHeader xmlns:rib="http://www.oracle.com/retail/integration/rib/v1">
      <rib:messageId>RIB-1</rib:messageId>
      <rib:family>Price</rib:family>
    </rib:RibMessageHeader>
  </soapenv:Header>
  <soapenv:Body>
    <PublishItemPriceDescCreate>
      <ItemPriceDesc>
        <store>US-0001</store><currency_code>USD</currency_code>
        <ItemPrice><item>SKU-A</item><selling_unit_retail>4.99</selling_unit_retail></ItemPrice>
      </ItemPriceDesc>
    </PublishItemPriceDescCreate>
  </soapenv:Body>
</soapenv:Envelope>`

func TestSOAPEndpointReturnsASOAPResponse(t *testing.T) {
	h := newHarness(t)
	resp, err := h.srv.Client().Post(h.srv.URL+"/v1/ingest/acme/rib/soap",
		"text/xml; charset=utf-8", strings.NewReader(soapEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// text/xml rather than application/soap+xml: RIB is a SOAP 1.1 client and
	// several versions reject the 1.2 media type outright.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Errorf("content type = %q", ct)
	}
	var probe struct {
		XMLName  xml.Name
		Status   string `xml:"Body>PublishItemPriceDescResponse>status"`
		Accepted int    `xml:"Body>PublishItemPriceDescResponse>changesAccepted"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(body, &probe); err != nil {
		t.Fatalf("response is not parseable SOAP: %v\n%s", err, body)
	}
	if probe.Status != "ACCEPTED" || probe.Accepted != 1 {
		t.Fatalf("response = %+v", probe)
	}
}

func TestSOAPFaultOnA4xxRatherThanA500(t *testing.T) {
	h := newHarness(t)
	// A body that will never parse. Answering 500 would put it at the head of
	// RIB's retry queue and block every price behind it.
	resp, err := h.srv.Client().Post(h.srv.URL+"/v1/ingest/acme/rib/soap",
		"text/xml", strings.NewReader(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">
		  <soapenv:Header><wsse:Security xmlns:wsse="x"><wsse:UsernameToken>
		    <wsse:Username>USSLP_RIB</wsse:Username><wsse:Password>rib-password</wsse:Password>
		  </wsse:UsernameToken></wsse:Security></soapenv:Header>
		  <soapenv:Body><PublishItemPriceDesc/></soapenv:Body></soapenv:Envelope>`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("status = %d; an unparseable SOAP message must not be retryable", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), string(oracle.FaultClient)) {
		t.Errorf("fault is not a client fault:\n%s", body)
	}
	if !strings.Contains(string(body), "<usslp:retryable>false</usslp:retryable>") {
		t.Errorf("fault does not say it is terminal:\n%s", body)
	}

	// A bad password is also a client fault, on a 401.
	unauth, err := h.srv.Client().Post(h.srv.URL+"/v1/ingest/acme/rib/soap", "text/xml",
		strings.NewReader(strings.Replace(soapEnvelope, oraclePass, "wrong", 1)))
	if err != nil {
		t.Fatal(err)
	}
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad password status = %d", unauth.StatusCode)
	}
}

func TestOperatorEndpointsRequireACredential(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/bindings/acme",
		"/v1/deliveries/acme",
	} {
		resp, err := h.srv.Client().Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, resp.StatusCode)
		}
	}
	resp, err := h.srv.Client().Post(h.srv.URL+"/v1/replay/acme/whatever", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay without a token = %d, want 401", resp.StatusCode)
	}

	// A gateway with no token configured refuses to serve the operator API at
	// all rather than serving it openly.
	gw, err := New(Config{Pipeline: h.pipe, Log: obs.NopLogger()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	open, err := srv.Client().Get(srv.URL + "/v1/bindings/acme")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Body.Close()
	if open.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unconfigured operator API = %d, want 503", open.StatusCode)
	}
}

func (h *harness) operatorGet(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestBindingsEndpointShowsConfigurationAndHealthWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	h.postShopify(t, productBody).Body.Close()

	resp := h.operatorGet(t, "/v1/bindings/acme")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The signing key must never appear in an operator response: these get
	// pasted into tickets.
	if strings.Contains(string(raw), shopifyKey) {
		t.Fatalf("the binding's signing key leaked:\n%s", raw)
	}
	if !strings.Contains(string(raw), "***redacted***") {
		t.Errorf("secrets are not marked as configured:\n%s", raw)
	}

	var body struct {
		TenantID string `json:"tenant_id"`
		Bindings []struct {
			ID          string                 `json:"id"`
			Adapter     string                 `json:"adapter"`
			POSInstance string                 `json:"pos_instance"`
			Health      pipeline.BindingHealth `json:"health"`
		} `json:"bindings"`
		Breakers map[string]string `json:"breakers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TenantID != "acme" || len(body.Bindings) != 2 {
		t.Fatalf("body = %+v", body)
	}
	var shop *struct {
		ID          string                 `json:"id"`
		Adapter     string                 `json:"adapter"`
		POSInstance string                 `json:"pos_instance"`
		Health      pipeline.BindingHealth `json:"health"`
	}
	for i := range body.Bindings {
		if body.Bindings[i].ID == "shop" {
			shop = &body.Bindings[i]
		}
	}
	if shop == nil {
		t.Fatal("the shopify binding is missing")
	}
	if shop.Adapter != shopify.Name || shop.POSInstance != "shopify-uk" {
		t.Errorf("binding = %+v", shop)
	}
	// "Configured" and "working" are different questions, and an operator
	// staring at a stale shelf needs the second.
	if shop.Health.Status != "ok" || shop.Health.Accepted != 1 || shop.Health.Emitted != 1 {
		t.Errorf("health = %+v", shop.Health)
	}
	// A binding that has never received anything reads as idle rather than
	// looking healthy.
	for i := range body.Bindings {
		if body.Bindings[i].ID == "rib" && body.Bindings[i].Health.Status != "idle" {
			t.Errorf("untouched binding health = %q", body.Bindings[i].Health.Status)
		}
	}
}

func TestDeliveriesEndpointForSupportTriage(t *testing.T) {
	h := newHarness(t)
	h.postShopify(t, productBody).Body.Close()
	for i := 0; i < 2; i++ {
		h.postShopify(t, `not json `+strconv.Itoa(i), func(r *http.Request) {
			r.Header.Set(shopify.HeaderWebhookID, "bad-"+strconv.Itoa(i))
		}).Body.Close()
	}
	if err := h.pipe.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	resp := h.operatorGet(t, "/v1/deliveries/acme?status=quarantined")
	var body struct {
		Count      int                 `json:"count"`
		Deliveries []deliveries.Record `json:"deliveries"`
	}
	decodeBody(t, resp, &body)
	if body.Count != 2 {
		t.Fatalf("quarantined = %d, want 2", body.Count)
	}
	for _, rec := range body.Deliveries {
		if rec.Status != deliveries.StatusQuarantined {
			t.Errorf("record status = %s", rec.Status)
		}
		if rec.Reason == "" || rec.Detail == "" {
			t.Errorf("a quarantined record must say why: %+v", rec)
		}
		// Bodies are withheld unless asked for, so the common triage call does
		// not ship retailer data into an access log.
		if len(rec.Body) != 0 {
			t.Error("a body was returned without include_bodies")
		}
	}

	withBodies := h.operatorGet(t, "/v1/deliveries/acme?status=quarantined&include_bodies=true&limit=1")
	var b2 struct {
		Count      int                 `json:"count"`
		Deliveries []deliveries.Record `json:"deliveries"`
	}
	decodeBody(t, withBodies, &b2)
	if b2.Count != 1 || len(b2.Deliveries[0].Body) == 0 {
		t.Fatalf("include_bodies returned %+v", b2)
	}

	// Bad query parameters are rejected rather than silently ignored.
	for _, q := range []string{"?status=nonsense", "?limit=0", "?limit=abc", "?since=yesterday"} {
		resp := h.operatorGet(t, "/v1/deliveries/acme"+q)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestReplayEndpoint(t *testing.T) {
	h := newHarness(t)
	// A delivery that fails to parse under today's configuration.
	bad := h.postShopify(t, `{"id":1,"variants":[]}`)
	var badBody struct {
		DeliveryID string `json:"delivery_id"`
		Status     string `json:"status"`
	}
	decodeBody(t, bad, &badBody)
	if badBody.Status != "quarantined" {
		t.Fatalf("expected a quarantine, got %+v", badBody)
	}

	replay := func(id string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/replay/acme/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+operatorToken)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Replaying it while the payload is still unusable reports the same
	// failure, under a new delivery id, without touching the guard.
	resp := replay(badBody.DeliveryID)
	var out struct {
		Replayed   string `json:"replayed"`
		DeliveryID string `json:"delivery_id"`
		Status     string `json:"status"`
		Upstream   int    `json:"upstream_response"`
	}
	decodeBody(t, resp, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d", resp.StatusCode)
	}
	if out.Replayed != badBody.DeliveryID || out.DeliveryID == badBody.DeliveryID {
		t.Fatalf("replay provenance = %+v", out)
	}
	if out.Upstream < 400 {
		t.Errorf("a still-broken payload reported upstream %d", out.Upstream)
	}

	// A missing delivery is a 404.
	missing := replay("01890000-0000-7000-8000-000000000000")
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing delivery = %d, want 404", missing.StatusCode)
	}

	// A successful delivery whose body was not retained is a 409 with an
	// explanation, not a 404: the delivery exists, it just cannot be replayed.
	ok := h.postShopify(t, productBody)
	var okBody struct {
		DeliveryID string `json:"delivery_id"`
	}
	decodeBody(t, ok, &okBody)
	if err := h.pipe.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	conflict := replay(okBody.DeliveryID)
	raw, err := io.ReadAll(conflict.Body)
	if err != nil {
		t.Fatal(err)
	}
	conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("not-replayable status = %d, want 409:\n%s", conflict.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "retain_raw") {
		t.Errorf("the error should tell an operator how to make it replayable:\n%s", raw)
	}
}

func TestRateLimitReturnsRetryAfter(t *testing.T) {
	h := newHarness(t)
	b, err := h.pipe.Bindings().Get("acme", "shop")
	if err != nil {
		t.Fatal(err)
	}
	tight := *b
	tight.RateLimit = adapter.RateLimitSpec{RatePerSecond: 1, Burst: 1}
	if err := h.pipe.Bindings().Put(&tight); err != nil {
		t.Fatal(err)
	}

	h.postShopify(t, productBody, func(r *http.Request) {
		r.Header.Set(shopify.HeaderWebhookID, "rl-1")
	}).Body.Close()
	resp := h.postShopify(t, productBody, func(r *http.Request) {
		r.Header.Set(shopify.HeaderWebhookID, "rl-2")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After makes a POS guess, and POS systems guess badly")
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
}

func TestCorrelationIDIsPropagatedFromTheCaller(t *testing.T) {
	h := newHarness(t)
	resp := h.postShopify(t, productBody, func(r *http.Request) {
		r.Header.Set("X-Correlation-Id", "retailer-123")
	})
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Correlation-Id"); got != "retailer-123" {
		t.Fatalf("correlation header = %q", got)
	}
}

func TestConcurrentIngestOverHTTPIsRaceFree(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				id := strconv.Itoa(i) + "-" + strconv.Itoa(j)
				resp := h.postShopify(t, productBody, func(r *http.Request) {
					r.Header.Set(shopify.HeaderWebhookID, id)
				})
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	if err := h.pipe.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.pub.count(canon.StreamPriceUpdates.Name); got != 80 {
		t.Fatalf("published %d price events, want 80", got)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a gateway with no pipeline was accepted")
	}
}
