package mesh

import (
	"fmt"
	"testing"
	"time"

	"github.com/usslp/usslp/edge/sim"
)

// aisle builds a zone shaped like a supermarket aisle: the controller at one
// end, mains-powered relays every few metres along the shelf rail, and battery
// labels clipped between them. It is the topology that forces multi-hop
// routing, which is the thing worth testing.
func aisle(t *testing.T, eng *sim.Engine, relays, labelsPerRelay int, spacingM float64) *Network {
	t.Helper()
	n := NewNetwork(eng, Config{PANID: 0x1234})
	if err := n.AddNode(NodeSpec{ID: "sec-1", Kind: KindCoordinator, Pos: Point{0, 0}}); err != nil {
		t.Fatalf("adding coordinator: %v", err)
	}
	for r := 0; r < relays; r++ {
		x := spacingM * float64(r+1)
		id := NodeID(fmt.Sprintf("relay-%02d", r))
		if err := n.AddNode(NodeSpec{ID: id, Kind: KindRouter, Pos: Point{x, 0}}); err != nil {
			t.Fatalf("adding %s: %v", id, err)
		}
		for l := 0; l < labelsPerRelay; l++ {
			lid := NodeID(fmt.Sprintf("label-%02d-%03d", r, l))
			if err := n.AddNode(NodeSpec{ID: lid, Kind: KindEndDevice, Pos: Point{x + 0.4*float64(l%5), 1 + 0.3*float64(l/5)}}); err != nil {
				t.Fatalf("adding %s: %v", lid, err)
			}
		}
	}
	return n
}

func form(t *testing.T, n *Network) time.Duration {
	t.Helper()
	var elapsed time.Duration
	got := false
	n.Form(func(d time.Duration) { elapsed, got = d, true })
	n.Engine().RunUntil(n.Engine().Elapsed() + 5*time.Minute)
	if !got {
		t.Fatal("formation never completed")
	}
	return elapsed
}

func TestAirtimeMatchesTheStandard(t *testing.T) {
	// A maximum-size 802.15.4 frame is 127 bytes at 250 kbps: 4.064 ms on air,
	// plus the acknowledgement and two turnarounds. Anything materially off
	// this means the contention model is lying about how expensive a store-wide
	// promotion is.
	one := Airtime(MaxFrameBytes - PHYOverheadBytes - MACOverheadBytes)
	if one < 4700*time.Microsecond || one > 4900*time.Microsecond {
		t.Fatalf("full-frame airtime %v, want ~4.8ms", one)
	}
	if got := Fragments(200); got != 3 {
		t.Fatalf("a 200-byte payload is %d fragments, want 3", got)
	}
	if Airtime(200) <= Airtime(90) {
		t.Fatal("fragmentation must cost more airtime, not less")
	}
}

func TestLinkBudgetDegradesWithDistance(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 1)
	n := NewNetwork(eng, Config{ShadowSigmaDB: 0.0001})
	mustAdd(t, n, NodeSpec{ID: "sec-1", Kind: KindCoordinator, Pos: Point{0, 0}})
	mustAdd(t, n, NodeSpec{ID: "near", Kind: KindEndDevice, Pos: Point{2, 0}})
	mustAdd(t, n, NodeSpec{ID: "far", Kind: KindEndDevice, Pos: Point{20, 0}})
	mustAdd(t, n, NodeSpec{ID: "beyond", Kind: KindEndDevice, Pos: Point{60, 0}})

	if _, ok := n.RSSI("sec-1", "beyond"); ok {
		t.Fatal("a node 60 m away is not a neighbour; the link budget cannot reach it")
	}
	near, _ := n.RSSI("sec-1", "near")
	far, _ := n.RSSI("sec-1", "far")
	if near <= far {
		t.Fatalf("near link %.1f dBm is not stronger than far link %.1f dBm", near, far)
	}
	if LQIFromRSSI(near) <= LQIFromRSSI(far) {
		t.Fatal("LQI must fall with distance")
	}
	if PacketErrorRate(near, NoiseFloorDBm) >= PacketErrorRate(far, NoiseFloorDBm) {
		t.Fatal("packet error rate must rise as the link weakens")
	}
	if LinkCost(255) != 1 || LinkCost(10) != 7 {
		t.Fatalf("Zigbee link cost is wrong at the extremes: %d, %d", LinkCost(255), LinkCost(10))
	}
}

func mustAdd(t *testing.T, n *Network, s NodeSpec) {
	t.Helper()
	if err := n.AddNode(s); err != nil {
		t.Fatalf("adding %s: %v", s.ID, err)
	}
}

func TestMultiHopDeliveryAndHopCount(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 7)
	n := aisle(t, eng, 4, 4, 9)
	form(t, n)

	// A label at the far end of the aisle is out of the coordinator's direct
	// range, so its route must traverse relays.
	dst := NodeID("label-03-000")
	route := n.Route(dst)
	if len(route) < 3 {
		t.Fatalf("route to the far label is %v; expected multiple hops", route)
	}
	if route[0] != n.Coordinator() || route[len(route)-1] != dst {
		t.Fatalf("route %v does not run from the coordinator to the destination", route)
	}

	var got Frame
	if err := n.SetReceiver(dst, func(f Frame) { got = f }); err != nil {
		t.Fatalf("setting receiver: %v", err)
	}
	var res TxResult
	n.Send(TxRequest{Dst: dst, Payload: make([]byte, 180), Done: func(r TxResult) { res = r }})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	if !res.Delivered {
		t.Fatalf("delivery failed: %v", res.Err)
	}
	if res.Hops != len(route)-1 {
		t.Fatalf("reported %d hops, route has %d", res.Hops, len(route)-1)
	}
	if got.Hops != res.Hops {
		t.Fatalf("frame says %d hops, result says %d", got.Hops, res.Hops)
	}
	// Per-hop cost is the fixed overhead plus the airtime of three fragments.
	perHop := 10*time.Millisecond + Airtime(180)
	lo, hi := time.Duration(res.Hops)*perHop/2, time.Duration(res.Hops)*perHop*3
	if res.Elapsed < lo || res.Elapsed > hi {
		t.Fatalf("%d-hop delivery took %v, outside the plausible band %v..%v", res.Hops, res.Elapsed, lo, hi)
	}
}

func TestSleepingLabelIsReachedInItsReceiveWindow(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 3)
	n := aisle(t, eng, 1, 1, 5)
	form(t, n)
	dst := NodeID("label-00-000")

	// A label listening once a second: the mesh must wait for the window rather
	// than deliver into a switched-off receiver.
	const window = time.Second
	if err := n.SetRxGate(dst, func(now time.Duration) time.Duration {
		return window - now%window
	}); err != nil {
		t.Fatalf("setting rx gate: %v", err)
	}
	var res TxResult
	n.Send(TxRequest{Dst: dst, Payload: make([]byte, 90), Done: func(r TxResult) { res = r }})
	eng.RunUntil(eng.Elapsed() + 10*time.Second)
	if !res.Delivered {
		t.Fatalf("delivery to a sleeping label failed: %v", res.Err)
	}
	if res.Elapsed < 100*time.Millisecond {
		t.Fatalf("delivery took %v; a duty-cycled label cannot answer that fast", res.Elapsed)
	}
}

func TestRefreshingLabelCannotReceive(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 5)
	n := aisle(t, eng, 1, 1, 5)
	form(t, n)
	dst := NodeID("label-00-000")

	// The label is driving a 1.5-second E-Ink waveform: its radio is off, the
	// frame is lost, and the MAC has to retry.
	n.SetBusyUntil(dst, eng.Elapsed()+400*time.Millisecond)
	var res TxResult
	n.Send(TxRequest{Dst: dst, Payload: make([]byte, 90), Done: func(r TxResult) { res = r }})
	eng.RunUntil(eng.Elapsed() + 10*time.Second)
	if res.Attempts < 2 {
		t.Fatalf("a frame sent into a refreshing label took %d attempts; it must cost at least one retry", res.Attempts)
	}
}

func TestNodeDeathTriggersReroute(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 11)
	n := aisle(t, eng, 4, 3, 8)
	form(t, n)

	dst := NodeID("label-03-000")
	before := n.Route(dst)
	if len(before) < 3 {
		t.Fatalf("route %v is not multi-hop; the test cannot kill an intermediate node", before)
	}
	victim := before[1]
	n.KillNode(victim)
	eng.RunUntil(eng.Elapsed() + time.Second)

	after := n.Route(dst)
	for _, hop := range after {
		if hop == victim {
			t.Fatalf("route %v still runs through the dead node %s", after, victim)
		}
	}
	// Killing a relay orphans its children; they have to re-associate, which is
	// what Form does. The surviving structure must still reach the far label.
	if len(after) == 0 {
		n.Form(func(time.Duration) {})
		eng.RunUntil(eng.Elapsed() + 2*time.Minute)
		after = n.Route(dst)
	}
	if len(after) == 0 {
		t.Fatal("the far label is unreachable after one relay died and the mesh re-formed")
	}
}

func TestCoordinatorRestartRebuildsWithinBudget(t *testing.T) {
	// The platform's operational commitment: a full topology rebuild in under
	// 90 seconds after a controller reboot.
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 23)
	n := aisle(t, eng, 8, 60, 7) // 8 relays, 480 labels: a dense zone
	form(t, n)

	joinedBefore := n.Stats().Joined
	if joinedBefore < 400 {
		t.Fatalf("only %d of %d nodes joined on first formation", joinedBefore, n.Stats().Nodes)
	}

	var rebuild time.Duration
	done := false
	n.RestartCoordinator(func(d time.Duration) { rebuild, done = d, true })
	eng.RunUntil(eng.Elapsed() + 10*time.Minute)
	if !done {
		t.Fatal("the mesh never finished re-forming")
	}
	if rebuild > 90*time.Second {
		t.Fatalf("topology rebuild took %v, budget is 90s", rebuild)
	}
	if got := n.Stats().Joined; got < joinedBefore {
		t.Fatalf("re-formation recovered %d nodes, had %d before the restart", got, joinedBefore)
	}
	t.Logf("rebuilt %d-node mesh in %v (channel utilisation %.1f%%)",
		n.Stats().Joined, rebuild.Round(time.Millisecond), 100*n.ChannelUtilisation())
}

func TestChannelContentionSerialisesTheZone(t *testing.T) {
	// 250 kbps is shared. Sending to a hundred labels at once cannot take the
	// time of one: the sum of the airtimes is a hard floor.
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 31)
	n := aisle(t, eng, 4, 25, 8)
	form(t, n)

	const payload = 180
	start := eng.Elapsed()
	delivered := 0
	var last time.Duration
	for _, st := range n.Topology() {
		if st.Kind != KindEndDevice || !st.Online {
			continue
		}
		n.Send(TxRequest{Dst: st.ID, Payload: make([]byte, payload), Done: func(r TxResult) {
			if r.Delivered {
				delivered++
				if end := start + r.Elapsed; end > last {
					last = end
				}
			}
		}})
	}
	eng.RunUntil(eng.Elapsed() + 5*time.Minute)
	span := last - start

	if delivered < 90 {
		t.Fatalf("only %d labels took the update", delivered)
	}
	floor := time.Duration(delivered) * Airtime(payload)
	if span < floor {
		t.Fatalf("flooded %d labels in %v, below the %v airtime floor: the channel model is not serialising",
			delivered, span, floor)
	}
	t.Logf("flooded %d labels in %v (airtime floor %v, utilisation %.1f%%)",
		delivered, span.Round(time.Millisecond), floor.Round(time.Millisecond), 100*n.ChannelUtilisation())
}

func TestInterferenceRaisesLoss(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 41)
	n := aisle(t, eng, 3, 4, 12)
	form(t, n)
	dst := NodeID("label-02-000")

	quiet := 0
	for i := 0; i < 30; i++ {
		n.Send(TxRequest{Dst: dst, Payload: make([]byte, 120), Done: func(r TxResult) {
			if r.Delivered {
				quiet++
			}
		}})
		eng.RunUntil(eng.Elapsed() + 2*time.Second)
	}

	n.SetInterference(25) // a microwave oven two aisles over
	noisy := 0
	for i := 0; i < 30; i++ {
		n.Send(TxRequest{Dst: dst, Payload: make([]byte, 120), Done: func(r TxResult) {
			if r.Delivered {
				noisy++
			}
		}})
		eng.RunUntil(eng.Elapsed() + 2*time.Second)
	}
	if noisy >= quiet {
		t.Fatalf("25 dB of interference changed delivery from %d/30 to %d/30; the noise floor is not reaching the link budget", quiet, noisy)
	}
	t.Logf("delivery fell from %d/30 to %d/30 under 25 dB of interference", quiet, noisy)
}

func TestDeterministicUnderSeed(t *testing.T) {
	run := func() (time.Duration, int) {
		eng := sim.New(time.Unix(1700000000, 0).UTC(), 99)
		n := aisle(t, eng, 4, 10, 8)
		var formed time.Duration
		n.Form(func(d time.Duration) { formed = d })
		eng.RunUntil(eng.Elapsed() + 5*time.Minute)
		total := 0
		for _, st := range n.Topology() {
			total += st.Depth*1000 + st.LQI
		}
		return formed, total
	}
	f1, t1 := run()
	f2, t2 := run()
	if f1 != f2 || t1 != t2 {
		t.Fatalf("two runs of the same seed differ: (%v,%d) vs (%v,%d)", f1, t1, f2, t2)
	}
}

func TestAvoidRoutesAroundALink(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 13)
	n := aisle(t, eng, 5, 4, 7)
	form(t, n)
	dst := NodeID("label-04-000")
	before := n.Route(dst)
	if len(before) < 3 {
		t.Fatalf("route %v is too short to reroute around", before)
	}
	a, b := before[0], before[1]
	n.Avoid(a, b, true)
	after := n.Route(dst)
	if len(after) >= 2 && after[0] == a && after[1] == b {
		t.Fatalf("route still uses the avoided link %s->%s: %v", a, b, after)
	}
	n.Avoid(a, b, false)
	if got := n.Route(dst); len(got) == 0 {
		t.Fatal("clearing the avoidance left the destination unreachable")
	}
}
