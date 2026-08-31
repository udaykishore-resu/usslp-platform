package apigw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// ---------------------------------------------------------------------------
// The live event stream
//
// GET /v1/stream is a WebSocket carrying the platform's events to whoever is
// watching a store: the operations console, a retailer's own dashboard, a
// support engineer during an incident.
//
// Two properties are load-bearing.
//
// Tenant filtering is structural. A subscriber's tenant is copied from the
// authenticated principal at upgrade time and there is no message a client can
// send that changes it; the fan-out compares it before it compares anything
// else. A client may narrow its subscription to fewer stores and fewer event
// types, and it may never widen it past what its credential already allowed.
//
// The per-connection send queue is bounded and the slow consumer is dropped.
// This is the difference between a gateway and an outage. A console left open
// on a laptop that went to sleep stops reading; its TCP window closes; without
// a bound the fan-out either blocks — stalling every other subscriber behind
// the slowest one — or buffers, and at 52,000 price updates per second an
// unbounded buffer per stalled connection is measured in gigabytes per minute.
// So the queue is small, a full queue evicts the connection with a close code
// that says "reconnect and resync", and the platform's read models are what
// the client resyncs from.
// ---------------------------------------------------------------------------

// Stream tuning.
const (
	// DefaultStreamQueue is the per-connection send queue depth.
	//
	// Two hundred and fifty six events is roughly five seconds of a busy
	// store's traffic. Deep enough to ride out a garbage collection pause or a
	// browser tab being backgrounded for a moment; shallow enough that a
	// genuinely stalled consumer is detected in seconds rather than minutes,
	// and that a thousand stalled consumers cost megabytes rather than
	// gigabytes.
	DefaultStreamQueue = 256
	// DefaultPingInterval is the keepalive period. Load balancers and
	// corporate proxies idle out an unused connection at sixty seconds with
	// depressing regularity, so the gateway pings inside that.
	DefaultPingInterval = 25 * time.Second
	// DefaultPongTimeout is how long a connection may go without any inbound
	// frame before it is presumed dead.
	DefaultPongTimeout = 70 * time.Second
	// DefaultCloseGrace bounds the closing handshake.
	DefaultCloseGrace = 2 * time.Second
	// maxSubscriptionFilters bounds a client's store and type lists, so a
	// subscription message cannot become an allocation attack.
	maxSubscriptionFilters = 512
)

// StreamEvent is what a subscriber receives.
//
// It is a projection of canon.Envelope rather than the envelope itself: the
// tenant is implicit (a subscriber only ever sees its own), and the internal
// bookkeeping — aggregate version, causation, idempotency key, source
// component — is deliberately not part of a public contract the platform would
// then have to keep.
type StreamEvent struct {
	// Type distinguishes an event from the stream's own control messages.
	Type string `json:"type"`
	// EventID and EventType identify the platform event.
	EventID   canon.EventID `json:"event_id"`
	EventType string        `json:"event_type"`
	// StoreID is the store the event belongs to, when it has one.
	StoreID canon.StoreID `json:"store_id,omitempty"`
	// AggregateType and AggregateID say what the event is about: a label, a
	// device, a promotion.
	AggregateType string `json:"aggregate_type,omitempty"`
	AggregateID   string `json:"aggregate_id,omitempty"`
	// OccurredAt is the source clock, RecordedAt is when USSLP took durable
	// responsibility. The console measures end-to-end latency from RecordedAt,
	// which is the number the 3-second SLO is written against.
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
	// TraceID lets a console row be pasted into a trace search.
	TraceID string `json:"trace_id,omitempty"`
	// Payload is the event body, passed through verbatim.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// streamControl is a gateway-to-client control message.
type streamControl struct {
	Type string `json:"type"`
	// Tenant, Stores and Types echo the subscription actually in force, so a
	// client can see that its filter was accepted and narrowed.
	Tenant canon.TenantID `json:"tenant,omitempty"`
	Stores []string       `json:"stores,omitempty"`
	Types  []string       `json:"types,omitempty"`
	// Message carries a human-readable note for an error control message.
	Message string `json:"message,omitempty"`
	// At timestamps the message.
	At time.Time `json:"at"`
}

// clientCommand is a client-to-gateway message. The only thing a client may
// ask for is a narrower subscription.
type clientCommand struct {
	Type   string   `json:"type"`
	Stores []string `json:"stores,omitempty"`
	Types  []string `json:"types,omitempty"`
}

// subscriber is one connected client.
type subscriber struct {
	// tenant is fixed at upgrade time from the credential. There is no setter.
	tenant canon.TenantID
	// allowed is the store scope the credential permits, nil when unscoped.
	allowed map[canon.StoreID]bool

	mu sync.RWMutex
	// stores and types are the client's chosen narrowing, nil meaning "all of
	// what the credential allows".
	stores map[canon.StoreID]bool
	types  map[string]bool

	send  chan []byte
	evict chan CloseCode
	done  chan struct{}

	closeOnce sync.Once
}

// matches decides whether an event reaches this subscriber.
func (s *subscriber) matches(tenant canon.TenantID, store canon.StoreID, eventType string) bool {
	// The tenant test is first and unconditional.
	if tenant != s.tenant {
		return false
	}
	// The credential's store scope binds even if the client asked for
	// everything: narrowing is a client's choice, widening is not.
	if s.allowed != nil && store != "" && !s.allowed[store] {
		return false
	}
	s.mu.RLock()
	stores, types := s.stores, s.types
	s.mu.RUnlock()
	if stores != nil && !stores[store] {
		return false
	}
	if types != nil && !types[eventType] {
		return false
	}
	return true
}

// narrow applies a client's subscription request, intersected with what the
// credential allows.
func (s *subscriber) narrow(stores []string, types []string) (appliedStores, appliedTypes []string) {
	var storeSet map[canon.StoreID]bool
	if len(stores) > 0 {
		storeSet = make(map[canon.StoreID]bool, len(stores))
		for _, raw := range stores {
			id := canon.StoreID(raw)
			if !canon.ValidID(raw) {
				continue
			}
			if s.allowed != nil && !s.allowed[id] {
				continue
			}
			storeSet[id] = true
			appliedStores = append(appliedStores, raw)
		}
		if len(storeSet) == 0 {
			// A filter that selected nothing would silently produce a stream
			// that never emits. Falling back to the credential's full scope is
			// the more useful failure, and the control message the gateway
			// echoes back tells the client what actually happened.
			storeSet = nil
			appliedStores = nil
		}
	}
	var typeSet map[string]bool
	if len(types) > 0 {
		typeSet = make(map[string]bool, len(types))
		for _, t := range types {
			if t = strings.TrimSpace(t); t != "" {
				typeSet[t] = true
				appliedTypes = append(appliedTypes, t)
			}
		}
		if len(typeSet) == 0 {
			typeSet = nil
		}
	}
	s.mu.Lock()
	s.stores, s.types = storeSet, typeSet
	s.mu.Unlock()
	return appliedStores, appliedTypes
}

// requestEvict asks the writer to close this connection. It is non-blocking
// and idempotent: the fan-out path must never block, and several events may
// discover the same stalled consumer at once.
func (s *subscriber) requestEvict(code CloseCode) {
	select {
	case s.evict <- code:
	default:
	}
}

// Hub fans events out to subscribers.
type Hub struct {
	mu      sync.RWMutex
	subs    map[*subscriber]struct{}
	closed  bool
	metrics *Metrics
	log     *obs.Logger
}

// NewHub creates an empty hub.
func NewHub(metrics *Metrics, log *obs.Logger) *Hub {
	return &Hub{subs: make(map[*subscriber]struct{}), metrics: metrics, log: log}
}

func (h *Hub) add(s *subscriber) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.subs[s] = struct{}{}
	h.metrics.StreamConnections.With("yes").Set(float64(len(h.subs)))
	return true
}

func (h *Hub) remove(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	n := len(h.subs)
	h.mu.Unlock()
	h.metrics.StreamConnections.With("yes").Set(float64(n))
}

// Len reports the number of live subscribers.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Publish fans one envelope out.
//
// The event is serialised once, not once per subscriber: at the platform's
// peak a re-marshal per connection would dominate the gateway's CPU, and the
// bytes are identical for every recipient because the tenant is implicit.
func (h *Hub) Publish(env canon.Envelope) {
	h.mu.RLock()
	if len(h.subs) == 0 || h.closed {
		h.mu.RUnlock()
		h.metrics.StreamEvents.With("no_subscribers").Inc()
		return
	}
	targets := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		if s.matches(env.TenantID, env.StoreID, env.EventType) {
			targets = append(targets, s)
		}
	}
	total := len(h.subs)
	h.mu.RUnlock()

	if len(targets) == 0 {
		h.metrics.StreamEvents.With("filtered").Add(uint64(total))
		return
	}
	body, err := json.Marshal(StreamEvent{
		Type: "event", EventID: env.EventID, EventType: env.EventType,
		StoreID: env.StoreID, AggregateType: env.AggregateType, AggregateID: env.AggregateID,
		OccurredAt: env.OccurredAt, RecordedAt: env.RecordedAt, TraceID: env.TraceID,
		Payload: env.Payload,
	})
	if err != nil {
		h.metrics.StreamEvents.With("encode_error").Inc()
		h.log.Error("stream event could not be encoded", "event_type", env.EventType, "error", err)
		return
	}
	for _, s := range targets {
		select {
		case s.send <- body:
			h.metrics.StreamEvents.With("delivered").Inc()
		default:
			// The queue is full: this consumer is not keeping up. Evict it
			// rather than block the fan-out or grow the queue.
			h.metrics.StreamEvents.With("dropped").Inc()
			s.requestEvict(CloseTryAgainLater)
		}
	}
	if filtered := total - len(targets); filtered > 0 {
		h.metrics.StreamEvents.With("filtered").Add(uint64(filtered))
	}
}

// Shutdown asks every subscriber to close and stops accepting new ones.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.requestEvict(CloseGoingAway)
	}
}

// ---------------------------------------------------------------------------
// Event source
// ---------------------------------------------------------------------------

// EventSource feeds the hub.
//
// It is an interface so that the gateway does not have to own an event-bus
// consumer to be testable: the test suite drives the hub from a channel, and
// production drives it from [BusSource].
type EventSource interface {
	// Run delivers envelopes until ctx is cancelled.
	Run(ctx context.Context, deliver func(canon.Envelope)) error
}

// StreamTopics are the streams the gateway forwards to consoles.
//
// label-telemetry is deliberately absent. At fifty million labels reporting
// every five minutes it is ~167,000 events per second, none of which any human
// is watching individually; the console gets telemetry as aggregated store
// health from the device registry instead.
var StreamTopics = []string{
	canon.StreamPriceUpdates.Name,
	canon.StreamDelivery.Name,
	canon.StreamDeviceEvents.Name,
	canon.StreamPromotions.Name,
	canon.StreamOTA.Name,
}

// BusSource consumes the event bus into the hub.
type BusSource struct {
	// Bus is the event-streaming port.
	Bus eventbus.Bus
	// Group must be unique per gateway replica. Members of a consumer group
	// share partitions, so two replicas in one group would each see half the
	// events — and a console connected to the wrong replica would miss the
	// other half. Every replica gets its own group and therefore its own full
	// copy of the stream.
	Group string
	// Topics to consume; StreamTopics when empty.
	Topics []string
	// Log records records that could not be decoded.
	Log *obs.Logger
}

// Run implements EventSource.
func (s *BusSource) Run(ctx context.Context, deliver func(canon.Envelope)) error {
	topics := s.Topics
	if len(topics) == 0 {
		topics = StreamTopics
	}
	consumer, err := s.Bus.Subscribe(eventbus.SubscribeOptions{
		Group:  s.Group,
		Topics: topics,
		// A console shows what is happening now. Replaying a week of price
		// updates into a browser on every gateway restart would be both
		// useless and expensive, so a fresh group starts at the tail.
		FromBeginning: false,
		// One retry, no dead-lettering worth the name: a record this consumer
		// cannot handle is a record some other consumer owns, and the gateway
		// must never be the reason a record is declared poison.
		MaxRetries: 1,
	})
	if err != nil {
		return err
	}
	defer consumer.Close()

	return consumer.Run(ctx, func(_ context.Context, m eventbus.Message) error {
		var env canon.Envelope
		if err := json.Unmarshal(m.Value, &env); err != nil {
			// Committed, not retried. This is a read-only fan-out; a record it
			// cannot parse is a problem for the service that owns the stream,
			// and wedging the console feed on it would help nobody.
			if s.Log != nil {
				s.Log.Warn("stream source skipped an undecodable record",
					"topic", m.Topic, "partition", m.Partition, "offset", m.Offset, "error", err)
			}
			return nil
		}
		deliver(env)
		return nil
	})
}

// ---------------------------------------------------------------------------
// The handler
// ---------------------------------------------------------------------------

// StreamConfig tunes the stream endpoint.
type StreamConfig struct {
	QueueDepth   int
	PingInterval time.Duration
	PongTimeout  time.Duration
	CloseGrace   time.Duration
}

func (c StreamConfig) withDefaults() StreamConfig {
	if c.QueueDepth <= 0 {
		c.QueueDepth = DefaultStreamQueue
	}
	if c.PingInterval <= 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.PongTimeout <= 0 {
		c.PongTimeout = DefaultPongTimeout
	}
	if c.CloseGrace <= 0 {
		c.CloseGrace = DefaultCloseGrace
	}
	return c
}

// handleStream upgrades a request and serves the live feed.
func (g *Gateway) handleStream(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	requested := splitList(r.URL.Query().Get("stores"))
	types := splitList(r.URL.Query().Get("types"))
	if len(requested) > maxSubscriptionFilters || len(types) > maxSubscriptionFilters {
		writeError(w, r, errBadRequest("a subscription may name at most %d stores and %d event types",
			maxSubscriptionFilters, maxSubscriptionFilters))
		return
	}
	for _, s := range requested {
		if !canon.ValidID(s) {
			writeError(w, r, errBadRequest("store id %q contains reserved characters", s))
			return
		}
		if !p.AllowsStore(canon.StoreID(s)) {
			// Same rule as everywhere else: an out-of-scope identifier is a
			// 404, so a subscription cannot be used to enumerate stores.
			writeError(w, r, errNotFound("store %s not found", s))
			return
		}
	}

	var allowed map[canon.StoreID]bool
	if len(p.Stores) > 0 {
		allowed = make(map[canon.StoreID]bool, len(p.Stores))
		for _, s := range p.Stores {
			allowed[s] = true
		}
	}

	cfg := g.streamCfg
	sub := &subscriber{
		tenant:  p.TenantID,
		allowed: allowed,
		send:    make(chan []byte, cfg.QueueDepth),
		evict:   make(chan CloseCode, 1),
		done:    make(chan struct{}),
	}
	appliedStores, appliedTypes := sub.narrow(requested, types)

	// The subprotocol is only echoed if the client offered it, and the
	// credential-bearing entry is never echoed back — a response header is far
	// more likely to be logged than a request one.
	var negotiated string
	for _, proto := range parseSubprotocols(r.Header.Get("Sec-WebSocket-Protocol")) {
		if proto == wsProtocol {
			negotiated = wsProtocol
			break
		}
	}

	conn, err := Upgrade(w, r, ConnConfig{ReadTimeout: cfg.PongTimeout}, negotiated)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !g.hub.add(sub) {
		_ = conn.CloseWithHandshake(CloseGoingAway, "gateway is shutting down", cfg.CloseGrace)
		return
	}
	defer g.hub.remove(sub)

	log := g.log.FromContext(r.Context()).With(
		"tenant_id", string(p.TenantID), "subject", p.Subject,
		"request_id", RequestIDFrom(r.Context()))
	log.Info("stream connected", "stores", appliedStores, "types", appliedTypes,
		"queue_depth", cfg.QueueDepth)

	hello, _ := json.Marshal(streamControl{
		Type: "ready", Tenant: p.TenantID, Stores: appliedStores, Types: appliedTypes, At: g.now(),
	})
	if err := conn.WriteMessage(OpText, hello); err != nil {
		_ = conn.Close()
		return
	}

	go g.streamReader(conn, sub, log)
	reason := g.streamWriter(conn, sub, cfg)

	sub.closeOnce.Do(func() { close(sub.done) })
	log.Info("stream disconnected", "reason", reason)
}

// streamReader owns inbound frames: keepalive pongs, subscription changes and
// the peer's close. It runs in its own goroutine because [Conn.ReadMessage]
// blocks, and the writer must stay free to fan out events.
func (g *Gateway) streamReader(conn *Conn, sub *subscriber, log *obs.Logger) {
	defer sub.closeOnce.Do(func() { close(sub.done) })
	for {
		op, payload, err := conn.ReadMessage()
		if err != nil {
			switch {
			case IsCloseError(err, CloseNormal, CloseGoingAway):
			case IsCloseError(err):
				log.Info("stream closed by peer", "error", err.Error())
			case errors.Is(err, context.Canceled):
			default:
				log.Debug("stream read ended", "error", err.Error())
			}
			return
		}
		if op != OpText {
			// The client has nothing binary to say. Refusing rather than
			// ignoring keeps the contract one-directional and explicit.
			_ = conn.WriteClose(CloseUnsupportedData, "this endpoint accepts text control messages only")
			return
		}
		var cmd clientCommand
		if err := json.Unmarshal(payload, &cmd); err != nil || cmd.Type != "subscribe" {
			body, _ := json.Marshal(streamControl{
				Type: "error", Message: `expected {"type":"subscribe","stores":[…],"types":[…]}`, At: g.now(),
			})
			select {
			case sub.send <- body:
			default:
			}
			continue
		}
		if len(cmd.Stores) > maxSubscriptionFilters || len(cmd.Types) > maxSubscriptionFilters {
			_ = conn.WriteClose(ClosePolicyViolation, "subscription filter is too large")
			return
		}
		stores, kinds := sub.narrow(cmd.Stores, cmd.Types)
		body, _ := json.Marshal(streamControl{
			Type: "subscribed", Tenant: sub.tenant, Stores: stores, Types: kinds, At: g.now(),
		})
		select {
		case sub.send <- body:
		default:
			sub.requestEvict(CloseTryAgainLater)
		}
	}
}

// streamWriter owns outbound frames and the connection's lifetime. It returns
// the reason the connection ended, for the access log.
func (g *Gateway) streamWriter(conn *Conn, sub *subscriber, cfg StreamConfig) string {
	ping := time.NewTicker(cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case body := <-sub.send:
			if err := conn.WriteMessage(OpText, body); err != nil {
				_ = conn.Close()
				return "write_failed"
			}
		case code := <-sub.evict:
			reason, text := "server_close", "gateway is shutting down"
			if code == CloseTryAgainLater {
				reason = "slow_consumer"
				text = "this connection fell behind; reconnect and resynchronise"
			}
			g.metrics.StreamEvictions.With(reason).Inc()
			g.closeStream(conn, sub, code, text, cfg.CloseGrace)
			return reason
		case <-ping.C:
			if err := conn.WritePing(nil); err != nil {
				_ = conn.Close()
				return "ping_failed"
			}
		case <-sub.done:
			// The reader ended: the peer closed, or the connection broke.
			_ = conn.Close()
			return "peer_closed"
		case <-g.streamDone:
			g.metrics.StreamEvictions.With("shutdown").Inc()
			g.closeStream(conn, sub, CloseGoingAway, "gateway is shutting down", cfg.CloseGrace)
			return "shutdown"
		}
	}
}

// closeStream performs the closing handshake from the writer's side.
//
// The writer sends the close frame; the *reader* goroutine is the one that
// observes the peer's reply, because a Conn supports one reader and one
// writer and having the writer also read would be a data race on the buffered
// reader. So this waits for the reader to finish — which it does as soon as
// the peer's close arrives — and drops the socket if the peer never answers,
// which also unblocks the reader.
func (g *Gateway) closeStream(conn *Conn, sub *subscriber, code CloseCode, reason string, grace time.Duration) {
	_ = conn.WriteClose(code, reason)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-sub.done:
	case <-timer.C:
	}
	_ = conn.Close()
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
