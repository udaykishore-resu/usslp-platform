package mqtt

import (
	"sync"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// packetSender is the half of a connection a session needs: somewhere to put
// outbound packets. It is an interface so session logic — redelivery ordering,
// queue overflow, packet-identifier reuse — is testable without a socket.
type packetSender interface {
	// send queues a packet for the writer goroutine. It never blocks: a
	// connection whose buffer is full is a client that has stopped reading, and
	// send closes it and reports false rather than stalling the publisher that
	// is fanning a price change out to the rest of the store.
	send(p packet) bool
}

// outState is where one outbound QoS 1/2 message sits in its handshake. It
// decides what gets re-sent on reconnect: a message that has been PUBRECed must
// be resumed with PUBREL, and re-sending the PUBLISH instead would deliver the
// payload twice.
type outState int

const (
	// awaitingPuback: QoS 1 PUBLISH sent.
	awaitingPuback outState = iota
	// awaitingPubrec: QoS 2 PUBLISH sent.
	awaitingPubrec
	// awaitingPubcomp: QoS 2 PUBREL sent.
	awaitingPubcomp
)

// outbound is one message occupying a packet identifier.
type outbound struct {
	id    uint16
	msg   msgbus.Message
	state outState
}

// session is one client's state on the broker. It outlives the connection when
// CleanSession is false, which is the entire point: a SEC that reboots during a
// price change must find its subscriptions intact and the update it missed
// waiting for it, without the cloud being involved.
type session struct {
	clientID string

	mu    sync.Mutex
	clean bool
	out   packetSender
	// present marks a session that has state worth resuming, which is what the
	// CONNACK session-present flag reports.
	present bool

	subs map[string]msgbus.QoS
	will *msgbus.Will

	inflight map[uint16]*outbound
	// order preserves send order for redelivery. MQTT does not require ordering
	// across packet identifiers, but a label that applies two price updates out
	// of order shows the wrong price until the next one arrives, so the broker
	// keeps them ordered.
	order []uint16
	// inboundQoS2 holds packet identifiers of QoS 2 publications received and
	// not yet released. A redelivered PUBLISH with an identifier already here is
	// acknowledged but not routed a second time — this is what makes an OTA
	// trigger exactly-once.
	inboundQoS2 map[uint16]struct{}

	queue  []msgbus.Message
	nextID uint16

	maxInflight int
	maxQueue    int
	metrics     *brokerMetrics
}

func newSession(clientID string, clean bool, maxInflight, maxQueue int, m *brokerMetrics) *session {
	return &session{
		clientID:    clientID,
		clean:       clean,
		subs:        make(map[string]msgbus.QoS),
		inflight:    make(map[uint16]*outbound),
		inboundQoS2: make(map[uint16]struct{}),
		nextID:      1,
		maxInflight: maxInflight,
		maxQueue:    maxQueue,
		metrics:     m,
	}
}

// attach binds a live connection and resumes the session: in-flight messages
// are re-sent in order with DUP set, then the offline queue drains. Ordering
// matters — a queued update published while the client was away is newer than
// an un-acked one from before it left.
func (s *session) attach(out packetSender, clean bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = out
	s.clean = clean
	s.present = true
	s.redeliverLocked()
	s.pumpLocked()
}

// detach marks the session offline. Subsequent deliveries queue instead of
// being written to a dead socket.
func (s *session) detach() {
	s.mu.Lock()
	s.out = nil
	s.mu.Unlock()
}

// reset discards all session state. It runs when a client connects with
// CleanSession set, and when a session ends for good.
func (s *session) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics.inflightDelta(-float64(len(s.inflight)))
	s.subs = make(map[string]msgbus.QoS)
	s.inflight = make(map[uint16]*outbound)
	s.order = nil
	s.inboundQoS2 = make(map[uint16]struct{})
	s.queue = nil
	s.nextID = 1
	s.present = false
}

// sender returns the connection currently attached, or nil. The broker uses it
// to find the connection a new CONNECT for the same client ID must displace.
func (s *session) sender() packetSender {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out
}

// setWill stores the last-will, and clearWill removes it. A clean DISCONNECT
// clears the will before the connection is torn down, which is precisely how
// MQTT distinguishes "this SEC was switched off for maintenance" from "this SEC
// died" — the difference the SGU acts on.
func (s *session) setWill(w *msgbus.Will) {
	s.mu.Lock()
	s.will = w
	s.mu.Unlock()
}

func (s *session) clearWill() {
	s.mu.Lock()
	s.will = nil
	s.mu.Unlock()
}

func (s *session) takeWill() *msgbus.Will {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.will
	s.will = nil
	return w
}

// addSub records a granted subscription and reports whether it is new, so the
// broker can keep its subscription gauge honest across re-subscribes.
func (s *session) addSub(filter string, qos msgbus.QoS) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.subs[filter]
	s.subs[filter] = qos
	return !existed
}

func (s *session) removeSub(filter string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.subs[filter]
	delete(s.subs, filter)
	return existed
}

// filters snapshots the session's subscriptions, used when a session is torn
// down and its entries must come out of the shared trie.
func (s *session) filters() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.subs))
	for f := range s.subs {
		out = append(out, f)
	}
	return out
}

// receiveQoS2 records an inbound QoS 2 packet identifier and reports whether it
// is the first sight of it. false means the publisher re-sent after losing our
// PUBREC, and the message must not be routed again.
func (s *session) receiveQoS2(id uint16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.inboundQoS2[id]; dup {
		return false
	}
	s.inboundQoS2[id] = struct{}{}
	return true
}

// releaseQoS2 completes the inbound side of the four-way handshake.
func (s *session) releaseQoS2(id uint16) {
	s.mu.Lock()
	delete(s.inboundQoS2, id)
	s.mu.Unlock()
}

// deliver routes one message to this session at the given effective QoS (the
// lesser of the publication's QoS and the subscription's granted QoS).
func (s *session) deliver(m msgbus.Message, qos msgbus.QoS) {
	m.QoS = qos
	s.mu.Lock()
	defer s.mu.Unlock()
	if qos == msgbus.AtMostOnce {
		// Fire-and-forget is not queued for an absent client. At 167k telemetry
		// events per second, holding QoS 0 traffic for a device that is not
		// there would evict the price updates the queue exists to protect.
		if s.out != nil {
			s.sendPublishLocked(&outbound{msg: m}, false)
		}
		return
	}
	s.enqueueLocked(m)
	s.pumpLocked()
}

// enqueueLocked appends to the pending queue, evicting the oldest message when
// the queue is full.
//
// Overflow policy: drop-oldest. For USSLP the newest value of a topic is the
// correct one — a label that missed three price changes needs the third, not
// the first — so discarding the head keeps the queue holding the most current
// view of the world. The drop is counted under dropOverflow because a non-zero
// rate means a store is losing updates and someone must resize the queue or fix
// the device that is not coming back.
func (s *session) enqueueLocked(m msgbus.Message) {
	if len(s.queue) >= s.maxQueue {
		s.queue = s.queue[1:]
		s.metrics.drop(dropOverflow)
	}
	s.queue = append(s.queue, m)
}

// pumpLocked moves queued messages onto the wire while the connection is up and
// the in-flight window has room. The window is what stops one slow SEC from
// making the broker hold 40,000 un-acked publications for it.
func (s *session) pumpLocked() {
	for s.out != nil && len(s.queue) > 0 && len(s.inflight) < s.maxInflight {
		m := s.queue[0]
		s.queue = s.queue[1:]
		id, ok := s.allocIDLocked()
		if !ok {
			// Every identifier is in flight. Put the message back; an
			// acknowledgement will free one and pump again.
			s.queue = append([]msgbus.Message{m}, s.queue...)
			return
		}
		o := &outbound{id: id, msg: m, state: awaitingPuback}
		if m.QoS == msgbus.ExactlyOnce {
			o.state = awaitingPubrec
		}
		s.inflight[id] = o
		s.order = append(s.order, id)
		s.metrics.inflightDelta(1)
		if !s.sendPublishLocked(o, false) {
			return
		}
	}
}

// redeliverLocked re-sends everything in flight after a reconnect, with DUP set
// on the PUBLISHes as the specification requires so the receiver knows it may
// have seen the message before.
func (s *session) redeliverLocked() {
	for _, id := range s.order {
		o, ok := s.inflight[id]
		if !ok {
			continue
		}
		if o.state == awaitingPubcomp {
			// The payload was already transferred and released; only the PUBREL
			// needs repeating.
			if !s.out.send(&ackPacket{Type: pktPUBREL, PacketID: id}) {
				return
			}
			continue
		}
		if !s.sendPublishLocked(o, true) {
			return
		}
	}
}

func (s *session) sendPublishLocked(o *outbound, dup bool) bool {
	p := &publishPacket{
		Dup:      dup,
		QoS:      o.msg.QoS,
		Retain:   o.msg.Retain,
		Topic:    o.msg.Topic,
		PacketID: o.id,
		Payload:  o.msg.Payload,
	}
	if s.out == nil {
		return false
	}
	ok := s.out.send(p)
	if ok {
		s.metrics.sent(o.msg.QoS)
	}
	return ok
}

// allocIDLocked hands out the next free packet identifier. Identifiers are
// released on the final acknowledgement of their handshake, so the search only
// runs long when the in-flight window is nearly full.
func (s *session) allocIDLocked() (uint16, bool) {
	for i := 0; i < 65535; i++ {
		id := s.nextID
		s.nextID++
		if s.nextID == 0 {
			s.nextID = 1
		}
		if _, busy := s.inflight[id]; !busy {
			return id, true
		}
	}
	return 0, false
}

// puback completes a QoS 1 handshake and frees the window.
func (s *session) puback(id uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.inflight[id]; ok && o.state == awaitingPuback {
		s.releaseLocked(id)
	}
	s.pumpLocked()
}

// pubrec advances a QoS 2 handshake to its release phase and answers with
// PUBREL. The packet identifier stays allocated until PUBCOMP, which is what
// keeps a duplicate from being mistaken for a new message.
func (s *session) pubrec(id uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.inflight[id]
	if !ok {
		// A PUBREC for an unknown identifier still gets a PUBREL: the peer is
		// holding state we lost, and refusing to release it would strand the
		// identifier on its side forever.
		if s.out != nil {
			s.out.send(&ackPacket{Type: pktPUBREL, PacketID: id})
		}
		return
	}
	o.state = awaitingPubcomp
	if s.out != nil {
		s.out.send(&ackPacket{Type: pktPUBREL, PacketID: id})
	}
}

// pubcomp completes a QoS 2 handshake.
func (s *session) pubcomp(id uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.inflight[id]; ok && o.state == awaitingPubcomp {
		s.releaseLocked(id)
	}
	s.pumpLocked()
}

func (s *session) releaseLocked(id uint16) {
	delete(s.inflight, id)
	s.metrics.inflightDelta(-1)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// inflightCount and queueLen report session depth: how much this client owes an
// acknowledgement for, and how much is waiting for it to come back. They are
// what an operator looks at to tell a device that is merely slow from one that
// is gone.
func (s *session) inflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

func (s *session) queueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}
