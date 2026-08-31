package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/app"
	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/features"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/ports"
	"github.com/usslp/usslp/platform/internal/pricing/registry"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxRequestBytes bounds a request body. A cross-store optimisation request
// carries every SKU in a category across a store cluster, which is thousands of
// states; four megabytes covers that with room and is small enough that an
// unbounded body cannot be used to exhaust the pod.
const maxRequestBytes = 4 << 20

// Handler returns the service's JSON-over-HTTP surface.
//
// The routes below are the whole specification. `evaluate` and `recommend` are
// deliberately separate endpoints rather than one with a mode flag: the first
// is the Tier-1 hot path with a sub-10-millisecond budget and no model
// involvement at all, and conflating them would put a model load on the hot
// path's error budget.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pricing/evaluate", s.instrument("EvaluateTier1", s.handleEvaluate))
	mux.HandleFunc("POST /v1/pricing/recommend", s.instrument("Recommend", s.handleRecommend))
	mux.HandleFunc("POST /v1/pricing/optimise", s.instrument("OptimiseCrossStore", s.handleOptimise))
	mux.HandleFunc("GET /v1/pricing/elasticity/{sku}", s.instrument("GetElasticity", s.handleElasticity))
	mux.HandleFunc("GET /v1/models", s.instrument("ListModels", s.handleListModels))
	mux.HandleFunc("POST /v1/models", s.instrument("TrainModel", s.handleTrainModel))
	mux.HandleFunc("GET /v1/models/{id}", s.instrument("GetModel", s.handleGetModel))
	mux.HandleFunc("POST /v1/models/{id}/promote", s.instrument("PromoteModel", s.handlePromote))
	mux.HandleFunc("GET /v1/anomalies", s.instrument("ListAnomalies", s.handleAnomalies))
	mux.HandleFunc("GET /v1/policy-pack/{store}", s.instrument("GetPolicyPack", s.handlePolicyPack))
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

func notFound(format string, args ...any) error {
	return &httpError{status: http.StatusNotFound, code: "not_found", err: fmt.Errorf(format, args...)}
}

// writeError maps an error to a status code. The mapping is a contract with
// callers: 422 means "this input will never work", 404 means "not here", 409
// means "the platform refused", and 400 means "you sent something malformed".
func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, code = he.status, he.code
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, features.ErrNotFound), errors.Is(err, ports.ErrNoConstraints):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, registry.ErrPromotionRefused):
		status, code = http.StatusConflict, "promotion_refused"
	case errors.Is(err, app.ErrInfeasible):
		status, code = http.StatusUnprocessableEntity, "infeasible"
	case errors.Is(err, app.ErrInsufficientData), errors.Is(err, ml.ErrTraining):
		status, code = http.StatusUnprocessableEntity, "insufficient_data"
	case errors.Is(err, domain.ErrInvalidConstraints), errors.Is(err, registry.ErrInvalid), errors.Is(err, features.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_argument"
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

func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("malformed request body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /v1/pricing/evaluate — Tier 1 only, the hot path
// ---------------------------------------------------------------------------

// EvaluateRequest asks whether a price is compliant.
type EvaluateRequest struct {
	StoreID canon.StoreID `json:"store_id"`
	SKU     canon.SKU     `json:"sku"`
	// PriceMinor is the price to test.
	PriceMinor int64 `json:"price_minor"`
	// Constraints, when supplied, override the configured rules. This is what
	// the "what if I set the margin floor to 25%" operator tool uses, and it is
	// why the endpoint stays pure: nothing about this request touches a model
	// or a database when the constraints come inline.
	Constraints *domain.Constraints `json:"constraints,omitempty"`
}

// EvaluateResponse is the Tier-1 decision.
type EvaluateResponse struct {
	Decision domain.Decision `json:"decision"`
	// TookMicros is the measured evaluation time. The endpoint's budget is
	// under ten milliseconds and this is the number that proves it, per call,
	// to the caller — not only in a dashboard.
	TookMicros int64 `json:"took_micros"`
}

func (s *Service) handleEvaluate(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req EvaluateRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	c, err := s.constraintsFor(r, tenant, req.StoreID, req.SKU, req.Constraints)
	if err != nil {
		return err
	}
	start := time.Now()
	d := s.evaluate(c, req.PriceMinor)
	writeJSON(w, http.StatusOK, EvaluateResponse{Decision: d, TookMicros: time.Since(start).Microseconds()})
	return nil
}

func (s *Service) constraintsFor(r *http.Request, tenant canon.TenantID, store canon.StoreID, sku canon.SKU, inline *domain.Constraints) (domain.Constraints, error) {
	if inline != nil {
		return *inline, nil
	}
	if s.cfg.ConstraintSource == nil {
		return domain.Constraints{}, badRequest("no constraint source is configured; supply constraints inline")
	}
	if store == "" || sku == "" {
		return domain.Constraints{}, badRequest("store_id and sku are required when constraints are not supplied inline")
	}
	c, err := s.cfg.ConstraintSource.Constraints(r.Context(), tenant, store, sku)
	if err != nil {
		return domain.Constraints{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// POST /v1/pricing/recommend — Tier 2
// ---------------------------------------------------------------------------

// RecommendRequest asks for an optimal price.
type RecommendRequest struct {
	StoreID canon.StoreID `json:"store_id"`
	SKU     canon.SKU     `json:"sku"`
	// Constraints override the configured Tier-1 rules when supplied.
	Constraints *domain.Constraints `json:"constraints,omitempty"`
	// Features override the feature store when supplied. A caller that already
	// has the state — the SGU, which is holding it anyway — saves a read.
	Features *domain.Features `json:"features,omitempty"`
	// Objective selects what is maximised. Empty means profit.
	Objective app.Objective `json:"objective,omitempty"`
	// ReferenceUnits anchors the elasticity projection when no model is
	// available. Zero falls back to the 7-day velocity feature.
	ReferenceUnits float64 `json:"reference_units,omitempty"`
	// IncludeCurve returns every evaluated candidate, for the operator UI plot.
	IncludeCurve bool `json:"include_curve,omitempty"`
	// AsOf makes the recommendation reproducible: features are read as they
	// were known at this instant. Zero means now.
	AsOf *time.Time `json:"as_of,omitempty"`
}

// RecommendResponse carries the recommendation.
type RecommendResponse struct {
	Recommendation app.Recommendation `json:"recommendation"`
	// ModelID names the model that produced it, or is empty when the
	// recommendation rests on the elasticity projection alone. It is what makes
	// "which model set this price" answerable.
	ModelID string `json:"model_id,omitempty"`
	// Elasticity is the estimate the recommendation used.
	Elasticity ml.Elasticity `json:"elasticity"`
	// MissingFeatures names features the store could not supply.
	MissingFeatures []string `json:"missing_features,omitempty"`
	TookMicros      int64    `json:"took_micros"`
}

func (s *Service) handleRecommend(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req RecommendRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.SKU == "" {
		return badRequest("sku is required")
	}
	start := time.Now()

	c, err := s.constraintsFor(r, tenant, req.StoreID, req.SKU, req.Constraints)
	if err != nil {
		return err
	}
	asOf := s.clock.Now()
	if req.AsOf != nil {
		asOf = req.AsOf.UTC()
	}

	feats, missing, err := s.resolveFeatures(tenant, req, asOf)
	if err != nil {
		return err
	}

	elast, err := app.EstimateElasticityFor(s.features, tenant, req.StoreID, req.SKU, asOf, 400, s.cfg.ElasticityPolicy)
	if err != nil {
		return err
	}
	s.metrics.elasticity.With(strconv.FormatBool(elast.Usable)).Inc()
	feats.Elasticity = elast.Coefficient

	slot := registry.Slot{Tenant: tenant, Store: req.StoreID, Purpose: registry.PurposeDemand}
	var model app.DemandModel
	var modelID string
	if m, err := s.demandModel(slot); err == nil {
		model = m
		if md, err := s.models.Champion(slot); err == nil {
			modelID = md.ID
		}
	} else if !errors.Is(err, registry.ErrNotFound) {
		return err
	}

	refUnits := req.ReferenceUnits
	if refUnits <= 0 {
		refUnits = feats.Velocity7
	}
	rec, err := app.Optimise(app.OptimisationInput{
		Constraints: c, Features: feats, Model: model, Elasticity: elast,
		ReferenceUnits: refUnits, Objective: req.Objective,
	})
	if err != nil {
		return err
	}
	if !req.IncludeCurve {
		rec.Curve = nil
	}
	s.metrics.tierLatency.With("2").Observe(time.Since(start).Seconds())
	s.metrics.decisions.With(rec.Decision.Outcome.String()).Inc()
	writeJSON(w, http.StatusOK, RecommendResponse{
		Recommendation: rec, ModelID: modelID, Elasticity: elast,
		MissingFeatures: missing, TookMicros: time.Since(start).Microseconds(),
	})
	return nil
}

// resolveFeatures assembles the model input, preferring an inline vector.
func (s *Service) resolveFeatures(tenant canon.TenantID, req RecommendRequest, asOf time.Time) (domain.Features, []string, error) {
	if req.Features != nil {
		return *req.Features, nil, nil
	}
	names := make([]string, domain.NumFeatures)
	copy(names, domain.FeatureNames[:])
	vec, missing, err := s.features.Vector(tenant, req.StoreID, req.SKU, names, asOf)
	if err != nil {
		return domain.Features{}, nil, err
	}
	f := domain.Features{
		PriceMinor: vec[domain.FeatPrice], HourOfDay: vec[domain.FeatHourOfDay],
		DayOfWeek: vec[domain.FeatDayOfWeek], DaysToExpiry: vec[domain.FeatDaysToExpiry],
		Season: vec[domain.FeatSeason], InventoryLevel: vec[domain.FeatInventoryLevel],
		DaysOfSupply: vec[domain.FeatDaysOfSupply], WasteRate: vec[domain.FeatWasteRate],
		CompetitorPrice: vec[domain.FeatCompetitorPrice], Velocity7: vec[domain.FeatVelocity7],
		Velocity14: vec[domain.FeatVelocity14], Velocity30: vec[domain.FeatVelocity30],
		Elasticity: vec[domain.FeatElasticity], WeatherBucket: vec[domain.FeatWeatherBucket],
		LocalEvent: vec[domain.FeatLocalEvent] != 0,
	}
	return f, missing, nil
}

// ---------------------------------------------------------------------------
// POST /v1/pricing/optimise — Tier 3
// ---------------------------------------------------------------------------

// OptimiseRequest is a cross-store optimisation pass.
type OptimiseRequest struct {
	States           []app.SKUState `json:"states"`
	Objective        app.Objective  `json:"objective,omitempty"`
	MaxRounds        int            `json:"max_rounds,omitempty"`
	ConvergenceMinor int64          `json:"convergence_minor,omitempty"`
	// UseStoreModel loads each store's champion demand model. Without it the
	// pass runs on the elasticity projections alone, which is what a tenant
	// with no trained models gets and is still worth running for the
	// cannibalisation correction.
	UseStoreModel bool `json:"use_store_model,omitempty"`
}

func (s *Service) handleOptimise(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req OptimiseRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.States) == 0 {
		return badRequest("at least one state is required")
	}
	in := app.CrossStoreInput{
		States: req.States, Objective: req.Objective,
		MaxRounds: req.MaxRounds, ConvergenceMinor: req.ConvergenceMinor,
	}
	if req.UseStoreModel {
		// One model per pass: the pass is scoped to a store cluster, and a
		// per-state model lookup inside the coordinate-descent inner loop would
		// turn a batch job into a registry hammer.
		slot := registry.Slot{Tenant: tenant, Store: req.States[0].Store, Purpose: registry.PurposeDemand}
		if m, err := s.demandModel(slot); err == nil {
			in.Model = m
		} else if !errors.Is(err, registry.ErrNotFound) {
			return err
		}
	}
	start := time.Now()
	report, err := app.OptimiseCrossStore(in)
	if err != nil {
		return err
	}
	s.metrics.tierLatency.With("3").Observe(time.Since(start).Seconds())
	writeJSON(w, http.StatusOK, report)
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/pricing/elasticity/{sku}
// ---------------------------------------------------------------------------

// ElasticityResponse carries the estimate and the curve it implies.
type ElasticityResponse struct {
	SKU        canon.SKU     `json:"sku"`
	StoreID    canon.StoreID `json:"store_id"`
	Elasticity ml.Elasticity `json:"elasticity"`
	// Curve is the projected demand at each feasible price, with the
	// confidence band. An operator looking at a wide band is looking at the
	// reason the platform refused to act.
	Curve []ElasticityPoint `json:"curve,omitempty"`
	AsOf  time.Time         `json:"as_of"`
}

// ElasticityPoint is one point on the projected demand curve.
type ElasticityPoint struct {
	PriceMinor int64   `json:"price_minor"`
	Units      float64 `json:"units"`
	UnitsLow   float64 `json:"units_low"`
	UnitsHigh  float64 `json:"units_high"`
}

func (s *Service) handleElasticity(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	sku := canon.SKU(r.PathValue("sku"))
	if !canon.ValidID(string(sku)) {
		return badRequest("sku %q contains reserved characters", sku)
	}
	store := canon.StoreID(r.URL.Query().Get("store_id"))
	if store != "" && !canon.ValidID(string(store)) {
		return badRequest("store_id %q contains reserved characters", store)
	}
	asOf := s.clock.Now()
	if raw := r.URL.Query().Get("as_of"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return badRequest("as_of must be RFC 3339: %v", err)
		}
		asOf = t.UTC()
	}
	e, err := app.EstimateElasticityFor(s.features, tenant, store, sku, asOf, 400, s.cfg.ElasticityPolicy)
	if err != nil {
		return err
	}
	resp := ElasticityResponse{SKU: sku, StoreID: store, Elasticity: e, AsOf: asOf}

	// The curve needs an anchor point. Without one there is nothing to project
	// from, and the endpoint returns the estimate alone rather than inventing a
	// reference price.
	if s.cfg.ConstraintSource != nil && store != "" {
		if c, cerr := s.cfg.ConstraintSource.Constraints(r.Context(), tenant, store, sku); cerr == nil && c.CurrentMinor > 0 {
			refUnits := 0.0
			if v, verr := s.features.AsOf(features.Key{
				Tenant: tenant, Store: store, SKU: sku, Name: domain.FeatureNames[domain.FeatVelocity7],
			}, asOf); verr == nil {
				refUnits = v.Number
			}
			if refUnits > 0 {
				for _, p := range c.Candidates(64) {
					lo, hi := e.Bounds(float64(c.CurrentMinor), refUnits, float64(p))
					resp.Curve = append(resp.Curve, ElasticityPoint{
						PriceMinor: p,
						Units:      e.DemandAt(float64(c.CurrentMinor), refUnits, float64(p)),
						UnitsLow:   lo, UnitsHigh: hi,
					})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (s *Service) handleListModels(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	purpose := registry.Purpose(r.URL.Query().Get("purpose"))
	if purpose == "" {
		purpose = registry.PurposeDemand
	}
	slot := registry.Slot{Tenant: tenant, Store: canon.StoreID(r.URL.Query().Get("store_id")), Purpose: purpose}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return badRequest("limit must be a positive integer")
		}
		limit = n
	}
	models, err := s.models.List(slot, limit)
	if err != nil {
		return err
	}
	champion := ""
	if md, err := s.models.Champion(slot); err == nil {
		champion = md.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models, "champion_id": champion, "count": len(models),
	})
	return nil
}

func (s *Service) handleGetModel(w http.ResponseWriter, r *http.Request) error {
	if _, err := tenantOf(r); err != nil {
		return err
	}
	md, err := s.models.Get(r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, md)
	return nil
}

// TrainRequest asks the service to fit a new model from the feature store.
type TrainRequest struct {
	StoreID canon.StoreID `json:"store_id"`
	// Examples are the supervised rows. Each carries the instant its features
	// must be assembled as of, which is what makes the resulting model
	// leakage-free.
	Examples []TrainExample `json:"examples"`
	// Params overrides the boosting configuration.
	Params *ml.GBTParams `json:"params,omitempty"`
	// HoldoutFraction is the tail reserved for evaluation.
	HoldoutFraction float64 `json:"holdout_fraction,omitempty"`
	Notes           string  `json:"notes,omitempty"`
}

// TrainExample is one supervised row in the API's vocabulary.
type TrainExample struct {
	SKU        canon.SKU `json:"sku"`
	DecisionAt time.Time `json:"decision_at"`
	Target     float64   `json:"target"`
}

func (s *Service) handleTrainModel(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req TrainRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.Examples) == 0 {
		return badRequest("at least one training example is required")
	}
	examples := make([]app.TrainingExample, 0, len(req.Examples))
	for i, e := range req.Examples {
		if e.SKU == "" || e.DecisionAt.IsZero() {
			return badRequest("example %d needs a sku and a decision_at", i)
		}
		examples = append(examples, app.TrainingExample{
			SKU: e.SKU, Store: req.StoreID, DecisionAt: e.DecisionAt.UTC(), Target: e.Target,
		})
	}
	names := make([]string, domain.NumFeatures)
	copy(names, domain.FeatureNames[:])
	set, err := app.BuildTrainingSet(s.features, tenant, examples, names)
	if err != nil {
		return err
	}
	params := ml.GBTParams{}
	if req.Params != nil {
		params = *req.Params
	}
	res, err := app.TrainDemandModel(s.models, app.TrainDemandModelInput{
		Slot:            registry.Slot{Tenant: tenant, Store: req.StoreID, Purpose: registry.PurposeDemand},
		Set:             set,
		Params:          params,
		HoldoutFraction: req.HoldoutFraction,
		Notes:           req.Notes,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"model":         res.Metadata,
		"metrics":       res.Metrics,
		"quantisation":  res.Quantisation,
		"comparison":    res.Comparison,
		"rows_used":     len(set.X),
		"rows_dropped":  set.Dropped,
		"window_start":  set.WindowStart,
		"window_end":    set.WindowEnd,
		"feature_names": names,
	})
	return nil
}

// PromoteRequest is an operator's promotion decision.
type PromoteRequest struct {
	// Force promotes against a negative champion/challenger verdict. The
	// verdict is recomputed here from the stored metrics, so a caller cannot
	// promote a worse model by supplying a flattering comparison.
	Force bool   `json:"force,omitempty"`
	Notes string `json:"notes,omitempty"`
}

func (s *Service) handlePromote(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req PromoteRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			return err
		}
	}
	id := r.PathValue("id")
	md, err := s.models.Get(id)
	if err != nil {
		return err
	}
	if md.Tenant != tenant {
		// Returning 404 rather than 403: a cross-tenant identifier probe must
		// not be able to distinguish "exists but not yours" from "does not
		// exist".
		return notFound("model %s", id)
	}
	res, err := s.models.Promote(id, registry.PromoteOptions{
		Comparison:              md.Comparison,
		Force:                   req.Force,
		MaxQuantisationDeltaPct: s.cfg.MaxQuantisationDeltaPct,
	})
	if err != nil {
		return err
	}
	// The promotion only reaches the shelves once serving instances drop their
	// cached champion.
	s.InvalidateModels(tenant)
	writeJSON(w, http.StatusOK, res)
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/anomalies
// ---------------------------------------------------------------------------

func (s *Service) handleAnomalies(w http.ResponseWriter, r *http.Request) error {
	if _, err := tenantOf(r); err != nil {
		return err
	}
	det := s.Detector()
	if det == nil {
		// An empty list with an explicit note, not a 404: "no detector is
		// trained yet" is a normal state for a new tenant and the caller's
		// dashboard should render an empty panel rather than an error.
		writeJSON(w, http.StatusOK, map[string]any{
			"anomalies": []app.AnomalyRecord{}, "detector_trained": false,
		})
		return nil
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return badRequest("limit must be a positive integer")
		}
		limit = n
	}
	store := canon.StoreID(r.URL.Query().Get("store_id"))
	records := det.Recent(store, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"anomalies": records, "detector_trained": true,
		"threshold": det.Threshold(), "count": len(records),
	})
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/policy-pack/{store}
// ---------------------------------------------------------------------------

// handlePolicyPack serves the compact Tier-1 rule table the Store Gateway Unit
// embeds. It is the mechanism behind "the same decision offline": the gateway
// downloads this, and the identical domain.Evaluate runs against it.
func (s *Service) handlePolicyPack(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	store := canon.StoreID(r.PathValue("store"))
	if !canon.ValidID(string(store)) {
		return badRequest("store %q contains reserved characters", store)
	}
	source, ok := s.cfg.ConstraintSource.(ports.PolicyPackSource)
	if !ok {
		return &httpError{status: http.StatusNotImplemented, code: "unsupported",
			err: fmt.Errorf("the configured constraint source cannot enumerate a store's rules")}
	}
	pack, err := source.PolicyPack(r.Context(), tenant, store)
	if err != nil {
		return err
	}
	body, err := pack.MarshalBinary()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/vnd.usslp.policy-pack")
	w.Header().Set("X-USSLP-Pack-Version", strconv.FormatInt(pack.Version, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// decodeEnvelope and the JSON helpers are here rather than in service.go so
// that the encoding choices for the wire and for the state store sit together.
func decodeEnvelope(b []byte, env *canon.Envelope) error { return json.Unmarshal(b, env) }
func decodeJSON(b []byte, dst any) error                 { return json.Unmarshal(b, dst) }
func encodeJSON(v any) ([]byte, error)                   { return json.Marshal(v) }
