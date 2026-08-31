package pipeline

import (
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// LatencyBudget is the UIG's slice of the platform's 3-second price path, as
// fixed in docs/architecture/INTERFACE-CONTRACTS.md §4: validate, dedupe,
// normalise and publish in 50ms.
//
// It is a constant in the code and not only a line in a document because a
// budget nobody measures is a wish. Every delivery is timed against it and
// every overrun increments a counter, so the alert fires when the gateway
// starts eating another hop's headroom rather than when a shelf is late.
const LatencyBudget = 50 * time.Millisecond

// IngestBuckets are latency histogram buckets concentrated below the budget.
// The platform-wide obs.LatencyBuckets jump 0.025 → 0.05 → 0.1, which cannot
// distinguish a gateway at half its budget from one at its limit — precisely
// the distinction that matters here.
var IngestBuckets = []float64{
	0.0005, 0.001, 0.002, 0.005, 0.0075, 0.01, 0.015, 0.02, 0.03, 0.04,
	0.05, 0.075, 0.1, 0.25, 0.5, 1, 2.5,
}

// Metrics are the UIG's series. Names are fixed contracts: dashboards and alert
// rules are written against them and outlive any given release.
type Metrics struct {
	// IngestTotal counts every delivery by adapter, tenant and outcome. The
	// tenant label is deliberate despite the cardinality cost: "is this one
	// retailer's integration broken or all of them" is the first question of
	// every UIG incident, and answering it from logs takes minutes that the
	// price path does not have.
	IngestTotal *obs.CounterVec
	// IngestDuration is the per-adapter latency against the 50ms budget.
	IngestDuration *obs.HistogramVec
	// DedupeHits counts deliveries recognised as redeliveries. A sudden rise is
	// how a POS in a retry loop announces itself.
	DedupeHits *obs.CounterVec
	// Quarantined counts deliveries stored for support, by adapter and reason.
	Quarantined *obs.CounterVec
	// ChangesEmitted counts canonical price changes published.
	ChangesEmitted *obs.CounterVec
	// ParseErrors is the per-adapter breakdown of *why* parsing failed. This is
	// the series an integration engineer reads: "shopify/decimal_conversion is
	// up and nothing else is" localises a defect to one field.
	ParseErrors *obs.CounterVec
	// RowFailures counts individual unusable records inside otherwise usable
	// deliveries, which is what tells you a nightly file is degrading before it
	// fails outright.
	RowFailures *obs.CounterVec
	// VerifyFailures counts authentication failures by adapter and reason.
	VerifyFailures *obs.CounterVec
	// RateLimited counts throttled deliveries.
	RateLimited *obs.CounterVec
	// PublishFailures counts durability failures — the only class of failure
	// the UIG answers 5xx to.
	PublishFailures *obs.CounterVec
	// BudgetExceeded counts deliveries that took longer than LatencyBudget.
	BudgetExceeded *obs.CounterVec
	// Replays counts operator replays by outcome.
	Replays *obs.CounterVec
	// InFlight is the number of deliveries currently being processed, which is
	// what distinguishes "slow" from "saturated" during an incident.
	InFlight *obs.GaugeVec
	// BindingsConfigured is the installed binding count; a drop to zero means a
	// configuration load failed.
	BindingsConfigured *obs.GaugeVec
	// BreakerState exports 0 closed, 1 half-open, 2 open per outbound
	// dependency.
	BreakerState *obs.GaugeVec
}

// NewMetrics registers the UIG's series on a registry.
func NewMetrics(r *obs.Registry) *Metrics {
	return &Metrics{
		IngestTotal: r.Counter("usslp_uig_ingest_total",
			"POS deliveries ingested", "adapter", "tenant", "outcome"),
		IngestDuration: r.Histogram("usslp_uig_ingest_duration_seconds",
			"Time from delivery receipt to durable publish", IngestBuckets, "adapter"),
		DedupeHits: r.Counter("usslp_uig_dedupe_hits_total",
			"Deliveries suppressed by the idempotency guard"),
		Quarantined: r.Counter("usslp_uig_quarantined_total",
			"Deliveries quarantined with their raw body retained", "adapter", "reason"),
		ChangesEmitted: r.Counter("usslp_uig_changes_emitted_total",
			"Canonical price changes published to price-updates"),
		ParseErrors: r.Counter("usslp_uig_parse_errors_total",
			"Parse and mapping failures by adapter and cause", "adapter", "reason"),
		RowFailures: r.Counter("usslp_uig_row_failures_total",
			"Individual unusable records inside otherwise usable deliveries", "adapter", "reason"),
		VerifyFailures: r.Counter("usslp_uig_verify_failures_total",
			"Deliveries rejected by adapter authentication", "adapter", "reason"),
		RateLimited: r.Counter("usslp_uig_rate_limited_total",
			"Deliveries refused by the per-binding rate limiter", "adapter", "tenant"),
		PublishFailures: r.Counter("usslp_uig_publish_failures_total",
			"Deliveries that could not be durably published", "adapter"),
		BudgetExceeded: r.Counter("usslp_uig_latency_budget_exceeded_total",
			"Deliveries slower than the gateway's 50ms slice of the price path", "adapter"),
		Replays: r.Counter("usslp_uig_replays_total",
			"Operator replays of stored deliveries", "adapter", "outcome"),
		InFlight: r.Gauge("usslp_uig_in_flight_deliveries",
			"Deliveries currently being processed"),
		BindingsConfigured: r.Gauge("usslp_uig_bindings_configured",
			"Installed POS bindings"),
		BreakerState: r.Gauge("usslp_uig_circuit_breaker_state",
			"Outbound circuit breaker state: 0 closed, 1 half-open, 2 open", "dependency"),
	}
}

// ObserveBreakers publishes the current state of every outbound breaker. It is
// called from the metrics scrape path rather than on every transition so that a
// flapping dependency cannot turn into a hot write loop on the registry.
func (m *Metrics) ObserveBreakers(set *reliability.BreakerSet) {
	if m == nil || set == nil {
		return
	}
	for name, st := range set.Snapshot() {
		var v float64
		switch st {
		case reliability.StateHalfOpen:
			v = 1
		case reliability.StateOpen:
			v = 2
		}
		m.BreakerState.With(name).Set(v)
	}
}

// ---------------------------------------------------------------------------
// Per-binding health
// ---------------------------------------------------------------------------

// BindingHealth is the operational view of one integration, returned by
// GET /v1/bindings/{tenant}.
//
// "Configured" and "working" are different questions, and an operator staring
// at a shelf that has not changed price needs the second one. A binding that
// has accepted nothing for a day is either a nightly feed behaving normally or
// a webhook subscription the retailer deleted, and the counters here are what
// tell those apart.
type BindingHealth struct {
	TenantID  canon.TenantID `json:"tenant_id"`
	BindingID string         `json:"binding_id"`
	Adapter   string         `json:"adapter"`

	LastDeliveryAt time.Time `json:"last_delivery_at,omitempty"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
	LastFailureAt  time.Time `json:"last_failure_at,omitempty"`
	// LastFailureReason is the low-cardinality token from the most recent
	// failure, and LastFailureDetail its explanation.
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	LastFailureDetail string `json:"last_failure_detail,omitempty"`

	Deliveries  uint64 `json:"deliveries"`
	Accepted    uint64 `json:"accepted"`
	Duplicates  uint64 `json:"duplicates"`
	Quarantined uint64 `json:"quarantined"`
	Rejected    uint64 `json:"rejected"`
	Ignored     uint64 `json:"ignored"`
	Emitted     uint64 `json:"changes_emitted"`

	// Status is "ok", "degraded" or "failing", derived from whether the most
	// recent delivery succeeded.
	Status string `json:"status"`
}

// HealthTracker accumulates per-binding health.
type HealthTracker struct {
	mu sync.RWMutex
	by map[string]*BindingHealth
}

// NewHealthTracker creates an empty tracker.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{by: make(map[string]*BindingHealth)}
}

func healthKey(t canon.TenantID, binding string) string { return string(t) + "/" + binding }

// Record folds one delivery outcome into a binding's health.
func (h *HealthTracker) Record(r *Result, at time.Time) {
	if h == nil || r == nil {
		return
	}
	k := healthKey(r.TenantID, r.BindingID)
	h.mu.Lock()
	defer h.mu.Unlock()
	bh, ok := h.by[k]
	if !ok {
		bh = &BindingHealth{TenantID: r.TenantID, BindingID: r.BindingID, Adapter: r.Adapter}
		h.by[k] = bh
	}
	if bh.Adapter == "" {
		bh.Adapter = r.Adapter
	}
	bh.Deliveries++
	bh.LastDeliveryAt = at
	bh.Emitted += uint64(r.Emitted)
	switch {
	case r.Duplicate:
		bh.Duplicates++
		// A duplicate is neither a success nor a failure of the integration: it
		// is the producer behaving correctly. Leaving the status untouched
		// stops a POS retry storm from painting a healthy binding green or red.
	case r.Status == statusAccepted || r.Status == statusPartial:
		bh.Accepted++
		bh.LastSuccessAt = at
		bh.Status = "ok"
		if r.Status == statusPartial {
			bh.Status = "degraded"
		}
	case r.Status == statusIgnored:
		bh.Ignored++
		bh.LastSuccessAt = at
		bh.Status = "ok"
	case r.Status == statusQuarantined:
		bh.Quarantined++
		bh.LastFailureAt = at
		bh.LastFailureReason, bh.LastFailureDetail = r.Reason, r.Detail
		bh.Status = "failing"
	default:
		bh.Rejected++
		bh.LastFailureAt = at
		bh.LastFailureReason, bh.LastFailureDetail = r.Reason, r.Detail
		bh.Status = "failing"
	}
	if bh.Status == "" {
		bh.Status = "unknown"
	}
}

// Get returns a copy of one binding's health, or a zero-valued entry marked
// "idle" when nothing has ever been delivered to it — which is itself the
// answer to "why is nothing updating".
func (h *HealthTracker) Get(t canon.TenantID, binding string) BindingHealth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if bh, ok := h.by[healthKey(t, binding)]; ok {
		return *bh
	}
	return BindingHealth{TenantID: t, BindingID: binding, Status: "idle"}
}
