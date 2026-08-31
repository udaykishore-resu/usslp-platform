// Package app holds the Label Service's use cases: the command handlers,
// projections and fan-out pipeline that turn an accepted price change into
// attested, per-label display updates and prove they landed.
//
// Everything here depends only on `domain` and `ports`. That is what makes the
// price path testable end to end in milliseconds with in-memory fakes, and it
// is what lets the same handlers run against the embedded event log on a store
// gateway and against Kafka in the cloud without a line changing.
package app

import (
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Outcome labels for usslp_price_updates_total. They are a closed set so that a
// dashboard can compute "accepted / total" without knowing every reason.
const (
	// OutcomeApplied: the price reached the glass.
	OutcomeApplied = "applied"
	// OutcomeScheduled: the price was accepted for a future effective time.
	OutcomeScheduled = "scheduled"
	// OutcomeRejected: an invariant refused it. The reason label says which.
	OutcomeRejected = "rejected"
	// OutcomeStale: a duplicate or out-of-order record that changed nothing.
	OutcomeStale = "stale"
	// OutcomeRepublished: an already-applied update whose device publish was
	// retried. It is counted separately from applied so that a spike in MQTT
	// failures is visible without polluting the acceptance rate.
	OutcomeRepublished = "republished"
	// OutcomeError: an infrastructure failure. These are retried and may be
	// counted more than once for one logical update, which is why they are not
	// in the acceptance denominator on the dashboard.
	OutcomeError = "error"
)

// Metrics are the Label Service's series.
//
// They are registered once and passed down rather than looked up, because
// obs.Registry panics on a duplicate name: constructing them in a handler would
// turn a second handler instance — which the batch pipeline creates per worker
// in an earlier draft, and which any test creates freely — into a panic at
// wiring time rather than a compile error.
type Metrics struct {
	// E2ELatency is the SLO: seconds from USSLP taking durable responsibility
	// for a price change to the label's pixels settling. Labelled by tenant and
	// store because the SLO is reported per store — a retailer verifies it by
	// looking at one shelf, in one building.
	E2ELatency *obs.HistogramVec
	// PriceUpdates counts every decision by outcome and, for rejections, by
	// reason.
	PriceUpdates *obs.CounterVec
	// DeliveryConfirmations counts edge acknowledgements by store and outcome,
	// which is the numerator and denominator of the delivery success rate.
	DeliveryConfirmations *obs.CounterVec
	// PendingDelivery is the number of labels with an authorised update that
	// has not been confirmed. A rising floor here is the earliest sign of a
	// store falling off the mesh.
	PendingDelivery *obs.GaugeVec
	// AttestationFailures counts prices the platform could not sign. Any
	// non-zero value is a page: an unsigned price cannot be displayed, so this
	// is a full stop on the price path, not a degradation.
	AttestationFailures *obs.CounterVec
	// GuardrailRejections counts suspected data errors refused before they
	// reached a shelf. It is broken out from PriceUpdates because it is the one
	// rejection reason a merchandising team, not an engineer, needs to see.
	GuardrailRejections *obs.CounterVec
	// FanOutBatchSize is the number of labels one batch touched.
	FanOutBatchSize *obs.HistogramVec
	// FanOutDuration is how long a batch took end to end.
	FanOutDuration *obs.HistogramVec
	// DevicePublishDuration is the MQTT hop, which owns 150 ms of the budget.
	DevicePublishDuration *obs.HistogramVec
	// ScheduledActivations counts future-dated changes brought to the glass.
	ScheduledActivations *obs.CounterVec
	// DirectoryEntries is the size of the placement read model, per store.
	DirectoryEntries *obs.GaugeVec
	// PromotionFanouts counts promotion lifecycle transitions by event type and
	// outcome. The unresolvable count is the one to alert on: it means a
	// promotion went live in the Promotion Service and no shelf changed.
	PromotionFanouts *obs.CounterVec
	// PromotionLabels is how many labels one promotion transition touched.
	PromotionLabels *obs.HistogramVec
}

// FanOutBuckets size a store-wide promotion rather than a request. A batch of
// 40,000 labels is the design point, so the buckets run an order of magnitude
// past it; without the tail buckets a pipeline that has started to degrade
// looks identical to one that has not.
var FanOutBuckets = []float64{1, 10, 50, 100, 500, 1000, 5000, 10000, 25000, 50000, 100000}

// NewMetrics registers the Label Service's series on a registry. Passing nil
// returns a set backed by a private registry, which is what tests and the batch
// benchmark use.
func NewMetrics(r *obs.Registry) *Metrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	return &Metrics{
		E2ELatency: r.Histogram("usslp_price_update_e2e_seconds",
			"Seconds from durable acceptance of a price change to confirmed display",
			obs.LatencyBuckets, "tenant", "store"),
		PriceUpdates: r.Counter("usslp_price_updates_total",
			"Price change decisions by outcome", "outcome", "reason"),
		DeliveryConfirmations: r.Counter("usslp_label_delivery_confirmations_total",
			"Edge delivery acknowledgements", "store", "outcome"),
		PendingDelivery: r.Gauge("usslp_labels_pending_delivery",
			"Labels with an authorised update awaiting confirmation", "store"),
		AttestationFailures: r.Counter("usslp_attestation_failures_total",
			"Prices that could not be signed by the price authority", "reason"),
		GuardrailRejections: r.Counter("usslp_price_guardrail_rejections_total",
			"Price changes refused as suspected data errors", "tenant"),
		FanOutBatchSize: r.Histogram("usslp_price_fanout_batch_size",
			"Labels touched by one batch price update", FanOutBuckets, "tenant"),
		FanOutDuration: r.Histogram("usslp_price_fanout_duration_seconds",
			"Wall time of one batch price update", obs.LatencyBuckets, "tenant"),
		DevicePublishDuration: r.Histogram("usslp_device_publish_duration_seconds",
			"MQTT publish latency for one label update", obs.LatencyBuckets, "store"),
		ScheduledActivations: r.Counter("usslp_scheduled_price_activations_total",
			"Future-dated price changes brought to the glass", "outcome"),
		DirectoryEntries: r.Gauge("usslp_label_directory_entries",
			"Label placements held in the directory read model", "store"),
		PromotionFanouts: r.Counter("usslp_promotion_fanouts_total",
			"Promotion lifecycle transitions applied to shelves", "event_type", "outcome"),
		PromotionLabels: r.Histogram("usslp_promotion_labels",
			"Labels touched by one promotion transition", FanOutBuckets, "event_type"),
	}
}
