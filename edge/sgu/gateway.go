package sgu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// Errors the gateway reports to its callers.
var (
	// ErrNoLocalAuthority means a locally originated price change was rejected
	// because this gateway holds no delegated signing key. It is not a bug: a
	// store that cannot attest a price must not display one, and the correct
	// response is to provision the delegation, not to bypass the check.
	ErrNoLocalAuthority = errors.New("sgu: no delegated price authority; a locally originated price cannot be attested")
	// ErrUnknownSKU means the local point of sale named a product no label in
	// this store is showing.
	ErrUnknownSKU = errors.New("sgu: no label in this store is showing that product")
	// ErrNotRunning is returned before Start or after Stop.
	ErrNotRunning = errors.New("sgu: gateway is not running")
)

// CloudFactory dials the cloud broker. It is a function rather than a client so
// the gateway can reconnect after an outage, and so a test can supply a broker
// it controls.
//
// Returning an error is normal and expected: it is what an outage looks like,
// and the gateway responds by staying autonomous and trying again.
type CloudFactory func(ctx context.Context) (msgbus.Client, error)

// Config parameterises a Store Gateway Unit.
type Config struct {
	SGUID   canon.SGUID
	StoreID canon.StoreID
	Scope   canon.TopicScope
	// BrokerAddr is where the store's own broker listens. Port 0 binds an
	// ephemeral port, which is what tests and the demo use.
	BrokerAddr string
	// BrokerAuthorizer gates the local broker. Nil means mqtt.AllowAll, which is
	// correct only for a development stack: production issues each controller a
	// certificate and enforces tenant isolation on every packet.
	BrokerAuthorizer mqtt.Authorizer
	// Cloud dials the cloud broker. Nil means the store has no cloud at all and
	// runs permanently autonomous, which is a supported configuration: it is what
	// a dark store or a disconnected pilot looks like.
	Cloud CloudFactory
	// Store is the gateway's durable state: the replica, the schedule, the
	// buffer and the rules.
	Store *kvstore.Store
	// KeyRing verifies attestations on prices arriving from the cloud, so a
	// compromised cloud broker cannot inject one. It may be nil, in which case
	// the gateway forwards without verifying and relies on the controllers,
	// which verify unconditionally.
	KeyRing *pki.KeyRing
	// LocalAuthority is the delegated, store-scoped price authority.
	//
	// Its existence is the answer to a question the architecture otherwise
	// leaves open: a store accepting price changes from a local point of sale
	// during an outage has to attest them, or its controllers will refuse them
	// and the shelf will not change. Delegation — a short-lived, store-scoped
	// key whose public half is in every local controller's key ring — is the
	// only way to have both autonomy and the guarantee in section 5 of the
	// interface contract. Nil disables local price origination entirely, which
	// is the right default for a store that has no local point of sale.
	LocalAuthority *pki.PriceAuthority
	Bridge         BridgeConfig
	Queue          QueueConfig
	Detector       DetectorConfig
	// ScheduleTick is how often the local promotion schedule is evaluated. Zero
	// means one second: a promotion that starts at 08:00:00 should not appear at
	// 08:00:30.
	ScheduleTick time.Duration
	// ReconcileSettle is how long reconciliation waits for the cloud's retained
	// state to arrive before merging. Zero means three seconds.
	ReconcileSettle time.Duration
	// MaxClockSkew bounds how far ahead of local time a peer's hybrid clock
	// timestamp may be.
	MaxClockSkew time.Duration
	// Now is the gateway's clock. Nil means time.Now.
	Now func() time.Time
	// AdminAddr is where the gateway's own diagnostics HTTP surface listens.
	// Empty disables it.
	AdminAddr string
	Log       *obs.Logger
	Registry  *obs.Registry
}

func (c Config) withDefaults() Config {
	if c.BrokerAddr == "" {
		c.BrokerAddr = "127.0.0.1:0"
	}
	if c.ScheduleTick == 0 {
		c.ScheduleTick = time.Second
	}
	if c.ReconcileSettle == 0 {
		c.ReconcileSettle = 3 * time.Second
	}
	if c.MaxClockSkew == 0 {
		c.MaxClockSkew = DefaultMaxSkew
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = obs.NopLogger()
	}
	c.Bridge = c.Bridge.withDefaults(c.Scope)
	return c
}

// Gateway is one Store Gateway Unit.
type Gateway struct {
	cfg Config

	broker   *mqtt.Broker
	brokerAt string
	local    msgbus.Client
	detector *Detector
	queue    *Queue
	replica  *Replica
	schedule *Schedule
	rules    *RulesEngine
	clock    *Clock
	admin    *adminServer

	mu           sync.Mutex
	cloud        msgbus.Client
	mode         Mode
	autonomousAt time.Time
	divergedAt   HLC
	lastReport   *ReconciliationReport
	// pendingCloud collects the cloud's retained state during the settle window
	// after a reconnect, which is what the merge runs against. The raw messages
	// are kept alongside the registers because a cloud value that wins a merge
	// has to be published to the controllers, and only the original attested
	// envelope can be: the gateway cannot re-sign someone else's price.
	pendingCloud map[string]Register
	pendingMsg   map[string]pendingPrice
	// collecting diverts downstream prices into pendingCloud instead of
	// applying them. It is set the moment the cloud link is re-established while
	// the store is autonomous — which is before the detector has confirmed
	// recovery — because the cloud's retained state arrives on re-subscription,
	// and that is the only chance to catch it.
	collecting       bool
	reconcileRunning bool
	running          bool
	stats            Stats
	secs             map[canon.SECID]SECStatus

	stopCh chan struct{}
	wg     sync.WaitGroup

	mMode      *obs.GaugeVec
	mQueue     *obs.GaugeVec
	mBridged   *obs.CounterVec
	mConflicts *obs.CounterVec
	mRules     *obs.HistogramVec
}

// SECStatus is what the gateway knows about one controller.
type SECStatus struct {
	SECID canon.SECID `json:"sec_id"`
	// Online is derived from the controller's retained status topic, which its
	// last will publishes on an unclean disconnect. That is how a controller
	// failure is noticed in under thirty seconds without polling twenty-five of
	// them.
	Online   bool      `json:"online"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	LastSeen time.Time `json:"last_seen"`
	Labels   int       `json:"labels,omitempty"`
	Dead     int       `json:"dead_labels,omitempty"`
}

// Stats is the gateway's own accounting.
type Stats struct {
	BridgedDown     uint64 `json:"bridged_downstream"`
	BridgedUp       uint64 `json:"bridged_upstream"`
	Buffered        uint64 `json:"buffered"`
	RejectedByRules uint64 `json:"rejected_by_rules"`
	AttestationBad  uint64 `json:"attestation_rejected"`
	LocalPrices     uint64 `json:"local_price_changes"`
	Activations     uint64 `json:"promotions_activated"`
	MissedPromos    uint64 `json:"promotions_missed"`
	Reconciliations uint64 `json:"reconciliations"`
	Conflicts       uint64 `json:"conflicts_resolved"`
}

// New builds a gateway. It binds nothing; call Start.
func New(cfg Config) (*Gateway, error) {
	cfg = cfg.withDefaults()
	switch {
	case cfg.SGUID == "":
		return nil, errors.New("sgu: gateway needs an SGU id")
	case cfg.StoreID == "":
		return nil, errors.New("sgu: gateway needs a store id")
	case cfg.Store == nil:
		return nil, errors.New("sgu: gateway needs a durable store; without one an outage loses everything the store did during it")
	}
	if err := cfg.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("sgu: gateway topic scope: %w", err)
	}

	q, err := NewQueue(cfg.Store, cfg.Queue)
	if err != nil {
		return nil, err
	}
	rep, err := NewReplica(cfg.Store)
	if err != nil {
		return nil, err
	}
	sch, err := NewSchedule(cfg.Store)
	if err != nil {
		return nil, err
	}
	rules, err := NewRulesEngine(cfg.Store)
	if err != nil {
		return nil, err
	}

	g := &Gateway{
		cfg: cfg, queue: q, replica: rep, schedule: sch, rules: rules,
		clock:        NewClock(string(cfg.SGUID), cfg.Now, cfg.MaxClockSkew),
		mode:         ModeConnected,
		pendingCloud: map[string]Register{},
		pendingMsg:   map[string]pendingPrice{},
		secs:         map[canon.SECID]SECStatus{},
		stopCh:       make(chan struct{}),
	}
	g.broker = mqtt.NewBroker(mqtt.Options{
		Addr:       cfg.BrokerAddr,
		Authorizer: cfg.BrokerAuthorizer,
		Logger:     cfg.Log,
	})
	g.detector = NewDetector(g.cloudUp, g.probeCloud, cfg.Detector)
	g.detector.SetClock(cfg.Now)
	g.detector.OnChange(g.onModeChange)

	if r := cfg.Registry; r != nil {
		g.mMode = r.Gauge("sgu_store_mode", "1 when the store is autonomous.", "store")
		g.mQueue = r.Gauge("sgu_upstream_queue", "Buffered upstream messages.", "store", "measure")
		g.mBridged = r.Counter("sgu_bridged_total", "Messages bridged, by direction and outcome.", "store", "direction", "outcome")
		g.mConflicts = r.Counter("sgu_merge_conflicts_total", "Reconciliation conflicts by resolution.", "store", "resolution")
		g.mRules = r.Histogram("sgu_rules_seconds", "Local pricing rule evaluation time.",
			[]float64{0.000001, 0.00001, 0.0001, 0.001, 0.01, 0.1}, "store")
	}
	return g, nil
}

// BrokerAddr returns the address the store's broker is serving on.
func (g *Gateway) BrokerAddr() string { return g.brokerAt }

// Broker returns the store's MQTT broker, which the demo binary uses to seed
// retained state directly.
func (g *Gateway) Broker() *mqtt.Broker { return g.broker }

// Replica returns the local state replica.
func (g *Gateway) Replica() *Replica { return g.replica }

// Schedule returns the local promotion schedule.
func (g *Gateway) Schedule() *Schedule { return g.schedule }

// Rules returns the local guard-rail engine.
func (g *Gateway) Rules() *RulesEngine { return g.rules }

// Queue returns the durable upstream buffer.
func (g *Gateway) Queue() *Queue { return g.queue }

// Clock returns the gateway's hybrid logical clock.
func (g *Gateway) Clock() *Clock { return g.clock }

// Detector returns the WAN detector, so an operator or a test can force a
// mode.
func (g *Gateway) Detector() *Detector { return g.detector }

// Mode returns the store's current operating mode.
func (g *Gateway) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// Start brings the gateway up: the local broker first, then the cloud link.
//
// The order is the whole design in miniature. The broker binds and starts
// serving before any attempt is made to reach the cloud, so a gateway that
// boots during an outage — a store that lost power and mains together — comes
// up serving its controllers from local state, and the cloud link is something
// that either arrives later or does not.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return nil
	}
	g.running = true
	g.mu.Unlock()

	addr, err := g.broker.Start()
	if err != nil {
		return fmt.Errorf("sgu: starting the store broker: %w", err)
	}
	g.brokerAt = addr.String()
	g.cfg.Log.Info("store broker listening", "store", g.cfg.StoreID, "addr", g.brokerAt)

	// Seed retained state from the replica before any controller connects, so a
	// controller that comes up first gets its zone's current prices from the
	// local broker rather than nothing.
	g.seedRetained()

	local, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL:      "tcp://" + g.brokerAt,
		ClientID:       "sgu-bridge-" + string(g.cfg.SGUID),
		CleanSession:   false,
		ConnectTimeout: 5 * time.Second,
		AckTimeout:     5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("sgu: connecting to the store's own broker: %w", err)
	}
	g.local = local

	for _, route := range g.cfg.Bridge.Upstream {
		r := route
		if err := g.local.Subscribe(ctx, r.Filter, r.QoS, func(ctx context.Context, m msgbus.Message) {
			g.handleUpstream(ctx, r, m)
		}); err != nil {
			return fmt.Errorf("sgu: subscribing to %s on the store broker: %w", r.Filter, err)
		}
	}

	// Reaching the cloud is best-effort: failing to is an outage, not a
	// start-up error.
	if g.cfg.Cloud != nil {
		if err := g.connectCloud(ctx); err != nil {
			g.cfg.Log.Warn("cloud unreachable at start-up; the store is autonomous from boot",
				"store", g.cfg.StoreID, "error", err)
			g.detector.ForceMode(ModeAutonomous, "cloud unreachable at start-up")
		}
	} else {
		g.detector.ForceMode(ModeAutonomous, "no cloud link is configured for this store")
	}

	g.wg.Add(2)
	go func() { defer g.wg.Done(); g.runDetector(ctx) }()
	go func() { defer g.wg.Done(); g.runSchedule(ctx) }()

	if g.cfg.AdminAddr != "" {
		srv, err := newAdminServer(g, g.cfg.AdminAddr)
		if err != nil {
			return err
		}
		g.admin = srv
		g.admin.start()
		g.cfg.Log.Info("gateway diagnostics listening", "store", g.cfg.StoreID, "addr", g.admin.addr())
	}
	return nil
}

// AdminAddr returns the diagnostics surface's bound address, or "".
func (g *Gateway) AdminAddr() string {
	if g.admin == nil {
		return ""
	}
	return g.admin.addr()
}

// Stop shuts the gateway down cleanly.
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	if !g.running {
		g.mu.Unlock()
		return nil
	}
	g.running = false
	cloud := g.cloud
	g.cloud = nil
	g.mu.Unlock()

	close(g.stopCh)
	g.detector.Stop()
	g.wg.Wait()

	if g.admin != nil {
		_ = g.admin.stop(ctx)
	}
	if g.local != nil {
		_ = g.local.Close()
	}
	if cloud != nil {
		_ = cloud.Close()
	}
	// The broker goes last: controllers keep being served until the very end of
	// a planned shutdown, and Shutdown deliberately suppresses last wills so a
	// maintenance window does not look like a store-wide controller failure.
	return g.broker.Shutdown(ctx)
}

// runDetector drives WAN probing.
func (g *Gateway) runDetector(ctx context.Context) {
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-g.stopCh:
			cancel()
		case <-dctx.Done():
		}
	}()
	g.detector.Run(dctx)
}

// runSchedule evaluates the local promotion calendar.
func (g *Gateway) runSchedule(ctx context.Context) {
	t := time.NewTicker(g.cfg.ScheduleTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-t.C:
			g.ActivateDue(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// The cloud link
// ---------------------------------------------------------------------------

func (g *Gateway) cloudUp() bool {
	g.mu.Lock()
	c := g.cloud
	g.mu.Unlock()
	return c != nil && c.Connected()
}

// probeCloud is the round-trip heartbeat.
//
// It is a QoS 1 publish rather than a read of the connection's state, because
// the failure this has to catch is the one a connection state cannot see: a TCP
// session to a load balancer that is still open while everything behind it is
// gone. Requiring the acknowledgement means the probe succeeds only if the
// store can genuinely still be heard.
func (g *Gateway) probeCloud(ctx context.Context) error {
	g.mu.Lock()
	c := g.cloud
	mode := g.mode
	g.mu.Unlock()
	if c == nil {
		if g.cfg.Cloud == nil {
			return errors.New("no cloud link is configured")
		}
		if err := g.connectCloud(ctx); err != nil {
			return err
		}
		g.mu.Lock()
		c = g.cloud
		g.mu.Unlock()
	}
	if c == nil || !c.Connected() {
		return msgbus.ErrNotConnected
	}
	// The MQTT client reconnects and restores its own subscriptions on a
	// persistent backoff, so a link can come back without the gateway having
	// dialled anything. That is the common case, and it is the one that would
	// silently skip the merge: the cloud's retained state arrives on the
	// client's own re-subscription, is treated as new instructions, and quietly
	// overwrites whatever the store decided while it was alone. Noticing the
	// link here is what closes that hole.
	if mode == ModeAutonomous {
		g.beginCollection(ctx)
	}
	body, err := json.Marshal(map[string]any{
		"sgu_id": g.cfg.SGUID, "store_id": g.cfg.StoreID, "at": g.cfg.Now().UTC(),
		"hlc": g.clock.Now().String(),
	})
	if err != nil {
		return err
	}
	return c.Publish(ctx, msgbus.Message{
		Topic: g.cfg.Scope.StoreTopic("probe"), Payload: body, QoS: msgbus.AtLeastOnce,
	})
}

// beginCollection starts diverting downstream prices into the merge buffer and
// asks the cloud to resend its retained state.
//
// The resend is the point: MQTT 3.1.1 section 3.8.4 requires a broker to
// deliver retained messages on every successful SUBSCRIBE, so re-subscribing to
// the downstream filters is a defined way to ask "what do you currently believe
// about this store?" without inventing a query protocol. It is idempotent, and
// it is cheap: a store's retained downstream state is one message per label.
func (g *Gateway) beginCollection(ctx context.Context) {
	g.mu.Lock()
	if g.collecting {
		g.mu.Unlock()
		return
	}
	g.collecting = true
	g.pendingCloud = map[string]Register{}
	g.pendingMsg = map[string]pendingPrice{}
	c := g.cloud
	g.mu.Unlock()
	if c == nil {
		return
	}
	for _, route := range g.cfg.Bridge.Downstream {
		r := route
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.Subscribe(sctx, r.Filter, r.QoS, func(ctx context.Context, m msgbus.Message) {
			g.handleDownstream(ctx, r, m)
		})
		cancel()
		if err != nil {
			g.cfg.Log.Warn("could not ask the cloud to resend its retained state",
				"store", g.cfg.StoreID, "filter", r.Filter, "error", err)
		}
	}
}

// connectCloud dials the cloud and installs the downstream subscriptions.
func (g *Gateway) connectCloud(ctx context.Context) error {
	if g.cfg.Cloud == nil {
		return errors.New("sgu: no cloud factory configured")
	}
	g.mu.Lock()
	if g.cloud != nil && g.cloud.Connected() {
		g.mu.Unlock()
		return nil
	}
	old := g.cloud
	g.cloud = nil
	if g.mode == ModeAutonomous {
		// Recovering. The cloud republishes its retained state the instant we
		// subscribe, and that state is the cloud's *view* rather than a set of
		// new instructions: it has to be collected for the merge, not applied
		// over the top of whatever the store decided while it was on its own.
		g.collecting = true
		g.pendingCloud = map[string]Register{}
		g.pendingMsg = map[string]pendingPrice{}
	}
	g.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	client, err := g.cfg.Cloud(ctx)
	if err != nil {
		return err
	}
	for _, route := range g.cfg.Bridge.Downstream {
		r := route
		if err := client.Subscribe(ctx, r.Filter, r.QoS, func(ctx context.Context, m msgbus.Message) {
			g.handleDownstream(ctx, r, m)
		}); err != nil {
			_ = client.Close()
			return fmt.Errorf("sgu: subscribing to %s on the cloud broker: %w", r.Filter, err)
		}
	}
	g.mu.Lock()
	g.cloud = client
	g.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Bridging
// ---------------------------------------------------------------------------

// handleDownstream bridges one message from the cloud into the store.
//
// Everything the store enforces locally happens here: attestation is checked so
// a compromised cloud broker cannot inject a price, the Tier-1 guard rails run
// before the message reaches a controller, the replica is updated so an outage
// starting a second later still knows what the shelf is showing, and scheduled
// promotions are filed rather than published.
func (g *Gateway) handleDownstream(ctx context.Context, r Route, m msgbus.Message) {
	g.mu.Lock()
	collecting := g.collecting
	g.mu.Unlock()

	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		g.cfg.Log.Warn("undecodable message from the cloud", "topic", m.Topic, "error", err)
		g.countBridge("downstream", "malformed")
		return
	}
	// Merging the sender's clock is what makes anything this store does next
	// causally after what the cloud just did, whatever either real-time clock
	// believes.
	//
	// Not while collecting, though. Retained state resent on a re-subscription
	// is old by construction — it may predate the outage by hours — and feeding
	// its age into the skew measurement would make every recovery look like a
	// clock fault, which would make the one measurement that matters after an
	// outage useless.
	if hlc := envelopeHLC(env); !hlc.IsZero() && !collecting {
		if _, err := g.clock.Observe(hlc); err != nil {
			g.cfg.Log.Warn("refusing a cloud timestamp", "topic", m.Topic, "error", err)
		}
	}

	switch env.EventType {
	case canon.EvtPriceUpdated:
		var upd canon.PriceUpdated
		if err := env.Decode(&upd); err != nil {
			g.countBridge("downstream", "malformed")
			return
		}
		if collecting {
			// During the settle window the cloud's retained prices are the
			// cloud's *view*, not new instructions. They are collected for the
			// merge and published only if they win it.
			g.collectCloudPrice(r, env, upd, m)
			return
		}
		g.applyCloudPrice(ctx, r, env, upd, m)
		return
	case canon.EvtPromotionActivated:
		if g.fileScheduledPromotion(env) {
			g.countBridge("downstream", "scheduled")
			return
		}
	}
	g.publishLocal(ctx, m.Topic, m.Payload, r.QoS, r.Retain)
	g.countBridge("downstream", "forwarded")
}

// applyCloudPrice verifies, guards, replicates and forwards a cloud price.
func (g *Gateway) applyCloudPrice(ctx context.Context, r Route, env canon.Envelope, upd canon.PriceUpdated, m msgbus.Message) {
	if g.cfg.KeyRing != nil {
		input := canon.AttestationInputFrom(env.TenantID, upd)
		if err := g.cfg.KeyRing.VerifyAt(input, upd.Attestation, g.cfg.Now()); err != nil {
			g.mu.Lock()
			g.stats.AttestationBad++
			g.mu.Unlock()
			g.countBridge("downstream", "attestation_rejected")
			g.cfg.Log.Error("refusing an unverifiable price from the cloud",
				"store", g.cfg.StoreID, "label", upd.LabelID, "kid", upd.Attestation.KeyID, "error", err)
			g.publishRejection(ctx, env, upd, "attestation rejected at the gateway: "+err.Error())
			return
		}
	}
	if v := g.evaluateRules(upd); !v.Allowed {
		g.mu.Lock()
		g.stats.RejectedByRules++
		g.mu.Unlock()
		g.countBridge("downstream", "rules_rejected")
		g.cfg.Log.Error("refusing a price that breaks a local guard rail",
			"store", g.cfg.StoreID, "label", upd.LabelID, "sku", upd.SKU,
			"price", upd.Price.String(), "violations", v.Error())
		g.publishRejection(ctx, env, upd, "local pricing rules: "+v.Error())
		return
	}

	ts := g.clock.Now()
	if err := g.replica.PutLabel(LabelState{
		LabelID: upd.LabelID, SECID: secOfTopic(m.Topic), SKU: upd.SKU, Price: upd.Price,
		Sequence: upd.Sequence, PromotionID: upd.PromotionID, Render: upd.Render,
		Attestation: upd.Attestation, EffectiveAt: upd.EffectiveAt,
		TS: ts, Origin: OriginCloud, UpdatedAt: g.cfg.Now().UTC(),
	}); err != nil {
		g.cfg.Log.Error("could not replicate a label's state", "label", upd.LabelID, "error", err)
	}
	g.publishLocal(ctx, m.Topic, m.Payload, r.QoS, r.Retain)
	g.countBridge("downstream", "forwarded")
}

// evaluateRules runs the guard rails and records the timing.
func (g *Gateway) evaluateRules(upd canon.PriceUpdated) Verdict {
	v := g.rules.Evaluate(upd.SKU, upd.Price)
	if g.mRules != nil {
		g.mRules.With(string(g.cfg.StoreID)).Observe(v.Elapsed.Seconds())
	}
	return v
}

// fileScheduledPromotion stores a promotion whose activation is in the future,
// reporting whether it did.
func (g *Gateway) fileScheduledPromotion(env canon.Envelope) bool {
	var p ScheduledPromotion
	if err := env.Decode(&p); err != nil {
		return false
	}
	p.Envelope = env
	p.ReceivedAt = g.cfg.Now().UTC()
	if err := p.Validate(); err != nil {
		g.cfg.Log.Error("refusing a scheduled promotion", "store", g.cfg.StoreID, "error", err)
		return false
	}
	if err := g.schedule.Add(p); err != nil {
		g.cfg.Log.Error("could not file a scheduled promotion", "store", g.cfg.StoreID, "error", err)
		return false
	}
	g.cfg.Log.Info("filed a scheduled promotion",
		"store", g.cfg.StoreID, "promotion", p.PromotionID,
		"activates_at", p.ActivateAt, "labels", len(p.Updates))
	return true
}

// handleUpstream bridges one message from the store toward the cloud, or
// buffers it.
func (g *Gateway) handleUpstream(ctx context.Context, r Route, m msgbus.Message) {
	g.noteSECStatus(r, m)

	g.mu.Lock()
	c := g.cloud
	autonomous := g.mode == ModeAutonomous
	g.mu.Unlock()

	entry := Entry{
		Topic: m.Topic, Payload: append([]byte(nil), m.Payload...), QoS: r.QoS,
		Retain: r.Retain, Class: r.Class, EnqueuedAt: g.cfg.Now().UTC(),
		IdempotencyKey: idempotencyKey(m.Payload), TS: g.clock.Now(),
	}

	if autonomous || c == nil || !c.Connected() {
		g.buffer(entry)
		return
	}
	if err := c.Publish(ctx, msgbus.Message{Topic: m.Topic, Payload: m.Payload, QoS: r.QoS, Retain: r.Retain}); err != nil {
		// The link went away between the mode check and the publish, which is
		// exactly the race autonomy exists to absorb. Buffer it; the detector
		// will notice the link shortly.
		g.buffer(entry)
		g.countBridge("upstream", "buffered_on_error")
		return
	}
	if entry.IdempotencyKey != "" {
		_ = g.queue.MarkSent(entry.IdempotencyKey)
	}
	g.mu.Lock()
	g.stats.BridgedUp++
	g.mu.Unlock()
	g.countBridge("upstream", "forwarded")
}

func (g *Gateway) buffer(e Entry) {
	if err := g.queue.Enqueue(e); err != nil {
		g.cfg.Log.Error("could not buffer an upstream message", "topic", e.Topic, "error", err)
		g.countBridge("upstream", "buffer_failed")
		return
	}
	g.mu.Lock()
	g.stats.Buffered++
	g.mu.Unlock()
	g.countBridge("upstream", "buffered")
	if g.mQueue != nil {
		s := g.queue.Stats()
		g.mQueue.With(string(g.cfg.StoreID), "depth").Set(float64(s.Depth))
		g.mQueue.With(string(g.cfg.StoreID), "bytes").Set(float64(s.Bytes))
	}
}

// noteSECStatus tracks controller liveness from the retained status and
// heartbeat topics, which is how the gateway learns about a controller failure
// within thirty seconds without polling.
func (g *Gateway) noteSECStatus(r Route, m msgbus.Message) {
	_, sec, leaf, ok := canon.ParseSECTopic(m.Topic)
	if !ok || sec == "" {
		return
	}
	if leaf != canon.LeafStatus && leaf != canon.LeafHeartbeat {
		return
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Labels int    `json:"labels"`
		Dead   int    `json:"dead_labels"`
	}
	_ = json.Unmarshal(m.Payload, &body)

	g.mu.Lock()
	st := g.secs[sec]
	st.SECID = sec
	st.LastSeen = g.cfg.Now().UTC()
	if body.Status != "" {
		st.Status = body.Status
		st.Online = body.Status == "online"
		st.Reason = body.Reason
	} else if leaf == canon.LeafHeartbeat {
		st.Online = true
		st.Status = "online"
	}
	if body.Labels > 0 {
		st.Labels = body.Labels
	}
	st.Dead = body.Dead
	g.secs[sec] = st
	g.mu.Unlock()
}

// SECs returns what the gateway knows about its controllers.
func (g *Gateway) SECs() []SECStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SECStatus, 0, len(g.secs))
	for _, st := range g.secs {
		out = append(out, st)
	}
	sortSECs(out)
	return out
}

// publishLocal republishes a message on the store's broker.
func (g *Gateway) publishLocal(ctx context.Context, topic string, payload []byte, qos msgbus.QoS, retain bool) {
	if err := g.broker.Publish(msgbus.Message{
		Topic: topic, Payload: payload, QoS: qos, Retain: retain,
	}); err != nil {
		g.cfg.Log.Error("could not publish to the store broker", "topic", topic, "error", err)
		return
	}
	g.mu.Lock()
	g.stats.BridgedDown++
	g.mu.Unlock()
}

// publishUpstream sends or buffers a locally originated event.
func (g *Gateway) publishUpstream(ctx context.Context, topic string, env canon.Envelope, class Class, qos msgbus.QoS, retain bool) {
	body, err := json.Marshal(env)
	if err != nil {
		return
	}
	// Publishing to the store's own broker rather than straight to the cloud
	// means the upstream bridge handles it exactly like a controller's message:
	// one buffering path, one ordering, one set of rules about what survives an
	// outage.
	if err := g.broker.Publish(msgbus.Message{Topic: topic, Payload: body, QoS: qos, Retain: retain}); err != nil {
		g.cfg.Log.Error("could not publish upstream", "topic", topic, "error", err)
	}
}

// publishRejection reports a refused price on the acknowledgement lane.
func (g *Gateway) publishRejection(ctx context.Context, parent canon.Envelope, upd canon.PriceUpdated, reason string) {
	child, err := parent.Caused(canon.EvtPriceRejected, "label", string(upd.LabelID), map[string]any{
		"label_id": upd.LabelID, "sku": upd.SKU, "store_id": g.cfg.StoreID,
		"sequence": upd.Sequence, "price": upd.Price, "reason": reason,
	})
	if err != nil {
		return
	}
	child.StoreID = g.cfg.StoreID
	child.Source = "sgu/" + string(g.cfg.SGUID)
	g.publishUpstream(ctx, g.cfg.Scope.SECLabelTopic(secOfLabel(g.replica, upd.LabelID), upd.LabelID, canon.LeafACK),
		child, ClassCritical, canon.QoSPrice, false)
}

func (g *Gateway) countBridge(direction, outcome string) {
	if g.mBridged != nil {
		g.mBridged.With(string(g.cfg.StoreID), direction, outcome).Inc()
	}
}

// seedRetained republishes the replica as retained messages on the local
// broker.
//
// This is what makes a controller's cold start work with the WAN down: it
// subscribes to its zone and immediately receives the current price of every
// label it owns, from a broker inside the building.
func (g *Gateway) seedRetained() {
	n := 0
	for _, st := range g.replica.Labels() {
		if st.Attestation.Signature == "" {
			// Never seed a price the controller would refuse. An unattested
			// retained message would produce a compliance alert on every
			// controller restart for as long as it sat there.
			continue
		}
		upd := st.PriceUpdate()
		upd.StoreID = g.cfg.StoreID
		env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", string(st.LabelID), g.cfg.Scope.Tenant, upd)
		if err != nil {
			continue
		}
		env.StoreID = g.cfg.StoreID
		env.Region = g.cfg.Scope.Region
		env.Source = "sgu/" + string(g.cfg.SGUID) + "/replica"
		env.RecordedAt = st.UpdatedAt
		body, err := json.Marshal(env)
		if err != nil {
			continue
		}
		if err := g.broker.Publish(msgbus.Message{
			Topic:   g.cfg.Scope.SECLabelTopic(st.SECID, st.LabelID, canon.LeafPrice),
			Payload: body, QoS: canon.QoSPrice, Retain: true,
		}); err == nil {
			n++
		}
	}
	if n > 0 {
		g.cfg.Log.Info("seeded retained label state from the local replica",
			"store", g.cfg.StoreID, "labels", n)
	}
}

// Stats returns the gateway's counters.
func (g *Gateway) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// idempotencyKey extracts the event identifier from an envelope so a flush can
// recognise a message the cloud already has.
func idempotencyKey(payload []byte) string {
	var env struct {
		EventID        string `json:"event_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return ""
	}
	if env.IdempotencyKey != "" {
		return env.IdempotencyKey
	}
	return env.EventID
}

// envelopeHLC reads a hybrid clock timestamp carried by an envelope, falling
// back to its recorded time.
//
// The fallback is deliberate and its limits are worth stating: a cloud that
// does not speak the gateway's hybrid clock still gets its wall-clock ordering
// respected, which is better than being treated as having no timestamp at all,
// and no worse than the pure wall-clock ordering it was already relying on.
func envelopeHLC(env canon.Envelope) HLC {
	if env.IdempotencyKey != "" {
		if h, err := ParseHLC(env.IdempotencyKey); err == nil {
			return h
		}
	}
	if env.RecordedAt.IsZero() {
		return HLC{}
	}
	return HLC{WallMS: env.RecordedAt.UnixMilli(), NodeID: env.Source}
}

// secOfTopic extracts the controller from a zone topic.
func secOfTopic(topic string) canon.SECID {
	if _, sec, _, _, ok := canon.ParseSECLabelTopic(topic); ok {
		return sec
	}
	if _, sec, _, ok := canon.ParseSECTopic(topic); ok {
		return sec
	}
	return ""
}

// secOfLabel finds which controller owns a label, from the replica.
func secOfLabel(r *Replica, id canon.LabelID) canon.SECID {
	if st, ok := r.Label(id); ok {
		return st.SECID
	}
	return "unknown"
}

// sortSECs orders controllers by identifier, so a diagnostics page refreshed
// twice shows them in the same order both times.
func sortSECs(s []SECStatus) {
	sort.Slice(s, func(i, j int) bool { return s[i].SECID < s[j].SECID })
}
