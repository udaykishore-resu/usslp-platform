package label

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// do issues a request against the service's HTTP surface and decodes the body.
func do(t *testing.T, h *harness, method, path, tenant string, body any, out any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	rec := httptest.NewRecorder()
	h.svc.Handler().ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response %s: %v", rec.Body.String(), err)
		}
	}
	return rec
}

func TestHTTPUpdatePrice(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	var res app.PriceResult
	rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", string(testTenant),
		PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(279, "USD"), InitiatedBy: "colleague-14"}, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !res.Applied() || res.Sequence != 1 || !res.Attested {
		t.Fatalf("result = %+v", res)
	}
	if rec.Header().Get("traceparent") == "" {
		t.Fatalf("no trace context returned; the price path is always sampled")
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)
}

func TestHTTPUpdatePriceRejectionIsUnprocessable(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", string(testTenant),
		PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(249, "USD")}, nil)

	var res app.PriceResult
	rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", string(testTenant),
		PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(24900, "USD")}, &res)
	// 422, not 400: the request was well formed and the platform refused it on
	// a business rule, and a client's response to those two is different.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	if res.Reason != domain.ReasonGuardrail {
		t.Fatalf("reason = %q, want %q", res.Reason, domain.ReasonGuardrail)
	}
}

func TestHTTPScheduledChangeIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	at := time.Now().UTC().Add(4 * time.Hour)

	var res app.PriceResult
	rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", string(testTenant),
		PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(199, "USD"), EffectiveAt: &at}, &res)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if res.Outcome != app.OutcomeScheduled {
		t.Fatalf("outcome = %s, want scheduled", res.Outcome)
	}
	time.Sleep(150 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("a future-dated change reached the glass immediately")
	}
}

func TestHTTPTenancy(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	body := PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(279, "USD")}

	if rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", "", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant header: status = %d, want 401", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", "acme/../rival", body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant with reserved characters: status = %d, want 400", rec.Code)
	}
	// A different tenant must be told the label does not exist. Confirming that
	// a label id belongs to another retailer is itself a cross-tenant leak.
	if rec := do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", "rival", body, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant write: status = %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/labels/lbl-milk-a", "rival", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read: status = %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/labels/lbl-milk-a/history", "rival", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant history: status = %d, want 404", rec.Code)
	}
}

func TestHTTPGetLabelAndHistory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	for _, amount := range []int64{249, 259, 269} {
		do(t, h, http.MethodPost, "/v1/labels/lbl-milk-a/price", string(testTenant),
			PriceUpdateRequest{SKU: "sku-milk", Price: canon.NewMoney(amount, "USD")}, nil)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 3, 5*time.Second)
	_, last := h.decodeUpdate(msgs[len(msgs)-1])
	if err := h.svc.DeliveryHandler().HandleEnvelope(ctx,
		ackEnvelope(t, "lbl-milk-a", testSEC, last.Sequence, 1300*time.Millisecond)); err != nil {
		t.Fatalf("ack: %v", err)
	}

	var view LabelView
	rec := do(t, h, http.MethodGet, "/v1/labels/lbl-milk-a", string(testTenant), nil, &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if view.Price.Amount != 269 || view.Sequence != 3 {
		t.Fatalf("view = %+v", view)
	}
	if !view.Healthy {
		t.Fatalf("a confirmed, active label should be healthy: %+v", view)
	}
	if view.LastLatencyMS != 1300 {
		t.Fatalf("last latency = %d, want 1300", view.LastLatencyMS)
	}

	var history HistoryResponse
	rec = do(t, h, http.MethodGet, "/v1/labels/lbl-milk-a/history?limit=2", string(testTenant), nil, &history)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(history.Events) != 2 {
		t.Fatalf("history returned %d events, want 2", len(history.Events))
	}
	// Newest first, and the payload is passed through verbatim so an audit is
	// reading the record rather than a re-encoding of it.
	if history.Events[0].Version <= history.Events[1].Version {
		t.Fatalf("history is not newest-first: %d then %d",
			history.Events[0].Version, history.Events[1].Version)
	}
	if len(history.Events[0].Payload) == 0 || history.Events[0].EventID == "" {
		t.Fatalf("history entry is missing its stored payload or identity: %+v", history.Events[0])
	}

	if rec := do(t, h, http.MethodGet, "/v1/labels/lbl-milk-a/history?limit=nonsense", string(testTenant), nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: status = %d, want 400", rec.Code)
	}
}

func TestHTTPStoreRosterAndSLO(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-a", testSEC, "sku-a")
	h.provisionLabel("lbl-b", testSEC, "sku-b")
	h.provisionLabel("lbl-c", testSEC, "sku-c")

	for _, sku := range []canon.SKU{"sku-a", "sku-b", "sku-c"} {
		if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope(sku, 399, "seed-"+string(sku))); err != nil {
			t.Fatalf("price %s: %v", sku, err)
		}
	}
	h.waitForMessages(canon.LeafPrice, 3, 5*time.Second)

	// Confirm two of the three, with different latencies so the percentiles
	// have something to separate.
	for i, id := range []canon.LabelID{"lbl-a", "lbl-b"} {
		if err := h.svc.DeliveryHandler().HandleEnvelope(ctx,
			ackEnvelope(t, id, testSEC, 1, time.Duration(800+i*600)*time.Millisecond)); err != nil {
			t.Fatalf("ack %s: %v", id, err)
		}
	}
	if _, err := h.svc.StateProjection().CatchUp(ctx); err != nil {
		t.Fatalf("catch up: %v", err)
	}

	var roster StoreRosterResponse
	rec := do(t, h, http.MethodGet, "/v1/stores/store-01/labels", string(testTenant), nil, &roster)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if roster.Total != 3 {
		t.Fatalf("roster total = %d, want 3", roster.Total)
	}
	if roster.Pending != 1 {
		t.Fatalf("roster pending = %d, want the one unconfirmed label", roster.Pending)
	}
	if roster.Healthy != 2 {
		t.Fatalf("roster healthy = %d, want 2", roster.Healthy)
	}
	for i := 1; i < len(roster.Labels); i++ {
		if roster.Labels[i-1].LabelID > roster.Labels[i].LabelID {
			t.Fatalf("roster is not ordered by label id")
		}
	}

	var slo SLOResponse
	rec = do(t, h, http.MethodGet, "/v1/stores/store-01/slo", string(testTenant), nil, &slo)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if slo.TargetSeconds != SLOTargetSeconds {
		t.Fatalf("target = %v, want %v", slo.TargetSeconds, SLOTargetSeconds)
	}
	if slo.Observations != 2 {
		t.Fatalf("observations = %d, want 2", slo.Observations)
	}
	if slo.P50 <= 0 || slo.P99 <= 0 {
		t.Fatalf("percentiles not reported: %+v", slo)
	}
	if slo.P99 > SLOTargetSeconds || !slo.WithinTarget {
		t.Fatalf("p99 = %.3fs against a %.1fs target, within=%v", slo.P99, slo.TargetSeconds, slo.WithinTarget)
	}
	if slo.SuccessRate != 1 {
		t.Fatalf("success rate = %v with no failures, want 1", slo.SuccessRate)
	}
	if slo.LabelsPending != 1 || slo.LabelsTotal != 3 {
		t.Fatalf("slo label counts = %d pending of %d", slo.LabelsPending, slo.LabelsTotal)
	}
	if got := h.svc.metrics.PendingDelivery.With(string(testStore)).Value(); got != 1 {
		t.Fatalf("pending gauge = %v after the query re-derived it, want 1", got)
	}
}

func TestHTTPBatch(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-a", testSEC, "sku-a")
	h.provisionLabel("lbl-b", testSEC, "sku-a")

	var report app.BatchReport
	rec := do(t, h, http.MethodPost, "/v1/prices:batch", string(testTenant), BatchPriceRequest{
		Items: []app.BatchItem{{
			StoreID: testStore, SKU: "sku-a", Price: canon.NewMoney(499, "USD"),
			EffectiveAt: time.Now().UTC(),
		}},
		InitiatedBy: "merchandising",
	}, &report)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if report.Resolved != 2 || report.Applied != 2 {
		t.Fatalf("report = %+v", report)
	}

	// Validation is at the edge, before any work is started.
	if rec := do(t, h, http.MethodPost, "/v1/prices:batch", string(testTenant),
		BatchPriceRequest{Items: nil}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty batch: status = %d, want 400", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/prices:batch", string(testTenant), BatchPriceRequest{
		Items: []app.BatchItem{{SKU: "sku-a", Price: canon.NewMoney(499, "USD")}},
	}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("item without a store: status = %d, want 400", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/prices:batch", string(testTenant), BatchPriceRequest{
		Items: []app.BatchItem{{StoreID: testStore, SKU: "sku-a", Price: canon.NewMoney(499, "DOLLAR")}},
	}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("item with a bad currency: status = %d, want 400", rec.Code)
	}
}

func TestHTTPUnknownLabel(t *testing.T) {
	h := newHarness(t)
	if rec := do(t, h, http.MethodGet, "/v1/labels/lbl-nope", string(testTenant), nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/labels/lbl-nope/price", string(testTenant),
		PriceUpdateRequest{SKU: "sku-x", Price: canon.NewMoney(100, "USD")}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/nonsense", string(testTenant), nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unrouted path: status = %d, want 404", rec.Code)
	}
}
