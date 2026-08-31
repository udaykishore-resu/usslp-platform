package apigw

import "github.com/usslp/usslp/platform/pkg/obs"

// Metrics are the gateway's series.
//
// The naming follows the platform convention (usslp_gateway_*) so that a
// single Grafana dashboard and a single set of alert rules cover it alongside
// every other USSLP service. Cardinality is controlled deliberately: series
// are labelled by *route operation*, never by path, because a label id in a
// metric label is an unbounded series count and a tenant identifier in one is
// a slow leak of the customer list into a shared monitoring system.
type Metrics struct {
	// Requests counts inbound requests by route, method, status and outcome.
	Requests *obs.CounterVec
	// Duration is inbound request latency per route.
	Duration *obs.HistogramVec
	// ResponseBytes sizes responses per route; it is what tells an operator
	// that a roster query has quietly become a megabyte.
	ResponseBytes *obs.HistogramVec
	// Auth counts authentication attempts by credential kind and outcome. A
	// rise in rejected API keys is either a rotation someone forgot or an
	// attacker working through a leaked list.
	Auth *obs.CounterVec
	// Denied counts authorisation refusals by route and reason, separating
	// "this role cannot do that" from "that store is not yours".
	Denied *obs.CounterVec
	// RateLimited counts throttled requests by bucket and route.
	RateLimited *obs.CounterVec
	// UpstreamDuration is per-upstream call latency by response status.
	UpstreamDuration *obs.HistogramVec
	// UpstreamFailures counts upstream call failures by reason.
	UpstreamFailures *obs.CounterVec
	// UpstreamRetries counts retried attempts.
	UpstreamRetries *obs.CounterVec
	// BreakerState is one gauge per (upstream, state) carrying 0 or 1.
	BreakerState *obs.GaugeVec
	// StreamConnections is the number of live WebSocket connections.
	StreamConnections *obs.GaugeVec
	// StreamEvents counts events fanned out to subscribers by outcome:
	// delivered, filtered, or dropped because a consumer could not keep up.
	StreamEvents *obs.CounterVec
	// StreamEvictions counts connections closed for falling behind. This is
	// the number that says a console tab has been left open on a laptop lid.
	StreamEvictions *obs.CounterVec
	// Panics counts recovered handler panics.
	Panics *obs.CounterVec
}

// NewMetrics registers the gateway's series on a registry.
//
// obs.Registry panics on duplicate registration, which means one Metrics per
// registry and therefore one gateway per registry. That is the correct
// constraint — two gateways in one process reporting into one series would
// produce numbers that are the sum of two unrelated populations — and the test
// harness gives every gateway its own registry.
func NewMetrics(r *obs.Registry) *Metrics {
	return &Metrics{
		Requests: r.Counter("usslp_gateway_requests_total",
			"Requests handled by the gateway", "operation", "method", "status", "outcome"),
		Duration: r.Histogram("usslp_gateway_request_duration_seconds",
			"Gateway request latency", obs.LatencyBuckets, "operation"),
		ResponseBytes: r.Histogram("usslp_gateway_response_bytes",
			"Response size", []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 8388608}, "operation"),
		Auth: r.Counter("usslp_gateway_auth_total",
			"Authentication attempts", "kind", "outcome"),
		Denied: r.Counter("usslp_gateway_authorization_denied_total",
			"Authorisation refusals", "operation", "reason"),
		RateLimited: r.Counter("usslp_gateway_rate_limited_total",
			"Requests refused by a rate limiter", "bucket", "operation"),
		UpstreamDuration: r.Histogram("usslp_gateway_upstream_duration_seconds",
			"Upstream call latency", obs.LatencyBuckets, "upstream", "status"),
		UpstreamFailures: r.Counter("usslp_gateway_upstream_failures_total",
			"Failed upstream calls", "upstream", "reason"),
		UpstreamRetries: r.Counter("usslp_gateway_upstream_retries_total",
			"Retried upstream attempts", "upstream", "operation"),
		BreakerState: r.Gauge("usslp_gateway_breaker_state",
			"Circuit breaker state, one series per state carrying 0 or 1", "upstream", "state"),
		StreamConnections: r.Gauge("usslp_gateway_stream_connections",
			"Live WebSocket connections", "tenant_present"),
		StreamEvents: r.Counter("usslp_gateway_stream_events_total",
			"Events considered for fan-out", "outcome"),
		StreamEvictions: r.Counter("usslp_gateway_stream_evictions_total",
			"WebSocket connections closed by the gateway", "reason"),
		Panics: r.Counter("usslp_gateway_panics_total",
			"Recovered handler panics", "operation"),
	}
}

// publishBreakerState writes the one-hot gauge set for an upstream.
func (m *Metrics) publishBreakerState(upstream string, state BreakerState) {
	for _, s := range []BreakerState{BreakerClosed, BreakerOpen, BreakerHalfOpen} {
		m.BreakerState.With(upstream, string(s)).Set(breakerStateValue(state, s))
	}
}
