// Package gateway is the UIG's HTTP surface: the endpoints a POS posts to and
// the endpoints an operator uses to see what happened.
//
// The two are deliberately different in almost every respect. The ingest
// endpoints are reachable from the public internet, authenticated per binding
// by the adapter, rate limited, and answer in a shape the calling POS
// understands — JSON for a webhook, a SOAP envelope for Oracle RIB. The
// operator endpoints are authenticated by a platform credential, are not
// tenant-facing, and expose things a POS must never see: which bindings exist,
// what is quarantined, and the ability to replay a delivery past the
// idempotency guard.
//
// The router is net/http's own ServeMux with method-and-path patterns. There is
// no framework here because there is nothing a framework would add to five
// routes, and because the request-handling path is inside a 50ms budget where
// every layer of middleware is measurable.
package gateway

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/adapters/oracle"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// DefaultMaxBodyBytes bounds an inbound delivery.
//
// Eight megabytes comfortably holds a 6,000-segment IDoc or a large catalogue
// webhook and is small enough that a hostile caller cannot make the gateway
// allocate its way out of memory. The limit is enforced before any parsing,
// because the adapters need the raw bytes in memory to verify a signature and
// there is no streaming verification of an HMAC over a body you have not read.
const DefaultMaxBodyBytes int64 = 8 << 20

// DefaultReadTimeout bounds how long the gateway waits for a slow client.
const DefaultReadTimeout = 15 * time.Second

// Config assembles a gateway.
type Config struct {
	// Pipeline processes deliveries.
	Pipeline *pipeline.Pipeline
	// OperatorToken authenticates the operator endpoints. A gateway with no
	// token refuses to serve them at all rather than serving them openly: an
	// unauthenticated replay endpoint would let anyone who can reach the pod
	// re-publish any stored price change.
	OperatorToken string
	// MaxBodyBytes bounds an inbound delivery; zero uses the default.
	MaxBodyBytes int64
	// Log is the request logger.
	Log *obs.Logger
	// Now injects a clock for tests.
	Now func() time.Time
}

// Gateway serves the UIG's HTTP endpoints.
type Gateway struct {
	cfg  Config
	mux  *http.ServeMux
	log  *obs.Logger
	now  func() time.Time
	maxB int64
}

// New builds a gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Pipeline == nil {
		return nil, errors.New("uig/gateway: nil pipeline")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	g := &Gateway{cfg: cfg, mux: http.NewServeMux(), log: cfg.Log, now: cfg.Now, maxB: cfg.MaxBodyBytes}
	if g.maxB <= 0 {
		g.maxB = DefaultMaxBodyBytes
	}
	g.routes()
	return g, nil
}

func (g *Gateway) routes() {
	g.mux.HandleFunc("POST /v1/ingest/{tenant}/{binding}", g.handleIngest)
	// The SOAP route is registered before the generic one only in intent; Go's
	// ServeMux picks the more specific pattern regardless of registration
	// order, which is what makes the trailing /soap segment safe to add.
	g.mux.HandleFunc("POST /v1/ingest/{tenant}/{binding}/soap", g.handleSOAP)
	g.mux.HandleFunc("GET /v1/bindings/{tenant}", g.operator(g.handleBindings))
	g.mux.HandleFunc("POST /v1/replay/{tenant}/{delivery_id}", g.operator(g.handleReplay))
	g.mux.HandleFunc("GET /v1/deliveries/{tenant}", g.operator(g.handleDeliveries))
}

// Handler returns the router.
func (g *Gateway) Handler() http.Handler { return g.mux }

// ---------------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------------

// ingestResponse is what a POS is answered with.
type ingestResponse struct {
	DeliveryID    string                  `json:"delivery_id"`
	Status        string                  `json:"status"`
	Emitted       int                     `json:"changes_accepted"`
	Duplicate     bool                    `json:"duplicate,omitempty"`
	Reason        string                  `json:"reason,omitempty"`
	Detail        string                  `json:"detail,omitempty"`
	RowFailures   []deliveries.RowFailure `json:"row_failures,omitempty"`
	CorrelationID string                  `json:"correlation_id,omitempty"`
	DurationMS    int64                   `json:"duration_ms"`
}

func (g *Gateway) handleIngest(w http.ResponseWriter, r *http.Request) {
	d, err := g.delivery(r)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, ingestResponse{
			Status: string(deliveries.StatusRejected),
			Reason: "body_too_large",
			Detail: err.Error(),
		})
		return
	}
	res := g.cfg.Pipeline.Ingest(r.Context(), d)
	g.setCommonHeaders(w, res)
	writeJSON(w, res.HTTPStatus, ingestResponse{
		DeliveryID:    res.DeliveryID,
		Status:        string(res.Status),
		Emitted:       res.Emitted,
		Duplicate:     res.Duplicate,
		Reason:        res.Reason,
		Detail:        res.Detail,
		RowFailures:   res.RowFailures,
		CorrelationID: string(res.CorrelationID),
		DurationMS:    res.DurationMS,
	})
}

// handleSOAP is the Oracle Retail endpoint.
//
// It differs from the JSON endpoint in exactly one way that matters: the
// response is a SOAP envelope, and a rejection is a SOAP Fault carrying
// faultcode soapenv:Client on a 4xx rather than the 500 the SOAP 1.1
// specification nominally requires. See the oracle package for why: RIB's error
// hospital retries a 5xx forever, and a message that cannot be parsed retried
// forever blocks every price behind it.
func (g *Gateway) handleSOAP(w http.ResponseWriter, r *http.Request) {
	d, err := g.delivery(r)
	if err != nil {
		writeSOAP(w, http.StatusRequestEntityTooLarge,
			oracle.Fault(oracle.FaultClient, "body_too_large", err.Error(), ""))
		return
	}
	res := g.cfg.Pipeline.Ingest(r.Context(), d)
	g.setCommonHeaders(w, res)
	if res.HTTPStatus >= 400 {
		code, status := oracle.FaultFor(res.HTTPStatus)
		writeSOAP(w, status, oracle.Fault(code, res.Reason, res.Detail, res.DeliveryID))
		return
	}
	writeSOAP(w, http.StatusOK, oracle.Response(
		strings.ToUpper(string(res.Status)), res.DeliveryID, string(res.CorrelationID),
		res.Emitted, res.Duplicate))
}

// delivery builds a Delivery from an HTTP request, reading the body under a
// hard limit.
func (g *Gateway) delivery(r *http.Request) (*adapter.Delivery, error) {
	tenant := canon.TenantID(r.PathValue("tenant"))
	binding := r.PathValue("binding")

	limited := http.MaxBytesReader(nil, r.Body, g.maxB)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("request body exceeds the %d byte limit", g.maxB)
	}
	d := &adapter.Delivery{
		ID:          canon.NewULID(),
		TenantID:    tenant,
		BindingID:   binding,
		Method:      r.Method,
		URL:         absoluteURL(r),
		Path:        r.URL.Path,
		Headers:     r.Header.Clone(),
		Query:       r.URL.Query(),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
		ReceivedAt:  g.now(),
		RemoteAddr:  r.RemoteAddr,
		// The peer identity comes from the completed TLS handshake, never from
		// a header. A proxy-supplied common name is a claim; this is a
		// credential.
		PeerIdentity: peerIdentity(r.TLS),
	}
	return d, nil
}

func peerIdentity(state *tls.ConnectionState) string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// absoluteURL reconstructs the URL the caller used. Square signs it, so it has
// to be reconstructed carefully — though the square adapter deliberately checks
// the *configured* notification URL rather than this one, precisely because a
// proxy can make this wrong.
func absoluteURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

func (g *Gateway) setCommonHeaders(w http.ResponseWriter, res *pipeline.Result) {
	if res.CorrelationID != "" {
		w.Header().Set("X-Correlation-Id", string(res.CorrelationID))
	}
	w.Header().Set("X-Usslp-Delivery-Id", res.DeliveryID)
	if res.RetryAfter > 0 {
		// A 429 without a Retry-After makes a POS guess, and POS systems guess
		// by retrying immediately.
		secs := int(res.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
}

// ---------------------------------------------------------------------------
// Operator endpoints
// ---------------------------------------------------------------------------

// operator wraps a handler with the platform-credential check.
func (g *Gateway) operator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.OperatorToken == "" {
			// Refusing outright rather than serving openly: an operator API
			// with no credential configured is a misconfiguration, and the safe
			// interpretation of a missing credential is "closed".
			writeJSON(w, http.StatusServiceUnavailable, errorBody{
				Reason: "operator_api_disabled",
				Detail: "no operator token is configured on this gateway",
			})
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(g.cfg.OperatorToken), []byte(strings.TrimSpace(provided))) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorBody{
				Reason: "unauthorized",
				Detail: "a valid operator bearer token is required",
			})
			return
		}
		next(w, r)
	}
}

type errorBody struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// bindingView joins a binding's configuration with its live health.
type bindingView struct {
	*adapter.Binding
	Health pipeline.BindingHealth `json:"health"`
}

func (g *Gateway) handleBindings(w http.ResponseWriter, r *http.Request) {
	tenant := canon.TenantID(r.PathValue("tenant"))
	list := g.cfg.Pipeline.Bindings().List(tenant)
	out := make([]bindingView, 0, len(list))
	for _, b := range list {
		out = append(out, bindingView{
			Binding: b,
			Health:  g.cfg.Pipeline.Health().Get(tenant, b.ID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenant,
		"bindings":  out,
		"breakers":  breakerView(g.cfg.Pipeline),
	})
}

func breakerView(p *pipeline.Pipeline) map[string]string {
	snap := p.Breakers().Snapshot()
	out := make(map[string]string, len(snap))
	for name, st := range snap {
		out[name] = st.String()
	}
	return out
}

func (g *Gateway) handleReplay(w http.ResponseWriter, r *http.Request) {
	tenant := canon.TenantID(r.PathValue("tenant"))
	id := r.PathValue("delivery_id")
	res, err := g.cfg.Pipeline.Replay(r.Context(), tenant, id)
	switch {
	case errors.Is(err, deliveries.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{
			Reason: "not_found", Detail: "no stored delivery with that id",
		})
		return
	case errors.Is(err, pipeline.ErrNotReplayable):
		// 409 rather than 404: the delivery exists, it just cannot be replayed,
		// and telling an operator the difference saves them looking for a typo
		// in an id that was right.
		writeJSON(w, http.StatusConflict, errorBody{Reason: "not_replayable", Detail: err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, errorBody{Reason: "replay_failed", Detail: err.Error()})
		return
	}
	g.setCommonHeaders(w, res)
	writeJSON(w, http.StatusOK, map[string]any{
		"replayed":          id,
		"delivery_id":       res.DeliveryID,
		"status":            res.Status,
		"changes_accepted":  res.Emitted,
		"reason":            res.Reason,
		"detail":            res.Detail,
		"row_failures":      res.RowFailures,
		"upstream_response": res.HTTPStatus,
		"duration_ms":       res.DurationMS,
	})
}

func (g *Gateway) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	tenant := canon.TenantID(r.PathValue("tenant"))
	q := r.URL.Query()
	query := deliveries.Query{
		Status:        deliveries.Status(strings.TrimSpace(q.Get("status"))),
		BindingID:     strings.TrimSpace(q.Get("binding")),
		IncludeBodies: q.Get("include_bodies") == "true",
	}
	if query.Status != "" && !query.Status.Valid() {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Reason: "bad_status",
			Detail: "status must be one of accepted, partial, quarantined, rejected, ignored",
		})
		return
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 1000 {
			writeJSON(w, http.StatusBadRequest, errorBody{
				Reason: "bad_limit", Detail: "limit must be a positive integer up to 1000",
			})
			return
		}
		query.Limit = n
	}
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{
				Reason: "bad_since", Detail: "since must be an RFC3339 timestamp",
			})
			return
		}
		query.Since = t
	}
	list, err := g.cfg.Pipeline.ListDeliveries(tenant, query)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Reason: "store_unavailable", Detail: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":  tenant,
		"count":      len(list),
		"deliveries": list,
	})
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Answers to a POS must never be cached by an intermediary: a cached 202
	// for a webhook would make a retailer believe a price landed that never
	// did.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeSOAP(w http.ResponseWriter, status int, body []byte) {
	// text/xml rather than application/soap+xml: RIB is a SOAP 1.1 client and
	// several versions of it reject the SOAP 1.2 media type outright.
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
