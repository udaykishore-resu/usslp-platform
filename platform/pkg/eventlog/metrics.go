package eventlog

import (
	"strconv"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// metrics is the log's Prometheus surface. Every method is nil-safe so that the
// hot path is identical whether or not a registry was supplied — an unobserved
// dev log should not pay for branches the production one needs, and neither
// should it need a second code path.
type metrics struct {
	appended  *obs.CounterVec
	bytes     *obs.CounterVec
	dlq       *obs.CounterVec
	dlqFailed *obs.CounterVec
	retries   *obs.CounterVec
	lag       *obs.GaugeVec
	segments  *obs.GaugeVec
	handler   *obs.HistogramVec
}

// newMetrics registers the log's series. It returns nil when no registry was
// configured, which every call site treats as "do nothing".
func newMetrics(r *obs.Registry) *metrics {
	if r == nil {
		return nil
	}
	return &metrics{
		appended: r.Counter("eventlog_records_appended_total",
			"Records durably appended to the log.", "topic"),
		bytes: r.Counter("eventlog_bytes_appended_total",
			"Framed bytes durably appended to the log.", "topic"),
		dlq: r.Counter("eventlog_dead_lettered_total",
			"Records routed to the dead-letter stream after exhausting retries.", "topic", "group"),
		dlqFailed: r.Counter("eventlog_dead_letter_failures_total",
			"Records that could not be written to the dead-letter stream and were dropped.", "topic", "group"),
		retries: r.Counter("eventlog_handler_retries_total",
			"Handler invocations that failed and were retried.", "topic", "group"),
		lag: r.Gauge("eventlog_consumer_lag_records",
			"Records appended but not yet committed by a consumer group.", "group", "topic", "partition"),
		segments: r.Gauge("eventlog_segments",
			"Segment files currently backing a partition.", "topic", "partition"),
		handler: r.Histogram("eventlog_handler_duration_seconds",
			"Wall time spent inside a subscriber's handler.", obs.LatencyBuckets, "topic", "group"),
	}
}

func (m *metrics) appendedRecords(topic string, records, bytes int64) {
	if m == nil {
		return
	}
	m.appended.With(topic).Add(uint64(records))
	m.bytes.With(topic).Add(uint64(bytes))
}

func (m *metrics) observeHandler(topic, group string, seconds float64) {
	if m == nil {
		return
	}
	m.handler.With(topic, group).Observe(seconds)
}

func (m *metrics) retried(topic, group string) {
	if m == nil {
		return
	}
	m.retries.With(topic, group).Inc()
}

func (m *metrics) deadLettered(topic, group string, ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.dlq.With(topic, group).Inc()
		return
	}
	m.dlqFailed.With(topic, group).Inc()
}

func (m *metrics) setLag(group, topic string, partition int, lag int64) {
	if m == nil {
		return
	}
	m.lag.With(group, topic, strconv.Itoa(partition)).Set(float64(lag))
}

func (m *metrics) setSegments(topic string, partition, n int) {
	if m == nil {
		return
	}
	m.segments.With(topic, strconv.Itoa(partition)).Set(float64(n))
}
