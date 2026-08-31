package mesh

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/sim"
)

// Errors a transmission or a topology operation can report. They are sentinels
// because the Shelf Edge Controller reacts differently to each: no route means
// repair the mesh, unreachable means the label may be dead, and busy means try
// again in a moment.
var (
	// ErrNoRoute means routing could not find a path to the destination even
	// after discovery. It is the mesh saying the label is partitioned, not that
	// a frame was lost.
	ErrNoRoute = errors.New("mesh: no route to destination")
	// ErrNodeDown means the destination is not participating in the network.
	ErrNodeDown = errors.New("mesh: destination node is down")
	// ErrLinkFailed means a hop exhausted its MAC retries. The link, not the
	// destination, is the problem, and the route should be repaired.
	ErrLinkFailed = errors.New("mesh: link failed after MAC retries")
	// ErrUnknownNode is a programming error: something addressed a node that
	// was never added to the network.
	ErrUnknownNode = errors.New("mesh: unknown node")
	// ErrNotFormed means the network has not been formed yet.
	ErrNotFormed = errors.New("mesh: network not formed")
)

// NodeID identifies one radio in the mesh. For labels it is the canon.LabelID;
// for the coordinator it is the canon.SECID, because the Shelf Edge
// Controller's radio *is* the Zigbee coordinator of its zone.
type NodeID string

// NodeKind is a node's role in the mesh, which determines whether it may relay.
type NodeKind int

const (
	// KindCoordinator is the single PAN coordinator: the SEC's radio. It forms
	// the network, hands out addresses and is the root of the tree.
	KindCoordinator NodeKind = iota
	// KindRouter is a mains-powered node — a shelf rail power module, a large
	// display label wired into the gondola — that keeps its receiver on and
	// relays for others. A battery label can never be one: relaying means never
	// sleeping, and never sleeping means a flat cell in a fortnight.
	KindRouter
	// KindEndDevice is a battery-powered label. It sleeps, wakes on its own
	// cadence, has a parent and never relays.
	KindEndDevice
)

// String names the kind for topology reports and logs.
func (k NodeKind) String() string {
	switch k {
	case KindCoordinator:
		return "coordinator"
	case KindRouter:
		return "router"
	default:
		return "end-device"
	}
}

// Point is a position on the shop floor in metres. Two dimensions is enough:
// shelf labels live on a plane, and modelling the half-metre of vertical spread
// within a gondola would add precision the path-loss model does not have.
type Point struct{ X, Y float64 }

// Distance returns the separation of two points in metres.
func (p Point) Distance(o Point) float64 {
	dx, dy := p.X-o.X, p.Y-o.Y
	return math.Sqrt(dx*dx + dy*dy)
}

// NodeSpec describes a node to be added to the network.
type NodeSpec struct {
	ID   NodeID
	Kind NodeKind
	Pos  Point
	// BatteryFraction is the node's remaining charge in [0,1]. The mesh itself
	// does not consume it — labelsim owns the power model — but routing and the
	// controller's failure predictor both use it, because a label about to go
	// flat is a link about to disappear.
	BatteryFraction float64
}

// node is the network's private view of a radio.
type node struct {
	spec    NodeSpec
	alive   bool
	joined  bool
	parent  NodeID
	depth   int
	kids    int
	recv    func(Frame)
	rxGate  func(now time.Duration) time.Duration
	busyEnd time.Duration
}

// linkKey identifies an unordered pair of nodes.
type linkKey struct{ a, b NodeID }

func makeLinkKey(a, b NodeID) linkKey {
	if a > b {
		a, b = b, a
	}
	return linkKey{a, b}
}

// link is the per-pair radio state that is not derivable from geometry.
type link struct {
	shadowDB float64
	extraDB  float64
	// ramp, when set, moves extraDB linearly from one value to another over a
	// window. It is the fault-injection primitive that produces a *degrading*
	// link rather than a broken one, which is the only scenario in which
	// predicting failure can beat reacting to it.
	ramp *linkRamp
	// fade, when set, adds a periodic deep null to the link. It is what
	// multipath in a busy aisle actually looks like: the mean is fine and the
	// link is unusable for a second at a time, several times a minute.
	fade *linkFade
	// avoided marks a link the controller has asked routing to route around.
	// It is not cut — frames still traverse it if there is no alternative —
	// but its cost is raised far above any healthy path.
	avoided bool
	cut     bool
}

// linkFade is a periodic multipath null.
type linkFade struct {
	amplitudeDB float64
	period      time.Duration
}

// at returns the extra attenuation from fading at an instant.
//
// The shape is a raised sine cubed rather than a plain sinusoid because that is
// what a multipath null looks like: mostly nothing, then a sharp, deep and
// brief collapse as the standing-wave pattern moves. A sinusoid would spread
// the same energy evenly and produce a link that is uniformly mediocre, which
// is a different and much easier failure to detect.
func (f *linkFade) at(now time.Duration) float64 {
	if f.period <= 0 {
		return 0
	}
	phase := 2 * math.Pi * float64(now%f.period) / float64(f.period)
	s := math.Sin(phase)
	if s < 0 {
		return 0
	}
	return f.amplitudeDB * s * s * s
}

type linkRamp struct {
	fromDB, toDB float64
	start, end   time.Duration
}

func (r *linkRamp) at(now time.Duration) float64 {
	switch {
	case now <= r.start:
		return r.fromDB
	case now >= r.end || r.end <= r.start:
		return r.toDB
	default:
		f := float64(now-r.start) / float64(r.end-r.start)
		return r.fromDB + f*(r.toDB-r.fromDB)
	}
}

// Config parameterises a network. The zero value is usable: Defaults fills
// every field with the figure justified in radio.go or below.
type Config struct {
	// PANID is the personal-area-network identifier. Two controllers in
	// adjacent aisles must differ, or their labels will try to join each other.
	PANID uint16
	// Channel is the 802.15.4 channel, 11–26. Zones are planned onto
	// non-overlapping channels; nodes only contend with others on their own.
	Channel int
	// HopOverhead is the fixed per-hop cost that is not airtime: radio
	// turnaround, MAC processing on a Cortex-M0, a routing table lookup, and
	// the store-and-forward delay of a relay that has to receive a whole frame
	// before it can start sending it. With a single-fragment frame's airtime on
	// top this produces the ~15 ms per hop the platform's latency budget
	// assumes.
	HopOverhead time.Duration
	// HopJitter is the fractional variation applied to HopOverhead.
	HopJitter float64
	// MACRetries is macMaxFrameRetries: how many times the MAC re-sends a frame
	// that was not acknowledged before giving up and reporting a link failure.
	MACRetries int
	// MACRetryDelay is the wait before a MAC retry.
	MACRetryDelay time.Duration
	// MaxDepth bounds the tree. Zigbee's default network radius is 5; a deeper
	// tree would blow the platform's SEC-to-label budget (INTERFACE-CONTRACTS
	// §4) on hop latency alone.
	MaxDepth int
	// MaxChildren bounds how many nodes one router will parent.
	MaxChildren int
	// JoinWindow is the spread over which nodes make their first association
	// attempt after hearing a beacon. Randomising it is what stops five hundred
	// labels from colliding on the same association slot.
	JoinWindow time.Duration
	// JoinRetry is the backoff before a node that found no eligible parent
	// tries again.
	JoinRetry time.Duration
	// JoinExchange is the airtime-bearing cost of one association: beacon
	// request, beacon, association request, association response.
	JoinExchange time.Duration
	// RouteDiscovery is how long an on-demand route request/reply round trip
	// takes across the zone.
	RouteDiscovery time.Duration
	// NoiseFloorDBm is the current noise floor. Raising it is how interference
	// is injected.
	NoiseFloorDBm float64
	// ShadowSigmaDB overrides the shadow-fading spread.
	ShadowSigmaDB float64
	// RSSINoiseDB is the per-sample measurement noise on a reported RSSI. Real
	// LQI readings jitter; a predictor trained on noiseless input would be
	// useless on hardware.
	RSSINoiseDB float64
	// MaxRangeM bounds neighbour discovery.
	MaxRangeM float64
}

// Defaults fills unset fields with the platform's figures.
func (c Config) Defaults() Config {
	if c.Channel == 0 {
		c.Channel = 15
	}
	if c.HopOverhead == 0 {
		c.HopOverhead = 10 * time.Millisecond
	}
	if c.HopJitter == 0 {
		c.HopJitter = 0.2
	}
	if c.MACRetries == 0 {
		c.MACRetries = 3
	}
	if c.MACRetryDelay == 0 {
		c.MACRetryDelay = 25 * time.Millisecond
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = 5
	}
	if c.MaxChildren == 0 {
		// Zigbee's own tree-addressing default is around twenty children, which
		// is sized for a home automation network. An electronic-shelf-label
		// relay is a purpose-built device whose only job is to parent labels
		// that transmit for a few milliseconds a day, and vendors size them in
		// the hundreds. Sixty-four is the conservative middle: enough that a
		// zone of five hundred labels needs eight relays, few enough that one
		// relay dying does not black out an aisle.
		c.MaxChildren = 64
	}
	if c.JoinWindow == 0 {
		c.JoinWindow = 8 * time.Second
	}
	if c.JoinRetry == 0 {
		c.JoinRetry = 2500 * time.Millisecond
	}
	if c.JoinExchange == 0 {
		c.JoinExchange = 40 * time.Millisecond
	}
	if c.RouteDiscovery == 0 {
		c.RouteDiscovery = 400 * time.Millisecond
	}
	if c.NoiseFloorDBm == 0 {
		c.NoiseFloorDBm = NoiseFloorDBm
	}
	if c.ShadowSigmaDB == 0 {
		c.ShadowSigmaDB = ShadowSigmaDB
	}
	if c.RSSINoiseDB == 0 {
		c.RSSINoiseDB = 1.5
	}
	if c.MaxRangeM == 0 {
		c.MaxRangeM = MaxLinkRangeM
	}
	return c
}

// Frame is what a receiver is handed when a transmission arrives.
type Frame struct {
	Src, Dst NodeID
	Payload  []byte
	// Hops is how many radio hops the frame actually traversed, which is the
	// number the controller reports in canon.LabelDelivered.MeshHops.
	Hops int
	// SentAt and ArrivedAt are virtual instants from the simulation clock.
	SentAt, ArrivedAt time.Time
	// Sequence is the network-layer frame counter, used only for tracing.
	Sequence uint64
}

// Stats is a point-in-time view of network health, exported on the SEC's
// diagnostics page and asserted on by the mesh tests.
type Stats struct {
	Nodes, Alive, Joined int
	Transmissions        uint64
	Delivered            uint64
	Failed               uint64
	MACRetries           uint64
	RouteDiscoveries     uint64
	RouteRepairs         uint64
	// ChannelBusy is total airtime consumed. Compared against elapsed time it
	// is the zone's channel utilisation, the number that explains why a
	// store-wide promotion takes minutes rather than milliseconds.
	ChannelBusy time.Duration
}

// NodeStatus is one node's place in the mesh, as reported upward.
type NodeStatus struct {
	ID     NodeID
	Parent NodeID
	Depth  int
	LQI    int
	RSSI   int
	Kind   NodeKind
	Online bool
	// Battery is the fraction of charge remaining, as last reported.
	Battery float64
}

// LinkSample is one measurement of a neighbour link, as an 802.15.4 MAC would
// report it after a frame exchange.
type LinkSample struct {
	Peer NodeID
	RSSI float64
	LQI  int
	At   time.Time
}

// Network is one Zigbee personal area network: the labels in a single Shelf
// Edge Controller's zone plus the controller's own radio.
//
// It is safe for concurrent use, because a controller runs its own goroutines
// and injects transmissions from them, but every callback it makes — receivers,
// transmission completions — runs on whichever goroutine is stepping the
// engine, with no lock held.
type Network struct {
	mu  sync.Mutex
	cfg Config
	eng *sim.Engine

	coord NodeID
	nodes map[NodeID]*node
	links map[linkKey]*link
	// neighbours is the static adjacency derived from geometry. Positions do
	// not move, so it is computed once; everything time-varying lives in link.
	neighbours map[NodeID][]NodeID

	routes map[NodeID][]NodeID
	// routesDirty defers route computation until something actually needs a
	// route. Five hundred labels joining during a rebuild would otherwise run
	// Dijkstra five hundred times to answer a question nobody asked yet.
	routesDirty bool
	formed      bool
	formSeq     uint64

	// mediumBusyUntil is the single shared channel. One value, because every
	// node in a zone is on one channel and — modulo hidden terminals, which
	// this model does not attempt — they all contend with one another.
	mediumBusyUntil time.Duration

	frameSeq uint64
	stats    Stats

	onTopologyChange func()
}

// NewNetwork creates an empty network bound to a simulation engine.
func NewNetwork(eng *sim.Engine, cfg Config) *Network {
	return &Network{
		cfg:        cfg.Defaults(),
		eng:        eng,
		nodes:      make(map[NodeID]*node),
		links:      make(map[linkKey]*link),
		neighbours: make(map[NodeID][]NodeID),
		routes:     make(map[NodeID][]NodeID),
	}
}

// Engine returns the simulation engine driving the network, so a caller that
// holds only a *Network can schedule its own work on the same clock.
func (n *Network) Engine() *sim.Engine { return n.eng }

// Config returns the network's effective configuration.
func (n *Network) Config() Config { return n.cfg }

// AddNode registers a radio. The first coordinator added becomes the root.
//
// Adding a node does not join it: joining is what Form does, and keeping the
// two apart is what lets a test add five hundred labels and then measure how
// long the network takes to come up.
func (n *Network) AddNode(spec NodeSpec) error {
	if spec.ID == "" {
		return fmt.Errorf("%w: empty node id", ErrUnknownNode)
	}
	if spec.BatteryFraction <= 0 {
		spec.BatteryFraction = 1
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, dup := n.nodes[spec.ID]; dup {
		return fmt.Errorf("mesh: node %q already present", spec.ID)
	}
	nd := &node{spec: spec, alive: true}
	n.nodes[spec.ID] = nd
	if spec.Kind == KindCoordinator {
		if n.coord != "" {
			return fmt.Errorf("mesh: network already has coordinator %q", n.coord)
		}
		n.coord = spec.ID
		nd.joined = true
		nd.depth = 0
	}
	// Extend the adjacency incrementally, in a deterministic order, so that the
	// neighbour table a node ends up with does not depend on Go's map
	// iteration. Peers are considered nearest first and the table is trimmed to
	// MaxNeighbours, which is what a real 802.15.4 stack with 32 KB of RAM does.
	candidates := make([]NodeID, 0, len(n.nodes))
	for id := range n.nodes {
		if id == spec.ID {
			continue
		}
		if n.nodes[id].spec.Pos.Distance(spec.Pos) > n.cfg.MaxRangeM {
			continue
		}
		candidates = append(candidates, id)
	}
	sort.Slice(candidates, func(i, j int) bool {
		di := n.nodes[candidates[i]].spec.Pos.Distance(spec.Pos)
		dj := n.nodes[candidates[j]].spec.Pos.Distance(spec.Pos)
		if di != dj {
			return di < dj
		}
		return candidates[i] < candidates[j]
	})
	for _, id := range candidates {
		n.neighbours[spec.ID] = append(n.neighbours[spec.ID], id)
		n.neighbours[id] = append(n.neighbours[id], spec.ID)
		n.links[makeLinkKey(spec.ID, id)] = &link{
			shadowDB: n.eng.Rand().Normal(0, n.cfg.ShadowSigmaDB),
		}
		n.trimNeighboursLocked(id)
	}
	n.trimNeighboursLocked(spec.ID)
	n.routesDirty = true
	return nil
}

// Coordinator returns the coordinator's node ID.
func (n *Network) Coordinator() NodeID {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.coord
}

// SetReceiver installs the callback invoked when a frame arrives for a node.
// It runs on the engine goroutine with no network lock held, so a receiver may
// send, sleep or reschedule freely.
func (n *Network) SetReceiver(id NodeID, fn func(Frame)) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, id)
	}
	nd.recv = fn
	return nil
}

// SetRxGate installs a function reporting how long the network must wait before
// the node can receive.
//
// This is how a sleeping end device's duty cycle reaches the mesh without
// putting 160,000 beacon events per simulated second into the queue: the label
// answers "my next receive window is 118 ms away" arithmetically, and the mesh
// defers the final hop by that much. Returning zero means the node is listening
// now; returning a negative value means it cannot receive at all.
func (n *Network) SetRxGate(id NodeID, fn func(now time.Duration) time.Duration) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, id)
	}
	nd.rxGate = fn
	return nil
}

// SetBusyUntil marks a node unable to receive until a virtual instant.
//
// A label driving an E-Ink waveform is exactly this: the display controller
// owns the SPI bus and the radio is off, so a frame arriving mid-refresh is not
// slowed down, it is lost, and the sender sees a missing acknowledgement. That
// behaviour is a real source of retries in deployed fleets and is modelled
// rather than smoothed away.
func (n *Network) SetBusyUntil(id NodeID, until time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd, ok := n.nodes[id]; ok {
		nd.busyEnd = until
	}
}

// SetBattery updates a node's remaining charge fraction, which feeds routing
// preference and the controller's failure predictor.
func (n *Network) SetBattery(id NodeID, frac float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd, ok := n.nodes[id]; ok {
		nd.spec.BatteryFraction = frac
	}
}

// OnTopologyChange registers a callback fired whenever the tree or the routing
// table changes: a join, a death, a repair. The Shelf Edge Controller uses it
// to publish canon.MeshTopology without polling.
func (n *Network) OnTopologyChange(fn func()) {
	n.mu.Lock()
	n.onTopologyChange = fn
	n.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Link budget
// ---------------------------------------------------------------------------

// rssiLocked returns the deterministic mean received power for a link, with no
// measurement noise. Callers holding the lock use it for routing decisions,
// where a jittering metric would make routes flap.
func (n *Network) rssiLocked(a, b NodeID, now time.Duration) (float64, bool) {
	na, ok := n.nodes[a]
	if !ok {
		return 0, false
	}
	nb, ok := n.nodes[b]
	if !ok {
		return 0, false
	}
	l, ok := n.links[makeLinkKey(a, b)]
	if !ok || l.cut {
		return 0, false
	}
	extra := l.extraDB
	if l.ramp != nil {
		extra = l.ramp.at(now)
	}
	if l.fade != nil {
		extra += l.fade.at(now)
	}
	d := na.spec.Pos.Distance(nb.spec.Pos)
	return TxPowerDBm - PathLossDB(d) + l.shadowDB - extra, true
}

// RSSI reports the current mean received power for a link in dBm.
func (n *Network) RSSI(a, b NodeID) (float64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rssiLocked(a, b, n.eng.Elapsed())
}

// SampleLinks returns one measurement per live neighbour of a node, with
// measurement noise applied, as the MAC would report after a beacon exchange.
// It is what the Shelf Edge Controller's 30-second LQI sampler calls.
func (n *Network) SampleLinks(id NodeID) []LinkSample {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.eng.Elapsed()
	at := n.eng.Now()
	peers := n.neighbours[id]
	out := make([]LinkSample, 0, len(peers))
	for _, p := range peers {
		pn, ok := n.nodes[p]
		if !ok || !pn.alive {
			continue
		}
		mean, ok := n.rssiLocked(id, p, now)
		if !ok {
			continue
		}
		measured := mean + n.eng.Rand().Normal(0, n.cfg.RSSINoiseDB)
		out = append(out, LinkSample{Peer: p, RSSI: measured, LQI: LQIFromRSSI(measured), At: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

// ---------------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------------

// KillNode takes a node off the air, as a flat battery, a shoplifted label or a
// tripped breaker would. Every route through it is invalidated and repaired.
func (n *Network) KillNode(id NodeID) {
	n.mu.Lock()
	nd, ok := n.nodes[id]
	if ok && nd.alive {
		nd.alive = false
		nd.joined = false
		nd.parent = ""
		// Orphan its children: in a real network they lose their parent and
		// rejoin, which is exactly the disruption the platform has to survive.
		for _, other := range n.nodes {
			if other.parent == id {
				other.parent = ""
				other.joined = false
				other.depth = 0
			}
		}
		n.recomputeRoutesLocked()
	}
	n.mu.Unlock()
	n.notifyTopology()
}

// ReviveNode puts a node back on the air. It is not rejoined: it has to
// re-associate, which is what Rejoin schedules.
func (n *Network) ReviveNode(id NodeID) {
	n.mu.Lock()
	if nd, ok := n.nodes[id]; ok {
		nd.alive = true
	}
	n.mu.Unlock()
}

// DegradeLink adds a fixed attenuation to one link, modelling an obstruction
// appearing between two nodes.
func (n *Network) DegradeLink(a, b NodeID, extraDB float64) {
	n.mu.Lock()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		l.extraDB = extraDB
		l.ramp = nil
	}
	n.mu.Unlock()
	n.RecomputeRoutes()
}

// RampLink moves a link's attenuation linearly from one value to another over a
// window starting now.
//
// This is the scenario the predictive healer exists for. A link that fails
// instantly cannot be predicted by anything; a link that degrades over four
// minutes — a promotional gondola wheeled into an aisle, a freezer door left
// open, a label's cell sagging under load — can be, and the difference between
// predicting it and reacting to it is the difference between zero missed price
// updates and several minutes of them.
func (n *Network) RampLink(a, b NodeID, fromDB, toDB float64, over time.Duration) {
	n.mu.Lock()
	now := n.eng.Elapsed()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		l.ramp = &linkRamp{fromDB: fromDB, toDB: toDB, start: now, end: now + over}
	}
	n.mu.Unlock()
}

// FadeLink gives a link a periodic multipath null of the given depth.
//
// It composes with RampLink, and the combination is the realistic form of a
// degrading link: a mean that drifts down over minutes while brief deep fades
// eat individual frames. It is the case a link-quality threshold handles worst,
// because the mean is still above the threshold long after the link has started
// losing traffic — which is precisely the gap predictive healing exists to
// close.
func (n *Network) FadeLink(a, b NodeID, amplitudeDB float64, period time.Duration) {
	n.mu.Lock()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		if amplitudeDB <= 0 || period <= 0 {
			l.fade = nil
		} else {
			l.fade = &linkFade{amplitudeDB: amplitudeDB, period: period}
		}
	}
	n.mu.Unlock()
}

// CutLink removes a link entirely, as a metal partition or a shelf reorganisation
// would.
func (n *Network) CutLink(a, b NodeID) {
	n.mu.Lock()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		l.cut = true
	}
	n.recomputeRoutesLocked()
	n.mu.Unlock()
	n.notifyTopology()
}

// RestoreLink undoes CutLink, DegradeLink and RampLink for one pair.
func (n *Network) RestoreLink(a, b NodeID) {
	n.mu.Lock()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		l.cut, l.extraDB, l.ramp, l.fade = false, 0, nil, nil
	}
	n.recomputeRoutesLocked()
	n.mu.Unlock()
	n.notifyTopology()
}

// SetInterference raises the noise floor across the whole zone by dB, modelling
// a microwave oven, a rogue Wi-Fi access point on an overlapping channel, or a
// neighbouring store's mesh.
func (n *Network) SetInterference(dB float64) {
	n.mu.Lock()
	n.cfg.NoiseFloorDBm = NoiseFloorDBm + dB
	n.mu.Unlock()
}

// Avoid marks a link so routing prefers any alternative. It is what the
// controller's predictive healer calls when its model says a link is about to
// fail: the link still works, and is still available as a last resort, but no
// new route will be built over it.
func (n *Network) Avoid(a, b NodeID, avoid bool) {
	n.mu.Lock()
	if l, ok := n.links[makeLinkKey(a, b)]; ok {
		l.avoided = avoid
	}
	n.recomputeRoutesLocked()
	n.mu.Unlock()
	n.notifyTopology()
}

// Alive reports whether a node is on the air.
func (n *Network) Alive(id NodeID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[id]
	return ok && nd.alive
}

// Stats returns a snapshot of network counters.
func (n *Network) Stats() Stats {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.stats
	s.Nodes = len(n.nodes)
	for _, nd := range n.nodes {
		if nd.alive {
			s.Alive++
		}
		if nd.joined {
			s.Joined++
		}
	}
	return s
}

// Topology returns every node's current place in the tree, sorted by ID so two
// reports of an unchanged network are byte-identical and an operator diffing
// them sees only real movement.
func (n *Network) Topology() []NodeStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.eng.Elapsed()
	out := make([]NodeStatus, 0, len(n.nodes))
	for id, nd := range n.nodes {
		st := NodeStatus{
			ID:      id,
			Parent:  nd.parent,
			Depth:   nd.depth,
			Kind:    nd.spec.Kind,
			Online:  nd.alive && (nd.joined || id == n.coord),
			Battery: nd.spec.BatteryFraction,
		}
		if nd.parent != "" {
			if rssi, ok := n.rssiLocked(id, nd.parent, now); ok {
				st.RSSI = int(math.Round(rssi))
				st.LQI = LQIFromRSSI(rssi)
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (n *Network) notifyTopology() {
	n.mu.Lock()
	fn := n.onTopologyChange
	n.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ParentOf returns a node's current parent in the tree.
//
// It exists separately from Topology because a label reporting telemetry needs
// exactly this one fact, and sorting a whole zone's node list to find it turns a
// five-minute health report into an O(n log n) operation performed n times.
func (n *Network) ParentOf(id NodeID) (NodeID, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[id]
	if !ok {
		return "", false
	}
	return nd.parent, true
}
