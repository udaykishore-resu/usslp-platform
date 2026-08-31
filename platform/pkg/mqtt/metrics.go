package mqtt

import (
	"strconv"

	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Metric names. They carry the usslp_mqtt_ prefix so that an SGU's embedded
// broker and the client talking to the cloud are distinguishable in one scrape
// of one process, which is the normal case: the gateway runs both.
const (
	metricBrokerConnected = "usslp_mqtt_broker_connected_clients"
	metricBrokerSessions  = "usslp_mqtt_broker_sessions"
	metricBrokerConnects  = "usslp_mqtt_broker_connections_total"
	metricBrokerIn        = "usslp_mqtt_broker_messages_received_total"
	metricBrokerOut       = "usslp_mqtt_broker_messages_sent_total"
	metricBrokerRetained  = "usslp_mqtt_broker_retained_messages"
	metricBrokerInflight  = "usslp_mqtt_broker_inflight_messages"
	metricBrokerDropped   = "usslp_mqtt_broker_dropped_total"
	metricBrokerSubs      = "usslp_mqtt_broker_subscriptions"

	metricClientState     = "usslp_mqtt_client_connected"
	metricClientConnects  = "usslp_mqtt_client_connect_attempts_total"
	metricClientPublished = "usslp_mqtt_client_published_total"
	metricClientReceived  = "usslp_mqtt_client_received_total"
	metricClientInflight  = "usslp_mqtt_client_inflight_messages"
	metricClientTimeouts  = "usslp_mqtt_client_ack_timeouts_total"
)

// qosLabel renders a QoS for use as a metric label value.
func qosLabel(q msgbus.QoS) string { return strconv.Itoa(int(q)) }

// brokerMetrics is the broker's instrumentation. Every field may be nil, which
// is what a broker built without a registry gets; the record* helpers are the
// only callers and they check once, so the hot path costs a nil compare rather
// than an interface call.
//
// A registry may back only one broker: obs.Registry rejects a duplicate metric
// name by panicking, which is the right behaviour for a process that would
// otherwise export two conflicting series under one name.
type brokerMetrics struct {
	connected *obs.Gauge
	sessions  *obs.Gauge
	retained  *obs.Gauge
	inflight  *obs.Gauge
	subs      *obs.Gauge
	connects  *obs.CounterVec
	in        *obs.CounterVec
	out       *obs.CounterVec
	dropped   *obs.CounterVec
}

func newBrokerMetrics(r *obs.Registry) *brokerMetrics {
	if r == nil {
		return nil
	}
	return &brokerMetrics{
		connected: r.Gauge(metricBrokerConnected, "Clients with an open MQTT connection").With(),
		sessions:  r.Gauge(metricBrokerSessions, "Sessions held, including offline CleanSession=false sessions").With(),
		retained:  r.Gauge(metricBrokerRetained, "Topics holding a retained message").With(),
		inflight:  r.Gauge(metricBrokerInflight, "QoS 1 and 2 messages awaiting acknowledgement from subscribers").With(),
		subs:      r.Gauge(metricBrokerSubs, "Active subscriptions across all sessions").With(),
		connects:  r.Counter(metricBrokerConnects, "CONNECT packets by outcome", "result"),
		in:        r.Counter(metricBrokerIn, "PUBLISH packets accepted from clients", "qos"),
		out:       r.Counter(metricBrokerOut, "PUBLISH packets delivered to subscribers", "qos"),
		dropped:   r.Counter(metricBrokerDropped, "Messages discarded rather than delivered", "reason"),
	}
}

// Drop reasons, kept as constants because they are the labels an operator
// alerts on: a non-zero rate of either means a store is losing price updates.
const (
	// dropOverflow: the offline queue for a disconnected session was full.
	dropOverflow = "offline_queue_overflow"
	// dropSlowConsumer: a connected client stopped reading and its send buffer
	// filled, so the broker closed the connection.
	dropSlowConsumer = "slow_consumer"
)

func (m *brokerMetrics) clientConnected(delta float64) {
	if m != nil {
		m.connected.Add(delta)
	}
}

func (m *brokerMetrics) sessionCount(n int) {
	if m != nil {
		m.sessions.Set(float64(n))
	}
}

func (m *brokerMetrics) connectResult(result string) {
	if m != nil {
		m.connects.With(result).Inc()
	}
}

func (m *brokerMetrics) received(q msgbus.QoS) {
	if m != nil {
		m.in.With(qosLabel(q)).Inc()
	}
}

func (m *brokerMetrics) sent(q msgbus.QoS) {
	if m != nil {
		m.out.With(qosLabel(q)).Inc()
	}
}

func (m *brokerMetrics) retainedCount(n int) {
	if m != nil {
		m.retained.Set(float64(n))
	}
}

func (m *brokerMetrics) inflightDelta(d float64) {
	if m != nil {
		m.inflight.Add(d)
	}
}

func (m *brokerMetrics) subsDelta(d float64) {
	if m != nil {
		m.subs.Add(d)
	}
}

func (m *brokerMetrics) drop(reason string) {
	if m != nil {
		m.dropped.With(reason).Inc()
	}
}

// clientMetrics is the client's instrumentation. connected is a gauge rather
// than a log line because the SGU's autonomous-mode dashboard is built from it:
// the operator question is "how long was this store cut off", which needs a
// time series.
type clientMetrics struct {
	connected *obs.Gauge
	inflight  *obs.Gauge
	attempts  *obs.CounterVec
	published *obs.CounterVec
	received  *obs.CounterVec
	timeouts  *obs.Counter
}

func newClientMetrics(r *obs.Registry) *clientMetrics {
	if r == nil {
		return nil
	}
	return &clientMetrics{
		connected: r.Gauge(metricClientState, "1 while the MQTT link is up, 0 while it is down").With(),
		inflight:  r.Gauge(metricClientInflight, "Published messages awaiting acknowledgement").With(),
		attempts:  r.Counter(metricClientConnects, "Connection attempts by outcome", "result"),
		published: r.Counter(metricClientPublished, "Messages published", "qos"),
		received:  r.Counter(metricClientReceived, "Messages delivered to handlers", "qos"),
		timeouts:  r.Counter(metricClientTimeouts, "Publishes abandoned after the acknowledgement timeout").With(),
	}
}

func (m *clientMetrics) setConnected(up bool) {
	if m == nil {
		return
	}
	if up {
		m.connected.Set(1)
		return
	}
	m.connected.Set(0)
}

func (m *clientMetrics) attempt(result string) {
	if m != nil {
		m.attempts.With(result).Inc()
	}
}

func (m *clientMetrics) publish(q msgbus.QoS) {
	if m != nil {
		m.published.With(qosLabel(q)).Inc()
	}
}

func (m *clientMetrics) receive(q msgbus.QoS) {
	if m != nil {
		m.received.With(qosLabel(q)).Inc()
	}
}

func (m *clientMetrics) inflightDelta(d float64) {
	if m != nil {
		m.inflight.Add(d)
	}
}

func (m *clientMetrics) timeout() {
	if m != nil {
		m.timeouts.Inc()
	}
}
