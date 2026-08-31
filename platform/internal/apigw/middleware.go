package apigw

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Headers the gateway reads and writes.
const (
	// HeaderRequestID correlates a client's retry, the access log, the trace
	// and the support ticket. The gateway honours a caller-supplied value so a
	// customer's own tracing survives the hop, but validates it first: an
	// unbounded, unvalidated header ends up in a log index and in an error
	// body.
	HeaderRequestID = "X-Request-Id"
	// HeaderTraceParent is W3C trace context.
	HeaderTraceParent = "traceparent"
	// HeaderTraceState is the vendor-specific companion to traceparent. It is
	// forwarded untouched because it belongs to whoever started the trace.
	HeaderTraceState = "tracestate"
	// HeaderTenant is how internal services learn the tenant. The gateway is
	// the only writer of this header on any request an upstream sees.
	HeaderTenant = "X-USSLP-Tenant"
	// HeaderSubject, HeaderRoles and HeaderStores carry the rest of the
	// authenticated principal to upstreams that want to record who did
	// something. They are advisory: an upstream must never make an
	// authorisation decision from them, because the gateway has already made
	// it and a second, weaker copy of the same decision is how they diverge.
	HeaderSubject = "X-USSLP-Subject"
	HeaderRoles   = "X-USSLP-Roles"
	HeaderStores  = "X-USSLP-Store-Scope"
	// HeaderForwardedFor is the client chain.
	HeaderForwardedFor = "X-Forwarded-For"
	// HeaderRateLimit, HeaderRateRemaining and HeaderRateReset describe the
	// bucket that governed this request.
	HeaderRateLimit     = "X-RateLimit-Limit"
	HeaderRateRemaining = "X-RateLimit-Remaining"
	HeaderRateReset     = "X-RateLimit-Reset"
	// HeaderRateBucket names which of the three buckets is the binding
	// constraint, so a client being throttled can tell "my key is too busy"
	// from "my whole organisation is".
	HeaderRateBucket = "X-RateLimit-Bucket"
	// HeaderRetryAfter is the RFC 7231 back-off instruction.
	HeaderRetryAfter = "Retry-After"
	// HeaderUpstream names the service that answered, for support triage.
	HeaderUpstream = "X-USSLP-Upstream"
)

// maxRequestIDLength bounds a caller-supplied request id.
const maxRequestIDLength = 64

type requestIDKey struct{}

// WithRequestID attaches a request id to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type principalSlotKey struct{}

// withPrincipalSlot installs a mutable slot for the authenticated caller.
//
// The access log is written by the outermost middleware, which built its
// request before any principal existed; the authentication middleware, two
// layers in, derives one and hands it to the rest of the chain in a *new*
// request. Without a slot the log line would have to re-derive the caller, or
// simply not name it — and "which tenant made this request" is the field an
// operator filters on first. The slot is written and read on the same
// goroutine, once each.
func withPrincipalSlot(ctx context.Context) (context.Context, *Principal) {
	slot := &Principal{}
	return context.WithValue(ctx, principalSlotKey{}, slot), slot
}

func principalSlotFrom(ctx context.Context) *Principal {
	slot, _ := ctx.Value(principalSlotKey{}).(*Principal)
	return slot
}

type routeKey struct{}

// RouteFrom returns the matched route in ctx. Middleware that runs after
// routing uses it to find the route's permission, cost class and timeout
// without re-deriving them from the path.
func RouteFrom(ctx context.Context) (*Route, bool) {
	rt, ok := ctx.Value(routeKey{}).(*Route)
	return rt, ok
}

func withRoute(ctx context.Context, rt *Route) context.Context {
	return context.WithValue(ctx, routeKey{}, rt)
}

// responseRecorder captures the status and byte count for the access log and
// metrics, and preserves the interfaces the WebSocket upgrade needs.
type responseRecorder struct {
	http.ResponseWriter
	status   int
	written  int64
	hijacked bool
}

func (rr *responseRecorder) WriteHeader(status int) {
	if rr.status == 0 {
		rr.status = status
		rr.ResponseWriter.WriteHeader(status)
	}
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.written += int64(n)
	return n, err
}

// Hijack forwards to the underlying connection so the WebSocket handler can
// take the socket. Recording the fact matters: a hijacked response has no
// status code, and logging one as a 200 would put every stream connection in
// the same latency bucket as a fast JSON call.
func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("apigw: the underlying ResponseWriter cannot be hijacked")
	}
	c, buf, err := hj.Hijack()
	if err == nil {
		rr.hijacked = true
	}
	return c, buf, err
}

// Flush forwards to the underlying writer when it supports flushing.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observability is the outermost middleware: it establishes the request id and
// the trace, recovers panics, records metrics and writes the access log.
//
// It wraps everything, including authentication, because a request that fails
// to authenticate is exactly the request an operator most wants a log line
// for, and because a panic in the authentication path must not take the
// process down.
func (g *Gateway) observability(route *Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := g.now()

		requestID := sanitiseRequestID(r.Header.Get(HeaderRequestID))
		if requestID == "" {
			requestID = canon.NewULID()
		}
		ctx := WithRequestID(r.Context(), requestID)
		ctx = withRoute(ctx, route)
		ctx, principalSlot := withPrincipalSlot(ctx)
		// Continue the caller's trace when they sent one; an unparseable
		// traceparent starts a fresh trace rather than dropping the request.
		ctx = obs.WithRemoteContext(ctx, obs.ParseTraceParent(r.Header.Get(HeaderTraceParent)))
		ctx, span := g.tracer.Start(ctx, "gateway."+route.Operation)
		defer span.End()
		span.SetAttr("http.method", r.Method).
			SetAttr("http.route", route.Pattern).
			SetAttr("usslp.operation", route.Operation).
			SetAttr("request_id", requestID)

		w.Header().Set(HeaderRequestID, requestID)
		w.Header().Set(HeaderTraceParent, span.Ctx.TraceParent())

		rr := &responseRecorder{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				// A panic below here has already been converted to a 500 by
				// this handler; reaching this point means the handler itself
				// blew up. Log the stack once, answer once, and keep serving.
				g.log.Error("gateway handler panicked",
					"route", route.Pattern, "operation", route.Operation,
					"request_id", requestID, "panic", rec, "stack", string(debug.Stack()))
				g.metrics.Panics.With(route.Operation).Inc()
				if rr.status == 0 && !rr.hijacked {
					writeError(rr, r.WithContext(ctx), errInternal("handler panic"))
				}
				span.Fail(errors.New("handler panic"))
			}
			g.record(route, r, principalSlot, rr, span, start, requestID)
		}()

		next.ServeHTTP(rr, r.WithContext(ctx))
	})
}

// record writes the access log line and the route's metrics.
func (g *Gateway) record(route *Route, r *http.Request, principal *Principal, rr *responseRecorder,
	span *obs.Span, start time.Time, requestID string) {
	elapsed := g.now().Sub(start)
	status := rr.status
	if status == 0 {
		status = http.StatusOK
	}
	outcome := outcomeOf(status)
	tenant := "-"
	subject := "-"
	auth := "none"
	if principal != nil && principal.TenantID != "" {
		tenant, subject, auth = string(principal.TenantID), principal.Subject, string(principal.Kind)
	}

	span.SetAttrInt("http.status_code", int64(status)).SetAttr("usslp.tenant", tenant)
	if status >= http.StatusInternalServerError {
		span.Fail(errors.New(http.StatusText(status)))
	}

	// A hijacked connection is a WebSocket; it is metered by the stream
	// subsystem's own gauges and counters, and folding its multi-hour lifetime
	// into the request-latency histogram would ruin every percentile on the
	// dashboard.
	if !rr.hijacked {
		g.metrics.Requests.With(route.Operation, r.Method, strconv.Itoa(status), outcome).Inc()
		g.metrics.Duration.With(route.Operation).Observe(elapsed.Seconds())
		g.metrics.ResponseBytes.With(route.Operation).Observe(float64(rr.written))
	}

	level := g.log.Info
	switch {
	case status >= http.StatusInternalServerError:
		level = g.log.Error
	case status >= http.StatusBadRequest:
		level = g.log.Warn
	}
	level("gateway request",
		"request_id", requestID,
		"trace_id", span.Ctx.TraceID,
		"method", r.Method,
		"path", r.URL.Path,
		"route", route.Pattern,
		"operation", route.Operation,
		"status", status,
		"outcome", outcome,
		"latency_ms", elapsed.Milliseconds(),
		"bytes", rr.written,
		"tenant_id", tenant,
		"subject", subject,
		"auth", auth,
		"upstream", route.Upstream,
		"websocket", rr.hijacked,
		"remote", clientIP(r),
	)
}

func outcomeOf(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "server_error"
	case status == http.StatusTooManyRequests:
		return "throttled"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "denied"
	case status >= http.StatusBadRequest:
		return "client_error"
	default:
		return "ok"
	}
}

// sanitiseRequestID accepts a caller-supplied id only if it is short and made
// of characters that are safe in a log index, a header and an error body.
func sanitiseRequestID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxRequestIDLength {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return ""
		}
	}
	return v
}

// clientIP returns the peer address for the access log.
//
// It reports the direct peer, not the left-most X-Forwarded-For entry: the
// gateway sits behind a load balancer it controls, and trusting a header a
// client can set would put attacker-chosen text into the field an operator
// blocks traffic on.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
