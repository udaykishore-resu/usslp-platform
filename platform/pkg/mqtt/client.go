package mqtt

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// Client defaults. The worker pool is small on purpose: handlers in USSLP write
// to a local store or push onto a mesh radio queue, and running hundreds in
// parallel would only convert a slow downstream into unbounded memory.
const (
	defaultHandlerWorkers = 4
	defaultHandlerQueue   = 256
	defaultClientBuffer   = 256
	// closeGrace bounds how long Close waits for the broker to finish the
	// shutdown handshake before pulling the socket down anyway.
	closeGrace = 2 * time.Second
)

// ConnectError reports a broker that answered CONNECT with a refusal. The code
// is carried through so a device can distinguish a revoked certificate
// (ConnectNotAuthorized — stop and alert) from a broker that is starting up
// (ConnectServerUnavailable — keep retrying).
type ConnectError struct{ Code ConnectReturnCode }

// Error renders the refusal in the words the specification uses, so a support
// engineer reading a device log does not have to look up a numeric code.
func (e *ConnectError) Error() string { return "mqtt: connection refused: " + e.Code.String() }

// ClientOption adjusts a Client beyond what msgbus.Config expresses. The port
// deliberately does not carry these — worker-pool sizing and metrics are
// properties of this implementation, not of the messaging contract.
type ClientOption func(*clientOptions)

type clientOptions struct {
	registry       *obs.Registry
	logger         *obs.Logger
	handlerWorkers int
	handlerQueue   int
	sendBuffer     int
	backoff        retry.Policy
	maxPacketSize  int
}

// WithClientRegistry sends the client's metrics to r. A registry may back only
// one client, since obs.Registry rejects duplicate metric names.
func WithClientRegistry(r *obs.Registry) ClientOption {
	return func(o *clientOptions) { o.registry = r }
}

// WithClientLogger routes connection events to l.
func WithClientLogger(l *obs.Logger) ClientOption {
	return func(o *clientOptions) { o.logger = l }
}

// WithHandlerPool sizes the dispatch pool: workers goroutines draining a queue
// of depth queue. When the queue fills, the read loop blocks, which propagates
// backpressure through TCP to the broker — the intended behaviour, and the
// reason handlers are documented as "must not block".
func WithHandlerPool(workers, queue int) ClientOption {
	return func(o *clientOptions) {
		if workers > 0 {
			o.handlerWorkers = workers
		}
		if queue > 0 {
			o.handlerQueue = queue
		}
	}
}

// WithBackoff replaces retry.Persistent as the reconnection schedule.
func WithBackoff(p retry.Policy) ClientOption {
	return func(o *clientOptions) { o.backoff = p }
}

// subscription is one registered filter and the handler it feeds.
type subscription struct {
	qos msgbus.QoS
	h   msgbus.Handler
}

// clientInflight is one outbound QoS 1/2 message awaiting its handshake.
type clientInflight struct {
	id    uint16
	msg   msgbus.Message
	state outState
	// done is closed by the read loop when the handshake completes. Publish
	// waits on it; a reconnect leaves it alone, because the message is re-sent
	// and the same waiter is satisfied by the acknowledgement that follows.
	done chan struct{}
}

// Client is an MQTT 3.1.1 client implementing msgbus.Client.
//
// It is the same code in a Shelf Edge Controller talking to its store's SGU and
// in the SGU talking to EMQX in the cloud, which is what makes the two links
// behave identically during an outage: the client reconnects forever on
// retry.Persistent backoff, restores its subscriptions and re-sends what was
// in flight, and reports the truth from Connected while it does.
type Client struct {
	cfg  msgbus.Config
	opts clientOptions
	log  *obs.Logger
	met  *clientMetrics
	tls  *tls.Config
	addr string

	// ctx is cancelled by Close and stops the reconnect loop.
	ctx    context.Context
	cancel context.CancelFunc

	connected atomic.Bool

	mu   sync.Mutex
	conn *clientConn
	subs map[string]*subscription
	// inflight and order mirror the broker's session state: the client is the
	// sender for its own publications and owes the same ordered redelivery.
	inflight map[uint16]*clientInflight
	order    []uint16
	// inboundQoS2 holds identifiers received and not yet released, so a
	// redelivered QoS 2 PUBLISH is acknowledged without reaching the handler a
	// second time.
	inboundQoS2  map[uint16]struct{}
	subWaiters   map[uint16]chan []byte
	unsubWaiters map[uint16]chan struct{}
	nextID       uint16
	closed       bool

	dispatch chan dispatchJob
	wg       sync.WaitGroup
	workerWG sync.WaitGroup
}

type dispatchJob struct {
	h msgbus.Handler
	m msgbus.Message
}

// Dial connects to the broker named by cfg.BrokerURL and returns a running
// client. It blocks until the first CONNACK, so a configuration or credential
// error surfaces to the caller rather than disappearing into a retry loop; from
// then on the client keeps itself connected in the background.
func Dial(ctx context.Context, cfg msgbus.Config, opts ...ClientOption) (*Client, error) {
	cfg = cfg.WithDefaults()
	o := clientOptions{
		handlerWorkers: defaultHandlerWorkers,
		handlerQueue:   defaultHandlerQueue,
		sendBuffer:     defaultClientBuffer,
		backoff:        retry.Persistent,
		maxPacketSize:  defaultMaxPacketSize,
	}
	for _, fn := range opts {
		fn(&o)
	}
	log := o.logger
	if log == nil {
		log = obs.NewLogger(obs.LogConfig{Service: "mqtt-client", Output: io.Discard})
	}

	addr, useTLS, err := parseBrokerURL(cfg.BrokerURL)
	if err != nil {
		return nil, err
	}
	var tlsCfg *tls.Config
	if cfg.TLSConfig != nil {
		t, ok := cfg.TLSConfig.(*tls.Config)
		if !ok {
			return nil, fmt.Errorf("mqtt: Config.TLSConfig is %T, want *tls.Config", cfg.TLSConfig)
		}
		tlsCfg = t
	}
	if useTLS && tlsCfg == nil {
		// A tls:// URL with no configuration still gets a verified handshake
		// against the system roots; silently falling back to plaintext would be
		// the worst possible reading of the caller's intent.
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	c := &Client{
		cfg:          cfg,
		opts:         o,
		log:          log,
		met:          newClientMetrics(o.registry),
		tls:          tlsCfg,
		addr:         addr,
		subs:         make(map[string]*subscription),
		inflight:     make(map[uint16]*clientInflight),
		inboundQoS2:  make(map[uint16]struct{}),
		subWaiters:   make(map[uint16]chan []byte),
		unsubWaiters: make(map[uint16]chan struct{}),
		nextID:       1,
		dispatch:     make(chan dispatchJob, o.handlerQueue),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	conn, err := c.connectOnce(ctx)
	if err != nil {
		c.cancel()
		c.met.attempt("failed")
		return nil, err
	}
	c.met.attempt("connected")
	c.startWorkers()
	// Adopt before returning: a caller that publishes on the next line must not
	// race the supervisor goroutine into deciding whether the link is up.
	c.adopt(conn)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.supervise(conn)
	}()
	return c, nil
}

// parseBrokerURL accepts tcp://, mqtt://, tls://, ssl:// and mqtts://. The
// aliases exist because device firmware, Helm charts and developer shells in
// this estate each picked a different spelling years ago.
func parseBrokerURL(raw string) (addr string, useTLS bool, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("mqtt: broker URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "tcp", "mqtt":
	case "tls", "ssl", "mqtts":
		useTLS = true
	default:
		return "", false, fmt.Errorf("mqtt: broker URL %q: unsupported scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("mqtt: broker URL %q has no host", raw)
	}
	if u.Port() == "" {
		if useTLS {
			return net.JoinHostPort(u.Host, "8883"), useTLS, nil
		}
		return net.JoinHostPort(u.Host, "1883"), useTLS, nil
	}
	return u.Host, useTLS, nil
}

// connectOnce performs one dial and MQTT handshake.
func (c *Client) connectOnce(ctx context.Context) (*clientConn, error) {
	d := net.Dialer{Timeout: c.cfg.ConnectTimeout}
	var nc net.Conn
	var err error
	if c.tls != nil {
		nc, err = (&tls.Dialer{NetDialer: &d, Config: c.tls}).DialContext(ctx, "tcp", c.addr)
	} else {
		nc, err = d.DialContext(ctx, "tcp", c.addr)
	}
	if err != nil {
		return nil, fmt.Errorf("mqtt: dial %s: %w", c.addr, err)
	}

	conn := newClientConn(nc, c.opts.sendBuffer)
	deadline := time.Now().Add(c.cfg.ConnectTimeout)
	if err := nc.SetDeadline(deadline); err != nil {
		nc.Close()
		return nil, err
	}
	cp := &connectPacket{
		ProtocolName:  protocolName,
		ProtocolLevel: protocolLevel,
		CleanSession:  c.cfg.CleanSession,
		KeepAlive:     uint16(c.cfg.KeepAlive / time.Second),
		ClientID:      c.cfg.ClientID,
		HasUsername:   c.cfg.Username != "",
		HasPassword:   c.cfg.Password != "",
		Username:      c.cfg.Username,
		Password:      []byte(c.cfg.Password),
	}
	if w := c.cfg.Will; w != nil {
		cp.WillFlag = true
		cp.WillTopic = w.Topic
		cp.WillMessage = w.Payload
		cp.WillQoS = w.QoS
		cp.WillRetain = w.Retain
	}
	if err := writePacket(conn.w, cp); err != nil {
		nc.Close()
		return nil, err
	}
	if err := conn.w.Flush(); err != nil {
		nc.Close()
		return nil, fmt.Errorf("mqtt: sending CONNECT: %w", err)
	}
	p, err := readPacket(conn.r, c.opts.maxPacketSize)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("mqtt: reading CONNACK: %w", err)
	}
	ack, ok := p.(*connackPacket)
	if !ok {
		nc.Close()
		return nil, fmt.Errorf("%w: broker answered CONNECT with %s", ErrProtocolViolation, p.pktType())
	}
	if ack.ReturnCode != ConnectAccepted {
		nc.Close()
		return nil, &ConnectError{Code: ack.ReturnCode}
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		nc.Close()
		return nil, err
	}
	conn.sessionPresent = ack.SessionPresent
	return conn, nil
}

// supervise owns the link for the client's lifetime: it runs the current
// connection until it dies, then reconnects on backoff until Close. This is the
// loop that lets a store ride out a WAN cut — the SGU's cloud client sits here
// for an hour and reattaches without anything above it noticing more than
// Connected going false.
func (c *Client) supervise(conn *clientConn) {
	// The first link has nothing to resume — Dial has just created the client —
	// and resuming it anyway would race a Subscribe made immediately after Dial
	// into being sent twice, which costs the caller a duplicate delivery of
	// every retained message under the filter.
	resuming := false
	for {
		c.runLink(conn, resuming)
		if c.isClosed() {
			return
		}
		next := c.reconnect()
		if next == nil {
			return
		}
		if !c.adopt(next) {
			return
		}
		conn = next
		resuming = true
	}
}

// adopt installs a freshly handshaken connection as the live link and starts
// the goroutines that own it.
func (c *Client) adopt(conn *clientConn) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.close(nil)
		return false
	}
	c.conn = conn
	c.mu.Unlock()
	conn.start(c.cfg.KeepAlive)
	c.connected.Store(true)
	c.met.setConnected(true)
	return true
}

func (c *Client) reconnect() *clientConn {
	var conn *clientConn
	err := retry.Do(c.ctx, c.opts.backoff, func(ctx context.Context, attempt int) error {
		nc, err := c.connectOnce(ctx)
		if err != nil {
			c.met.attempt("failed")
			c.log.Debug("mqtt reconnect failed", "attempt", attempt, "error", err.Error())
			return err
		}
		c.met.attempt("reconnected")
		c.log.Info("mqtt reconnected", "client_id", c.cfg.ClientID, "attempt", attempt)
		conn = nc
		return nil
	})
	if err != nil || conn == nil {
		return nil
	}
	return conn
}

// runLink serves one already-adopted connection until it fails, then reports
// the link down.
func (c *Client) runLink(conn *clientConn, resuming bool) {
	err := error(nil)
	if resuming {
		err = c.resume(conn)
	}
	if err != nil {
		c.log.Warn("mqtt session resume failed", "error", err.Error())
		conn.close(err)
	} else {
		c.readLoop(conn)
	}

	conn.close(nil)
	conn.wait()
	c.connected.Store(false)
	c.met.setConnected(false)
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

// resume restores everything the link owes after a reconnect: every filter is
// re-subscribed and every un-acked publication is re-sent with DUP.
//
// Subscriptions are restored unconditionally rather than only when the broker
// reports SessionPresent=false. It costs one round trip and removes a whole
// class of silent failure in which a gateway believes it is subscribed to a
// store's price topics and is not.
func (c *Client) resume(conn *clientConn) error {
	c.mu.Lock()
	filters := make([]topicFilter, 0, len(c.subs))
	for f, s := range c.subs {
		filters = append(filters, topicFilter{Filter: f, QoS: s.qos})
	}
	c.mu.Unlock()

	if len(filters) > 0 {
		// SessionPresent is logged rather than acted on: it is the operator's
		// evidence for whether the broker still held this device's state, which
		// is the first question asked after a store-wide reconnect.
		c.log.Info("mqtt resuming session",
			"client_id", c.cfg.ClientID, "session_present", conn.sessionPresent,
			"filters", len(filters))
		// The read loop is not running yet, so this exchange is done inline:
		// nothing else may be interleaved with a resume anyway.
		if err := c.resubscribe(conn, filters); err != nil {
			return err
		}
	}

	c.mu.Lock()
	pending := make([]*clientInflight, 0, len(c.order))
	for _, id := range c.order {
		if f, ok := c.inflight[id]; ok {
			pending = append(pending, f)
		}
	}
	c.mu.Unlock()
	for _, f := range pending {
		var p packet
		if f.state == awaitingPubcomp {
			p = &ackPacket{Type: pktPUBREL, PacketID: f.id}
		} else {
			p = &publishPacket{Dup: true, QoS: f.msg.QoS, Retain: f.msg.Retain,
				Topic: f.msg.Topic, PacketID: f.id, Payload: f.msg.Payload}
		}
		if !conn.send(p) {
			return errors.New("mqtt: link lost while re-sending in-flight messages")
		}
	}
	return nil
}

// resubscribe sends one SUBSCRIBE carrying every filter and waits for its
// SUBACK synchronously, before the read loop starts.
func (c *Client) resubscribe(conn *clientConn, filters []topicFilter) error {
	c.mu.Lock()
	id, ok := c.allocIDLocked()
	c.mu.Unlock()
	if !ok {
		return errors.New("mqtt: no free packet identifier for resubscribe")
	}
	defer c.releaseID(id)

	if !conn.send(&subscribePacket{PacketID: id, Filters: filters}) {
		return errors.New("mqtt: link lost while re-subscribing")
	}
	deadline := time.Now().Add(c.cfg.AckTimeout)
	for {
		if err := conn.nc.SetReadDeadline(deadline); err != nil {
			return err
		}
		p, err := readPacket(conn.r, c.opts.maxPacketSize)
		if err != nil {
			return err
		}
		if err := conn.nc.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		sa, ok := p.(*subackPacket)
		if !ok || sa.PacketID != id {
			// Anything else arriving mid-resume is genuine traffic: hand it to
			// the normal path and keep waiting for the SUBACK.
			if err := c.handle(conn, p); err != nil {
				return err
			}
			continue
		}
		for i, code := range sa.Codes {
			if code == subackFailure {
				return fmt.Errorf("%w: broker refused filter %q on resubscribe", ErrNotAuthorized, filters[i].Filter)
			}
		}
		return nil
	}
}

func (c *Client) readLoop(conn *clientConn) {
	for {
		if c.cfg.KeepAlive > 0 {
			// Twice the keepalive: the broker owes a PINGRESP within one
			// interval, so silence for two means the link is dead even though
			// the socket has not noticed.
			if err := conn.nc.SetReadDeadline(time.Now().Add(2 * c.cfg.KeepAlive)); err != nil {
				conn.close(err)
				return
			}
		}
		p, err := readPacket(conn.r, c.opts.maxPacketSize)
		if err != nil {
			if !c.isClosed() && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				c.log.Debug("mqtt link read failed", "error", err.Error())
			}
			conn.close(err)
			return
		}
		if err := c.handle(conn, p); err != nil {
			c.log.Warn("mqtt protocol error from broker", "error", err.Error())
			conn.close(err)
			return
		}
	}
}

func (c *Client) handle(conn *clientConn, p packet) error {
	switch pk := p.(type) {
	case *publishPacket:
		return c.handlePublish(conn, pk)
	case *ackPacket:
		switch pk.Type {
		case pktPUBACK:
			c.completeInflight(pk.PacketID, awaitingPuback)
		case pktPUBREC:
			c.advanceToPubrel(conn, pk.PacketID)
		case pktPUBREL:
			c.releaseInbound(pk.PacketID)
			conn.send(&ackPacket{Type: pktPUBCOMP, PacketID: pk.PacketID})
		case pktPUBCOMP:
			c.completeInflight(pk.PacketID, awaitingPubcomp)
		case pktUNSUBACK:
			c.completeUnsuback(pk.PacketID)
		default:
			return fmt.Errorf("%w: broker sent %s", ErrProtocolViolation, pk.Type)
		}
		return nil
	case *subackPacket:
		c.completeSuback(pk)
		return nil
	case *emptyPacket:
		if pk.Type == pktPINGRESP {
			return nil
		}
		return fmt.Errorf("%w: broker sent %s", ErrProtocolViolation, pk.Type)
	default:
		return fmt.Errorf("%w: broker sent %s", ErrProtocolViolation, p.pktType())
	}
}

func (c *Client) handlePublish(conn *clientConn, p *publishPacket) error {
	m := msgbus.Message{
		Topic:      p.Topic,
		Payload:    p.Payload,
		QoS:        p.QoS,
		Retain:     p.Retain,
		Duplicate:  p.Dup,
		ReceivedAt: time.Now(),
	}
	switch p.QoS {
	case msgbus.AtMostOnce:
		c.deliver(m)
	case msgbus.AtLeastOnce:
		// Enqueued before the PUBACK: acknowledging first would let the broker
		// forget a message the worker pool has not accepted yet.
		c.deliver(m)
		conn.send(&ackPacket{Type: pktPUBACK, PacketID: p.PacketID})
	case msgbus.ExactlyOnce:
		if c.claimInbound(p.PacketID) {
			c.deliver(m)
		}
		conn.send(&ackPacket{Type: pktPUBREC, PacketID: p.PacketID})
	}
	return nil
}

// deliver hands a message to every matching handler through the bounded worker
// pool. It blocks when the pool is saturated, which is deliberate: the read
// loop stops, the TCP window closes and the broker slows down, instead of this
// process growing a goroutine per undelivered message.
func (c *Client) deliver(m msgbus.Message) {
	c.mu.Lock()
	var jobs []dispatchJob
	for filter, s := range c.subs {
		if MatchTopic(filter, m.Topic) {
			jobs = append(jobs, dispatchJob{h: s.h, m: m})
		}
	}
	c.mu.Unlock()
	for _, j := range jobs {
		c.met.receive(m.QoS)
		select {
		case c.dispatch <- j:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) startWorkers() {
	for i := 0; i < c.opts.handlerWorkers; i++ {
		c.workerWG.Add(1)
		go func() {
			defer c.workerWG.Done()
			for {
				select {
				case <-c.ctx.Done():
					return
				case j := <-c.dispatch:
					j.h(c.ctx, j.m)
				}
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// msgbus.Client
// ---------------------------------------------------------------------------

// Publish sends a message, blocking for QoS 1 and 2 until the handshake
// completes. It returns msgbus.ErrTimeout when the acknowledgement does not
// arrive within Config.AckTimeout, msgbus.ErrNotConnected when the link is
// down, and msgbus.ErrClosed after Close.
func (c *Client) Publish(ctx context.Context, m msgbus.Message) error {
	if !ValidTopicName(m.Topic) {
		return fmt.Errorf("%w: %q is not a valid topic name", ErrMalformedPacket, m.Topic)
	}
	if m.QoS > msgbus.ExactlyOnce {
		return fmt.Errorf("%w: QoS %d", ErrProtocolViolation, m.QoS)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return msgbus.ErrClosed
	}
	conn := c.conn
	if conn == nil || !c.connected.Load() {
		c.mu.Unlock()
		return msgbus.ErrNotConnected
	}
	if m.QoS == msgbus.AtMostOnce {
		c.mu.Unlock()
		if !conn.send(&publishPacket{QoS: m.QoS, Retain: m.Retain, Topic: m.Topic, Payload: m.Payload}) {
			return msgbus.ErrNotConnected
		}
		c.met.publish(m.QoS)
		return nil
	}
	id, ok := c.allocIDLocked()
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("mqtt: no free packet identifier; %d messages are in flight", len(c.inflight))
	}
	f := &clientInflight{id: id, msg: m, state: awaitingPuback, done: make(chan struct{})}
	if m.QoS == msgbus.ExactlyOnce {
		f.state = awaitingPubrec
	}
	c.inflight[id] = f
	c.order = append(c.order, id)
	c.met.inflightDelta(1)
	c.mu.Unlock()

	if !conn.send(&publishPacket{QoS: m.QoS, Retain: m.Retain, Topic: m.Topic, PacketID: id, Payload: m.Payload}) {
		// The link died before the bytes left. The message stays in flight and
		// is re-sent on reconnect, so the wait below still has a chance to
		// succeed inside the ack timeout.
		c.log.Debug("mqtt publish queued across a reconnect", "topic", m.Topic, "packet_id", id)
	}
	c.met.publish(m.QoS)

	timer := time.NewTimer(c.cfg.AckTimeout)
	defer timer.Stop()
	select {
	case <-f.done:
		return nil
	case <-timer.C:
		c.abandon(id)
		c.met.timeout()
		return msgbus.ErrTimeout
	case <-ctx.Done():
		c.abandon(id)
		return ctx.Err()
	case <-c.ctx.Done():
		c.abandon(id)
		return msgbus.ErrClosed
	}
}

// Subscribe registers a handler and waits for the broker's SUBACK. A filter the
// broker refuses — a cross-tenant subscribe, in USSLP — returns an error rather
// than silently never delivering.
func (c *Client) Subscribe(ctx context.Context, filter string, qos msgbus.QoS, h msgbus.Handler) error {
	if !ValidTopicFilter(filter) {
		return fmt.Errorf("%w: %q is not a valid topic filter", ErrMalformedPacket, filter)
	}
	if h == nil {
		return errors.New("mqtt: Subscribe requires a handler")
	}
	if qos > msgbus.ExactlyOnce {
		return fmt.Errorf("%w: QoS %d", ErrProtocolViolation, qos)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return msgbus.ErrClosed
	}
	conn := c.conn
	if conn == nil || !c.connected.Load() {
		c.mu.Unlock()
		return msgbus.ErrNotConnected
	}
	id, ok := c.allocIDLocked()
	if !ok {
		c.mu.Unlock()
		return errors.New("mqtt: no free packet identifier for SUBSCRIBE")
	}
	ch := make(chan []byte, 1)
	c.subWaiters[id] = ch
	// The handler is registered before the SUBSCRIBE goes out, not after the
	// SUBACK comes back. Retained messages follow the SUBACK immediately, and a
	// handler installed afterwards would miss the recovered state of the zone it
	// just subscribed to — which is the whole reason a rebooting SEC subscribes.
	previous, had := c.subs[filter]
	c.subs[filter] = &subscription{qos: qos, h: h}
	c.mu.Unlock()
	defer c.releaseID(id)

	restore := func() {
		c.mu.Lock()
		if had {
			c.subs[filter] = previous
		} else {
			delete(c.subs, filter)
		}
		c.mu.Unlock()
	}

	if !conn.send(&subscribePacket{PacketID: id, Filters: []topicFilter{{Filter: filter, QoS: qos}}}) {
		restore()
		return msgbus.ErrNotConnected
	}

	timer := time.NewTimer(c.cfg.AckTimeout)
	defer timer.Stop()
	select {
	case codes := <-ch:
		if len(codes) == 0 || codes[0] == subackFailure {
			restore()
			return fmt.Errorf("%w: broker refused subscription to %q", ErrNotAuthorized, filter)
		}
		c.mu.Lock()
		// The granted QoS, not the requested one, is what the broker will
		// honour, so it is what a resubscribe must ask for again.
		c.subs[filter] = &subscription{qos: msgbus.QoS(codes[0]), h: h}
		c.mu.Unlock()
		return nil
	case <-timer.C:
		restore()
		return msgbus.ErrTimeout
	case <-ctx.Done():
		restore()
		return ctx.Err()
	case <-c.ctx.Done():
		restore()
		return msgbus.ErrClosed
	}
}

// Unsubscribe removes a filter and waits for UNSUBACK. The handler is dropped
// even if the broker never answers, because the caller has said it no longer
// wants the traffic.
func (c *Client) Unsubscribe(ctx context.Context, filter string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return msgbus.ErrClosed
	}
	delete(c.subs, filter)
	conn := c.conn
	if conn == nil || !c.connected.Load() {
		c.mu.Unlock()
		return msgbus.ErrNotConnected
	}
	id, ok := c.allocIDLocked()
	if !ok {
		c.mu.Unlock()
		return errors.New("mqtt: no free packet identifier for UNSUBSCRIBE")
	}
	ch := make(chan struct{})
	c.unsubWaiters[id] = ch
	c.mu.Unlock()
	defer c.releaseID(id)

	if !conn.send(&unsubscribePacket{PacketID: id, Filters: []string{filter}}) {
		return msgbus.ErrNotConnected
	}
	timer := time.NewTimer(c.cfg.AckTimeout)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return msgbus.ErrTimeout
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return msgbus.ErrClosed
	}
}

// Connected reports whether the link is up right now. The SGU keys its
// autonomous-mode decision off this, so it is set from the connection's own
// lifecycle — established after CONNACK, cleared the moment the socket fails —
// and never from an optimistic guess about a reconnect in progress.
func (c *Client) Connected() bool { return c.connected.Load() }

// Close disconnects cleanly. Sending DISCONNECT is what suppresses the will, so
// a SEC that is being decommissioned does not announce its own death to the
// gateway on the way out.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	c.mu.Unlock()

	if conn != nil {
		conn.sendDisconnect()
		conn.closeWrite()
	}
	c.cancel()

	// Give the broker a moment to acknowledge the half-close with one of its
	// own, so the DISCONNECT is definitely consumed; then close regardless.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if conn != nil {
			conn.close(nil)
		}
		<-done
	}
	c.workerWG.Wait()
	c.met.setConnected(false)
	return nil
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// ---------------------------------------------------------------------------
// Handshake bookkeeping
// ---------------------------------------------------------------------------

// allocIDLocked hands out a packet identifier that no publish, subscribe or
// unsubscribe currently holds. MQTT gives all three one 16-bit space.
func (c *Client) allocIDLocked() (uint16, bool) {
	for i := 0; i < 65535; i++ {
		id := c.nextID
		c.nextID++
		if c.nextID == 0 {
			c.nextID = 1
		}
		if _, busy := c.inflight[id]; busy {
			continue
		}
		if _, busy := c.subWaiters[id]; busy {
			continue
		}
		if _, busy := c.unsubWaiters[id]; busy {
			continue
		}
		return id, true
	}
	return 0, false
}

func (c *Client) releaseID(id uint16) {
	c.mu.Lock()
	delete(c.subWaiters, id)
	delete(c.unsubWaiters, id)
	c.mu.Unlock()
}

// completeInflight finishes a handshake and wakes the blocked Publish.
func (c *Client) completeInflight(id uint16, expect outState) {
	c.mu.Lock()
	f, ok := c.inflight[id]
	if ok && f.state == expect {
		c.removeInflightLocked(id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if ok {
		close(f.done)
	}
}

// advanceToPubrel answers a PUBREC with PUBREL, the third leg of QoS 2. The
// identifier stays held until PUBCOMP so it cannot be reused while the broker
// still associates it with this message.
func (c *Client) advanceToPubrel(conn *clientConn, id uint16) {
	c.mu.Lock()
	if f, ok := c.inflight[id]; ok {
		f.state = awaitingPubcomp
	}
	c.mu.Unlock()
	conn.send(&ackPacket{Type: pktPUBREL, PacketID: id})
}

// abandon drops an in-flight message whose caller has given up, freeing the
// identifier. The broker may still complete the handshake; the late
// acknowledgement is then ignored, which is correct — nobody is waiting.
func (c *Client) abandon(id uint16) {
	c.mu.Lock()
	if _, ok := c.inflight[id]; ok {
		c.removeInflightLocked(id)
	}
	c.mu.Unlock()
}

func (c *Client) removeInflightLocked(id uint16) {
	delete(c.inflight, id)
	c.met.inflightDelta(-1)
	for i, v := range c.order {
		if v == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *Client) completeSuback(p *subackPacket) {
	c.mu.Lock()
	ch, ok := c.subWaiters[p.PacketID]
	delete(c.subWaiters, p.PacketID)
	c.mu.Unlock()
	if ok {
		ch <- p.Codes
	}
}

func (c *Client) completeUnsuback(id uint16) {
	c.mu.Lock()
	ch, ok := c.unsubWaiters[id]
	delete(c.unsubWaiters, id)
	c.mu.Unlock()
	if ok {
		close(ch)
	}
}

// claimInbound reports whether a QoS 2 packet identifier is being seen for the
// first time; false means the broker re-sent and the application has already
// had the message.
func (c *Client) claimInbound(id uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.inboundQoS2[id]; dup {
		return false
	}
	c.inboundQoS2[id] = struct{}{}
	return true
}

func (c *Client) releaseInbound(id uint16) {
	c.mu.Lock()
	delete(c.inboundQoS2, id)
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// clientConn
// ---------------------------------------------------------------------------

// clientConn is one TCP or TLS connection to a broker, with the writer and
// keepalive pinger it owns.
type clientConn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer

	// wmu serializes socket writes. The writer goroutine is not the only writer:
	// Close writes DISCONNECT inline so it cannot be lost to a racing shutdown,
	// and two goroutines interleaving bytes into one bufio.Writer would produce
	// a packet the broker could only read as garbage.
	wmu sync.Mutex
	// draining stops new packets once DISCONNECT has been written; anything
	// after it would be a protocol violation.
	draining atomic.Bool

	out            chan packet
	done           chan struct{}
	closeOnce      sync.Once
	wg             sync.WaitGroup
	sessionPresent bool
}

func newClientConn(nc net.Conn, buffer int) *clientConn {
	return &clientConn{
		nc:   nc,
		r:    bufio.NewReaderSize(nc, 4096),
		w:    bufio.NewWriterSize(nc, 4096),
		out:  make(chan packet, buffer),
		done: make(chan struct{}),
	}
}

// start launches the writer and, when the configuration asks for one, the
// keepalive pinger.
func (c *clientConn) start(keepalive time.Duration) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.writeLoop()
	}()
	if keepalive > 0 {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.pingLoop(keepalive)
		}()
	}
}

// send queues a packet. Unlike the broker's send it blocks while the buffer is
// full: there is one link and one application here, so backpressure belongs on
// the caller rather than on a disconnect.
func (c *clientConn) send(p packet) bool {
	if c.draining.Load() {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- p:
		return true
	case <-c.done:
		return false
	}
}

func (c *clientConn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case p := <-c.out:
			if err := c.writeBatch(p); err != nil {
				c.close(err)
				return
			}
		}
	}
}

func (c *clientConn) writeBatch(first packet) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.draining.Load() {
		return nil
	}
	if err := c.nc.SetWriteDeadline(time.Now().Add(defaultWriteTimeout)); err != nil {
		return err
	}
	if err := writePacket(c.w, first); err != nil {
		return err
	}
	for {
		select {
		case p := <-c.out:
			if err := writePacket(c.w, p); err != nil {
				return err
			}
		default:
			return c.w.Flush()
		}
	}
}

// pingLoop keeps the link warm. It pings every half interval rather than every
// interval so that one lost PINGREQ does not by itself trip the broker's 1.5x
// keepalive timer and take a store's labels offline.
func (c *clientConn) pingLoop(keepalive time.Duration) {
	t := time.NewTicker(keepalive / 2)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if !c.send(&emptyPacket{Type: pktPINGREQ}) {
				return
			}
		}
	}
}

// sendDisconnect writes DISCONNECT synchronously, under the write lock, so it
// cannot be lost to the close that follows it. Losing it would mean the will
// fires on a clean exit — a decommissioned SEC announcing its own death.
func (c *clientConn) sendDisconnect() {
	select {
	case <-c.done:
		return
	default:
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.draining.Swap(true) {
		return
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(time.Second))
	if err := writePacket(c.w, &emptyPacket{Type: pktDISCONNECT}); err != nil {
		return
	}
	_ = c.w.Flush()
}

// closeWrite half-closes the socket after DISCONNECT, so the broker reads the
// packet and then a clean EOF. Closing outright can reset the connection and
// lose the DISCONNECT that was the entire point of a graceful exit.
func (c *clientConn) closeWrite() {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.nc.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func (c *clientConn) close(error) {
	c.closeOnce.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

func (c *clientConn) wait() { c.wg.Wait() }
