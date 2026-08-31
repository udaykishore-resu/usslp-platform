package mqtt

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// keepaliveGrace is the specification's 1.5x multiplier: a client that has sent
// nothing for one and a half keepalive intervals is gone, and the broker closes
// the socket and fires its will. This is the mechanism the SGU relies on to
// notice a dead SEC inside 30 seconds without polling anything.
const keepaliveGrace = 3

// brokerConn is one client connection. Reading happens on the goroutine that
// calls serve; writing happens on a second goroutine draining out, so that a
// publisher fanning a price change across the store never blocks on one
// client's socket.
type brokerConn struct {
	b  *Broker
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer

	out       chan packet
	done      chan struct{}
	closeOnce sync.Once
	writerWG  sync.WaitGroup

	sess *session
	info ConnInfo

	keepalive time.Duration
	// cleanExit records a DISCONNECT. It is the single bit that decides whether
	// the will is published, so it is atomic: it is set on the read goroutine
	// and read during teardown.
	cleanExit  atomic.Bool
	superseded atomic.Bool
	willSent   atomic.Bool
	closeErr   atomic.Pointer[error]
}

func newBrokerConn(b *Broker, nc net.Conn) *brokerConn {
	return &brokerConn{
		b:    b,
		nc:   nc,
		r:    bufio.NewReaderSize(nc, 4096),
		w:    bufio.NewWriterSize(nc, 4096),
		out:  make(chan packet, b.opts.SendBuffer),
		done: make(chan struct{}),
	}
}

// send implements packetSender. It never blocks: a full buffer means the peer
// has stopped reading, which for a shelf controller means it is wedged, and the
// right response is to disconnect it so its session queues the traffic and its
// will tells the gateway something is wrong.
func (c *brokerConn) send(p packet) bool {
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
	default:
		c.b.metrics.drop(dropSlowConsumer)
		c.close(fmt.Errorf("mqtt: client send buffer full after %d packets", cap(c.out)))
		return false
	}
}

// close tears the connection down once. err, when non-nil, is the reason
// recorded for the log line written during teardown.
func (c *brokerConn) close(err error) {
	c.closeOnce.Do(func() {
		if err != nil {
			c.closeErr.Store(&err)
		}
		close(c.done)
		c.nc.Close()
	})
}

// supersede marks the connection as displaced by a newer one for the same
// client ID, so its teardown leaves the shared session alone.
func (c *brokerConn) supersede()         { c.superseded.Store(true) }
func (c *brokerConn) isSuperseded() bool { return c.superseded.Load() }

// suppressWill stops the will from being published. Used on graceful broker
// shutdown, where every connected device would otherwise be reported dead.
func (c *brokerConn) suppressWill() { c.willSent.Store(true) }

// serve runs the connection: handshake, then read packets until the peer goes
// away or breaks the protocol.
func (c *brokerConn) serve() {
	defer c.b.removeConn(c)
	defer c.close(nil)

	if err := c.handshake(); err != nil {
		c.b.log.Debug("mqtt connection rejected", "remote", c.nc.RemoteAddr().String(), "error", err.Error())
		c.close(err)
		// A handshake can fail after the session was acquired — the link can
		// die between the authorization check and the CONNACK — and a session
		// that was never attached must not be left in the broker's map holding
		// a client ID that a retry would then have to take over.
		if c.sess != nil {
			c.b.endSession(c)
		}
		c.drainWriter()
		return
	}
	c.b.metrics.clientConnected(1)

	err := c.readLoop()
	c.close(err)
	c.teardown(err)
	c.drainWriter()
	c.b.metrics.clientConnected(-1)
}

// drainWriter waits for the writer goroutine, if one was started, so that a
// CONNACK carrying a rejection reaches the client before the socket closes.
func (c *brokerConn) drainWriter() { c.writerWG.Wait() }

// handshake reads CONNECT, authenticates, and answers with CONNACK.
func (c *brokerConn) handshake() error {
	if err := c.nc.SetReadDeadline(time.Now().Add(c.b.opts.ConnectTimeout)); err != nil {
		return fmt.Errorf("mqtt: setting connect deadline: %w", err)
	}
	p, err := readPacket(c.r, c.b.opts.MaxPacketSize)
	if err != nil {
		return fmt.Errorf("mqtt: reading CONNECT: %w", err)
	}
	cp, ok := p.(*connectPacket)
	if !ok {
		c.b.metrics.connectResult("not_connect")
		return fmt.Errorf("%w: first packet was %s, not CONNECT", ErrProtocolViolation, p.pktType())
	}
	if cp.ProtocolName != protocolName || cp.ProtocolLevel != protocolLevel {
		c.b.metrics.connectResult("bad_protocol")
		c.writeSync(&connackPacket{ReturnCode: ConnectUnacceptableProto})
		return fmt.Errorf("%w: protocol %q level %d", ErrProtocolViolation, cp.ProtocolName, cp.ProtocolLevel)
	}

	clientID := cp.ClientID
	if clientID == "" {
		// An empty client ID is only meaningful with CleanSession: there would
		// be nothing to resume it by.
		if !cp.CleanSession {
			c.b.metrics.connectResult("id_rejected")
			c.writeSync(&connackPacket{ReturnCode: ConnectIdentifierRejected})
			return fmt.Errorf("%w: empty client identifier without CleanSession", ErrProtocolViolation)
		}
		if clientID, err = newClientID(); err != nil {
			c.b.metrics.connectResult("id_rejected")
			c.writeSync(&connackPacket{ReturnCode: ConnectServerUnavailable})
			return err
		}
	}

	c.info = ConnInfo{
		ClientID:   clientID,
		Username:   cp.Username,
		Password:   cp.Password,
		RemoteAddr: c.nc.RemoteAddr(),
	}
	if tc, ok := c.nc.(*tls.Conn); ok {
		// The handshake has already completed: readPacket forced it by reading.
		state := tc.ConnectionState()
		c.info.TLS = &state
		if len(state.PeerCertificates) > 0 {
			c.info.PeerCertificate = state.PeerCertificates[0]
		}
	}

	if err := c.authenticate(); err != nil {
		code := ConnectBadCredentials
		if errors.Is(err, ErrNotAuthorized) {
			code = ConnectNotAuthorized
		}
		c.b.metrics.connectResult("unauthenticated")
		c.writeSync(&connackPacket{ReturnCode: code})
		return err
	}

	var will *msgbus.Will
	if cp.WillFlag {
		if !ValidTopicName(cp.WillTopic) {
			c.b.metrics.connectResult("bad_will")
			c.writeSync(&connackPacket{ReturnCode: ConnectNotAuthorized})
			return fmt.Errorf("%w: will topic %q", ErrMalformedPacket, cp.WillTopic)
		}
		// The will is checked now, against the connecting credential. Otherwise
		// a client could have the broker publish, on its behalf and after it is
		// gone, to a topic it was never allowed to publish to itself.
		if err := c.authorize(cp.WillTopic, ActionPublish); err != nil {
			c.b.metrics.connectResult("unauthorized_will")
			c.writeSync(&connackPacket{ReturnCode: ConnectNotAuthorized})
			return err
		}
		will = &msgbus.Will{
			Topic:   cp.WillTopic,
			Payload: cp.WillMessage,
			QoS:     cp.WillQoS,
			Retain:  cp.WillRetain,
		}
	}

	c.keepalive = time.Duration(cp.KeepAlive) * time.Second

	sess, present := c.b.acquireSession(c, clientID, cp.CleanSession)
	c.sess = sess
	sess.setWill(will)

	c.startWriter()
	if !c.send(&connackPacket{SessionPresent: present, ReturnCode: ConnectAccepted}) {
		return errors.New("mqtt: connection closed before CONNACK")
	}
	c.b.attachSession(c, sess, cp.CleanSession)
	c.b.metrics.connectResult("accepted")
	c.b.log.Debug("mqtt client connected",
		"client_id", clientID, "clean_session", cp.CleanSession,
		"session_present", present, "keepalive_s", cp.KeepAlive,
		"remote", c.nc.RemoteAddr().String(), "subject", c.info.Subject())
	return nil
}

// writeSync writes one packet straight to the socket, used for the CONNACK that
// rejects a connection before the writer goroutine exists.
func (c *brokerConn) writeSync(p packet) {
	_ = c.nc.SetWriteDeadline(time.Now().Add(c.b.opts.WriteTimeout))
	if err := writePacket(c.w, p); err == nil {
		_ = c.w.Flush()
	}
}

func (c *brokerConn) authenticate() error {
	if a, ok := c.b.opts.Authorizer.(CertAuthenticator); ok {
		return a.AuthenticateConn(c.info)
	}
	return c.b.opts.Authorizer.Authenticate(c.info.ClientID, c.info.Username, c.info.Password)
}

func (c *brokerConn) authorize(topic string, action Action) error {
	if a, ok := c.b.opts.Authorizer.(CertAuthorizer); ok {
		return a.AuthorizeConn(c.info, topic, action)
	}
	return c.b.opts.Authorizer.Authorize(c.info.ClientID, c.info.Username, topic, action)
}

// startWriter runs the goroutine that owns the socket's write side. It batches:
// after writing one packet it drains whatever else is queued before flushing,
// which turns a fan-out burst into one syscall.
func (c *brokerConn) startWriter() {
	c.writerWG.Add(1)
	go func() {
		defer c.writerWG.Done()
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
	}()
}

func (c *brokerConn) writeBatch(first packet) error {
	if err := c.nc.SetWriteDeadline(time.Now().Add(c.b.opts.WriteTimeout)); err != nil {
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

// readLoop consumes packets until the peer disconnects, stops talking for
// longer than its keepalive allows, or breaks the protocol.
func (c *brokerConn) readLoop() error {
	for {
		if err := c.setReadDeadline(); err != nil {
			return err
		}
		p, err := readPacket(c.r, c.b.opts.MaxPacketSize)
		if err != nil {
			return err
		}
		if err := c.handle(p); err != nil {
			return err
		}
		if c.cleanExit.Load() {
			return nil
		}
	}
}

func (c *brokerConn) setReadDeadline() error {
	if c.keepalive <= 0 {
		// Keepalive 0 disables the timer, as the specification says it must.
		// The SGU's own cloud link uses it; devices never do.
		return c.nc.SetReadDeadline(time.Time{})
	}
	return c.nc.SetReadDeadline(time.Now().Add(c.keepalive * keepaliveGrace / 2))
}

func (c *brokerConn) handle(p packet) error {
	switch pk := p.(type) {
	case *publishPacket:
		return c.handlePublish(pk)
	case *ackPacket:
		return c.handleAck(pk)
	case *subscribePacket:
		return c.handleSubscribe(pk)
	case *unsubscribePacket:
		return c.handleUnsubscribe(pk)
	case *emptyPacket:
		switch pk.Type {
		case pktPINGREQ:
			c.send(&emptyPacket{Type: pktPINGRESP})
			return nil
		case pktDISCONNECT:
			// A clean DISCONNECT means "I am going away on purpose": drop the
			// will so the gateway does not raise an alarm for a device that was
			// switched off deliberately.
			c.sess.clearWill()
			c.cleanExit.Store(true)
			return nil
		default:
			return fmt.Errorf("%w: client sent %s", ErrProtocolViolation, pk.Type)
		}
	case *connectPacket:
		return fmt.Errorf("%w: second CONNECT on an established connection", ErrProtocolViolation)
	default:
		return fmt.Errorf("%w: client sent %s", ErrProtocolViolation, p.pktType())
	}
}

func (c *brokerConn) handlePublish(p *publishPacket) error {
	if !ValidTopicName(p.Topic) {
		return fmt.Errorf("%w: PUBLISH to %q", ErrMalformedPacket, p.Topic)
	}
	if err := c.authorize(p.Topic, ActionPublish); err != nil {
		// MQTT 3.1.1 has no way to refuse a publication in-band. Closing is the
		// specification's own suggested response, and it is the right one here:
		// a device publishing outside its tenant is either mis-provisioned or
		// hostile, and either way should stop.
		return err
	}
	c.b.metrics.received(p.QoS)
	m := msgbus.Message{
		Topic:      p.Topic,
		Payload:    p.Payload,
		QoS:        p.QoS,
		Retain:     p.Retain,
		ReceivedAt: time.Now(),
	}
	switch p.QoS {
	case msgbus.AtMostOnce:
		c.b.route(m)
	case msgbus.AtLeastOnce:
		c.b.route(m)
		c.send(&ackPacket{Type: pktPUBACK, PacketID: p.PacketID})
	case msgbus.ExactlyOnce:
		// Route on first sight only. A redelivery after a lost PUBREC is
		// acknowledged again but not routed again, which is what keeps an OTA
		// trigger from starting two firmware downloads.
		if c.sess.receiveQoS2(p.PacketID) {
			c.b.route(m)
		}
		c.send(&ackPacket{Type: pktPUBREC, PacketID: p.PacketID})
	}
	return nil
}

func (c *brokerConn) handleAck(p *ackPacket) error {
	switch p.Type {
	case pktPUBACK:
		c.sess.puback(p.PacketID)
	case pktPUBREC:
		c.sess.pubrec(p.PacketID)
	case pktPUBREL:
		c.sess.releaseQoS2(p.PacketID)
		c.send(&ackPacket{Type: pktPUBCOMP, PacketID: p.PacketID})
	case pktPUBCOMP:
		c.sess.pubcomp(p.PacketID)
	default:
		return fmt.Errorf("%w: client sent %s", ErrProtocolViolation, p.Type)
	}
	return nil
}

func (c *brokerConn) handleSubscribe(p *subscribePacket) error {
	codes := make([]byte, len(p.Filters))
	granted := make([]topicFilter, 0, len(p.Filters))
	for i, f := range p.Filters {
		if !ValidTopicFilter(f.Filter) {
			return fmt.Errorf("%w: SUBSCRIBE to %q", ErrMalformedPacket, f.Filter)
		}
		if err := c.authorize(f.Filter, ActionSubscribe); err != nil {
			// A refused filter is reported per-filter rather than by closing
			// the connection: SUBACK has a failure code for exactly this, and a
			// SEC that asks for one filter it may not have should still get the
			// ones it may.
			codes[i] = subackFailure
			c.b.log.Warn("mqtt subscribe denied",
				"client_id", c.info.ClientID, "filter", f.Filter, "error", err.Error())
			continue
		}
		codes[i] = byte(f.QoS)
		if c.sess.addSub(f.Filter, f.QoS) {
			c.b.metrics.subsDelta(1)
		}
		c.b.subs.Subscribe(f.Filter, c.info.ClientID, f.QoS)
		granted = append(granted, f)
	}
	c.send(&subackPacket{PacketID: p.PacketID, Codes: codes})
	// Retained messages follow the SUBACK, per the specification's ordering.
	for _, f := range granted {
		c.b.deliverRetained(c.sess, f.Filter, f.QoS)
	}
	return nil
}

func (c *brokerConn) handleUnsubscribe(p *unsubscribePacket) error {
	for _, f := range p.Filters {
		if !ValidTopicFilter(f) {
			return fmt.Errorf("%w: UNSUBSCRIBE from %q", ErrMalformedPacket, f)
		}
		if c.sess.removeSub(f) {
			c.b.metrics.subsDelta(-1)
		}
		c.b.subs.Unsubscribe(f, c.info.ClientID)
	}
	c.send(&ackPacket{Type: pktUNSUBACK, PacketID: p.PacketID})
	return nil
}

// teardown ends the session and fires the will if the client did not leave
// cleanly.
func (c *brokerConn) teardown(cause error) {
	if c.sess == nil {
		return
	}
	will := c.sess.takeWill()
	if !c.isSuperseded() {
		c.b.endSession(c)
	}
	if c.cleanExit.Load() || will == nil {
		return
	}
	if c.willSent.Swap(true) {
		return
	}
	c.logDisconnect(cause)
	// Published after the session teardown and outside b.mu: routing takes the
	// broker's read lock, and the will must not be able to deadlock against the
	// teardown that produced it.
	c.b.route(msgbus.Message{
		Topic:      will.Topic,
		Payload:    will.Payload,
		QoS:        will.QoS,
		Retain:     will.Retain,
		ReceivedAt: time.Now(),
	})
}

func (c *brokerConn) logDisconnect(cause error) {
	if cause == nil {
		if p := c.closeErr.Load(); p != nil {
			cause = *p
		}
	}
	reason := "connection closed"
	switch {
	case cause == nil, errors.Is(cause, io.EOF), errors.Is(cause, net.ErrClosed):
	default:
		reason = cause.Error()
	}
	c.b.log.Info("mqtt client lost, publishing will",
		"client_id", c.info.ClientID, "reason", reason)
}
