package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/app"
	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxRequestBytes bounds a request body. A query document with a
// thousand-store `in` filter is the largest legitimate request, and 256 kB
// covers it several times over.
const maxRequestBytes = 256 << 10

// DefaultWindow is the period a report covers when the caller does not say.
// Seven days is what every dashboard on the platform opens on.
const DefaultWindow = 7 * 24 * time.Hour

// Handler returns the service's JSON-over-HTTP surface.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", s.instrument("Query", s.handleQuery))
	mux.HandleFunc("GET /v1/tables", s.instrument("ListTables", s.handleTables))
	mux.HandleFunc("GET /v1/slo", s.instrument("GetSLO", s.handleSLO))
	mux.HandleFunc("GET /v1/reports/elasticity/{sku}", s.instrument("ElasticityCurve", s.handleElasticity))
	mux.HandleFunc("GET /v1/reports/promotion/{id}", s.instrument("PromotionLift", s.handlePromotionLift))
	mux.HandleFunc("GET /v1/reports/interaction", s.instrument("LabelInteraction", s.handleInteraction))
	mux.HandleFunc("GET /v1/reports/competitive", s.instrument("CompetitivePosition", s.handleCompetitive))
	mux.HandleFunc("GET /v1/reports/shrinkage", s.instrument("Shrinkage", s.handleShrinkage))
	mux.HandleFunc("GET /v1/reports/benchmark", s.instrument("Benchmark", s.handleBenchmark))
	mux.HandleFunc("GET /v1/retention", s.instrument("GetRetention", s.handleRetention))
	mux.HandleFunc("POST /v1/retention:sweep", s.instrument("SweepRetention", s.handleSweep))
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
		s.metrics.queries.With(operation).Observe(time.Since(start).Seconds())
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

func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, code = he.status, he.code
	case errors.Is(err, columnar.ErrQuery):
		status, code = http.StatusBadRequest, "invalid_query"
	case errors.Is(err, columnar.ErrCorrupt):
		// Corruption is the service's problem, not the caller's, and it is the
		// one error here worth a 500 with a distinct code so it is greppable.
		status, code = http.StatusInternalServerError, "corrupt_data"
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

// scopeOf builds a report scope from the query string.
func scopeOf(r *http.Request, tenant canon.TenantID) (app.Scope, error) {
	scope := app.Scope{Tenant: tenant, Zone: r.URL.Query().Get("zone")}
	now := time.Now().UTC()
	scope.To = now
	scope.From = now.Add(-DefaultWindow)

	if raw := r.URL.Query().Get("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return app.Scope{}, badRequest("from must be RFC 3339: %v", err)
		}
		scope.From = t.UTC()
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return app.Scope{}, badRequest("to must be RFC 3339: %v", err)
		}
		scope.To = t.UTC()
	}
	if !scope.To.After(scope.From) {
		return app.Scope{}, badRequest("the window %s..%s is empty", scope.From, scope.To)
	}
	if raw := r.URL.Query().Get("stores"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id := strings.TrimSpace(part)
			if !canon.ValidID(id) {
				return app.Scope{}, badRequest("store %q contains reserved characters", id)
			}
			scope.Stores = append(scope.Stores, canon.StoreID(id))
		}
	}
	if scope.Zone != "" {
		if _, err := time.LoadLocation(scope.Zone); err != nil {
			return app.Scope{}, badRequest("unknown zone %q", scope.Zone)
		}
	}
	return scope, nil
}

// ---------------------------------------------------------------------------
// POST /v1/query
// ---------------------------------------------------------------------------

// QueryRequest is the structured query API.
//
// Note what is absent: a tenant filter. The service injects the authenticated
// tenant's predicate itself, so a caller cannot read another tenant's rows by
// omitting or altering one. A filter on tenant_id in the request is rejected
// rather than merged, because two tenant predicates would be an ambiguity where
// there must not be one.
type QueryRequest struct {
	Table      string               `json:"table"`
	From       *time.Time           `json:"from,omitempty"`
	To         *time.Time           `json:"to,omitempty"`
	Filters    []columnar.Filter    `json:"filters,omitempty"`
	GroupBy    []string             `json:"group_by,omitempty"`
	Bucket     string               `json:"bucket,omitempty"`
	BucketZone string               `json:"bucket_zone,omitempty"`
	Aggregates []columnar.Aggregate `json:"aggregates,omitempty"`
	Limit      int                  `json:"limit,omitempty"`
	OrderBy    string               `json:"order_by,omitempty"`
	Ascending  bool                 `json:"ascending,omitempty"`
	Tiers      []columnar.Tier      `json:"tiers,omitempty"`
}

func (s *Service) handleQuery(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	var req QueryRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	store, ok := s.tables[req.Table]
	if !ok {
		return badRequest("unknown table %q", req.Table)
	}
	for _, f := range req.Filters {
		if f.Column == "tenant_id" {
			return &httpError{status: http.StatusForbidden, code: "forbidden",
				err: errors.New("the tenant filter is set by the service and cannot be supplied")}
		}
	}

	q := columnar.Query{
		Filters:    append([]columnar.Filter{{Column: "tenant_id", Op: columnar.OpEq, Value: string(tenant)}}, req.Filters...),
		GroupBy:    req.GroupBy,
		BucketZone: req.BucketZone,
		Aggregates: req.Aggregates,
		Limit:      req.Limit,
		OrderBy:    req.OrderBy,
		Ascending:  req.Ascending,
		Tiers:      req.Tiers,
	}
	if req.From != nil {
		q.From = req.From.UTC()
	}
	if req.To != nil {
		q.To = req.To.UTC()
	}
	if req.Bucket != "" {
		d, err := time.ParseDuration(req.Bucket)
		if err != nil {
			return badRequest("bucket must be a duration like 1h or 24h: %v", err)
		}
		if d <= 0 {
			return badRequest("bucket must be positive")
		}
		q.Bucket = d
	}

	res, err := store.Query(q)
	if err != nil {
		return err
	}
	s.metrics.blocks.With("scanned").Add(uint64(res.Stats.BlocksScanned))
	s.metrics.blocks.With("skipped").Add(uint64(res.Stats.BlocksSkipped))
	writeJSON(w, http.StatusOK, res)
	return nil
}

// handleTables describes the schemas, so a caller can build a valid query
// without reading this service's source.
func (s *Service) handleTables(w http.ResponseWriter, r *http.Request) error {
	if _, err := tenantOf(r); err != nil {
		return err
	}
	type tableInfo struct {
		Schema columnar.Schema `json:"schema"`
		Stats  columnar.Stats  `json:"stats"`
	}
	out := map[string]tableInfo{}
	for name, store := range s.tables {
		st, err := store.Stats()
		if err != nil {
			return err
		}
		out[name] = tableInfo{Schema: store.Schema(), Stats: st}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tables":     out,
		"operators":  []columnar.Op{columnar.OpEq, columnar.OpNe, columnar.OpLt, columnar.OpLte, columnar.OpGt, columnar.OpGte, columnar.OpIn, columnar.OpPrefix},
		"aggregates": []columnar.AggFunc{columnar.AggCount, columnar.AggSum, columnar.AggAvg, columnar.AggMin, columnar.AggMax, columnar.AggQuantile, columnar.AggCountDistinct},
	})
	return nil
}

// ---------------------------------------------------------------------------
// GET /v1/slo
// ---------------------------------------------------------------------------

func (s *Service) handleSLO(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	report, err := app.ComputeSLOReport(s.tables, scope, s.cfg.SLOs)
	if err != nil {
		return err
	}
	// The worst severity across the objectives is what a status page renders,
	// so it is computed here rather than by every consumer.
	worst := "ok"
	rank := map[string]int{"ok": 0, "breached": 1, "ticket": 2, "page": 3}
	for _, res := range report.Results {
		if rank[res.Severity()] > rank[worst] {
			worst = res.Severity()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "severity": worst})
	return nil
}

// ---------------------------------------------------------------------------
// Named reports
// ---------------------------------------------------------------------------

func (s *Service) handleElasticity(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	sku := r.PathValue("sku")
	if !canon.ValidID(sku) {
		return badRequest("sku %q contains reserved characters", sku)
	}
	curve, err := app.PriceElasticityCurve(s.tables, scope, canon.SKU(sku))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, curve)
	return nil
}

func (s *Service) handlePromotionLift(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if !canon.ValidID(id) {
		return badRequest("promotion id %q contains reserved characters", id)
	}
	// The promotion's own window, which the caller supplies because the
	// analytics service does not hold promotion definitions — the promotion
	// service does, and duplicating them here is how the two come to disagree.
	start, end := scope.From, scope.To
	if raw := r.URL.Query().Get("promo_start"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return badRequest("promo_start must be RFC 3339: %v", err)
		}
		start = t.UTC()
	}
	if raw := r.URL.Query().Get("promo_end"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return badRequest("promo_end must be RFC 3339: %v", err)
		}
		end = t.UTC()
	}
	lift, err := app.PromotionLiftReport(s.tables, scope, canon.PromotionID(id), start, end)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, lift)
	return nil
}

func (s *Service) handleInteraction(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	groupBy := []string{"store_id"}
	if raw := r.URL.Query().Get("group_by"); raw != "" {
		groupBy = nil
		for _, part := range strings.Split(raw, ",") {
			groupBy = append(groupBy, strings.TrimSpace(part))
		}
	}
	var bucket time.Duration
	if raw := r.URL.Query().Get("bucket"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return badRequest("bucket must be a duration: %v", err)
		}
		bucket = d
	}
	report, err := app.LabelInteraction(s.tables, scope, groupBy, bucket)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, report)
	return nil
}

func (s *Service) handleCompetitive(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	var groupBy []string
	if raw := r.URL.Query().Get("group_by"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			groupBy = append(groupBy, strings.TrimSpace(part))
		}
	}
	report, err := app.CompetitivePosition(s.tables, scope, groupBy)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, report)
	return nil
}

func (s *Service) handleShrinkage(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	report, err := app.ShrinkageAgainstPricingDelay(s.tables, scope)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, report)
	return nil
}

func (s *Service) handleBenchmark(w http.ResponseWriter, r *http.Request) error {
	tenant, err := tenantOf(r)
	if err != nil {
		return err
	}
	scope, err := scopeOf(r, tenant)
	if err != nil {
		return err
	}
	table := r.URL.Query().Get("table")
	if table == "" {
		table = domain.TableDelivery
	}
	if _, ok := s.tables[table]; !ok {
		return badRequest("unknown table %q", table)
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "latency_ms"
	}
	fn := columnar.AggFunc(r.URL.Query().Get("func"))
	if fn == "" {
		fn = columnar.AggQuantile
	}
	agg := columnar.Aggregate{Func: fn, Column: metric, As: "metric"}
	if fn == columnar.AggQuantile {
		agg.Q = 0.95
		if raw := r.URL.Query().Get("q"); raw != "" {
			q, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return badRequest("q must be a number: %v", err)
			}
			agg.Q = q
		}
	}
	// Direction defaults to "lower is better", which is right for latency and
	// waste — the two metrics anybody benchmarks first — and is overridable for
	// sales.
	higherIsBetter := r.URL.Query().Get("higher_is_better") == "true"

	report, err := app.CrossStoreBenchmark(s.tables, scope, table, agg, higherIsBetter)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, report)
	return nil
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

func (s *Service) handleRetention(w http.ResponseWriter, r *http.Request) error {
	if _, err := tenantOf(r); err != nil {
		return err
	}
	type policyView struct {
		domain.RetentionPolicy
		HotHuman  string         `json:"hot_human"`
		WarmHuman string         `json:"warm_human"`
		ColdHuman string         `json:"cold_human"`
		Stats     columnar.Stats `json:"stats"`
	}
	out := make([]policyView, 0, len(s.cfg.Retention))
	for _, p := range s.cfg.Retention {
		v := policyView{RetentionPolicy: p,
			HotHuman: p.Hot.String(), WarmHuman: p.Warm.String(), ColdHuman: p.Cold.String()}
		if store, ok := s.tables[p.Table]; ok {
			st, err := store.Stats()
			if err != nil {
				return err
			}
			v.Stats = st
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
	return nil
}

// handleSweep runs the retention sweep on demand, which is what an operator
// reclaiming disk at three in the morning needs and what the nightly job in a
// single-store deployment calls.
func (s *Service) handleSweep(w http.ResponseWriter, r *http.Request) error {
	if _, err := tenantOf(r); err != nil {
		return err
	}
	before := map[string]columnar.Stats{}
	for name, store := range s.tables {
		st, err := store.Stats()
		if err != nil {
			return err
		}
		before[name] = st
	}
	if err := s.SweepRetention(); err != nil {
		return err
	}
	after := map[string]columnar.Stats{}
	for name, store := range s.tables {
		st, err := store.Stats()
		if err != nil {
			return err
		}
		after[name] = st
	}
	writeJSON(w, http.StatusOK, map[string]any{"before": before, "after": after})
	return nil
}

// decodeEnvelope is here beside the rest of the encoding so the wire format is
// decided in one file.
func decodeEnvelope(b []byte, env *canon.Envelope) error { return json.Unmarshal(b, env) }
