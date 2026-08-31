package label

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxRequestBytes bounds a request body.
//
// Eight megabytes is one 40,000-item batch with room to spare. The limit exists
// because the batch endpoint is the one place a caller can hand this service an
// unbounded allocation, and an OOM on a price service is a store full of stale
// shelves.
const maxRequestBytes = 8 << 20

// defaultHistoryLimit is how many events a history query returns when the
// caller does not say. Fifty is about a year of price changes for a typical
// grocery line, which is the question that is actually being asked.
const defaultHistoryLimit = 50

// maxHistoryLimit bounds a history query.
const maxHistoryLimit = 1000

// Handler returns the service's JSON-over-HTTP surface.
//
// Routing uses the standard library's method-and-pattern matching, so the
// routes below are the whole specification: there is no router configuration
// elsewhere, and a path that is not listed here returns 404 rather than falling
// through to something unintended.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/labels/{id}/price", s.instrument("UpdatePrice", s.handleUpdatePrice))
	mux.HandleFunc("POST /v1/prices:batch", s.instrument("BatchUpdatePrices", s.handleBatch))
	mux.HandleFunc("GET /v1/labels/{id}", s.instrument("GetLabel", s.handleGetLabel))
	mux.HandleFunc("GET /v1/labels/{id}/history", s.instrument("GetLabelHistory", s.handleHistory))
	mux.HandleFunc("GET /v1/stores/{id}/labels", s.instrument("ListStoreLabels", s.handleStoreLabels))
	mux.HandleFunc("GET /v1/stores/{id}/slo", s.instrument("GetStoreSLO", s.handleStoreSLO))
	return mux
}

// instrument wraps a handler with the platform's standard tracing and metering.
//
// The price path is always sampled. At 52,000 updates per second head sampling
// at one percent is right for volume and wrong for the one update a regulator
// asks about, and the trace is the only artefact that can answer which of the
// nine hops ate the budget.
func (s *Service) instrument(operation string, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := obs.WithRemoteContext(r.Context(), obs.ParseTraceParent(r.Header.Get("traceparent")))
		ctx, span := s.tracer.StartAlwaysSampled(ctx, "http."+operation)
		defer span.End()
		span.SetAttr("http.method", r.Method).SetAttr("http.route", operation)
		if tenant := r.Header.Get(TenantHeader); tenant != "" {
			span.SetAttr("tenant", tenant)
		}
		w.Header().Set("traceparent", span.Ctx.TraceParent())

		start := time.Now()
		err := fn(w, r.WithContext(ctx))
		if err != nil {
			span.Fail(err)
			writeError(w, err)
		}
		if s.cfg.Standard != nil {
			s.cfg.Standard.ObserveRequest("http", operation, err, time.Since(start))
		}
	}
}

// httpError carries a status code alongside an error.
type httpError struct {
	status int
	code   string
	err    error
}

// Error implements error.
func (e *httpError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying cause so errors.Is still matches the sentinels
// the application layer returns.
func (e *httpError) Unwrap() error { return e.err }

func badRequest(format string, args ...any) error {
	return &httpError{status: http.StatusBadRequest, code: "invalid_argument", err: fmt.Errorf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &httpError{status: http.StatusNotFound, code: "not_found", err: fmt.Errorf(format, args...)}
}

func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, code = he.status, he.code
	case errors.Is(err, ports.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, ports.ErrRateLimited):
		status, code = http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, domain.ErrInvalidCommand), errors.Is(err, canon.ErrEnvelopeInvalid):
		status, code = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ports.ErrConcurrency):
		status, code = http.StatusConflict, "conflict"
	}
	writeJSON(w, status, map[string]string{"code": code, "message": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func tenantOf(r *http.Request) (canon.TenantID, error) {
	raw := strings.TrimSpace(r.Header.Get(TenantHeader))
	if raw == "" {
		return "", &httpError{
			status: http.StatusUnauthorized, code: "unauthenticated",
			err: fmt.Errorf("missing %s header", TenantHeader),
		}
	}
	if !canon.ValidID(raw) {
		return "", badRequest("%s %q contains reserved characters", TenantHeader, raw)
	}
	return canon.TenantID(raw), nil
}

func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("malformed request body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /v1/labels/{id}/price
// ---------------------------------------------------------------------------

// PriceUpdateRequest is the body of a single-label price update.
type PriceUpdateRequest struct {
	// SKU must match the label's current assignment; a mismatch is refused
	// rather than silently repricing the wrong product.
	SKU canon.SKU `json:"sku"`
	// Price is the new price in minor units.
	Price canon.Money `json:"price"`
	// WasPrice, UnitPrice and UnitMeasure are the optional comparison prices.
	WasPrice    *canon.Money `json:"was_price,omitempty"`
	UnitPrice   *canon.Money `json:"unit_price,omitempty"`
	UnitMeasure string       `json:"unit_measure,omitempty"`
	// EffectiveAt is when the price takes effect. Omitted means now; a future
	// value schedules rather than displaying.
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
	// ExpiresAt is when a promotional price lapses.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// PromotionID, Reason and Attributes drive template selection.
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	// InitiatedBy names the operator, for the audit trail.
	InitiatedBy string `json:"initiated_by,omitempty"`
	// Sequence, when positive, asserts the per-label sequence this update must
	// take. It must exceed the displayed one. A caller that supplies it is
	// asking for compare-and-set semantics against the shelf.
	Sequence int64 `json:"sequence,omitempty"`
	// IdempotencyKey makes a retried request a no-op.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func (s *Service) handleUpdatePrice(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id := canon.LabelID(r.PathValue("id"))
	if !canon.ValidID(string(id)) {
		return badRequest("label id %q contains reserved characters", id)
	}
	var req PriceUpdateRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.SKU == "" {
		return badRequest("sku is required")
	}
	if !req.Price.Valid() {
		return badRequest("price currency %q is not an ISO 4217 code", req.Price.Currency)
	}

	ctx := r.Context()
	placement, err := s.directory.Lookup(ctx, id)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return notFound("label %s is not in the directory", id)
		}
		return err
	}
	if placement.TenantID != "" && placement.TenantID != tenant {
		// Answer as though the label does not exist. Confirming that a label id
		// belongs to a different retailer is itself a cross-tenant leak.
		return notFound("label %s is not in the directory", id)
	}

	now := s.clock.Now()
	effective := now
	if req.EffectiveAt != nil {
		effective = *req.EffectiveAt
	}
	res, err := s.price.Apply(ctx, app.PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: req.SKU, Price: req.Price, WasPrice: req.WasPrice,
			UnitPrice: req.UnitPrice, UnitMeasure: req.UnitMeasure,
			EffectiveAt: effective, ExpiresAt: req.ExpiresAt,
			PromotionID: req.PromotionID, Reason: req.Reason,
			Attributes: req.Attributes, InitiatedBy: req.InitiatedBy,
			Sequence: req.Sequence, OccurredAt: now, Now: now,
		},
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	status := http.StatusOK
	switch res.Outcome {
	case app.OutcomeRejected:
		// The request was well formed and the platform refused it on a business
		// rule. 422 rather than 400 so a client can tell "I sent nonsense" from
		// "you would not display this".
		status = http.StatusUnprocessableEntity
	case app.OutcomeScheduled:
		status = http.StatusAccepted
	}
	writeJSON(w, status, res)
	return nil
}

// ---------------------------------------------------------------------------
// POST /v1/prices:batch
// ---------------------------------------------------------------------------

// BatchPriceRequest is the body of a bulk price change. The tenant comes from
// the header, never the body, so a caller cannot reprice another retailer's
// estate by naming it.
type BatchPriceRequest struct {
	// Region scopes MQTT topics for placements without one.
	Region canon.Region `json:"region,omitempty"`
	// Items are the changes.
	Items []app.BatchItem `json:"items"`
	// InitiatedBy names the operator, for the audit trail.
	InitiatedBy string `json:"initiated_by,omitempty"`
}

func (s *Service) handleBatch(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req BatchPriceRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.Items) == 0 {
		return badRequest("items must not be empty")
	}
	for i, item := range req.Items {
		if item.StoreID == "" {
			return badRequest("items[%d]: store_id is required", i)
		}
		if item.SKU == "" && item.LabelID == "" {
			return badRequest("items[%d]: one of sku or label_id is required", i)
		}
		if !item.Price.Valid() {
			return badRequest("items[%d]: currency %q is not an ISO 4217 code", i, item.Price.Currency)
		}
	}
	report, err := s.batch.BatchUpdatePrices(r.Context(), app.BatchRequest{
		TenantID: tenant, Region: req.Region, Items: req.Items,
		InitiatedBy: req.InitiatedBy,
	})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if report.Partial {
		// 207: some labels were repriced and some were not, and the caller has
		// to look at the results to know which. Collapsing that into 200 or 500
		// would both be lies.
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, report)
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/labels/{id}
// ---------------------------------------------------------------------------

// LabelView is the query-side representation of one label.
type LabelView struct {
	ports.LabelState
	// Healthy is the roster's one-glance verdict: reachable, with nothing
	// outstanding.
	Healthy bool `json:"healthy"`
	// PendingAgeMS is how long an unconfirmed update has been outstanding. It
	// is the number that turns "pending" into "pending and worrying".
	PendingAgeMS int64 `json:"pending_age_ms,omitempty"`
}

func (s *Service) viewOf(row ports.LabelState, now time.Time) LabelView {
	v := LabelView{LabelState: row, Healthy: row.Healthy()}
	if row.PendingSequence != 0 && !row.PendingSince.IsZero() {
		v.PendingAgeMS = now.Sub(row.PendingSince).Milliseconds()
	}
	return v
}

func (s *Service) handleGetLabel(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id := canon.LabelID(r.PathValue("id"))
	row, err := s.state.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return notFound("label %s has no state", id)
		}
		return err
	}
	if row.TenantID != "" && row.TenantID != tenant {
		return notFound("label %s has no state", id)
	}
	writeJSON(w, http.StatusOK, s.viewOf(row, s.clock.Now()))
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/labels/{id}/history
// ---------------------------------------------------------------------------

// HistoryEntry is one event in a label's stored history.
type HistoryEntry struct {
	// Version is the event's place in the label's stream.
	Version int64 `json:"version"`
	// Position is its place in the store's global order.
	Position int64 `json:"position"`
	// EventType is the canonical dotted name.
	EventType string `json:"event_type"`
	// OccurredAt is the source clock; RecordedAt is when USSLP accepted it.
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
	// EventID, CorrelationID and CausationID reconstruct the lineage from a
	// shelf back to a POS webhook.
	EventID       canon.EventID       `json:"event_id"`
	CorrelationID canon.CorrelationID `json:"correlation_id,omitempty"`
	CausationID   canon.EventID       `json:"causation_id,omitempty"`
	// Payload is the event body as stored. It is passed through verbatim,
	// because an audit answer that re-encodes the record is an answer about the
	// encoder rather than about the record.
	Payload json.RawMessage `json:"payload"`
}

// HistoryResponse is a page of a label's price history.
type HistoryResponse struct {
	// LabelID is the label.
	LabelID canon.LabelID `json:"label_id"`
	// Events are newest first.
	Events []HistoryEntry `json:"events"`
}

func (s *Service) handleHistory(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id := canon.LabelID(r.PathValue("id"))
	limit := defaultHistoryLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n <= 0 {
			return badRequest("limit %q is not a positive integer", raw)
		}
		limit = n
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	ctx := r.Context()
	agg, err := s.repo.Load(ctx, id)
	if err != nil {
		return err
	}
	if !agg.Exists() {
		return notFound("label %s has no history", id)
	}
	if agg.TenantID != tenant {
		return notFound("label %s has no history", id)
	}
	stored, err := s.repo.History(ctx, id, limit)
	if err != nil {
		return err
	}
	resp := HistoryResponse{LabelID: id, Events: make([]HistoryEntry, 0, len(stored))}
	for _, se := range stored {
		resp.Events = append(resp.Events, HistoryEntry{
			Version: se.Version, Position: se.Position,
			EventType: se.Envelope.EventType, OccurredAt: se.Envelope.OccurredAt,
			RecordedAt: se.Envelope.RecordedAt, EventID: se.Envelope.EventID,
			CorrelationID: se.Envelope.CorrelationID, CausationID: se.Envelope.CausationID,
			Payload: se.Envelope.Payload,
		})
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/stores/{id}/labels
// ---------------------------------------------------------------------------

// StoreRosterResponse is a store's labels with their health.
type StoreRosterResponse struct {
	// StoreID is the store.
	StoreID canon.StoreID `json:"store_id"`
	// Total, Healthy, Offline, Pending and Failing summarise the roster so an
	// operator does not have to count rows.
	Total   int `json:"total"`
	Healthy int `json:"healthy"`
	Offline int `json:"offline"`
	Pending int `json:"pending"`
	Failing int `json:"failing"`
	// Labels are the rows, in label-id order.
	Labels []LabelView `json:"labels"`
}

func (s *Service) handleStoreLabels(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	store := canon.StoreID(r.PathValue("id"))
	rows, err := s.state.ListByStore(r.Context(), tenant, store)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	resp := StoreRosterResponse{StoreID: store, Labels: make([]LabelView, 0, len(rows))}
	for _, row := range rows {
		v := s.viewOf(row, now)
		resp.Labels = append(resp.Labels, v)
		resp.Total++
		if v.Healthy {
			resp.Healthy++
		}
		if row.State == string(domain.StateOffline) {
			resp.Offline++
		}
		if row.PendingSequence != 0 {
			resp.Pending++
		}
		if row.LastFailureReason != "" {
			resp.Failing++
		}
	}
	sort.Slice(resp.Labels, func(i, j int) bool { return resp.Labels[i].LabelID < resp.Labels[j].LabelID })
	// Re-derive the pending gauge from the read model rather than trusting the
	// incrementally maintained one. The counters on the hot path can drift
	// across a restart; this query already has the exact answer in hand.
	s.metrics.PendingDelivery.With(string(store)).Set(float64(resp.Pending))
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/stores/{id}/slo
// ---------------------------------------------------------------------------

// SLOResponse is the measured end-to-end price propagation performance of one
// store.
//
// It reports what this replica has observed since it started, not a fleet-wide
// figure: the histogram behind it is process-local, and a p99 averaged across
// replicas is a number about the averaging rather than about the shelves. The
// fleet-wide view is Prometheus' job, aggregating the same series by store.
type SLOResponse struct {
	// StoreID is the store.
	StoreID canon.StoreID `json:"store_id"`
	// TargetSeconds is the platform's end-to-end SLO.
	TargetSeconds float64 `json:"target_seconds"`
	// Observations is how many confirmed deliveries the percentiles are drawn
	// from. A percentile over a handful of observations is noise, and reporting
	// the count is what lets a caller tell the difference.
	Observations uint64 `json:"observations"`
	// P50, P95 and P99 are the measured end-to-end latencies in seconds.
	P50 float64 `json:"p50_seconds"`
	P95 float64 `json:"p95_seconds"`
	P99 float64 `json:"p99_seconds"`
	// MeanSeconds is the arithmetic mean.
	MeanSeconds float64 `json:"mean_seconds"`
	// WithinTarget reports whether p99 is inside the SLO.
	WithinTarget bool `json:"within_target"`
	// Confirmed, Failed and Duplicates are the delivery outcome counts.
	Confirmed  uint64 `json:"confirmed"`
	Failed     uint64 `json:"failed"`
	Duplicates uint64 `json:"duplicate_acks"`
	// SuccessRate is confirmed / (confirmed + failed). It is 1 when nothing has
	// been attempted, because "no failures" is the honest answer to "how is a
	// store with no traffic doing".
	SuccessRate float64 `json:"delivery_success_rate"`
	// LabelsPending is the number of labels with an outstanding update right
	// now, taken from the read model rather than from a counter.
	LabelsPending int `json:"labels_pending"`
	// LabelsTotal and LabelsOffline size the store.
	LabelsTotal   int `json:"labels_total"`
	LabelsOffline int `json:"labels_offline"`
}

// SLOTargetSeconds is the platform's end-to-end price propagation budget.
const SLOTargetSeconds = 3.0

func (s *Service) handleStoreSLO(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	store := canon.StoreID(r.PathValue("id"))
	hist := s.metrics.E2ELatency.With(string(tenant), string(store))
	confirmed := s.metrics.DeliveryConfirmations.With(string(store), app.DeliveryConfirmed).Value()
	failed := s.metrics.DeliveryConfirmations.With(string(store), app.DeliveryFailed).Value()
	duplicates := s.metrics.DeliveryConfirmations.With(string(store), app.DeliveryDuplicate).Value()

	resp := SLOResponse{
		StoreID: store, TargetSeconds: SLOTargetSeconds,
		Observations: hist.Count(),
		P50:          hist.Quantile(0.50), P95: hist.Quantile(0.95), P99: hist.Quantile(0.99),
		Confirmed: confirmed, Failed: failed, Duplicates: duplicates,
		SuccessRate: 1,
	}
	if resp.Observations > 0 {
		resp.MeanSeconds = hist.Sum() / float64(resp.Observations)
	}
	if total := confirmed + failed; total > 0 {
		resp.SuccessRate = float64(confirmed) / float64(total)
	}
	resp.WithinTarget = resp.Observations == 0 || resp.P99 <= SLOTargetSeconds

	rows, err := s.state.ListByStore(r.Context(), tenant, store)
	if err != nil {
		return err
	}
	for _, row := range rows {
		resp.LabelsTotal++
		if row.PendingSequence != 0 {
			resp.LabelsPending++
		}
		if row.State == string(domain.StateOffline) {
			resp.LabelsOffline++
		}
	}
	s.metrics.PendingDelivery.With(string(store)).Set(float64(resp.LabelsPending))
	writeJSON(w, http.StatusOK, resp)
	return nil
}
