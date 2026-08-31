package mesh

import (
	"container/heap"
	"math"
	"sort"
	"time"
)

// Neighbour-table capacities, by node role.
//
// These are not optimisations, they are the hardware. An 802.15.4 stack on a
// Cortex-M0 with 32 KB of RAM keeps on the order of twenty neighbour entries,
// and a label that hears three hundred peers simply does not remember most of
// them; modelling the cap matters, because an unbounded table would let the
// simulated mesh find routes that real firmware could never hold. The
// coordinator is a Linux gateway with a real radio and no such constraint,
// which is precisely why the tree is rooted there.
const (
	// MaxNeighbours is an end device's neighbour table.
	MaxNeighbours = 24
	// MaxRouterNeighbours is a mains-powered relay's table.
	MaxRouterNeighbours = 64
	// MaxCoordinatorNeighbours bounds the coordinator's table only to keep the
	// simulation's memory finite.
	MaxCoordinatorNeighbours = 1024
)

// neighbourCapacity returns the table size for a node's role.
func neighbourCapacity(k NodeKind) int {
	switch k {
	case KindCoordinator:
		return MaxCoordinatorNeighbours
	case KindRouter:
		return MaxRouterNeighbours
	default:
		return MaxNeighbours
	}
}

// avoidPenalty is the routing cost added to a link the controller has asked to
// route around. It exceeds the worst possible cost of any plausible path
// (7 per hop times the 5-hop radius) so that an avoided link is used only when
// there is genuinely no alternative — the label keeps working, degraded,
// rather than going dark because a prediction was pessimistic.
const avoidPenalty = 64

// lowBatteryPenalty biases routes away from relays that are nearly flat. A
// router about to die is a route about to break, and paying two cost units to
// avoid it is cheaper than a repair.
const lowBatteryPenalty = 2

// trimNeighboursLocked enforces the neighbour-table capacity on one node,
// removing evicted links symmetrically as a real stack does when it ages an
// entry out of a full table.
//
// Eviction prefers to keep potential parents: a device that fills its table
// with sibling end devices it can never route through has a full table and no
// way onto the network, which is a real deployment failure and not one worth
// reproducing by accident. Routers and the coordinator are kept first, then the
// nearest peers.
func (n *Network) trimNeighboursLocked(id NodeID) {
	self := n.nodes[id]
	capacity := neighbourCapacity(self.spec.Kind)
	list := n.neighbours[id]
	if len(list) <= capacity {
		return
	}
	rank := func(p NodeID) int {
		switch n.nodes[p].spec.Kind {
		case KindCoordinator:
			return 0
		case KindRouter:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if ri, rj := rank(list[i]), rank(list[j]); ri != rj {
			return ri < rj
		}
		di := self.spec.Pos.Distance(n.nodes[list[i]].spec.Pos)
		dj := self.spec.Pos.Distance(n.nodes[list[j]].spec.Pos)
		if di != dj {
			return di < dj
		}
		return list[i] < list[j]
	})
	evicted := append([]NodeID(nil), list[capacity:]...)
	n.neighbours[id] = list[:capacity:capacity]
	for _, peer := range evicted {
		delete(n.links, makeLinkKey(id, peer))
		pl := n.neighbours[peer]
		for i, x := range pl {
			if x == id {
				n.neighbours[peer] = append(pl[:i], pl[i+1:]...)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Network formation
// ---------------------------------------------------------------------------

// Form brings the network up: every live node associates with the best parent
// it can hear, the tree fills in depth by depth, and done is called with the
// elapsed virtual time once no node can still join.
//
// Formation is scheduled work on the simulation engine, not a computation, and
// that is the point. The platform's operational budget is "a full topology
// rebuild in under 90 seconds after a coordinator restart", which is a claim
// about association exchanges contending for one 250 kbps channel while five
// hundred labels all try at once. Computing a spanning tree instantly would
// answer a different, easier question.
func (n *Network) Form(done func(elapsed time.Duration)) {
	n.mu.Lock()
	if n.coord == "" {
		n.mu.Unlock()
		if done != nil {
			n.eng.At(0, func() { done(0) })
		}
		return
	}
	n.formSeq++
	seq := n.formSeq
	start := n.eng.Elapsed()
	n.formed = false
	window := int64(n.cfg.JoinWindow)
	ids := make([]NodeID, 0, len(n.nodes))
	for id, nd := range n.nodes {
		if id == n.coord || !nd.alive || nd.joined {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	timeout := n.cfg.FormTimeout()
	n.mu.Unlock()

	fired := false
	finish := func() {
		if fired {
			return
		}
		fired = true
		n.mu.Lock()
		n.formed = true
		n.routesDirty = true
		elapsed := n.eng.Elapsed() - start
		n.mu.Unlock()
		n.notifyTopology()
		if done != nil {
			done(elapsed)
		}
	}

	// Randomising the association order stops formation from depending on the
	// order labels happen to appear in a planogram file, and reproduces the
	// contention a real store sees when the power comes back on.
	//
	// Routers go first, in the opening quarter of the window. That is not a
	// convenience: a mains-powered relay boots the instant power returns, while
	// a battery label only tries to associate when its own duty cycle next
	// wakes it. Letting the backbone form before five hundred labels compete
	// for child slots is the difference between a mesh that re-forms in half a
	// minute and one that strands most of an aisle on a full parent.
	quarter := window / 4 // window is an int64 count of nanoseconds
	for _, idx := range n.eng.Rand().Perm(len(ids)) {
		id := ids[idx]
		var delay time.Duration
		if n.kindOf(id) == KindRouter {
			delay = time.Duration(n.eng.Rand().Duration(quarter))
		} else {
			delay = time.Duration(quarter) + time.Duration(n.eng.Rand().Duration(window-quarter))
		}
		n.eng.At(delay, func() { n.attemptJoin(id, seq, finish) })
	}
	n.eng.At(timeout, finish)
	if len(ids) == 0 {
		n.eng.At(0, finish)
	}
}

// FormTimeout bounds how long formation waits for stragglers. Nodes that cannot
// reach any parent — a label behind a freezer door, a label whose battery died
// overnight — must not hold the whole rebuild open.
func (c Config) FormTimeout() time.Duration {
	return c.JoinWindow + 20*c.JoinRetry
}

// attemptJoin is one association attempt by one node.
func (n *Network) attemptJoin(id NodeID, seq uint64, finish func()) {
	n.mu.Lock()
	if seq != n.formSeq {
		n.mu.Unlock()
		return
	}
	nd, ok := n.nodes[id]
	if !ok || !nd.alive || nd.joined {
		n.mu.Unlock()
		return
	}
	now := n.eng.Elapsed()
	parent, ok := n.bestParentLocked(id, now)
	if !ok {
		// No eligible parent has joined yet. Back off and try again — this is
		// what makes the tree fill in depth by depth rather than all at once.
		retry := time.Duration(n.eng.Rand().Jitter(int64(n.cfg.JoinRetry), 0.4))
		n.mu.Unlock()
		n.eng.At(retry, func() { n.attemptJoin(id, seq, finish) })
		return
	}
	// The association exchange is four frames and contends for the channel like
	// any other traffic, which is why five hundred simultaneous joins take tens
	// of seconds rather than milliseconds.
	_, end := n.reserveMediumLocked(Airtime(24) * 4)
	n.mu.Unlock()

	n.eng.At(end-now+n.cfg.JoinExchange, func() {
		n.mu.Lock()
		if seq != n.formSeq {
			n.mu.Unlock()
			return
		}
		nd, ok := n.nodes[id]
		pn, pok := n.nodes[parent]
		if !ok || !pok || !nd.alive || !pn.alive || !pn.joined || pn.kids >= n.cfg.MaxChildren {
			// The candidate parent went away or filled up during the exchange.
			retry := time.Duration(n.eng.Rand().Jitter(int64(n.cfg.JoinRetry), 0.4))
			n.mu.Unlock()
			n.eng.At(retry, func() { n.attemptJoin(id, seq, finish) })
			return
		}
		nd.joined = true
		nd.parent = parent
		nd.depth = pn.depth + 1
		pn.kids++
		n.routesDirty = true
		remaining := n.unjoinedAliveLocked()
		n.mu.Unlock()
		if remaining == 0 {
			finish()
		}
	})
}

// bestParentLocked picks the parent a joining node would choose: the strongest
// link among joined routers with spare capacity and room in the tree, breaking
// ties toward the shallower node so the tree stays wide rather than deep.
func (n *Network) bestParentLocked(id NodeID, now time.Duration) (NodeID, bool) {
	var best NodeID
	bestScore := math.Inf(-1)
	for _, peer := range n.neighbours[id] {
		pn, ok := n.nodes[peer]
		if !ok || !pn.alive || !pn.joined {
			continue
		}
		if pn.spec.Kind == KindEndDevice {
			continue // an end device sleeps; it cannot parent anything
		}
		if pn.kids >= n.cfg.MaxChildren || pn.depth+1 > n.cfg.MaxDepth {
			continue
		}
		rssi, ok := n.rssiLocked(id, peer, now)
		if !ok {
			continue
		}
		lqi := LQIFromRSSI(rssi)
		if lqi < 40 {
			continue // below this the association itself would not complete
		}
		score := float64(lqi) - 12*float64(pn.depth)
		if score > bestScore {
			best, bestScore = peer, score
		}
	}
	return best, best != ""
}

// kindOf reports a node's role, or KindEndDevice for an unknown node so that a
// caller never accidentally treats a missing node as routing infrastructure.
func (n *Network) kindOf(id NodeID) NodeKind {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd, ok := n.nodes[id]; ok {
		return nd.spec.Kind
	}
	return KindEndDevice
}

func (n *Network) unjoinedAliveLocked() int {
	count := 0
	for id, nd := range n.nodes {
		if id != n.coord && nd.alive && !nd.joined {
			count++
		}
	}
	return count
}

// Formed reports whether formation has completed.
func (n *Network) Formed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.formed
}

// RestartCoordinator models the SEC rebooting: every association is lost and
// the whole tree has to re-form. It is the scenario behind the 90-second
// rebuild budget, and the one a store manager causes by power-cycling a
// controller that "looks stuck".
func (n *Network) RestartCoordinator(done func(elapsed time.Duration)) {
	n.mu.Lock()
	for id, nd := range n.nodes {
		if id == n.coord {
			nd.kids = 0
			continue
		}
		nd.joined = false
		nd.parent = ""
		nd.depth = 0
		nd.kids = 0
	}
	n.routes = make(map[NodeID][]NodeID)
	n.routesDirty = true
	n.formed = false
	n.mu.Unlock()
	n.notifyTopology()
	n.Form(done)
}

// Rejoin schedules a single revived node to re-associate without disturbing the
// rest of the tree, which is what happens when one label's cell is replaced.
func (n *Network) Rejoin(id NodeID, done func()) {
	n.mu.Lock()
	seq := n.formSeq
	n.mu.Unlock()
	n.eng.At(time.Duration(n.eng.Rand().Duration(int64(n.cfg.JoinWindow))), func() {
		n.attemptJoin(id, seq, func() {
			if done != nil {
				done()
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// dijkstraItem is a priority-queue entry for route computation.
type dijkstraItem struct {
	id    NodeID
	cost  int
	index int
}

type dijkstraQueue []*dijkstraItem

func (q dijkstraQueue) Len() int { return len(q) }
func (q dijkstraQueue) Less(i, j int) bool {
	if q[i].cost != q[j].cost {
		return q[i].cost < q[j].cost
	}
	return q[i].id < q[j].id // deterministic tie-break
}
func (q dijkstraQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *dijkstraQueue) Push(x any) {
	it := x.(*dijkstraItem)
	it.index = len(*q)
	*q = append(*q, it)
}
func (q *dijkstraQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return it
}

// recomputeRoutesLocked rebuilds the coordinator's routing table.
//
// Zigbee routers do this with AODV route discovery, hop by hop, and the result
// is a least-cost path under the specification's link-cost metric. Computing
// the same answer centrally with Dijkstra is faithful to the outcome — the
// coordinator is the source of essentially all downstream traffic, so it is the
// node that would hold the routes anyway — while being tractable for a zone of
// several hundred nodes. The cost of *discovering* a route is charged
// separately, in Send, as RouteDiscovery latency.
func (n *Network) recomputeRoutesLocked() {
	n.routesDirty = false
	if n.coord == "" {
		n.routes = map[NodeID][]NodeID{}
		return
	}
	now := n.eng.Elapsed()
	dist := make(map[NodeID]int, len(n.nodes))
	prev := make(map[NodeID]NodeID, len(n.nodes))
	items := make(map[NodeID]*dijkstraItem, len(n.nodes))

	q := &dijkstraQueue{}
	heap.Init(q)
	root := &dijkstraItem{id: n.coord, cost: 0}
	dist[n.coord] = 0
	items[n.coord] = root
	heap.Push(q, root)

	for q.Len() > 0 {
		cur := heap.Pop(q).(*dijkstraItem)
		if d, ok := dist[cur.id]; ok && cur.cost > d {
			continue
		}
		curNode := n.nodes[cur.id]
		// Only the coordinator and mains-powered routers relay. Reaching an end
		// device therefore terminates the path there.
		if cur.id != n.coord && curNode.spec.Kind == KindEndDevice {
			continue
		}
		for _, peer := range n.neighbours[cur.id] {
			pn, ok := n.nodes[peer]
			if !ok || !pn.alive {
				continue
			}
			if peer != n.coord && !pn.joined {
				continue
			}
			l := n.links[makeLinkKey(cur.id, peer)]
			if l == nil || l.cut {
				continue
			}
			rssi, ok := n.rssiLocked(cur.id, peer, now)
			if !ok {
				continue
			}
			lqi := LQIFromRSSI(rssi)
			if lqi < 20 {
				continue // unusable: the MAC would never get an acknowledgement
			}
			cost := cur.cost + LinkCost(lqi)
			if l.avoided {
				cost += avoidPenalty
			}
			if pn.spec.Kind == KindRouter && pn.spec.BatteryFraction < 0.2 {
				cost += lowBatteryPenalty
			}
			if old, seen := dist[peer]; seen && cost >= old {
				continue
			}
			dist[peer] = cost
			prev[peer] = cur.id
			it := &dijkstraItem{id: peer, cost: cost}
			items[peer] = it
			heap.Push(q, it)
		}
	}

	routes := make(map[NodeID][]NodeID, len(dist))
	for id := range dist {
		if id == n.coord {
			continue
		}
		path := []NodeID{id}
		for cur := id; cur != n.coord; {
			p, ok := prev[cur]
			if !ok {
				path = nil
				break
			}
			path = append(path, p)
			cur = p
		}
		if path == nil {
			continue
		}
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		if len(path)-1 > n.cfg.MaxDepth {
			continue // beyond the network radius; the NWK layer would drop it
		}
		routes[id] = path
	}
	n.routes = routes
}

// RecomputeRoutes rebuilds the routing table immediately. The controller calls
// it after a proactive reroute so the next transmission uses the new path.
func (n *Network) RecomputeRoutes() {
	n.mu.Lock()
	n.recomputeRoutesLocked()
	n.mu.Unlock()
	n.notifyTopology()
}

// routeLocked returns the cached path to a destination, rebuilding the table
// first if a topology change has invalidated it.
func (n *Network) routeLocked(dst NodeID) ([]NodeID, bool) {
	if n.routesDirty {
		n.recomputeRoutesLocked()
	}
	p, ok := n.routes[dst]
	return p, ok
}

// Route returns the current path from the coordinator to a destination, or nil
// if there is none. The slice is a copy; callers may keep it.
func (n *Network) Route(dst NodeID) []NodeID {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.routeLocked(dst)
	if !ok {
		return nil
	}
	return append([]NodeID(nil), p...)
}

// Hops returns the number of radio hops to a destination, or -1 if unreachable.
func (n *Network) Hops(dst NodeID) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.routeLocked(dst)
	if !ok {
		return -1
	}
	return len(p) - 1
}
