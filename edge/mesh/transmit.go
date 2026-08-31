package mesh

import (
	"fmt"
	"time"
)

// TxRequest is one application-layer transmission between the coordinator and
// a node in its zone.
type TxRequest struct {
	// Src is the originating node. The empty value means the coordinator, which
	// is where nearly all traffic starts. A label originates only two things —
	// an acknowledgement and a telemetry report — and both are addressed to the
	// coordinator, so those are the only two shapes routing has to support.
	Src NodeID
	// Dst is the destination node.
	Dst NodeID
	// Payload is the encoded frame body. Its length drives fragmentation and
	// therefore airtime, so callers pass the bytes they would really send — a
	// price update carrying a 64-byte Ed25519 signature costs three frames per
	// hop and the model should say so.
	Payload []byte
	// Done is called exactly once, on the engine goroutine, with the outcome.
	Done func(TxResult)
}

// TxResult is the outcome of a transmission, with the timings the Shelf Edge
// Controller reports upstream in canon.LabelDelivered.
type TxResult struct {
	Delivered bool
	// Hops is the number of radio hops actually traversed.
	Hops int
	// Attempts counts MAC-layer frame transmissions across the whole path,
	// including retries. It is the honest measure of what the transmission cost
	// the shared channel and the labels' batteries.
	Attempts int
	// Elapsed is the virtual time from the moment the coordinator was asked to
	// send to the moment the destination had the frame.
	Elapsed             time.Duration
	SentAt, DeliveredAt time.Time
	Path                []NodeID
	Err                 error
	// FailedFrom and FailedTo name the link that gave up, when Err is
	// ErrLinkFailed. The controller feeds them straight into a reroute.
	FailedFrom, FailedTo NodeID
}

// txState carries one transmission through its hops.
type txState struct {
	req      TxRequest
	path     []NodeID
	hop      int
	attempts int
	macTries int
	gated    bool
	start    time.Duration
	startAt  time.Time
	seq      uint64
}

// Send transmits a payload from the coordinator to a node.
//
// It returns immediately; the outcome arrives through TxRequest.Done on the
// engine goroutine. Everything that makes a real mesh slow happens in between:
// the destination may be asleep and have to be waited for, the channel may be
// busy with another label's update, each hop may need MAC retries, and a relay
// may have died since the route was computed.
func (n *Network) Send(req TxRequest) {
	done := req.Done
	if done == nil {
		done = func(TxResult) {}
	}
	n.mu.Lock()
	n.stats.Transmissions++
	start := n.eng.Elapsed()
	startAt := n.eng.Now()
	n.frameSeq++
	seq := n.frameSeq
	formed := n.formed
	if req.Src == "" {
		req.Src = n.coord
	}
	coord := n.coord
	dstNode, known := n.nodes[req.Dst]
	srcNode, srcKnown := n.nodes[req.Src]
	n.mu.Unlock()

	fail := func(err error, from, to NodeID, hops, attempts int) {
		n.mu.Lock()
		n.stats.Failed++
		elapsed := n.eng.Elapsed() - start
		at := n.eng.Now()
		n.mu.Unlock()
		done(TxResult{Hops: hops, Attempts: attempts, Elapsed: elapsed, SentAt: startAt,
			DeliveredAt: at, Err: err, FailedFrom: from, FailedTo: to})
	}

	if !known || !srcKnown {
		n.eng.At(0, func() { fail(fmt.Errorf("%w: %q -> %q", ErrUnknownNode, req.Src, req.Dst), "", "", 0, 0) })
		return
	}
	if !formed {
		n.eng.At(0, func() { fail(ErrNotFormed, "", "", 0, 0) })
		return
	}
	if !dstNode.alive || !srcNode.alive {
		n.eng.At(0, func() { fail(fmt.Errorf("%w: %q -> %q", ErrNodeDown, req.Src, req.Dst), "", "", 0, 0) })
		return
	}
	// Routing is rooted at the coordinator, so a transmission is either
	// downstream from it or upstream to it. Peer-to-peer between two labels is
	// refused rather than approximated: USSLP has no such traffic, and
	// pretending otherwise would let a test depend on a path the firmware
	// cannot take.
	target := req.Dst
	upstream := false
	switch {
	case req.Src == coord:
	case req.Dst == coord:
		target, upstream = req.Src, true
	default:
		n.eng.At(0, func() {
			fail(fmt.Errorf("%w: %q -> %q does not involve the coordinator", ErrNoRoute, req.Src, req.Dst), "", "", 0, 0)
		})
		return
	}

	n.mu.Lock()
	path, ok := n.routeLocked(target)
	n.mu.Unlock()
	if ok && upstream {
		path = reversePath(path)
	}
	if !ok {
		// No cached route. A real coordinator floods a route request and waits
		// for a reply; that costs real time whether or not it succeeds, and
		// charging it here is what makes a partitioned label expensive rather
		// than instantly failing.
		n.mu.Lock()
		n.stats.RouteDiscoveries++
		n.mu.Unlock()
		n.eng.At(n.cfg.RouteDiscovery, func() {
			n.mu.Lock()
			n.routesDirty = true
			p, ok := n.routeLocked(target)
			n.mu.Unlock()
			if !ok {
				fail(fmt.Errorf("%w: %q", ErrNoRoute, target), "", "", 0, 0)
				return
			}
			if upstream {
				p = reversePath(p)
			}
			n.walk(&txState{req: req, path: append([]NodeID(nil), p...), start: start, startAt: startAt, seq: seq}, done)
		})
		return
	}
	n.walk(&txState{req: req, path: append([]NodeID(nil), path...), start: start, startAt: startAt, seq: seq}, done)
}

// reversePath returns a copy of a path walked the other way, which is how an
// acknowledgement gets home: the mesh is symmetric at the link layer, so the
// route the update took is the route the answer takes back.
func reversePath(p []NodeID) []NodeID {
	out := make([]NodeID, len(p))
	for i, id := range p {
		out[len(p)-1-i] = id
	}
	return out
}

// walk attempts the next hop of a transmission.
func (n *Network) walk(st *txState, done func(TxResult)) {
	if st.hop >= len(st.path)-1 {
		n.deliver(st, done)
		return
	}
	from, to := st.path[st.hop], st.path[st.hop+1]

	n.mu.Lock()
	fromNode, fok := n.nodes[from]
	toNode, tok := n.nodes[to]
	if !fok || !tok || !fromNode.alive || !toNode.alive {
		n.stats.Failed++
		n.stats.RouteRepairs++
		n.routesDirty = true
		elapsed := n.eng.Elapsed() - st.start
		at := n.eng.Now()
		n.mu.Unlock()
		n.notifyTopology()
		done(TxResult{Hops: st.hop, Attempts: st.attempts, Elapsed: elapsed, SentAt: st.startAt,
			DeliveredAt: at, Path: st.path, Err: ErrLinkFailed, FailedFrom: from, FailedTo: to})
		return
	}

	now := n.eng.Elapsed()

	// A sleeping end device is only reachable in its receive window. Waiting for
	// it must not hold the channel, so the gate is applied before the medium is
	// reserved.
	if !st.gated && to == st.req.Dst && toNode.rxGate != nil {
		gate := toNode.rxGate
		n.mu.Unlock()
		wait := gate(now)
		st.gated = true
		if wait < 0 {
			n.mu.Lock()
			n.stats.Failed++
			elapsed := n.eng.Elapsed() - st.start
			at := n.eng.Now()
			n.mu.Unlock()
			done(TxResult{Hops: st.hop, Attempts: st.attempts, Elapsed: elapsed, SentAt: st.startAt,
				DeliveredAt: at, Path: st.path, Err: fmt.Errorf("%w: %q is not listening", ErrNodeDown, to)})
			return
		}
		if wait > 0 {
			n.eng.At(wait, func() { n.walk(st, done) })
			return
		}
		n.mu.Lock()
		toNode, fromNode = n.nodes[to], n.nodes[from]
		if toNode == nil || fromNode == nil || !toNode.alive || !fromNode.alive {
			// The node went dark while we waited for its receive window.
			n.stats.Failed++
			n.routesDirty = true
			elapsed := n.eng.Elapsed() - st.start
			at := n.eng.Now()
			n.mu.Unlock()
			done(TxResult{Hops: st.hop, Attempts: st.attempts, Elapsed: elapsed, SentAt: st.startAt,
				DeliveredAt: at, Path: st.path, Err: fmt.Errorf("%w: %q", ErrNodeDown, to)})
			return
		}
		now = n.eng.Elapsed()
	}

	rssi, ok := n.rssiLocked(from, to, now)
	if !ok {
		n.stats.Failed++
		n.stats.RouteRepairs++
		n.routesDirty = true
		elapsed := n.eng.Elapsed() - st.start
		at := n.eng.Now()
		n.mu.Unlock()
		n.notifyTopology()
		done(TxResult{Hops: st.hop, Attempts: st.attempts, Elapsed: elapsed, SentAt: st.startAt,
			DeliveredAt: at, Path: st.path, Err: ErrLinkFailed, FailedFrom: from, FailedTo: to})
		return
	}

	air := Airtime(len(st.req.Payload))
	_, end := n.reserveMediumLocked(air)
	per := PacketErrorRate(rssi, n.cfg.NoiseFloorDBm)
	// A node driving an E-Ink waveform has its radio off. The frame is not
	// delayed, it is lost, and the sender only learns of it through the missing
	// acknowledgement — which is exactly a MAC retry.
	busy := toNode.busyEnd > end
	overhead := time.Duration(n.eng.Rand().Jitter(int64(n.cfg.HopOverhead), n.cfg.HopJitter))
	lost := busy || n.eng.Rand().Bernoulli(per)
	st.attempts++
	delay := end - now + overhead
	n.mu.Unlock()

	if !lost {
		st.macTries = 0
		n.eng.At(delay, func() {
			st.hop++
			n.walk(st, done)
		})
		return
	}

	st.macTries++
	n.mu.Lock()
	n.stats.MACRetries++
	retries := n.cfg.MACRetries
	retryDelay := n.cfg.MACRetryDelay
	n.mu.Unlock()

	if st.macTries <= retries {
		n.eng.At(delay+retryDelay, func() { n.walk(st, done) })
		return
	}

	// The link is gone as far as the MAC is concerned. Invalidate the route so
	// the next transmission takes a different path, and tell the caller which
	// link failed so it can reroute deliberately rather than waiting for the
	// table to be rebuilt.
	n.eng.At(delay, func() {
		n.mu.Lock()
		n.stats.Failed++
		n.stats.RouteRepairs++
		n.routesDirty = true
		elapsed := n.eng.Elapsed() - st.start
		at := n.eng.Now()
		n.mu.Unlock()
		n.notifyTopology()
		done(TxResult{Hops: st.hop, Attempts: st.attempts, Elapsed: elapsed, SentAt: st.startAt,
			DeliveredAt: at, Path: st.path, Err: ErrLinkFailed, FailedFrom: from, FailedTo: to})
	})
}

// deliver hands the frame to the destination's receiver and reports success.
func (n *Network) deliver(st *txState, done func(TxResult)) {
	n.mu.Lock()
	n.stats.Delivered++
	nd := n.nodes[st.req.Dst]
	var recv func(Frame)
	if nd != nil {
		recv = nd.recv
	}
	elapsed := n.eng.Elapsed() - st.start
	at := n.eng.Now()
	n.mu.Unlock()

	frame := Frame{
		Src:       st.path[0],
		Dst:       st.req.Dst,
		Payload:   st.req.Payload,
		Hops:      len(st.path) - 1,
		SentAt:    st.startAt,
		ArrivedAt: at,
		Sequence:  st.seq,
	}
	if recv != nil {
		recv(frame)
	}
	done(TxResult{
		Delivered:   true,
		Hops:        len(st.path) - 1,
		Attempts:    st.attempts,
		Elapsed:     elapsed,
		SentAt:      st.startAt,
		DeliveredAt: at,
		Path:        st.path,
	})
}

// reserveMediumLocked claims the shared channel for a transmission, returning
// the virtual instants at which it starts and finishes.
//
// This is the contention model, and it is deliberately a single serialising
// resource per zone. Every node on a Zigbee channel shares 250 kbps; two labels
// cannot be updated simultaneously however many goroutines the simulator has.
// It is also why a store-wide promotion is measured in minutes: the cost of
// updating a zone is the sum of its airtimes, and no amount of parallelism in
// the controller changes that.
//
// The backoff is unslotted CSMA-CA: a uniform draw over 2^BE-1 backoff periods,
// with BE starting at macMinBE (3) and rising toward macMaxBE (5) when the
// channel is already busy — the mechanism that makes a congested zone degrade
// gracefully instead of collapsing into collisions.
func (n *Network) reserveMediumLocked(air time.Duration) (start, end time.Duration) {
	now := n.eng.Elapsed()
	be := 3
	if n.mediumBusyUntil > now {
		be = 5
	}
	slots := (1 << be) - 1
	backoff := time.Duration(n.eng.Rand().IntN(slots+1)) * BackoffPeriod

	start = now + backoff
	if n.mediumBusyUntil+TurnaroundTime > start {
		start = n.mediumBusyUntil + TurnaroundTime + backoff
	}
	end = start + air
	n.mediumBusyUntil = end
	n.stats.ChannelBusy += air
	return start, end
}

// ChannelUtilisation reports the fraction of elapsed virtual time the zone's
// channel has been occupied. Above roughly 0.4 an 802.15.4 network's latency
// starts to climb sharply, so it is the first number to look at when a store's
// updates are late.
func (n *Network) ChannelUtilisation() float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	elapsed := n.eng.Elapsed()
	if elapsed <= 0 {
		return 0
	}
	return float64(n.stats.ChannelBusy) / float64(elapsed)
}
