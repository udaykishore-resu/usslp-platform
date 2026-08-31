package mqtt

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// ErrBrokerClosed is returned by Serve and by Publish after Shutdown.
var ErrBrokerClosed = errors.New("mqtt: broker closed")

// Defaults chosen for a Store Gateway Unit: one store, a few thousand sessions,
// messages measured in hundreds of bytes.
const (
	defaultMaxPacketSize    = 1 << 20
	defaultMaxInflight      = 64
	defaultOfflineQueueSize = 1024
	defaultSendBuffer       = 256
	defaultConnectTimeout   = 10 * time.Second
	defaultWriteTimeout     = 30 * time.Second
)

// Options configures a Broker. The zero value is usable — it listens on
// 127.0.0.1:1883 with AllowAll — but production deployments set Authorizer and
// TLSConfig, and the broker logs a warning when they are absent.
type Options struct {
	// Addr is the TCP address to bind, e.g. ":1883" or "127.0.0.1:0". Port 0
	// binds an ephemeral port; Start reports the one that was chosen.
	Addr string
	// TLSConfig, when non-nil, makes the listener a TLS listener. Set
	// ClientAuth to tls.RequireAndVerifyClientCert with a ClientCAs pool to get
	// mTLS, which is how production devices authenticate: the peer certificate
	// reaches the Authorizer through ConnInfo, so the device certificate is the
	// identity and there is no password to leak.
	TLSConfig *tls.Config
	// Authorizer gates connections and topics. Nil means AllowAll.
	Authorizer Authorizer
	// Registry, when non-nil, receives the broker's metrics. A registry may
	// back only one broker: obs.Registry rejects duplicate metric names.
	Registry *obs.Registry
	// Logger receives connection and protocol-error events. Nil discards them.
	Logger *obs.Logger
	// MaxPacketSize bounds an inbound packet. It is enforced before
	// authentication, so it is the limit on what an unauthenticated peer can
	// make the broker allocate.
	MaxPacketSize int
	// MaxInflight bounds un-acknowledged QoS 1/2 messages per session. Beyond
	// it, messages wait in the session queue rather than on the wire, which
	// stops one slow SEC from making the broker hold a whole store's updates.
	MaxInflight int
	// OfflineQueueSize bounds messages held for a disconnected
	// CleanSession=false session. On overflow the oldest is dropped: the newest
	// price is the correct one. See session.enqueueLocked.
	OfflineQueueSize int
	// SendBuffer is the per-connection outbound packet buffer. A client that
	// lets it fill is treated as a dead peer and disconnected, because blocking
	// on it would stall the fan-out to every other label in the store.
	SendBuffer int
	// ConnectTimeout bounds the wait for CONNECT after a socket opens, so a
	// half-open scanner cannot hold a connection slot indefinitely.
	ConnectTimeout time.Duration
	// WriteTimeout bounds one socket write.
	WriteTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:1883"
	}
	if o.Authorizer == nil {
		o.Authorizer = AllowAll{}
	}
	if o.MaxPacketSize <= 0 {
		o.MaxPacketSize = defaultMaxPacketSize
	}
	if o.MaxInflight <= 0 {
		o.MaxInflight = defaultMaxInflight
	}
	if o.OfflineQueueSize <= 0 {
		o.OfflineQueueSize = defaultOfflineQueueSize
	}
	if o.SendBuffer <= 0 {
		o.SendBuffer = defaultSendBuffer
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = defaultConnectTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	return o
}

// Broker is an MQTT 3.1.1 server.
//
// It is embedded in the Store Gateway Unit so that a store keeps operating with
// the WAN cut: SECs and labels talk to the SGU's broker, not to the cloud, and
// the cloud link is just one more client. In cloud production EMQX serves the
// same role, which is why this implementation is held to the wire specification
// rather than to whatever its own client happens to send.
type Broker struct {
	opts    Options
	log     *obs.Logger
	metrics *brokerMetrics

	subs     *subTrie
	retained *retainStore

	// mu guards session and connection lifecycle. Message-level state lives
	// behind each session's own mutex; the lock order is always mu then
	// session, and the routing path takes them one after the other, never
	// nested, so a slow session cannot block a connection from being accepted.
	mu        sync.RWMutex
	sessions  map[string]*session
	conns     map[*brokerConn]struct{}
	listeners map[net.Listener]struct{}
	closed    bool

	wg sync.WaitGroup
}

// NewBroker builds a broker. It does not bind anything; call Start or Serve.
func NewBroker(opts Options) *Broker {
	opts = opts.withDefaults()
	log := opts.Logger
	if log == nil {
		log = obs.NewLogger(obs.LogConfig{Service: "mqtt-broker", Output: io.Discard})
	}
	b := &Broker{
		opts:      opts,
		log:       log,
		metrics:   newBrokerMetrics(opts.Registry),
		subs:      newSubTrie(),
		retained:  newRetainStore(),
		sessions:  make(map[string]*session),
		conns:     make(map[*brokerConn]struct{}),
		listeners: make(map[net.Listener]struct{}),
	}
	if _, allowAll := opts.Authorizer.(AllowAll); allowAll {
		b.log.Warn("mqtt broker running without an authorizer: every client shares one topic namespace")
	}
	return b
}

// Start binds Options.Addr and accepts connections in the background, returning
// the bound address. Tests and `make dev` bind port 0 and read the address back
// from here; a long-running server can use ListenAndServe instead.
func (b *Broker) Start() (net.Addr, error) {
	l, err := b.listen()
	if err != nil {
		return nil, err
	}
	addr := l.Addr()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := b.serve(l); err != nil && !errors.Is(err, ErrBrokerClosed) {
			b.log.Error("mqtt accept loop stopped", "error", err)
		}
	}()
	return addr, nil
}

// ListenAndServe binds Options.Addr and serves until Shutdown.
func (b *Broker) ListenAndServe() error {
	l, err := b.listen()
	if err != nil {
		return err
	}
	return b.serve(l)
}

func (b *Broker) listen() (net.Listener, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBrokerClosed
	}
	b.mu.Unlock()
	l, err := net.Listen("tcp", b.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("mqtt: listen %s: %w", b.opts.Addr, err)
	}
	if b.opts.TLSConfig != nil {
		l = tls.NewListener(l, b.opts.TLSConfig)
	}
	return l, nil
}

// Serve accepts connections on l until Shutdown, and closes l on return. It is
// exported so a deployment can supply its own listener — a Unix socket for the
// SGU's co-located processes, or a listener already wrapped in a proxy-protocol
// reader.
func (b *Broker) Serve(l net.Listener) error { return b.serve(l) }

func (b *Broker) serve(l net.Listener) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		l.Close()
		return ErrBrokerClosed
	}
	b.listeners[l] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.listeners, l)
		b.mu.Unlock()
		l.Close()
	}()

	for {
		nc, err := l.Accept()
		if err != nil {
			b.mu.RLock()
			closed := b.closed
			b.mu.RUnlock()
			if closed {
				return ErrBrokerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("mqtt: accept: %w", err)
		}
		c := newBrokerConn(b, nc)
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			nc.Close()
			return ErrBrokerClosed
		}
		b.conns[c] = struct{}{}
		b.mu.Unlock()
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			c.serve()
		}()
	}
}

// Addrs reports the addresses currently being served, which a supervisor uses
// to publish the gateway's endpoint after an ephemeral bind.
func (b *Broker) Addrs() []net.Addr {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]net.Addr, 0, len(b.listeners))
	for l := range b.listeners {
		out = append(out, l.Addr())
	}
	return out
}

// Shutdown stops accepting, closes every client connection and waits for the
// handlers to finish, or until ctx expires, after which the remaining sockets
// are closed regardless.
//
// Last-will messages are deliberately not published: a planned SGU restart must
// not make every SEC in the store look like it died, which is exactly the
// false-alarm storm the will mechanism would otherwise cause during a
// maintenance window.
func (b *Broker) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	for l := range b.listeners {
		l.Close()
	}
	conns := make([]*brokerConn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()

	for _, c := range conns {
		c.suppressWill()
		c.close(nil)
	}

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Publish injects a message as though a client had sent it. The SGU uses it to
// seed retained state on start-up — the store's current prices, republished
// from local storage — so labels recover after a power cut even before the WAN
// comes back.
func (b *Broker) Publish(m msgbus.Message) error {
	if !ValidTopicName(m.Topic) {
		return fmt.Errorf("%w: %q is not a valid topic name", ErrMalformedPacket, m.Topic)
	}
	if m.QoS > msgbus.ExactlyOnce {
		return fmt.Errorf("%w: QoS %d", ErrProtocolViolation, m.QoS)
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrBrokerClosed
	}
	b.route(m)
	return nil
}

// route applies the retain rule and fans a message out to every matching
// session. It holds no lock while delivering, so a session that is slow to
// accept cannot block the publisher.
func (b *Broker) route(m msgbus.Message) {
	if m.Retain {
		n := b.retained.Store(retainedMessage{Topic: m.Topic, Payload: m.Payload, QoS: m.QoS})
		b.metrics.retainedCount(n)
	}
	targets := b.subs.Match(m.Topic)
	if len(targets) == 0 {
		return
	}
	b.mu.RLock()
	deliveries := make([]struct {
		s   *session
		qos msgbus.QoS
	}, 0, len(targets))
	for id, granted := range targets {
		if s, ok := b.sessions[id]; ok {
			deliveries = append(deliveries, struct {
				s   *session
				qos msgbus.QoS
			}{s, granted})
		}
	}
	b.mu.RUnlock()

	for _, d := range deliveries {
		out := m
		// The retain flag is set only when a message is sent because of a new
		// subscription. An existing subscriber must see Retain=false, or it
		// would mistake live traffic for recovered state.
		out.Retain = false
		out.Duplicate = false
		qos := m.QoS
		if d.qos < qos {
			qos = d.qos
		}
		d.s.deliver(out, qos)
	}
}

// deliverRetained sends the retained messages matching a newly granted filter.
// This is the power-cut recovery path: a SEC subscribing to its zone gets the
// current price of every label in it, from the broker, with no cloud round trip.
func (b *Broker) deliverRetained(s *session, filter string, granted msgbus.QoS) {
	for _, r := range b.retained.Match(filter) {
		qos := r.QoS
		if granted < qos {
			qos = granted
		}
		s.deliver(msgbus.Message{
			Topic:   r.Topic,
			Payload: r.Payload,
			QoS:     qos,
			Retain:  true,
		}, qos)
	}
}

// acquireSession resolves the session a CONNECT should use, applying the
// clean-session rule and displacing any existing connection for the same client
// ID. It runs under b.mu so that a takeover cannot interleave with the
// displaced connection's own teardown.
//
// Acquiring and attaching are separate steps because the CONNACK has to reach
// the client before any of the messages the attach releases: a subscriber that
// received a PUBLISH before its CONNACK would be looking at a protocol
// violation from us.
func (b *Broker) acquireSession(c *brokerConn, clientID string, clean bool) (*session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if old, ok := b.sessions[clientID]; ok {
		if pc, ok := old.sender().(*brokerConn); ok && pc != c {
			// MQTT 3.1.1 §3.1.4: a second connection with the same client ID
			// displaces the first. Marking it superseded stops its teardown
			// from tearing down the session we are about to hand to the new
			// connection.
			pc.supersede()
			pc.close(errors.New("session taken over by a new connection"))
		}
	}

	s, existing := b.sessions[clientID]
	present := existing && !clean && s.present
	if existing && clean {
		// The client asked for a fresh start: drop its subscriptions from the
		// routing trie before resetting the session that names them.
		for _, f := range s.filters() {
			b.subs.Unsubscribe(f, clientID)
			b.metrics.subsDelta(-1)
		}
		s.reset()
	}
	if !existing {
		s = newSession(clientID, clean, b.opts.MaxInflight, b.opts.OfflineQueueSize, b.metrics)
		b.sessions[clientID] = s
	}
	b.metrics.sessionCount(len(b.sessions))
	return s, present
}

// attachSession binds the connection to its session and releases everything the
// session was holding: in-flight messages first, with DUP set, then the offline
// queue.
func (b *Broker) attachSession(c *brokerConn, s *session, clean bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.isSuperseded() {
		// A second CONNECT for this client ID won the race while we were
		// sending the CONNACK. The session belongs to that connection now.
		return
	}
	s.attach(c, clean)
}

// endSession runs when a connection closes. A CleanSession=true session is
// discarded outright; a persistent one keeps its subscriptions and in-flight
// messages for the reconnect.
func (b *Broker) endSession(c *brokerConn) {
	s := c.sess
	if s == nil {
		return
	}
	b.mu.Lock()
	if cur, ok := b.sessions[s.clientID]; !ok || cur != s {
		b.mu.Unlock()
		return
	}
	s.detach()
	if s.clean {
		for _, f := range s.filters() {
			b.subs.Unsubscribe(f, s.clientID)
			b.metrics.subsDelta(-1)
		}
		s.reset()
		delete(b.sessions, s.clientID)
	}
	b.metrics.sessionCount(len(b.sessions))
	b.mu.Unlock()
}

func (b *Broker) removeConn(c *brokerConn) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
}

// SessionCount reports how many sessions the broker holds, connected or not.
// The SGU exports it so an operator can see persistent sessions accumulating
// for devices that were decommissioned without a clean disconnect.
func (b *Broker) SessionCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.sessions)
}

// RetainedCount reports the number of topics holding a retained value.
func (b *Broker) RetainedCount() int { return b.retained.Count() }

// newClientID invents an identifier for a client that connected with an empty
// one, which MQTT permits only alongside CleanSession. The "auto-" prefix makes
// these obvious in a session listing, since they can never be resumed.
func newClientID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mqtt: generating a client identifier: %w", err)
	}
	return "auto-" + hex.EncodeToString(buf[:]), nil
}
