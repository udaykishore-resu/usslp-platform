package stack

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sec"
	"github.com/usslp/usslp/edge/sgu"
	"github.com/usslp/usslp/edge/sim"
	regdomain "github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// formationWindow is how much virtual time a store's meshes are given to form
// before the wall clock takes over. Ten simulated minutes is far more than a
// zone of a few hundred labels needs — formation is typically under a minute —
// and costs a few thousand heap pops rather than ten real minutes.
const formationWindow = 10 * time.Minute

// Zone is one Shelf Edge Controller and the labels on its shelf section.
type Zone struct {
	SECID canon.SECID
	// Sim is the simulated hardware: the mesh, the relays and the labels.
	Sim *labelsim.Zone
	// Coordinator drives the zone's radio.
	Coordinator *sec.Coordinator
	// Controller is the production Shelf Edge Controller, unmodified.
	Controller *sec.Controller

	bus msgbus.Client
	// link is this controller's drop to the store's LAN switch. It exists so
	// that Kill can sever the connection rather than close it politely; see
	// Kill.
	link    *wanLink
	stopped bool
	mu      sync.Mutex

	// provisioned are the labels enrolled at boot; spare is the one held back
	// so that the zero-touch path can be exercised against a running platform.
	provisioned []canon.LabelID
	spare       canon.LabelID
}

// Labels returns the commissioned label identifiers in this zone, in shelf
// order. The uncommissioned spare is deliberately not among them: it is
// hardware on a rail that the platform has never been told about.
func (z *Zone) Labels() []canon.LabelID {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]canon.LabelID(nil), z.provisioned...)
}

// Spare is the zone's uncommissioned label, or "" once it has been
// commissioned.
func (z *Zone) Spare() canon.LabelID {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.spare
}

// Relay returns a mains-powered relay node in this zone's mesh, which is what a
// resilience test kills to force a reroute. A zone with no relays returns false,
// which happens only when the label count is below the mesh's child limit.
func (z *Zone) Relay() (mesh.NodeID, bool) {
	relays := z.Sim.Relays()
	if len(relays) == 0 {
		return "", false
	}
	return relays[0], true
}

// Label returns one simulated label.
func (z *Zone) Label(id canon.LabelID) (*labelsim.Label, bool) { return z.Sim.Label(id) }

// Kill stops this controller as if its power had been pulled: no clean status
// publication, so the gateway learns about it from the last will.
//
// It is deliberately not Controller.Stop, which publishes a tidy "offline,
// clean shutdown". A maintenance window and a failure must be distinguishable,
// and this is the failure.
//
// # Why the link and not the client
//
// Closing the MQTT client would be the obvious implementation and it is the
// wrong one, because msgbus clients close politely: mqtt.Client.Close sends a
// DISCONNECT, and MQTT 3.1.1 §3.14 says a DISCONNECT discards the will. A
// controller "killed" that way vanishes without the gateway ever being told,
// which is the opposite of what this is meant to demonstrate — and the mistake
// is invisible, because the controller does stop responding either way.
//
// So the kill happens a layer down: the controller's own drop to the store LAN
// is severed, the TCP connection dies mid-session with no DISCONNECT on it, and
// the broker publishes the will once the keep-alive grace expires. That is what
// a pulled power lead looks like from the gateway's side, and how long it takes
// to notice is a real property of the deployment rather than a number this
// runtime asserts.
func (z *Zone) Kill() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.stopped {
		return nil
	}
	z.stopped = true
	if z.link == nil {
		// No severable drop (an older assembly path): fall back to closing the
		// client, and accept that the will will not fire.
		if z.bus == nil {
			return nil
		}
		return z.bus.Close()
	}
	z.link.Cut()
	return nil
}

// Store is one store: its gateway, its controllers and its label fleet.
type Store struct {
	ID     canon.StoreID
	Tenant canon.TenantID
	Scope  canon.TopicScope

	Gateway *sgu.Gateway
	Zones   []*Zone
	// Engine is the discrete-event clock every label and radio in this store
	// runs on.
	Engine *sim.Engine
	// Link is the store's uplink, and the handle fault injection pulls on.
	Link *wanLink
	// LocalAuthority is the store's delegated signing key: what lets a price
	// originated locally during an outage be verified by the controllers.
	LocalAuthority *pki.PriceAuthority

	BrokerAddr  string
	DiagnoseURL string
	AdminURL    string

	labelToSEC  map[canon.LabelID]canon.SECID
	skuOf       map[canon.LabelID]canon.SKU
	planogram   *regdomain.Planogram
	spareRecord map[canon.LabelID]spareIdentity
}

// LabelCount is the number of commissioned labels in the store.
func (st *Store) LabelCount() int {
	n := 0
	for _, z := range st.Zones {
		n += len(z.Labels())
	}
	return n
}

// Zone returns one controller's zone.
func (st *Store) Zone(id canon.SECID) (*Zone, bool) {
	for _, z := range st.Zones {
		if z.SECID == id {
			return z, true
		}
	}
	return nil, false
}

// FindLabel locates a label anywhere in the store.
func (st *Store) FindLabel(id canon.LabelID) (*Zone, *labelsim.Label, bool) {
	for _, z := range st.Zones {
		if l, ok := z.Sim.Label(id); ok {
			return z, l, true
		}
	}
	return nil, nil, false
}

// SKUOf returns the product a label is showing according to the planogram.
func (st *Store) SKUOf(id canon.LabelID) (canon.SKU, bool) {
	s, ok := st.skuOf[id]
	return s, ok
}

// Labels returns every label in the store, in controller and shelf order.
func (st *Store) Labels() []canon.LabelID {
	var out []canon.LabelID
	for _, z := range st.Zones {
		out = append(out, z.Labels()...)
	}
	return out
}

// SKUs returns every product on the store's shelves, sorted.
func (st *Store) SKUs() []canon.SKU {
	seen := map[canon.SKU]bool{}
	var out []canon.SKU
	for _, s := range st.skuOf {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------------------
// Start-up
// ---------------------------------------------------------------------------

func (s *Stack) startStores(ctx context.Context) error {
	n := 0
	for _, tenant := range s.cfg.Tenants {
		for i := 0; i < s.cfg.Stores; i++ {
			st, err := s.startStore(ctx, tenant, i, n)
			if err != nil {
				return err
			}
			s.stores = append(s.stores, st)
			n++
		}
	}
	// The OTA service's fleet directory is attached once every store exists, so
	// a rollout created a second after boot has real targets rather than an
	// empty cohort that looks like a success.
	if s.cloudSvcs != nil && s.cloudSvcs.otaFleet != nil {
		s.cloudSvcs.otaFleet.attach(s.cloudSvcs.registry, "UTC")
	}
	return nil
}

// startStore builds one store from the shelf up.
func (s *Stack) startStore(ctx context.Context, tenant canon.TenantID, index, ordinal int) (*Store, error) {
	storeID := StoreIDFor(tenant, index)
	scope := canon.TopicScope{Tenant: tenant, Region: canon.Region(s.cfg.Region), Store: storeID}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("usslpd: store %s topic scope: %w", storeID, err)
	}

	rt, err := s.runtimeFor("sgu", s.storeAdminPort(ordinal))
	if err != nil {
		return nil, err
	}
	log := rt.Log.WithTenant(string(tenant), string(storeID))

	st := &Store{
		ID: storeID, Tenant: tenant, Scope: scope,
		labelToSEC:  map[canon.LabelID]canon.SECID{},
		skuOf:       map[canon.LabelID]canon.SKU{},
		spareRecord: map[canon.LabelID]spareIdentity{},
		AdminURL:    "http://" + rt.Admin.Addr(),
	}

	// 1. The store's delegated price authority, and the key ring everything in
	//    the store verifies against.
	//
	//    This comes before the hardware, and the ordering is load-bearing rather
	//    than tidy. A label built without a ring is a label that has no way to
	//    check a price, and edge/labelsim's default — end-to-end attestation —
	//    correctly refuses every update in that state. The ring is therefore a
	//    prerequisite of the fleet existing, not a thing configured onto it
	//    afterwards, and the assembly order says so.
	local, err := pki.NewPriceAuthority(pki.PriceAuthorityConfig{Logger: rt.Log})
	if err != nil {
		return nil, fmt.Errorf("usslpd: creating the delegated authority for %s: %w", storeID, err)
	}
	st.LocalAuthority = local
	ring, err := s.storeKeyRing(local)
	if err != nil {
		return nil, err
	}

	// 2. The hardware. Every zone is built and its mesh formed before anything
	//    is connected, because mesh formation is fast-forwarded through virtual
	//    time and a controller that subscribed first would be delivering
	//    retained prices into a network that had not converged.
	// The epoch is set formationWindow *behind* the wall clock, because the very
	// next thing this function does is fast-forward exactly that far to form the
	// meshes. Without the offset the simulated clock would end up ten minutes
	// ahead of real time, and since the controller measures the platform's SLO
	// on this clock — Delivery.IssuedAt is the envelope's RecordedAt, a real
	// timestamp — every latency it reported would be ten minutes too large.
	// This one line is the difference between a latency measurement and a
	// number.
	engineBorn := time.Now()
	st.Engine = sim.New(engineBorn.UTC().Add(-formationWindow), uint64(s.cfg.Seed+int64(ordinal)))
	// Registered here, before anything that runs on this engine, so that on the
	// way down it is the very last thing to stop. sim.Engine.Stop empties the
	// event queue without marking the events it drops as cancelled, so a timer
	// cancelled afterwards — which is exactly what sec.Controller.Stop does —
	// indexes into a queue that is no longer there and panics. Stopping the
	// clock before its controllers is therefore a crash, and ordering is the
	// only fix available from outside edge/sim.
	s.push(string(storeID)+" simulation clock", func(context.Context) error {
		st.Engine.Stop()
		return nil
	})
	for j := 0; j < s.cfg.ControllersPerStore; j++ {
		secID := SECIDFor(storeID, j)
		// One label more than the store commissions: the last one is real
		// hardware on the rail that the platform has never been told about, so
		// the zero-touch path can be exercised against a running store rather
		// than only during boot.
		z, err := labelsim.NewZone(st.Engine, labelsim.ZoneSpec{
			StoreID: storeID, SECID: secID, Labels: s.cfg.LabelsPerController + 1,
			AisleLengthM: 24, TelemetryInterval: 30 * time.Second,
			FirmwareVersion: InitialFirmware,
			// The public half of the price authority the Label Service signs
			// with, plus the store's delegated key. Every label verifies the
			// signed tuple for itself before driving a pixel — labelsim's
			// AttestEndToEnd default, deliberately left at its default.
			//
			// A shelf label is a device the public can reach and a controller
			// is a box in a back room, so verifying only at the controller
			// leaves the last hop inside a trust boundary that a rooted or
			// physically swapped controller is already past. Handing the ring
			// to the fleet is what makes the price on the glass provable rather
			// than merely delivered, and it is why the platform can answer a
			// trading standards officer with something better than a log line.
			//
			// StrictClock is deliberately left off: a label that has not yet
			// acquired trusted time must still be able to take a price, and the
			// signature is the real control. Turning it on is a deployment
			// choice for a fleet with a trusted time source.
			KeyRing: ring,
			// One personal-area network per controller on its own channel,
			// which is how a site survey plans them: zones that share a channel
			// share its 250 kbps and update each other's labels slowly.
			Mesh: mesh.Config{PANID: uint16(0x1000 + ordinal*64 + j), Channel: 11 + j%16},
		})
		if err != nil {
			return nil, fmt.Errorf("usslpd: building zone %s: %w", secID, err)
		}
		zn := &Zone{SECID: secID, Sim: z}
		all := z.Labels()
		for _, l := range all[:len(all)-1] {
			zn.provisioned = append(zn.provisioned, l.ID())
		}
		zn.spare = all[len(all)-1].ID()
		st.Zones = append(st.Zones, zn)
	}
	for _, z := range st.Zones {
		z.Sim.Form(func(time.Duration) {})
		// The active window is opened *before* the fast-forward, not after.
		//
		// A resting label does not hear a wake instruction until its next
		// receive window, which on the 30-second resting interval is up to
		// thirty seconds away — correct hardware behaviour, and modelled
		// faithfully by labelsim.Label.OpenActiveWindow. Opening the window
		// after formation would therefore leave the whole store on its resting
		// duty cycle for the first thirty seconds of wall-clock life, and every
		// price delivered in that window would carry up to thirty seconds of
		// sleep in its latency. Opening it inside the fast-forward means the
		// thirty seconds are spent in virtual time, and a store that reports
		// itself open for trade is genuinely reachable inside the
		// controller-to-label budget (INTERFACE-CONTRACTS §4).
		//
		// A window this long is a lab and demonstration choice, and it has a
		// cost: a label held permanently on its 250 ms interval draws far more
		// than one that duty-cycles, which is exactly what
		// labelsim.PowerProfile.Project quantifies. A real store opens the
		// window for the length of a price load.
		z.Sim.OpenActiveWindow(365 * 24 * time.Hour)
	}
	// Formation is fast-forwarded rather than waited out: the mesh's own timing
	// model is what the platform measures, and a store coming up should not
	// spend real minutes reproducing it. RunUntil lands virtual time exactly on
	// formationWindow whether or not the last join was that late, which is what
	// makes the epoch offset above exact.
	st.Engine.RunUntil(formationWindow)
	joined := 0
	for _, z := range st.Zones {
		joined += z.Sim.Net.Stats().Joined
	}

	// 3. The uplink, then the gateway.
	link, err := newWANLink(s.cloudAddr, s.cfg.Seed+int64(ordinal))
	if err != nil {
		return nil, fmt.Errorf("usslpd: opening the WAN link for %s: %w", storeID, err)
	}
	st.Link = link
	s.push(string(storeID)+" wan link", func(context.Context) error { return link.Close() })

	kv, err := s.kvFor("sgu-"+string(storeID), rt, kvstore.SyncAlways)
	if err != nil {
		return nil, err
	}
	gw, err := sgu.New(sgu.Config{
		SGUID: canon.SGUID("sgu-" + storeID), StoreID: storeID, Scope: scope,
		BrokerAddr: s.addr(s.storeBrokerPort(ordinal)),
		Cloud: func(ctx context.Context) (msgbus.Client, error) {
			return mqtt.Dial(ctx, msgbus.Config{
				BrokerURL: link.URL(), ClientID: "sgu-" + string(storeID),
				CleanSession: false, KeepAlive: 10 * time.Second,
				ConnectTimeout: 5 * time.Second, AckTimeout: 5 * time.Second,
			}, mqtt.WithClientLogger(rt.Log))
		},
		Store: kv, KeyRing: ring, LocalAuthority: local,
		// The detector's production defaults take twelve seconds to declare an
		// outage and fifteen to declare a recovery, which is right for a store
		// whose broadband flaps: the cost of a false positive is a spurious
		// reconciliation. Here the whole point is to demonstrate the transition,
		// so it is tuned to notice in about a second. The mechanism is
		// unchanged — a QoS 1 probe that must be acknowledged, so a TCP session
		// to a dead load balancer still counts as down.
		Detector: sgu.DetectorConfig{
			Interval: 300 * time.Millisecond, Timeout: 700 * time.Millisecond,
			FailThreshold: 2, FailFor: 900 * time.Millisecond,
			RecoverThreshold: 2, RecoverFor: 900 * time.Millisecond,
		},
		ScheduleTick:    250 * time.Millisecond,
		ReconcileSettle: time.Second,
		AdminAddr:       s.addr(s.storeDiagPort(ordinal)),
		Log:             rt.Log, Registry: rt.Metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("usslpd: assembling the gateway for %s: %w", storeID, err)
	}
	if err := gw.Start(ctx); err != nil {
		return nil, fmt.Errorf("usslpd: starting the gateway for %s: %w", storeID, err)
	}
	s.push(string(storeID)+" gateway", func(ctx context.Context) error { return gw.Stop(ctx) })
	st.Gateway = gw
	st.BrokerAddr = gw.BrokerAddr()
	st.DiagnoseURL = "http://" + gw.AdminAddr()

	rt.Health.Register("store-broker", func(context.Context) error {
		if gw.BrokerAddr() == "" {
			return errors.New("the store's MQTT broker is not listening")
		}
		return nil
	})
	rt.Ready()

	// 4. Zero-touch provisioning of every device, through the real path.
	if err := s.provisionStore(ctx, st); err != nil {
		return nil, err
	}

	// 5. The readiness gate. The Label Service builds its fan-out directory
	//    from `device-events`, and until that projection has caught up the
	//    cloud can sign a price for a label it cannot address. Starting the
	//    controllers before then would mean the first price of the run silently
	//    resolving to nothing.
	if err := s.awaitDirectory(ctx, st); err != nil {
		return nil, err
	}

	// 6. The controllers.
	for i, z := range st.Zones {
		if err := s.attachController(ctx, rt, st, z, i == 0); err != nil {
			return nil, err
		}
	}

	// 7. The fleet's radio.
	//
	//    Before the paced runner takes over, virtual time is advanced to meet
	//    the wall clock. Everything between the fast-forward and here —
	//    issuing a few hundred certificates, enrolling them, waiting for a
	//    projection — happened in real time while the simulated clock stood
	//    still, and the gap is not cosmetic: the platform's SLO is measured
	//    from a wall-clock timestamp the cloud stamped (the envelope's
	//    RecordedAt) to a simulated-clock timestamp the controller stamped when
	//    the pixels settled. A clock offset between the two is subtracted
	//    directly from every latency the platform reports. Closing it here is
	//    what makes the numbers in test/e2e and `make demo` measurements rather
	//    than artefacts.
	st.Engine.RunUntil(formationWindow + time.Since(engineBorn))
	startClockTick(st.Engine)
	go func() {
		if err := st.Engine.Run(s.backgroundCtx(), s.cfg.SimSpeed); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, sim.ErrStopped) {
			log.Warn("simulation clock stopped", "error", err)
		}
	}()
	log.Info("store ready",
		"broker", st.BrokerAddr, "diagnostics", st.DiagnoseURL,
		"controllers", len(st.Zones), "labels", st.LabelCount(),
		"mesh_joined", joined, "mode", string(gw.Mode()))
	return st, nil
}

// clockTick is how often a store's simulation clock is nudged forward when
// nothing else is happening. See startClockTick.
const clockTick = 20 * time.Millisecond

// startClockTick schedules a self-renewing no-op on a store's engine so that
// its clock keeps step with the wall clock even when the store is idle.
//
// # Why an idle store needs a heartbeat
//
// edge/sim is a discrete-event engine: virtual time advances only when an event
// fires, and edge/sim.Engine.Run sleeps until the next one is due. That is
// exactly right for a simulation being fast-forwarded, and it has a consequence
// that is easy to miss when the same engine is also the clock a latency is
// measured against. On a quiet store — no price changes, telemetry every thirty
// seconds — the queue is empty for seconds at a time and Engine.Now() stops
// moving. When a price finally arrives, everything it schedules is scheduled
// relative to that stale instant, so the controller stamps the delivery seconds
// in the past and the platform reports a latency that is too small, or
// negative.
//
// A price change on a quiet store is the headline measurement, so the clock has
// to be honest precisely when the engine is least busy. Twenty milliseconds
// costs fifty events a second per store — nothing next to a hundred labels'
// radio traffic — and bounds the error at one tick.
//
// The tick runs on the paced runner's own goroutine, like every other event, so
// it introduces no concurrency. It stops itself when the engine does: At
// returns nil on a stopped engine and the chain ends there.
func startClockTick(eng *sim.Engine) {
	var tick func()
	tick = func() {
		eng.At(clockTick, tick)
	}
	eng.At(clockTick, tick)
}

// storeKeyRing is the platform ring plus one store's delegated key.
//
// A controller has to accept two signers: the cloud's price authority for
// everything that comes down the bridge, and its own store's delegated key for
// a price originated locally while the WAN is down. Publishing both in one ring
// is what makes INTERFACE-CONTRACTS §5 and store autonomy compatible — without
// the delegation a store that goes autonomous can record a local price change
// and report it and never get it onto a shelf, because the controller correctly
// refuses a price it cannot verify.
func (s *Stack) storeKeyRing(local *pki.PriceAuthority) (*pki.KeyRing, error) {
	ring := pki.NewKeyRing()
	for _, k := range s.keyRing.Keys() {
		if err := ring.Add(k); err != nil {
			return nil, fmt.Errorf("usslpd: building the store key ring: %w", err)
		}
	}
	lr, err := local.KeyRing()
	if err != nil {
		return nil, err
	}
	for _, k := range lr.Keys() {
		if err := ring.Add(k); err != nil {
			return nil, fmt.Errorf("usslpd: adding the delegated key: %w", err)
		}
	}
	return ring, nil
}

// attachController wires a production Shelf Edge Controller to a simulated
// zone.
//
// Only the first controller in a store is handed the metrics registry:
// obs.Registry refuses a duplicate metric name, and a registry per controller
// would produce a /metrics page with four copies of every series and no way to
// tell them apart. The `sec` label on each series carries the controller
// identity for the one that does register.
func (s *Stack) attachController(ctx context.Context, rt *obs.Runtime, st *Store, z *Zone, first bool) error {
	// Each controller reaches the gateway's broker through its own severable
	// drop rather than dialling it directly, so that Zone.Kill can cut a live
	// TCP session instead of closing one politely. See Zone.Kill for why that
	// distinction is the whole point of the fault.
	link, err := newWANLink(st.BrokerAddr, s.cfg.Seed+int64(len(st.Zones)))
	if err != nil {
		return fmt.Errorf("usslpd: opening the LAN drop for %s: %w", z.SECID, err)
	}
	z.link = link
	s.push(string(z.SECID)+" lan drop", func(context.Context) error { return link.Close() })

	bus, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: link.URL(), ClientID: "sec-" + string(z.SECID),
		// Ten seconds rather than the usual thirty. The keep-alive is what
		// decides how long a controller can be dead before its last will is
		// published — the broker allows the specification's 1.5x grace — and
		// therefore how long the gateway believes in a controller that has
		// stopped existing. Thirty seconds means forty-five seconds of a store
		// map that is wrong; ten means fifteen. The cost is a PINGREQ every few
		// seconds over a store LAN, which is nothing, and this link never
		// crosses the WAN.
		CleanSession: false, KeepAlive: 10 * time.Second,
		ConnectTimeout: 10 * time.Second, AckTimeout: 10 * time.Second,
		Will: sec.WillFor(st.Scope, z.SECID),
	}, mqtt.WithClientLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("usslpd: connecting %s to its gateway: %w", z.SECID, err)
	}
	z.bus = bus

	var reg *obs.Registry
	if first {
		reg = rt.Metrics
	}
	ring, err := s.storeKeyRing(st.LocalAuthority)
	if err != nil {
		return err
	}
	z.Coordinator = sec.NewCoordinator(z.Sim.Net, sec.SimScheduler(st.Engine), sec.CoordinatorConfig{
		SECID: z.SECID, StoreID: st.ID, Healing: sec.HealPredictive,
		LabelReportInterval: 30 * time.Second,
		// The coordinator's 25-second default acknowledgement timeout is sized
		// for the 5.85-inch seven-colour panel, whose waveform alone is fifteen
		// seconds, and for labels that may be asleep when the frame arrives.
		// Neither applies to this fleet: every generated label is the 2.9-inch
		// black/white/red panel with a 1.5-second full waveform, and the store
		// is held in its active receive window for the length of the run.
		//
		// The worst case this fleet can actually produce is about 2.1 seconds:
		// a 300 ms radio hop out, a 1.5 s full waveform, and the acknowledgement
		// back. Four seconds is a little under twice that, and the margin is
		// deliberate rather than generous — the difference is not cosmetic.
		//
		// The radio model drops acknowledgements as readily as it drops updates,
		// and until this timer fires the label's slot is occupied, so the
		// timeout is the price of a lost ack. At the 25-second default a single
		// lost ack stalls one label for most of a minute before the first retry,
		// which is long enough to be mistaken for a stuck platform; at four it
		// is a visible tail on a thousand-sample run and nothing worse. The
		// retry policy itself is left at the platform's own three attempts.
		//
		// It matters more since the fleet moved to end-to-end attestation: the
		// signed tuple makes the air frame 199 bytes larger, which is more
		// airtime per transmission and measurably more chance of losing one.
		AckTimeout: 4 * time.Second,
		Log:        rt.Log, Registry: reg,
	})
	// The controller's cache is SyncNever: it is a performance optimisation
	// over state the gateway also holds and republishes as retained messages,
	// so losing the last fraction of a second of it costs a re-sync, not a
	// price. In a shipped controller it is SyncEvery, because the disk there is
	// not a container's.
	cache, err := kvstore.OpenWith(kvstore.Options{
		Dir:  filepath.Join(s.cfg.DataDir, "sec", string(z.SECID)),
		Sync: kvstore.SyncNever,
	})
	if err != nil {
		return fmt.Errorf("usslpd: opening the label cache for %s: %w", z.SECID, err)
	}
	s.push(string(z.SECID)+" cache", func(context.Context) error { return cache.Close() })

	specs := make([]sec.LabelSpec, 0, len(z.Sim.Labels()))
	for _, l := range z.Sim.Labels() {
		specs = append(specs, sec.LabelSpec{
			ID: l.ID(), Node: l.NodeID(), Tier: l.Tier(), SKU: st.skuOf[l.ID()],
		})
	}
	ctl, err := sec.New(sec.Config{
		SECID: z.SECID, StoreID: st.ID, Scope: st.Scope, Bus: bus, Store: cache,
		KeyRing: ring, Coordinator: z.Coordinator, Sched: sec.SimScheduler(st.Engine),
		Labels: specs, TelemetryInterval: 30 * time.Second, HeartbeatInterval: 5 * time.Second,
		Log: rt.Log, Registry: reg,
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling controller %s: %w", z.SECID, err)
	}
	// Every delivery is observed before the controller starts, so no update is
	// missed by the SLO record — including the opening price book, which is the
	// first thing the platform does after this returns.
	ctl.OnDelivery(func(res sec.DeliveryResult) {
		s.deliveries.record(st.ID, z.SECID, res)
	})
	if err := ctl.Start(ctx); err != nil {
		return fmt.Errorf("usslpd: starting controller %s: %w", z.SECID, err)
	}
	z.Controller = ctl
	s.push(string(z.SECID)+" controller", func(ctx context.Context) error {
		z.mu.Lock()
		stopped := z.stopped
		z.mu.Unlock()
		if !stopped {
			ctl.Stop(ctx)
			return bus.Close()
		}
		return nil
	})
	return nil
}

// awaitDirectory blocks until the Label Service's placement read model knows
// every label in the store, with the product the planogram assigned it.
//
// It polls rather than waiting on a signal because the projection is an
// eventual-consistency boundary by construction: it is fed by a consumer group
// on `device-events` and there is deliberately no synchronous handshake between
// the registry's write and the Label Service's read model. Polling here is the
// same thing an operator does — ask until the answer is right — and the timeout
// turns "the projection never caught up" into a start-up failure that names the
// gap rather than a store that quietly cannot be repriced.
func (s *Stack) awaitDirectory(ctx context.Context, st *Store) error {
	want := st.LabelCount()
	dir := s.cloudSvcs.label.Directory()
	deadline := time.Now().Add(45 * time.Second)
	var have int
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		placements, err := dir.StoreLabels(ctx, st.Tenant, st.ID)
		if err == nil {
			have = 0
			for _, p := range placements {
				if !p.Retired && p.SKU != "" && p.SECID != "" {
					have++
				}
			}
			if have >= want {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("usslpd: the label service's directory reached %d of %d labels for %s "+
		"within the start-up budget; the device-events projection is not keeping up", have, want, st.ID)
}

// storeBrokerPort, storeDiagPort and storeAdminPort lay out one store's
// listeners, or return 0 when the operating system is choosing.
func (s *Stack) storeBrokerPort(n int) int {
	if s.cfg.Ports.StoreMQTTBse <= 0 {
		return 0
	}
	return s.cfg.Ports.StoreMQTTBse + n
}

func (s *Stack) storeDiagPort(n int) int {
	if s.cfg.Ports.StoreAdmnBse <= 0 {
		return 0
	}
	return s.cfg.Ports.StoreAdmnBse + n
}

func (s *Stack) storeAdminPort(n int) int {
	if s.cfg.Ports.AdminBase <= 0 {
		return 0
	}
	return s.cfg.Ports.AdminBase + 10 + n
}
