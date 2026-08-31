package apigw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// ---------------------------------------------------------------------------
// Reverse proxy
//
// The gateway does not stream. Every request and every response is buffered
// within a hard byte limit before it is forwarded, and that is a deliberate
// trade rather than an oversight:
//
//   - the entire public API is JSON, and the largest legitimate body in it is
//     a 40,000-item price batch at a few megabytes;
//   - buffering the request is what makes a retry possible at all, because a
//     consumed io.Reader cannot be replayed;
//   - buffering the response is what makes the size limit enforceable, because
//     once a byte of the body has been written to the client there is no way
//     to withdraw a 200 and say 502 instead.
//
// The one thing that genuinely needs streaming — the live event feed — does
// not go through here; it is a hijacked connection served by stream.go.
// ---------------------------------------------------------------------------

// Byte limits. Eight megabytes in matches the batch limit label-service
// enforces on itself, so a body the gateway accepts is one the upstream will
// also accept; thirty-two megabytes out is well above any real response and
// low enough that a misbehaving upstream cannot exhaust the gateway's heap.
const (
	DefaultMaxRequestBytes  = 8 << 20
	DefaultMaxResponseBytes = 32 << 20
)

// DefaultUpstreamTimeout bounds one attempt at an upstream call. Five seconds
// is above the three-second end-to-end price SLO, so a call that hits it has
// already blown the budget it was serving and there is nothing to wait for.
const DefaultUpstreamTimeout = 5 * time.Second

// Upstream is one internal service the gateway fronts.
type Upstream struct {
	// Name is the routing key used in the route table and in metrics.
	Name string
	// BaseURL is the service's address. Only scheme, host and any path prefix
	// are used.
	BaseURL *url.URL
	// Timeout bounds a single attempt.
	Timeout time.Duration
	// MaxRequestBytes and MaxResponseBytes bound the bodies.
	MaxRequestBytes  int64
	MaxResponseBytes int64
	// Retry is the backoff schedule for idempotent requests.
	Retry retry.Policy
	// Client is the HTTP client. Sharing one per upstream keeps the connection
	// pool warm, which matters: TCP and TLS setup on every price update would
	// add a round trip to the hot path.
	Client *http.Client
	// Breaker guards this upstream.
	Breaker *Breaker
}

// UpstreamConfig is the declarative form used by the service's configuration.
type UpstreamConfig struct {
	Name             string
	Address          string
	Timeout          time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
	Retry            retry.Policy
	Breaker          BreakerConfig
	// Transport, when set, replaces the default. Tests point it at an
	// httptest server's transport; production leaves it nil and gets the tuned
	// pool below.
	Transport http.RoundTripper
}

// The upstream service names. They are constants because the route table and
// the configuration loader both name them, and a typo in one of the two would
// otherwise be a nil upstream discovered in production.
const (
	UpstreamLabel     = "label-service"
	UpstreamRegistry  = "device-registry"
	UpstreamOTA       = "ota-service"
	UpstreamPricing   = "pricing-service"
	UpstreamPromotion = "promotion-service"
	UpstreamAnalytics = "analytics-service"
	UpstreamUIG       = "uig"
)

// newUpstream builds a live upstream from configuration.
func newUpstream(cfg UpstreamConfig, onState func(name string, from, to BreakerState)) (*Upstream, error) {
	if cfg.Name == "" {
		return nil, errors.New("apigw: upstream has no name")
	}
	base, err := url.Parse(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("apigw: upstream %s address %q: %w", cfg.Name, cfg.Address, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("apigw: upstream %s address %q must be an absolute URL", cfg.Name, cfg.Address)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultUpstreamTimeout
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.Retry.MaxAttempts == 0 && cfg.Retry.Base == 0 {
		cfg.Retry = retry.Aggressive
	}
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			// Sized for a gateway replica fronting seven services: enough idle
			// connections that the hot path never pays for a handshake, and a
			// short idle timeout so a scaled-down upstream's sockets are not
			// held open across a deployment.
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: time.Second,
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
		}
	}
	name := cfg.Name
	bcfg := cfg.Breaker
	if onState != nil {
		bcfg.OnStateChange = func(from, to BreakerState) { onState(name, from, to) }
	}
	return &Upstream{
		Name: cfg.Name, BaseURL: base, Timeout: cfg.Timeout,
		MaxRequestBytes: cfg.MaxRequestBytes, MaxResponseBytes: cfg.MaxResponseBytes,
		Retry:   cfg.Retry,
		Breaker: NewBreaker(bcfg),
		// No client timeout: the per-attempt deadline is carried on the request
		// context so it covers exactly one attempt and is visible to the
		// upstream, whereas Client.Timeout spans all redirects and retries and
		// cannot be varied per route.
		Client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// An internal service that answers a gateway call with a
				// redirect is misconfigured, and following it would let an
				// upstream aim the gateway's credentials at an arbitrary host.
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// hopByHopHeaders are the RFC 7230 §6.1 connection-scoped headers. They
// describe the hop the gateway terminates and must not be forwarded onto the
// next one.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// idempotentMethods may be retried.
//
// POST and PATCH are absent, and that is the whole point: retrying a POST that
// timed out after the upstream accepted it is how one price change becomes two
// audit records, or one OTA job becomes two staged rollouts. The platform's
// idempotency machinery (canon.Envelope.IdempotencyKey, idem.Guard) makes a
// *client's* retry safe; it does not make a silent retry inside the gateway
// safe, because the gateway does not know whether the caller supplied a key.
var idempotentMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodPut:     true,
	http.MethodDelete:  true,
}

// Proxy forwards requests to upstream services.
type Proxy struct {
	upstreams map[string]*Upstream
	metrics   *Metrics
	log       *obs.Logger
	tracer    *obs.Tracer
}

// Upstream returns a configured upstream by name.
func (p *Proxy) Upstream(name string) (*Upstream, bool) {
	u, ok := p.upstreams[name]
	return u, ok
}

// proxyResult is a buffered upstream response.
type proxyResult struct {
	status  int
	header  http.Header
	body    []byte
	attempt int
}

// Do forwards one request and returns the buffered upstream response.
//
// It is exported through the gateway's composed handlers as well as the plain
// proxy middleware, because /v1/prices and /v1/stores/{id}/overview are built
// by calling upstreams through exactly the same timeout, retry, breaker and
// size-limit machinery as a pass-through route. A composed endpoint that
// bypassed the breaker would be the one call that keeps a dying upstream
// under load.
func (p *Proxy) Do(ctx context.Context, up *Upstream, route *Route, method, path, rawQuery string,
	header http.Header, body []byte, principal Principal, requestID string) (*proxyResult, error) {

	timeout := route.Timeout
	if timeout <= 0 {
		timeout = up.Timeout
	}
	policy := up.Retry
	if !idempotentMethods[method] {
		policy.MaxAttempts = 1
	}

	target := *up.BaseURL
	target.Path = singleJoiningSlash(up.BaseURL.Path, path)
	target.RawQuery = rawQuery

	var result *proxyResult
	attempts := 0
	err := retry.Do(ctx, policy, func(ctx context.Context, attempt int) error {
		attempts = attempt
		done, allowed := up.Breaker.Allow()
		if !allowed {
			p.metrics.UpstreamFailures.With(up.Name, "breaker_open").Inc()
			// Permanent: the breaker is not going to change its mind inside a
			// retry schedule measured in milliseconds, and burning the
			// remaining attempts against an open circuit is exactly the load
			// the breaker exists to remove.
			return retry.Stop(&apiError{
				status: http.StatusServiceUnavailable, code: "upstream_unavailable",
				err:     fmt.Errorf("%w: %s", ErrBreakerOpen, up.Name),
				headers: map[string]string{HeaderRetryAfter: "1"},
			})
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		res, callErr := p.attempt(attemptCtx, up, method, target.String(), header, body, principal, requestID)
		if callErr != nil {
			done(false)
			return callErr
		}
		// A 5xx from an upstream counts against the breaker exactly as a
		// transport error does. From the gateway's point of view "the service
		// answered 503" and "the service did not answer" are the same event,
		// and only counting the latter means a service that is up but failing
		// never trips anything.
		done(res.status < http.StatusInternalServerError)
		result = res
		if isRetryableStatus(res.status) {
			p.metrics.UpstreamFailures.With(up.Name, "status_"+strconv.Itoa(res.status)).Inc()
			return fmt.Errorf("upstream %s returned %d", up.Name, res.status)
		}
		return nil
	})

	if result != nil {
		result.attempt = attempts
		if attempts > 1 {
			p.metrics.UpstreamRetries.With(up.Name, route.Operation).Add(uint64(attempts - 1))
		}
		// A retry schedule that ended on a 5xx still has a real upstream
		// response in hand. Forwarding it is more useful than replacing it
		// with the gateway's own 502: the upstream's body says which of its
		// dependencies failed.
		return result, nil
	}
	if attempts > 1 {
		p.metrics.UpstreamRetries.With(up.Name, route.Operation).Add(uint64(attempts - 1))
	}
	return nil, p.classify(up, err)
}

// attempt performs a single upstream call and buffers the response.
func (p *Proxy) attempt(ctx context.Context, up *Upstream, method, target string,
	header http.Header, body []byte, principal Principal, requestID string) (*proxyResult, error) {

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	copyProxyHeaders(req.Header, header)
	stampPrincipal(req.Header, principal)
	req.Header.Set(HeaderRequestID, requestID)
	if sc := obs.SpanContextFrom(ctx); sc.Valid() {
		req.Header.Set(HeaderTraceParent, sc.TraceParent())
	}

	start := time.Now()
	res, err := up.Client.Do(req)
	if err != nil {
		p.metrics.UpstreamDuration.With(up.Name, "error").Observe(time.Since(start).Seconds())
		return nil, err
	}
	defer res.Body.Close()

	// Read one byte past the limit: if it arrives, the body is over the limit,
	// and we know that without having read the rest of it.
	buf, err := io.ReadAll(io.LimitReader(res.Body, up.MaxResponseBytes+1))
	p.metrics.UpstreamDuration.With(up.Name, strconv.Itoa(res.StatusCode)).Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, fmt.Errorf("reading the %s response: %w", up.Name, err)
	}
	if int64(len(buf)) > up.MaxResponseBytes {
		p.metrics.UpstreamFailures.With(up.Name, "response_too_large").Inc()
		return nil, retry.Stop(&apiError{
			status: http.StatusBadGateway, code: "upstream_response_too_large",
			err: fmt.Errorf("%s returned more than %d bytes", up.Name, up.MaxResponseBytes),
		})
	}
	out := &proxyResult{status: res.StatusCode, header: make(http.Header, len(res.Header)), body: buf}
	copyProxyHeaders(out.header, res.Header)
	return out, nil
}

// classify turns a transport-level failure into the status the client sees.
func (p *Proxy) classify(up *Upstream, err error) error {
	if err == nil {
		return nil
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		p.metrics.UpstreamFailures.With(up.Name, "timeout").Inc()
		// 504, not 502: the difference tells an operator whether the upstream
		// is slow or absent, and those have different runbooks.
		return errUpstream(http.StatusGatewayTimeout, "upstream_timeout",
			"%s did not respond within its deadline", up.Name)
	case errors.Is(err, context.Canceled):
		// The client hung up. There is nobody to answer, but the status is
		// recorded so that a spike of them is visible as client behaviour
		// rather than as a gateway fault.
		return errUpstream(499, "client_closed_request", "the client closed the connection")
	default:
		p.metrics.UpstreamFailures.With(up.Name, "transport").Inc()
		p.log.Error("upstream call failed", "upstream", up.Name, "error", err.Error())
		return errUpstream(http.StatusBadGateway, "upstream_unreachable",
			"%s could not be reached", up.Name)
	}
}

// isRetryableStatus reports whether a status is worth another attempt.
//
// 502, 503 and 504 mean the request may never have been processed. 500 is not
// retried: it means the upstream ran the request and blew up inside it, and
// running it again produces the same crash plus a second partial side effect.
func isRetryableStatus(status int) bool {
	return status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// copyProxyHeaders copies headers, dropping the hop-by-hop set and anything
// the Connection header nominates as connection-scoped.
func copyProxyHeaders(dst, src http.Header) {
	nominated := map[string]bool{}
	for _, v := range src.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if token = strings.TrimSpace(token); token != "" {
				nominated[http.CanonicalHeaderKey(token)] = true
			}
		}
	}
	for k, vs := range src {
		if nominated[k] {
			continue
		}
		skip := false
		for _, h := range hopByHopHeaders {
			if strings.EqualFold(k, h) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// stampPrincipal writes the authenticated identity onto an outbound request.
//
// This is the other half of the tenant boundary: the inbound copies of these
// headers were deleted at the door, and this is the only code in the platform
// that writes them.
func stampPrincipal(h http.Header, p Principal) {
	h.Set(HeaderTenant, string(p.TenantID))
	if p.Subject != "" {
		h.Set(HeaderSubject, p.Subject)
	}
	if len(p.Roles) > 0 {
		parts := make([]string, len(p.Roles))
		for i, r := range p.Roles {
			parts[i] = string(r)
		}
		h.Set(HeaderRoles, strings.Join(parts, ","))
	}
	if len(p.Stores) > 0 {
		parts := make([]string, len(p.Stores))
		for i, s := range p.Stores {
			parts[i] = string(s)
		}
		h.Set(HeaderStores, strings.Join(parts, ","))
	}
	// The gateway terminates the client connection, so it is the only hop that
	// can honestly state the credential type the caller used.
	h.Set("X-USSLP-Auth", string(p.Kind))
}

func singleJoiningSlash(base, path string) string {
	switch {
	case base == "" || base == "/":
		return path
	case strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/"):
		return base + path[1:]
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/"):
		return base + "/" + path
	default:
		return base + path
	}
}

// readLimitedBody buffers a request body, refusing one that is over the limit.
func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	// Content-Length is checked first as a courtesy so that a caller uploading
	// a gigabyte learns it is too large before uploading it, but it is not
	// trusted: a chunked body has none, and a lying one is caught below.
	if r.ContentLength > limit {
		return nil, errTooLarge("request body is %d bytes, the limit is %d", r.ContentLength, limit)
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, errBadRequest("reading the request body: %v", err)
	}
	if int64(len(buf)) > limit {
		return nil, errTooLarge("request body exceeds the %d byte limit", limit)
	}
	return buf, nil
}

// proxyHandler is the pass-through handler installed by the route table.
func (g *Gateway) proxyHandler(route *Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		up, ok := g.proxy.Upstream(route.Upstream)
		if !ok {
			writeError(w, r, errInternal("route %s names unconfigured upstream %q", route.Pattern, route.Upstream))
			return
		}
		p, err := principalOf(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		body, err := readLimitedBody(r, up.MaxRequestBytes)
		if err != nil {
			writeError(w, r, err)
			return
		}
		path, err := route.upstreamPath(r, p)
		if err != nil {
			writeError(w, r, err)
			return
		}
		header := forwardableHeader(r)

		res, err := g.proxy.Do(r.Context(), up, route, r.Method, path, r.URL.RawQuery,
			header, body, p, RequestIDFrom(r.Context()))
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeProxyResult(w, route, up, res)
	}
}

// forwardableHeader builds the header set sent upstream from the client's.
func forwardableHeader(r *http.Request) http.Header {
	h := make(http.Header, len(r.Header))
	copyProxyHeaders(h, r.Header)
	// The credential stops here. An upstream that received the caller's API
	// key could replay it, and an upstream that logged it would put a working
	// credential in a log index.
	h.Del("Authorization")
	h.Del("Cookie")
	h.Set(HeaderForwardedFor, appendForwardedFor(r))
	h.Set("X-Forwarded-Host", r.Host)
	h.Set("X-Forwarded-Proto", schemeOf(r))
	return h
}

func appendForwardedFor(r *http.Request) string {
	ip := clientIP(r)
	if prior := r.Header.Get(HeaderForwardedFor); prior != "" {
		return prior + ", " + ip
	}
	return ip
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// writeProxyResult relays a buffered upstream response to the client.
func writeProxyResult(w http.ResponseWriter, route *Route, up *Upstream, res *proxyResult) {
	dst := w.Header()
	for k, vs := range res.header {
		// Never let an upstream set the headers the gateway owns: a service
		// echoing back a rate-limit header, or its own request id, would make
		// the gateway's contract with its clients depend on an internal
		// implementation detail.
		if isGatewayOwnedHeader(k) {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
	dst.Set(HeaderUpstream, up.Name)
	if _, ok := dst["Cache-Control"]; !ok {
		dst.Set("Cache-Control", "no-store")
	}
	dst.Del("Content-Length")
	dst.Set("Content-Length", strconv.Itoa(len(res.body)))
	w.WriteHeader(res.status)
	if route.Method != http.MethodHead {
		_, _ = w.Write(res.body)
	}
}

func isGatewayOwnedHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case http.CanonicalHeaderKey(HeaderRequestID),
		http.CanonicalHeaderKey(HeaderRateLimit),
		http.CanonicalHeaderKey(HeaderRateRemaining),
		http.CanonicalHeaderKey(HeaderRateReset),
		http.CanonicalHeaderKey(HeaderRateBucket),
		http.CanonicalHeaderKey(HeaderUpstream),
		http.CanonicalHeaderKey(HeaderTraceParent):
		return true
	}
	return false
}
