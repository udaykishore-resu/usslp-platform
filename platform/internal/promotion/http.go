package promotion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion/app"
	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxRequestBytes bounds a request body. A promotion document with a
// twenty-thousand-SKU include list is a legitimate national promotion, and two
// megabytes covers it with room; the limit exists because an unbounded body on
// a listener is a memory-exhaustion primitive.
const maxRequestBytes = 2 << 20

// Handler returns the service's JSON-over-HTTP surface.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/promotions", s.instrument("CreatePromotion", s.handleCreate))
	mux.HandleFunc("GET /v1/promotions", s.instrument("ListPromotions", s.handleList))
	mux.HandleFunc("GET /v1/promotions/conflicts", s.instrument("ListConflicts", s.handleConflicts))
	mux.HandleFunc("GET /v1/promotions/{id}", s.instrument("GetPromotion", s.handleGet))
	mux.HandleFunc("PUT /v1/promotions/{id}", s.instrument("UpdatePromotion", s.handleUpdate))
	mux.HandleFunc("DELETE /v1/promotions/{id}", s.instrument("DeletePromotion", s.handleDelete))
	mux.HandleFunc("POST /v1/promotions/{id}/simulate", s.instrument("SimulatePromotion", s.handleSimulate))
	mux.HandleFunc("POST /v1/promotions/{id}/activate", s.instrument("ActivatePromotion", s.handleActivate))
	mux.HandleFunc("POST /v1/promotions/{id}/cancel", s.instrument("CancelPromotion", s.handleCancel))
	mux.HandleFunc("GET /v1/promotions/{id}/performance", s.instrument("GetPerformance", s.handlePerformance))
	mux.HandleFunc("POST /v1/promotions:resolve", s.instrument("ResolveShelf", s.handleResolve))
	return mux
}

func (s *Service) instrument(operation string, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var span *obs.Span
		if s.tracer != nil {
			ctx = obs.WithRemoteContext(ctx, obs.ParseTraceParent(r.Header.Get("traceparent")))
			ctx, span = s.tracer.Start(ctx, "http."+operation)
			defer span.End()
			span.SetAttr("http.method", r.Method).SetAttr("http.route", operation)
			w.Header().Set("traceparent", span.Ctx.TraceParent())
		}
		start := time.Now()
		err := fn(w, r.WithContext(ctx))
		if err != nil {
			if span != nil {
				span.Fail(err)
			}
			writeError(w, err)
		}
		if s.cfg.Standard != nil {
			s.cfg.Standard.ObserveRequest("http", operation, err, time.Since(start))
		}
	}
}

type httpError struct {
	status int
	code   string
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

func badRequest(format string, args ...any) error {
	return &httpError{status: http.StatusBadRequest, code: "invalid_argument", err: fmt.Errorf(format, args...)}
}

// writeError maps an error to a status. The mapping is a contract: 409 means
// "re-read and retry", 422 means "this document will never be accepted", and
// 400 means "you sent something malformed".
func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, code = he.status, he.code
	case errors.Is(err, app.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, app.ErrDuplicate):
		status, code = http.StatusConflict, "already_exists"
	case errors.Is(err, app.ErrConcurrency):
		status, code = http.StatusConflict, "version_conflict"
	case errors.Is(err, domain.ErrTransition):
		status, code = http.StatusConflict, "illegal_transition"
	case errors.Is(err, domain.ErrInvalidRule):
		status, code = http.StatusUnprocessableEntity, "invalid_rule"
	case errors.Is(err, canon.ErrCurrencyMismatch):
		status, code = http.StatusUnprocessableEntity, "currency_mismatch"
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
		return "", &httpError{status: http.StatusUnauthorized, code: "unauthenticated",
			err: fmt.Errorf("missing %s header", TenantHeader)}
	}
	if !canon.ValidID(raw) {
		return "", badRequest("%s %q contains reserved characters", TenantHeader, raw)
	}
	return canon.TenantID(raw), nil
}

// readBody reads a bounded request body.
//
// The promotion endpoints read the raw bytes rather than decoding straight into
// a struct because domain.ParseRule owns the decoding: it is the one place that
// rejects unknown fields and normalises the display enums, and duplicating that
// in the transport layer is how the two drift apart.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, badRequest("request body exceeds %d bytes", maxRequestBytes)
		}
		return nil, badRequest("could not read the request body: %v", err)
	}
	if len(body) == 0 {
		return nil, badRequest("empty request body")
	}
	return body, nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("malformed request body: %v", err)
	}
	return nil
}

func promotionIDOf(r *http.Request) (canon.PromotionID, error) {
	id := r.PathValue("id")
	if !canon.ValidID(id) {
		return "", badRequest("promotion id %q contains reserved characters", id)
	}
	return canon.PromotionID(id), nil
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	rule, err := domain.ParseRule(body)
	if err != nil {
		return err
	}
	if rule.TenantID == "" {
		rule.TenantID = tenant
	}
	if rule.TenantID != tenant {
		return badRequest("the document's tenant_id %q does not match the authenticated tenant", rule.TenantID)
	}
	rec, err := s.store.Create(rule, s.clock.Now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, rec)
	return nil
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var states []domain.State
	if raw := r.URL.Query().Get("state"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			st := domain.State(strings.TrimSpace(part))
			switch st {
			case domain.StateDraft, domain.StateScheduled, domain.StateActive,
				domain.StateExpired, domain.StateCancelled:
				states = append(states, st)
			default:
				return badRequest("unknown state %q", part)
			}
		}
	}
	records, err := s.store.List(tenant, states)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"promotions": records, "count": len(records)})
	return nil
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	rec, err := s.store.Get(tenant, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	rule, err := domain.ParseRule(body)
	if err != nil {
		return err
	}
	if rule.TenantID == "" {
		rule.TenantID = tenant
	}
	if rule.TenantID != tenant || rule.ID != id {
		return badRequest("the document identifies %s/%s, the path identifies %s/%s",
			rule.TenantID, rule.ID, tenant, id)
	}
	// If-Match carries the version the caller read. Without it the update is a
	// last-write-wins, which is acceptable for a single-operator console and
	// not for two people editing the same promotion before a launch.
	var expected int64
	if raw := r.Header.Get("If-Match"); raw != "" {
		v, err := strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
		if err != nil {
			return badRequest("If-Match must be a version number")
		}
		expected = v
	}
	rec, err := s.store.Update(rule, expected, s.clock.Now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	if err := s.store.Delete(tenant, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// ActivateRequest names the operator taking responsibility.
type ActivateRequest struct {
	By string `json:"by,omitempty"`
}

func (s *Service) handleActivate(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	var req ActivateRequest
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &req); err != nil {
			return err
		}
	}
	rec, err := s.Activate(r.Context(), tenant, id, req.By)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

// CancelRequest records who cancelled and why. The reason is not optional in
// spirit — a promotion pulled without a reason is a question somebody will ask
// in three months — but it is not enforced, because forcing text produces "x".
type CancelRequest struct {
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	var req CancelRequest
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &req); err != nil {
			return err
		}
	}
	rec, err := s.Cancel(r.Context(), tenant, id, req.By, req.Reason)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

// ---------------------------------------------------------------------------
// Simulation
// ---------------------------------------------------------------------------

// SimulateRequest asks what a promotion would affect.
type SimulateRequest struct {
	// Stores narrows the catalogue. Empty means the whole estate.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// ElasticityOf supplies per-SKU elasticities for the volume projection.
	// Without them the simulation claims no volume response at all.
	ElasticityOf map[canon.SKU]float64 `json:"elasticity_of,omitempty"`
	// AgainstLive includes the promotions already active, so the answer is the
	// incremental effect rather than a hypothetical empty shelf.
	AgainstLive bool `json:"against_live,omitempty"`
	// MaxExamples bounds the priced examples returned.
	MaxExamples int `json:"max_examples,omitempty"`
	// Catalogue overrides the configured catalogue, for a console previewing a
	// rule against a hand-picked set of products.
	Catalogue []domain.Product `json:"catalogue,omitempty"`
}

func (s *Service) handleSimulate(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	var req SimulateRequest
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &req); err != nil {
			return err
		}
	}
	rec, err := s.store.Get(tenant, id)
	if err != nil {
		return err
	}

	catalogue := req.Catalogue
	if len(catalogue) == 0 {
		if s.cfg.Catalogue == nil {
			return badRequest("no catalogue is configured; supply one in the request")
		}
		catalogue, err = s.cfg.Catalogue.Products(r.Context(), tenant, req.Stores)
		if err != nil {
			return err
		}
	}

	var others []domain.Rule
	if req.AgainstLive {
		live, err := s.store.List(tenant, []domain.State{domain.StateActive})
		if err != nil {
			return err
		}
		for _, l := range live {
			if l.Rule.ID != id {
				others = append(others, l.Rule)
			}
		}
	}

	start := time.Now()
	res, err := app.Simulate(app.SimulationInput{
		Rule: rec.Rule, Others: others, Catalogue: catalogue,
		ElasticityOf: req.ElasticityOf, MaxExamples: req.MaxExamples,
	})
	if err != nil {
		return err
	}
	s.metrics.evaluations.With("simulate").Observe(time.Since(start).Seconds())
	writeJSON(w, http.StatusOK, res)
	return nil
}

// ---------------------------------------------------------------------------
// Conflicts
// ---------------------------------------------------------------------------

func (s *Service) handleConflicts(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	// Conflicts are checked across everything that could run, not only what is
	// running: the point is for an operator to find out before activation, and
	// a draft that clashes with a live promotion is exactly the case worth
	// catching.
	states := []domain.State{domain.StateDraft, domain.StateScheduled, domain.StateActive}
	if raw := r.URL.Query().Get("state"); raw != "" {
		states = nil
		for _, part := range strings.Split(raw, ",") {
			states = append(states, domain.State(strings.TrimSpace(part)))
		}
	}
	records, err := s.store.List(tenant, states)
	if err != nil {
		return err
	}
	rules := make([]domain.Rule, 0, len(records))
	for _, rec := range records {
		rules = append(rules, rec.Rule)
	}
	if s.cfg.Catalogue == nil {
		return badRequest("no catalogue is configured, so overlaps cannot be evaluated against real products")
	}
	catalogue, err := s.cfg.Catalogue.Products(r.Context(), tenant, nil)
	if err != nil {
		return err
	}
	start := time.Now()
	conflicts := domain.DetectConflicts(rules, catalogue)
	s.metrics.evaluations.With("conflicts").Observe(time.Since(start).Seconds())

	counts := map[domain.Severity]int{}
	for _, c := range conflicts {
		counts[c.Severity]++
	}
	for _, sev := range []domain.Severity{domain.SeverityInfo, domain.SeverityWarn, domain.SeverityError} {
		s.metrics.conflicts.With(string(sev)).Set(float64(counts[sev]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"count":     len(conflicts),
		"by_severity": map[string]int{
			"info": counts[domain.SeverityInfo], "warn": counts[domain.SeverityWarn],
			"error": counts[domain.SeverityError],
		},
		"rules_examined":    len(rules),
		"products_examined": len(catalogue),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Performance
// ---------------------------------------------------------------------------

func (s *Service) handlePerformance(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	id, err := promotionIDOf(r)
	if err != nil {
		return err
	}
	rec, err := s.store.Get(tenant, id)
	if err != nil {
		return err
	}
	if s.cfg.Sales == nil {
		return &httpError{status: http.StatusNotImplemented, code: "unsupported",
			err: errors.New("no sales source is configured, so lift cannot be measured")}
	}

	// The measurement window uses the promotion's *real* dates where it has
	// them. A promotion cancelled after two days of a fortnight's schedule must
	// be measured over the two days it ran.
	zone := ""
	if s.cfg.Directory != nil {
		if z, err := s.cfg.Directory.Zone(r.Context(), tenant, ""); err == nil {
			zone = z
		}
	}
	win, err := rec.Rule.Schedule.ResolveWindow(zone)
	if err != nil {
		return err
	}
	start, end := win.Start, win.End
	if rec.ActivatedAt != nil {
		start = *rec.ActivatedAt
	}
	if rec.EndedAt != nil {
		end = *rec.EndedAt
	}
	if !end.After(start) {
		end = start.Add(24 * time.Hour)
	}
	window := app.DefaultLiftWindow(start, end)

	test, control, controlStores, err := s.cfg.Sales.Sales(r.Context(), tenant, id, window.PreStart, window.PostEnd)
	if err != nil {
		return err
	}
	var result app.LiftResult
	if len(control) > 0 {
		result = app.MeasureLiftWithControl(id, "all stores", test, control, controlStores, window)
	} else {
		result = app.MeasureLift(id, "all stores", test, window)
		result.Caveats = append(result.Caveats,
			"no control group was available, so seasonality and everything else that moved in this period "+
				"is attributed to the promotion")
	}

	body := map[string]any{
		"promotion": rec, "lift": result,
		"window": map[string]any{
			"pre_start": window.PreStart, "during_start": window.DuringStart,
			"during_end": window.DuringEnd, "post_end": window.PostEnd,
		},
	}
	if s.cfg.Directory != nil {
		clusterOf := map[canon.StoreID]string{}
		for _, p := range test {
			if _, seen := clusterOf[p.StoreID]; seen {
				continue
			}
			c, err := s.cfg.Directory.Cluster(r.Context(), tenant, p.StoreID)
			if err != nil {
				continue
			}
			clusterOf[p.StoreID] = c
		}
		body["by_cluster"] = app.ClusterLift(id, test, clusterOf, window)
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

// ---------------------------------------------------------------------------
// Shelf resolution
// ---------------------------------------------------------------------------

// ResolveRequest asks what the live promotion set does to a product. It is what
// the Label Service calls when it needs the answer synchronously rather than
// from the event stream — a shelf audit, or a support engineer asking why a
// label shows what it shows.
type ResolveRequest struct {
	Product domain.Product `json:"product"`
	// At overrides the evaluation instant, so an operator can ask what the
	// shelf will look like on Saturday.
	At *time.Time `json:"at,omitempty"`
	// Zone is the store's IANA location, needed to answer a wall-clock
	// schedule.
	Zone string `json:"zone,omitempty"`
}

func (s *Service) handleResolve(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req ResolveRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if req.Product.SKU == "" {
		return badRequest("product.sku is required")
	}
	at := s.clock.Now()
	if req.At != nil {
		at = req.At.UTC()
	}
	zone := req.Zone
	if zone == "" && s.cfg.Directory != nil {
		if z, err := s.cfg.Directory.Zone(r.Context(), tenant, req.Product.StoreID); err == nil {
			zone = z
		}
	}

	records, err := s.store.List(tenant, []domain.State{domain.StateActive})
	if err != nil {
		return err
	}
	// Filter by the store's own local window before resolving, so a promotion
	// that is active as a whole but has not opened in this store does not price
	// its shelves.
	rules := make([]domain.Rule, 0, len(records))
	for _, rec := range records {
		open, err := rec.Rule.Schedule.ActiveInStore(zone, at)
		if err != nil {
			return err
		}
		if open {
			rules = append(rules, rec.Rule)
		}
	}
	set := domain.CompileSet(rules)
	matched := set.Match(req.Product, make([]domain.Rule, 0, 8))
	res, err := domain.Resolve(matched, req.Product)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resolution": res, "evaluated_at": at, "zone": zone,
		"active_promotions": len(rules),
	})
	return nil
}

// marshalEnvelope serialises an envelope for the bus. It lives here beside the
// other encoding so that the wire format is decided in one file.
func marshalEnvelope(env canon.Envelope) ([]byte, error) { return json.Marshal(env) }
