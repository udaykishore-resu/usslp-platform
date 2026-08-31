package sgu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/edge/sec"
	"github.com/usslp/usslp/edge/sim"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// ---------------------------------------------------------------------------
// A cuttable WAN
//
// The outage is modelled as a TCP proxy in front of the cloud broker rather
// than by stopping the broker, because those are genuinely different failures
// and only one of them is the one the platform claims to survive. Stopping the
// broker would also destroy its retained state, and the cloud's retained state
// is exactly what reconciliation has to merge against: the interesting case is
// a link that comes back to a cloud which has been busy in the meantime.
// ---------------------------------------------------------------------------

type wanProxy struct {
	ln     net.Listener
	target string

	mu     sync.Mutex
	cut    bool
	conns  []net.Conn
	closed bool
}

func newWANProxy(t *testing.T, target string) *wanProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding the WAN proxy: %v", err)
	}
	p := &wanProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(p.close)
	return p
}

func (p *wanProxy) addr() string { return p.ln.Addr().String() }

func (p *wanProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		cut := p.cut
		p.mu.Unlock()
		if cut {
			c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, up)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); up.Close(); c.Close() }()
		go func() { _, _ = io.Copy(c, up); c.Close(); up.Close() }()
	}
}

// cutLink severs every open connection and refuses new ones, which is what a
// failed DSL line or a dead 4G modem looks like to the gateway.
func (p *wanProxy) cutLink() {
	p.mu.Lock()
	p.cut = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (p *wanProxy) restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

func (p *wanProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	p.ln.Close()
}

// ---------------------------------------------------------------------------
// The store under test
// ---------------------------------------------------------------------------

const (
	outTenant = canon.TenantID("acme-retail")
	outStore  = canon.StoreID("store-0417")
	outSEC    = canon.SECID("sec-0042")
	outSGU    = canon.SGUID("sgu-0417")
)

// testLogger sends the gateway's own narration to the test log when -v is on.
// A store going autonomous and reconciling is a sequence of decisions, and when
// one of these tests fails the decisions are what you need to see.
func testLogger() *obs.Logger {
	if !testing.Verbose() {
		return obs.NopLogger()
	}
	return obs.NewLogger(obs.LogConfig{Service: "sgu", Level: "info", Format: "text", Output: os.Stderr})
}

func outScope() canon.TopicScope {
	return canon.TopicScope{Tenant: outTenant, Region: "eu-west-1", Store: outStore}
}

// storeRig is a whole store: a cloud broker behind a cuttable link, a gateway,
// a controller and a zone of simulated labels.
type storeRig struct {
	t *testing.T

	cloudBroker *mqtt.Broker
	cloudAddr   string
	proxy       *wanProxy
	headOffice  msgbus.Client

	gw        *Gateway
	eng       *sim.Engine
	zone      *labelsim.Zone
	ctl       *sec.Controller
	coord     *sec.Coordinator
	secBus    msgbus.Client
	cloudAuth *pki.PriceAuthority
	localAuth *pki.PriceAuthority
	ring      *pki.KeyRing

	mu        sync.Mutex
	cloudSeen []msgbus.Message
	cancel    context.CancelFunc
}

func newStoreRig(t *testing.T, labels int) *storeRig {
	t.Helper()
	r := &storeRig{t: t}

	// --- cloud -------------------------------------------------------------
	r.cloudBroker = mqtt.NewBroker(mqtt.Options{Addr: "127.0.0.1:0"})
	addr, err := r.cloudBroker.Start()
	if err != nil {
		t.Fatalf("starting the cloud broker: %v", err)
	}
	r.cloudAddr = addr.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.cloudBroker.Shutdown(ctx)
	})
	r.proxy = newWANProxy(t, r.cloudAddr)

	// Head office publishes straight to the cloud broker, not through the WAN
	// proxy: the cloud carries on working while this store cannot reach it,
	// which is the whole premise of the scenario.
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.headOffice = dial(t, ctx, r.cloudAddr, "head-office")

	// An observer on the cloud side, so the test can assert what the cloud
	// actually received and in what order.
	obs := dial(t, ctx, r.cloudAddr, "cloud-observer")
	if err := obs.Subscribe(ctx, canon.SubscribeTenant(outTenant), msgbus.AtLeastOnce,
		func(_ context.Context, m msgbus.Message) {
			r.mu.Lock()
			r.cloudSeen = append(r.cloudSeen, m)
			r.mu.Unlock()
		}); err != nil {
		t.Fatalf("subscribing the cloud observer: %v", err)
	}

	// --- keys --------------------------------------------------------------
	//
	// Two authorities: the cloud's, which signs everything head office issues,
	// and a delegated store-scoped one that lets this gateway attest a price of
	// its own during an outage. Both public halves are in the controller's key
	// ring, which is precisely what makes local autonomy compatible with the
	// rule that a label never displays a price it cannot verify.
	r.cloudAuth = mustAuthority(t)
	r.localAuth = mustAuthority(t)
	r.ring = pki.NewKeyRing()
	for _, a := range []*pki.PriceAuthority{r.cloudAuth, r.localAuth} {
		ring, err := a.KeyRing()
		if err != nil {
			t.Fatalf("publishing a key ring: %v", err)
		}
		for _, k := range ring.Keys() {
			if err := r.ring.Add(k); err != nil {
				t.Fatalf("adding a key: %v", err)
			}
		}
	}

	// --- gateway -----------------------------------------------------------
	gwStore := testStore(t)
	proxyAddr := r.proxy.addr()
	gw, err := New(Config{
		SGUID: outSGU, StoreID: outStore, Scope: outScope(),
		BrokerAddr: "127.0.0.1:0", Store: gwStore, KeyRing: r.ring,
		LocalAuthority: r.localAuth,
		Cloud: func(ctx context.Context) (msgbus.Client, error) {
			dctx, dcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer dcancel()
			return mqtt.Dial(dctx, msgbus.Config{
				BrokerURL: "tcp://" + proxyAddr, ClientID: "sgu-" + string(outSGU),
				CleanSession: true, ConnectTimeout: time.Second, AckTimeout: time.Second,
			})
		},
		Detector: DetectorConfig{
			Interval: 200 * time.Millisecond, Timeout: 700 * time.Millisecond,
			FailThreshold: 3, FailFor: 500 * time.Millisecond,
			RecoverThreshold: 3, RecoverFor: 500 * time.Millisecond,
		},
		ScheduleTick: 100 * time.Millisecond, ReconcileSettle: 700 * time.Millisecond,
		AdminAddr: "127.0.0.1:0", Log: testLogger(),
	})
	if err != nil {
		t.Fatalf("building the gateway: %v", err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("starting the gateway: %v", err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = gw.Stop(sctx)
	})
	r.gw = gw

	// --- labels and controller ---------------------------------------------
	//
	// The label power profile is compressed for the test: a 100 ms listen
	// interval instead of the fleet's 30 seconds, so a scenario that has to
	// exercise a dozen price changes does not have to wait out a real duty
	// cycle. Everything else about the labels is the shipping model.
	power := labelsim.DefaultPower()
	power.BeaconFast = 100 * time.Millisecond
	power.BeaconSlow = 100 * time.Millisecond
	power.ActiveWindow = time.Hour

	r.eng = sim.New(time.Now().UTC(), 424242)
	// The labels verify for themselves: they hold the same ring the controller
	// does, carrying both the cloud's price authority and the store's delegated
	// one. That the delegated key is in there is what lets a price originated by
	// this gateway during an outage reach the glass at all.
	zone, err := labelsim.NewZone(r.eng, labelsim.ZoneSpec{
		StoreID: outStore, SECID: outSEC, Labels: labels, AisleLengthM: 10, Power: power,
		KeyRing: r.ring,
	})
	if err != nil {
		t.Fatalf("building the zone: %v", err)
	}
	r.zone = zone
	formed := false
	zone.Form(func(time.Duration) { formed = true })
	r.eng.RunUntil(r.eng.Elapsed() + 3*time.Minute)
	if !formed {
		t.Fatal("the zone never formed")
	}
	// From here the mesh runs paced against the wall clock, so that its timings
	// and the gateway's MQTT traffic are on the same footing.
	go func() { _ = r.eng.Run(ctx, 1) }()

	secStore := testStore(t)
	r.secBus = dialWithWill(t, ctx, gw.BrokerAddr(), "sec-"+string(outSEC), sec.WillFor(outScope(), outSEC))
	coord := sec.NewCoordinator(zone.Net, sec.SimScheduler(r.eng), sec.CoordinatorConfig{
		SECID: outSEC, StoreID: outStore, SampleInterval: 30 * time.Second, MaxInflight: 16,
	})
	specs := make([]sec.LabelSpec, 0, labels)
	for i, l := range zone.Labels() {
		specs = append(specs, sec.LabelSpec{ID: l.ID(), Node: l.NodeID(), Tier: l.Tier(),
			SKU: canon.SKU(fmt.Sprintf("SKU-%03d", i))})
	}
	ctl, err := sec.New(sec.Config{
		SECID: outSEC, StoreID: outStore, Scope: outScope(), Bus: r.secBus,
		Store: secStore, KeyRing: r.ring, Coordinator: coord, Sched: sec.SimScheduler(r.eng),
		Labels: specs, TelemetryInterval: 2 * time.Second, HeartbeatInterval: time.Second,
		MeshReportInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("building the controller: %v", err)
	}
	if err := ctl.Start(ctx); err != nil {
		t.Fatalf("starting the controller: %v", err)
	}
	t.Cleanup(func() { ctl.Stop(context.Background()) })
	r.ctl = ctl
	r.coord = coord

	// Seed the gateway's replica so it knows which controller owns which label,
	// which is how it addresses a locally originated or scheduled price.
	for _, s := range specs {
		if err := gw.Replica().PutLabel(LabelState{
			LabelID: s.ID, SECID: outSEC, SKU: s.SKU, Origin: OriginCloud,
			TS: gw.Clock().Now(), Render: canon.RenderSpec{Template: "standard"},
		}); err != nil {
			t.Fatalf("seeding the replica: %v", err)
		}
	}
	waitFor(t, 5*time.Second, "the store to reach connected mode", func() bool {
		return gw.Mode() == ModeConnected
	})
	return r
}

func mustAuthority(t *testing.T) *pki.PriceAuthority {
	t.Helper()
	a, err := pki.NewPriceAuthority(pki.PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("creating a price authority: %v", err)
	}
	return a
}

func dial(t *testing.T, ctx context.Context, addr, id string) msgbus.Client {
	t.Helper()
	return dialWithWill(t, ctx, addr, id, nil)
}

func dialWithWill(t *testing.T, ctx context.Context, addr, id string, will *msgbus.Will) msgbus.Client {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := mqtt.Dial(dctx, msgbus.Config{
		BrokerURL: "tcp://" + addr, ClientID: id, CleanSession: true,
		ConnectTimeout: 3 * time.Second, AckTimeout: 3 * time.Second, Will: will,
	})
	if err != nil {
		t.Fatalf("dialing %s as %q: %v", addr, id, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// publishFromCloud issues an attested price change from head office.
func (r *storeRig) publishFromCloud(t *testing.T, id canon.LabelID, sku canon.SKU, minor int64, seq int64) canon.PriceUpdated {
	t.Helper()
	upd := canon.PriceUpdated{
		LabelID: id, SKU: sku, StoreID: outStore, Price: canon.NewMoney(minor, "GBP"),
		EffectiveAt: time.Now().UTC(), Render: canon.RenderSpec{Template: "standard"}, Sequence: seq,
	}
	att, err := r.cloudAuth.Sign(canon.AttestationInputFrom(outTenant, upd))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	upd.Attestation = att
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", string(id), outTenant, upd)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.StoreID = outStore
	env.Region = "eu-west-1"
	env.Source = "label-service"
	env.RecordedAt = time.Now().UTC()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.headOffice.Publish(ctx, msgbus.Message{
		Topic:   outScope().SECLabelTopic(outSEC, id, canon.LeafPrice),
		Payload: body, QoS: canon.QoSPrice, Retain: true,
	}); err != nil {
		t.Fatalf("publishing from head office: %v", err)
	}
	return upd
}

// waitForGlass waits for a label to be showing a price, and on failure reports
// where in the chain the update stopped — which is the only useful thing a
// timeout can say when six components are involved.
func (r *storeRig) waitForGlass(t *testing.T, d time.Duration, id canon.LabelID, seq int64, minor int64) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if price, got, done := r.displayed(id); done && got == seq && price.Amount == minor {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := r.ctl.Record(id)
	st, _ := r.gw.Replica().Label(id)
	t.Fatalf("timed out after %v waiting for %s to show %d at sequence %d.\n"+
		"  gateway mode=%s replica=%s@%d origin=%s\n"+
		"  controller record: accepted=%d displayed=%d price=%s last_error=%q\n"+
		"  controller stats: %+v\n  coordinator: %+v\n  compliance alerts: %+v\n"+
		"  simulated clock has advanced %v since the mesh formed",
		d, id, minor, seq, r.gw.Mode(), st.Price, st.Sequence, st.Origin,
		rec.Sequence, rec.DisplayedSequence, rec.Price, rec.LastError,
		r.ctl.Stats(), r.coordStats(), r.ctl.ComplianceAlerts(), r.eng.Elapsed())
}

// coordStats exposes the zone coordinator's counters for failure reports.
func (r *storeRig) coordStats() any { return r.coord.Stats() }

// displayed reports what the controller believes is on a label's glass.
func (r *storeRig) displayed(id canon.LabelID) (canon.Money, int64, bool) {
	rec, ok := r.ctl.Record(id)
	if !ok {
		return canon.Money{}, 0, false
	}
	return rec.Price, rec.DisplayedSequence, rec.DisplayedSequence == rec.Sequence
}

func (r *storeRig) cloudMessages() []msgbus.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]msgbus.Message(nil), r.cloudSeen...)
}

// ---------------------------------------------------------------------------
// The scenario
// ---------------------------------------------------------------------------

func TestWANOutageEndToEnd(t *testing.T) {
	r := newStoreRig(t, 6)
	labels := r.zone.Labels()
	first := labels[0].ID()
	second := labels[1].ID()

	// --- 1. connected: a price from head office reaches the glass ----------
	r.publishFromCloud(t, first, "SKU-000", 249, 1)
	r.waitForGlass(t, 20*time.Second, first, 1, 249)
	if st, ok := r.gw.Replica().Label(first); !ok || st.Price.Amount != 249 || st.Origin != OriginCloud {
		t.Fatalf("the gateway's replica does not hold the cloud price: %+v", st)
	}
	t.Log("connected: a cloud price reached the glass and the gateway replicated it")

	// --- 2. a promotion is scheduled for the near future -------------------
	//
	// Attested now, activated later. That ordering is the whole reason a store
	// can activate a promotion with no cloud: the signatures were issued while
	// the link was up.
	activateAt := time.Now().Add(2 * time.Second).UTC()
	promoUpd := canon.PriceUpdated{
		LabelID: first, SKU: "SKU-000", StoreID: outStore, Price: canon.NewMoney(179, "GBP"),
		EffectiveAt: activateAt, PromotionID: "promo-spring", Sequence: 2,
		Render: canon.RenderSpec{Template: "promo", Badge: "SALE", ShowWas: true},
	}
	att, err := r.cloudAuth.Sign(canon.AttestationInputFrom(outTenant, promoUpd))
	if err != nil {
		t.Fatalf("signing the promotion: %v", err)
	}
	promoUpd.Attestation = att
	promo := ScheduledPromotion{
		PromotionID: "promo-spring", ActivateAt: activateAt,
		ExpireAt: activateAt.Add(time.Hour), Updates: []canon.PriceUpdated{promoUpd},
	}
	env, err := canon.NewEnvelope(canon.EvtPromotionActivated, "promotion", "promo-spring", outTenant, promo)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.StoreID = outStore
	env.Source = "promotion-service"
	env.RecordedAt = time.Now().UTC()
	body, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := r.headOffice.Publish(ctx, msgbus.Message{
		Topic: outScope().StoreTopic(canon.LeafPromotion), Payload: body, QoS: canon.QoSPrice,
	}); err != nil {
		t.Fatalf("publishing the promotion: %v", err)
	}
	cancel()
	waitFor(t, 10*time.Second, "the gateway to file the scheduled promotion", func() bool {
		return len(r.gw.Schedule().Pending()) == 1
	})
	t.Log("connected: a promotion was scheduled, pre-attested, for two seconds' time")

	// --- 3. cut the WAN ----------------------------------------------------
	beforeOutage := len(r.cloudMessages())
	r.proxy.cutLink()
	outageStart := time.Now()
	waitFor(t, 15*time.Second, "the store to go autonomous", func() bool {
		return r.gw.Mode() == ModeAutonomous
	})
	detect := time.Since(outageStart)
	t.Logf("outage: the store went autonomous %v after the link was cut", detect.Round(time.Millisecond))
	if detect < 500*time.Millisecond {
		t.Fatalf("the store went autonomous in %v; the hysteresis window is 500ms and it must be respected", detect)
	}

	// --- 4. autonomous: the scheduled promotion activates on local time -----
	waitFor(t, 20*time.Second, "the scheduled promotion to activate with no cloud", func() bool {
		price, _, done := r.displayed(first)
		return done && price.Amount == 179
	})
	if r.gw.Mode() != ModeAutonomous {
		t.Fatal("the store recovered before the promotion activated; the test proved nothing")
	}
	activated := r.gw.Schedule().All()
	if len(activated) != 1 || activated[0].ActivatedAt == nil {
		t.Fatal("the promotion is not recorded as activated")
	}
	t.Logf("autonomous: the scheduled promotion activated on local time (clock skew at activation %v)",
		activated[0].ActivationSkew)

	// --- 5. autonomous: a local point-of-sale price change ------------------
	//
	// The gateway attests it with its delegated key. Without that delegation the
	// controller would refuse it, and the shelf would not change — which is the
	// correct behaviour and is asserted separately.
	posCtx, posCancel := context.WithTimeout(context.Background(), 5*time.Second)
	updated, err := r.gw.LocalPriceChange(posCtx, canon.PriceChangeRequested{
		SKU: "SKU-001", StoreID: outStore, Price: canon.NewMoney(99, "GBP"),
		EffectiveAt: time.Now().UTC(), InitiatedBy: "store-manager", SourceSystem: "local-pos",
	})
	posCancel()
	if err != nil {
		t.Fatalf("the local point of sale was refused during the outage: %v", err)
	}
	if len(updated) != 1 || updated[0] != second {
		t.Fatalf("the local change touched %v, want just %s", updated, second)
	}
	waitFor(t, 20*time.Second, "the locally originated price to reach the glass", func() bool {
		price, _, done := r.displayed(second)
		return done && price.Amount == 99
	})
	t.Log("autonomous: a local point-of-sale price was attested by the delegated key and reached the glass")

	// --- 6. autonomous: upstream traffic buffers, nothing reaches the cloud --
	waitFor(t, 10*time.Second, "the upstream buffer to fill", func() bool {
		return r.gw.Queue().Depth() > 0
	})
	depth := r.gw.Queue().Depth()
	if got := len(r.cloudMessages()); got != beforeOutage {
		t.Fatalf("the cloud received %d messages during the outage; it must receive none", got-beforeOutage)
	}
	t.Logf("autonomous: %d upstream messages buffered, nothing reached the cloud", depth)

	// --- 7. head office changes the same label while the store is offline ---
	//
	// This is the conflict: the store changed SKU-001 locally, head office
	// changed it centrally, and neither could see the other.
	conflicted := r.publishFromCloud(t, second, "SKU-001", 149, 9)
	_ = conflicted

	// --- 8. zero label downtime during the outage --------------------------
	for _, l := range labels[:2] {
		if _, _, done := r.displayed(l.ID()); !done {
			t.Fatalf("label %s has an undelivered update at the end of the outage", l.ID())
		}
	}

	// --- 9. restore the link ------------------------------------------------
	r.proxy.restore()
	waitFor(t, 30*time.Second, "the store to reconnect", func() bool {
		return r.gw.Mode() == ModeConnected
	})
	waitFor(t, 30*time.Second, "reconciliation to finish", func() bool {
		_, ok := r.gw.LastReconciliation()
		return ok
	})
	report, _ := r.gw.LastReconciliation()
	t.Logf("recovered: %s", report.Summary())

	// --- 10. the buffer flushed, in order, with the conflict resolved -------
	waitFor(t, 20*time.Second, "the upstream buffer to drain", func() bool {
		return r.gw.Queue().Depth() == 0
	})
	if report.Flushed <= 0 {
		t.Fatalf("reconciliation reports %d flushed messages after buffering %d", report.Flushed, depth)
	}
	if report.Conflicts < 1 {
		t.Fatalf("reconciliation found %d conflicts; the store and head office both changed SKU-001", report.Conflicts)
	}
	foundPolicy := false
	for _, c := range report.ConflictDetail {
		if c.Key == PriceKey(second) && c.Resolution == ResolutionPolicyCloudPricing {
			foundPolicy = true
			if c.WinnerFrom != OriginCloud {
				t.Fatalf("the pricing conflict was won by %s", c.WinnerFrom)
			}
		}
	}
	if !foundPolicy {
		t.Fatalf("the pricing conflict on %s was not resolved by the cloud-wins policy: %+v",
			second, report.ConflictDetail)
	}
	if st, _ := r.gw.Replica().Label(second); st.Price.Amount != 149 {
		t.Fatalf("after the merge the replica holds %s for %s, head office said £1.49", st.Price, second)
	}

	// The cloud must have received the store's mode transitions and the buffered
	// evidence, in the order the store accepted it. The outage announcement was
	// buffered — the link was already down when it was made — so its arrival
	// after the recovery announcement's is itself the ordering guarantee working.
	modesSeen := func() []string {
		var out []string
		for _, m := range r.cloudMessages() {
			if m.Topic != outScope().StoreTopic(canon.LeafMode) {
				continue
			}
			var env canon.Envelope
			if err := json.Unmarshal(m.Payload, &env); err != nil {
				continue
			}
			var mode canon.StoreModeChanged
			if err := env.Decode(&mode); err != nil {
				continue
			}
			out = append(out, mode.Mode)
		}
		return out
	}
	waitFor(t, 20*time.Second, "the cloud to learn about the outage and the recovery", func() bool {
		modes := modesSeen()
		var sawAutonomous, sawConnected bool
		for _, m := range modes {
			switch m {
			case "autonomous":
				sawAutonomous = true
			case "connected":
				sawConnected = true
			}
		}
		return sawAutonomous && sawConnected
	})
	modes := modesSeen()
	if modes[0] != "autonomous" {
		t.Fatalf("the cloud saw store modes %v; the buffered outage announcement must arrive first", modes)
	}
	t.Logf("the cloud received %d messages after recovery; store mode transitions in arrival order: %v",
		len(r.cloudMessages())-beforeOutage, modes)

	// --- 11. and the store keeps working --------------------------------------
	r.publishFromCloud(t, first, "SKU-000", 219, 20)
	waitFor(t, 20*time.Second, "a post-recovery price to reach the glass", func() bool {
		price, seq, done := r.displayed(first)
		return done && seq == 20 && price.Amount == 219
	})
	t.Log("recovered: prices from head office reach the glass again")
}

func TestOrderedFlushAfterAnOutage(t *testing.T) {
	// The ordering guarantee, isolated: the cloud consumes per-key in order, and
	// a flush that reordered a store's acknowledgements would make its delivery
	// history unusable.
	st := testStore(t)
	q, err := NewQueue(st, QueueConfig{})
	if err != nil {
		t.Fatalf("opening queue: %v", err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := q.Enqueue(Entry{
			Topic: "ack", Payload: []byte(fmt.Sprintf("%d", i)), Class: ClassCritical,
			IdempotencyKey: fmt.Sprintf("evt-%03d", i),
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// Half of them were in fact delivered before the process died, which is the
	// one duplicate this design can actually produce.
	for i := 0; i < n; i += 2 {
		if err := q.MarkSent(fmt.Sprintf("evt-%03d", i)); err != nil {
			t.Fatalf("marking sent: %v", err)
		}
	}

	var published []string
	deduped := 0
	for {
		entries, err := q.Peek(32)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			if q.AlreadySent(e.IdempotencyKey) {
				deduped++
				q.NoteDeduplicated()
				_ = q.Remove(e.Seq)
				continue
			}
			published = append(published, string(e.Payload))
			_ = q.MarkSent(e.IdempotencyKey)
			_ = q.Remove(e.Seq)
		}
	}
	if deduped != n/2 {
		t.Fatalf("deduplicated %d, want %d", deduped, n/2)
	}
	if len(published) != n/2 {
		t.Fatalf("published %d, want %d", len(published), n/2)
	}
	for i, v := range published {
		want := fmt.Sprintf("%d", i*2+1)
		if v != want {
			t.Fatalf("position %d holds %q, want %q: the flush reordered the buffer", i, v, want)
		}
	}
}

func TestLocalPriceRefusedWithoutADelegatedKey(t *testing.T) {
	// The rule that makes the whole platform's guarantee hold: a store with no
	// signing key cannot author a price. Autonomy is bought with a delegation,
	// not with an exception.
	gw, err := New(Config{
		SGUID: outSGU, StoreID: outStore, Scope: outScope(),
		BrokerAddr: "127.0.0.1:0", Store: testStore(t),
	})
	if err != nil {
		t.Fatalf("building the gateway: %v", err)
	}
	if err := gw.Replica().PutLabel(LabelState{
		LabelID: "lbl-1", SECID: outSEC, SKU: "SKU-1", Origin: OriginCloud, TS: gw.Clock().Now(),
	}); err != nil {
		t.Fatalf("seeding the replica: %v", err)
	}
	_, err = gw.LocalPriceChange(context.Background(), canon.PriceChangeRequested{
		SKU: "SKU-1", StoreID: outStore, Price: canon.NewMoney(199, "GBP"),
		EffectiveAt: time.Now().UTC(), InitiatedBy: "manager", SourceSystem: "pos",
	})
	if !errors.Is(err, ErrNoLocalAuthority) {
		t.Fatalf("a store with no delegated key produced a price anyway: %v", err)
	}
}

func TestGatewayRefusesAPriceThatBreaksAGuardRail(t *testing.T) {
	gw, err := New(Config{
		SGUID: outSGU, StoreID: outStore, Scope: outScope(),
		BrokerAddr: "127.0.0.1:0", Store: testStore(t), LocalAuthority: mustAuthority(t),
	})
	if err != nil {
		t.Fatalf("building the gateway: %v", err)
	}
	if err := gw.Rules().Set(ProductRules{SKU: "SKU-1", Currency: "GBP", FloorMinor: 500}); err != nil {
		t.Fatalf("setting rules: %v", err)
	}
	if err := gw.Replica().PutLabel(LabelState{
		LabelID: "lbl-1", SECID: outSEC, SKU: "SKU-1", Origin: OriginCloud, TS: gw.Clock().Now(),
	}); err != nil {
		t.Fatalf("seeding the replica: %v", err)
	}
	_, err = gw.LocalPriceChange(context.Background(), canon.PriceChangeRequested{
		SKU: "SKU-1", StoreID: outStore, Price: canon.NewMoney(199, "GBP"),
		EffectiveAt: time.Now().UTC(), InitiatedBy: "manager", SourceSystem: "pos",
	})
	if !errors.Is(err, ErrRuleViolation) {
		t.Fatalf("a price below the statutory floor was accepted during an outage: %v", err)
	}
	if got := gw.Stats().RejectedByRules; got != 1 {
		t.Fatalf("the rejection was not counted: %d", got)
	}
}

var _ = kvstore.ErrNotFound
