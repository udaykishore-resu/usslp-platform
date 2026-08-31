package apigw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// Gateway is the assembled front door.
type Gateway struct {
	service string
	version string

	log     *obs.Logger
	tracer  *obs.Tracer
	metrics *Metrics
	health  *obs.Health

	auth    *Authenticator
	keys    *KeyIssuer
	limiter *RateLimiter
	proxy   *Proxy

	hub       *Hub
	source    EventSource
	streamCfg StreamConfig
	// streamDone is closed on shutdown. Every WebSocket writer selects on it,
	// which is how hijacked connections — invisible to http.Server.Shutdown,
	// because hijacking removes them from its tracking — are drained at all.
	streamDone chan struct{}

	mux         *http.ServeMux
	methodProbe *http.ServeMux
	allowed     map[string][]string

	now func() time.Time

	wg       sync.WaitGroup
	stopOnce sync.Once
	// janitorStop and cancelBackground stop the goroutines Start launched.
	// cancelBackground is written once, by Start, before any goroutine that
	// reads it exists, and read once, by Shutdown.
	janitorStop      chan struct{}
	cancelBackground context.CancelFunc
}

// Config assembles a gateway.
type Config struct {
	// Service and Version identify this build in logs, metrics and /healthz.
	Service string
	Version string
	// Log, Tracer, Registry and Health come from obs.Runtime in production and
	// are constructed per-test in the suite.
	Log      *obs.Logger
	Tracer   *obs.Tracer
	Registry *obs.Registry
	Health   *obs.Health
	// Auth configures the three credential schemes.
	Auth AuthConfig
	// Keys, when set, enables /v1/keys. It is the same issuer AuthConfig.Keys
	// verifies against; passing one without the other would produce a gateway
	// that mints credentials it cannot check.
	Keys *KeyIssuer
	// RateLimit configures the three buckets.
	RateLimit RateLimitConfig
	// Upstreams are the internal services to front.
	Upstreams []UpstreamConfig
	// Stream tunes the WebSocket feed.
	Stream StreamConfig
	// Source feeds the live stream. Without one the endpoint still works and
	// simply never emits, which is the right behaviour for a deployment whose
	// event bus is not reachable yet.
	Source EventSource
	// Now supplies the clock.
	Now func() time.Time
}

// New builds a gateway.
//
// Everything that can be wrong with the route table, the upstream set or the
// OpenAPI document is checked here, at start-up, so that a misconfiguration is
// a pod that will not start rather than a route that 500s in production.
func New(cfg Config) (*Gateway, error) {
	if cfg.Registry == nil {
		return nil, errors.New("apigw: a metrics registry is required")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Tracer == nil {
		cfg.Tracer = obs.NewTracer("api-gateway", 1)
	}
	if cfg.Health == nil {
		cfg.Health = obs.NewHealth()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Service == "" {
		cfg.Service = "api-gateway"
	}
	if cfg.Version == "" {
		cfg.Version = obs.BuildVersion()
	}
	if cfg.RateLimit.Now == nil {
		cfg.RateLimit.Now = cfg.Now
	}
	if cfg.Auth.Now == nil {
		cfg.Auth.Now = cfg.Now
	}

	metrics := NewMetrics(cfg.Registry)
	g := &Gateway{
		service: cfg.Service, version: cfg.Version,
		log: cfg.Log, tracer: cfg.Tracer, metrics: metrics, health: cfg.Health,
		auth: NewAuthenticator(cfg.Auth), keys: cfg.Keys,
		limiter:     NewRateLimiter(cfg.RateLimit),
		hub:         NewHub(metrics, cfg.Log),
		source:      cfg.Source,
		streamCfg:   cfg.Stream.withDefaults(),
		streamDone:  make(chan struct{}),
		janitorStop: make(chan struct{}),
		now:         cfg.Now,
	}

	ups := make(map[string]*Upstream, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		up, err := newUpstream(uc, func(name string, from, to BreakerState) {
			metrics.publishBreakerState(name, to)
			cfg.Log.Warn("upstream circuit breaker changed state",
				"upstream", name, "from", string(from), "to", string(to))
		})
		if err != nil {
			return nil, err
		}
		if _, dup := ups[up.Name]; dup {
			return nil, fmt.Errorf("apigw: upstream %q is configured twice", up.Name)
		}
		ups[up.Name] = up
		metrics.publishBreakerState(up.Name, BreakerClosed)
		// Readiness, never liveness: an upstream outage must take this replica
		// out of the load balancer's rotation, not restart it. See §7 of the
		// interface contracts.
		cfg.Health.Register("upstream:"+up.Name, upstreamReadiness(up))
	}
	g.proxy = &Proxy{upstreams: ups, metrics: metrics, log: cfg.Log, tracer: cfg.Tracer}

	natives := g.nativeHandlers()
	if err := validateRoutes(routes, ups, natives); err != nil {
		return nil, err
	}
	if err := g.buildMux(natives); err != nil {
		return nil, err
	}
	return g, nil
}

// nativeHandlers maps the route table's Native names to handlers.
func (g *Gateway) nativeHandlers() map[string]http.Handler {
	return map[string]http.Handler{
		"health":        http.HandlerFunc(g.handleHealth),
		"ready":         http.HandlerFunc(g.handleReady),
		"openapi":       http.HandlerFunc(g.handleOpenAPI),
		"docs":          http.HandlerFunc(g.handleDocs),
		"console":       http.HandlerFunc(g.handleConsole),
		"me":            http.HandlerFunc(g.handleMe),
		"listKeys":      http.HandlerFunc(g.handleListKeys),
		"issueKey":      http.HandlerFunc(g.handleIssueKey),
		"revokeKey":     http.HandlerFunc(g.handleRevokeKey),
		"stream":        http.HandlerFunc(g.handleStream),
		"updatePrice":   http.HandlerFunc(g.handleUpdatePrice),
		"storeOverview": http.HandlerFunc(g.handleStoreOverview),
	}
}

// buildMux assembles the router.
//
// Every route gets the same chain, in the same order, built here and nowhere
// else:
//
//	observability → authenticate → authorize → rate limit → handler
//
// Observability is outermost so a rejected request is still logged and traced.
// Authentication precedes authorisation because there is nothing to authorise
// without a principal. Rate limiting is innermost of the three because it is
// keyed on the credential, and because an unauthenticated flood should be
// rejected by the cheapest check, not counted against a tenant that is not
// making it.
func (g *Gateway) buildMux(natives map[string]http.Handler) error {
	mux := http.NewServeMux()
	probe := http.NewServeMux()
	allowed := make(map[string][]string)

	for i := range routes {
		route := &routes[i]
		var h http.Handler
		if route.Native != "" {
			h = natives[route.Native]
		} else {
			h = g.proxyHandler(route)
		}
		h = g.rateLimit(route, h)
		h = g.authorize(route, h)
		h = g.authenticate(route, h)
		h = g.observability(route, h)
		mux.Handle(route.Key(), h)
		allowed[route.Pattern] = append(allowed[route.Pattern], route.Method)
	}
	for pattern, methods := range allowed {
		sort.Strings(methods)
		// The probe mux is method-less: it answers "does this path correspond
		// to a route at all", which is what separates a 404 from a 405 in the
		// fallback below.
		probe.Handle(pattern, http.NotFoundHandler())
	}

	fallback := &Route{Method: "*", Pattern: "/", Operation: "notFound", Public: true,
		Summary: "unmatched", Native: "notFound"}
	mux.Handle("/", g.observability(fallback, http.HandlerFunc(g.handleUnmatched)))

	g.mux, g.methodProbe, g.allowed = mux, probe, allowed
	return nil
}

// handleUnmatched answers a request that matched no route.
//
// It distinguishes "no such path" from "wrong method on a real path" because
// the two mean completely different things to whoever is debugging: one is a
// typo, the other is a client using an endpoint it half-remembers.
func (g *Gateway) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	if _, pattern := g.methodProbe.Handler(r); pattern != "" {
		if methods, ok := g.allowed[pattern]; ok {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			writeError(w, r, statusError(http.StatusMethodNotAllowed, "method_not_allowed",
				"%s is not allowed on %s; allowed: %s", r.Method, pattern, strings.Join(methods, ", ")))
			return
		}
	}
	writeError(w, r, errNotFound("no route matches %s %s", r.Method, r.URL.Path))
}

// Handler returns the gateway's HTTP handler.
func (g *Gateway) Handler() http.Handler { return g.mux }

// Hub exposes the event fan-out, so a caller can publish into the stream
// without an event bus — used by the console's test-price action path and by
// the test suite.
func (g *Gateway) Hub() *Hub { return g.hub }

// Metrics exposes the registered series, for tests and for the admin surface.
func (g *Gateway) Metrics() *Metrics { return g.metrics }

// Start launches the background work: the event-stream consumer and the
// rate-limiter janitor. It returns immediately.
//
// The background work runs under a context derived from ctx and cancelled by
// [Gateway.Shutdown], so a caller does not have to cancel its own context to
// let the gateway drain — an ordering that is easy to get backwards and
// produces a shutdown that always runs to its timeout.
func (g *Gateway) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	g.cancelBackground = cancel
	if g.source != nil {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			if err := g.source.Run(ctx, g.hub.Publish); err != nil && !errors.Is(err, context.Canceled) {
				g.log.Error("stream event source stopped", "error", err)
			}
		}()
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		// Every thirty seconds, drop rate-limit buckets that are full and
		// untouched for five minutes. Both numbers are deliberately far longer
		// than the time it takes a bucket to refill, so sweeping can never
		// forgive a client that is still being limited.
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				g.limiter.Sweep(5 * time.Minute)
			case <-g.janitorStop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Shutdown drains the gateway.
//
// The order matters and is the reverse of what looks natural. WebSockets are
// closed *first*, with a proper close frame, because http.Server.Shutdown does
// not know about them: hijacking removes a connection from the server's
// tracking, so a graceful HTTP shutdown would return while every stream was
// still open and then have them killed by process exit — which a client sees
// as a connection reset rather than a "going away", and reacts to with an
// error rather than a reconnect.
//
// The caller shuts the http.Server down after this returns, which drains the
// ordinary in-flight requests.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.stopOnce.Do(func() {
		close(g.streamDone)
		close(g.janitorStop)
		if g.cancelBackground != nil {
			g.cancelBackground()
		}
	})
	g.hub.Shutdown()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	// Poll rather than wait on a condition variable: the connections being
	// drained are served by goroutines this function does not own — they
	// belong to http.Server — so the only thing to synchronise on is the hub's
	// population going to zero.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if g.hub.Len() == 0 {
			select {
			case <-done:
				return nil
			default:
			}
		}
		select {
		case <-ctx.Done():
			g.log.Warn("gateway shutdown timed out with connections still open",
				"streams", g.hub.Len())
			return ctx.Err()
		case <-tick.C:
		}
	}
}
