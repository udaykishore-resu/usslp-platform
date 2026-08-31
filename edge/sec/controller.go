package sec

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// Errors the update path can refuse an update with. Each one is a different
// operational story, which is why they are distinguishable rather than one
// "bad update" error.
var (
	// ErrAttestationRejected means the price could not be shown to have been
	// authorised by the platform. It is the hard rule of
	// INTERFACE-CONTRACTS section 5: the previous price stays on the glass.
	ErrAttestationRejected = errors.New("sec: price attestation rejected")
	// ErrSequenceRegression means the update is not newer than what the label is
	// already displaying. It is the expected outcome of an at-least-once
	// redelivery and is not an incident.
	ErrSequenceRegression = errors.New("sec: update sequence is not newer than the displayed one")
	// ErrUnknownLabel means the update addressed a label this controller does
	// not own, which usually means a planogram change left a stale retained
	// message on a zone topic.
	ErrUnknownLabel = errors.New("sec: label is not in this controller's zone")
)

// LabelSpec is one label in the controller's roster.
type LabelSpec struct {
	ID   canon.LabelID
	Node mesh.NodeID
	Tier labelsim.DisplayTier
	SKU  canon.SKU
}

// LabelRecord is the durable per-label state the controller keeps.
//
// It is what makes a cold start possible without the cloud: a controller that
// has been power-cycled knows what every label in its zone is displaying, what
// sequence it is at, and what image is on the glass, so it can decide whether a
// retained price update it receives on reconnect is news.
type LabelRecord struct {
	LabelID canon.LabelID `json:"label_id"`
	SKU     canon.SKU     `json:"sku,omitempty"`
	// Sequence is the highest sequence accepted, whether or not it reached the
	// glass. It is written before transmission so that a controller restarting
	// mid-delivery cannot re-issue a sequence it has already used.
	Sequence int64 `json:"sequence"`
	// DisplayedSequence is the highest sequence the label confirmed. The gap
	// between the two is a delivery in flight or one that failed.
	DisplayedSequence int64       `json:"displayed_sequence"`
	Price             canon.Money `json:"price"`
	PromotionID       canon.PromotionID
	// Attestation is retained because it is the evidence a trading standards
	// officer asks for, and the store may be offline when they ask.
	Attestation canon.Attestation `json:"attestation"`
	UpdatedAt   time.Time         `json:"updated_at"`
	DeliveredAt time.Time         `json:"delivered_at,omitempty"`
	LastError   string            `json:"last_error,omitempty"`
	BatteryPct  int               `json:"battery_pct,omitempty"`
	// ImageHash identifies the framebuffer stored alongside this record.
	ImageHash uint64 `json:"image_hash,omitempty"`
}

// ComplianceAlert records an update that was refused for a reason a compliance
// team has to see.
type ComplianceAlert struct {
	LabelID  canon.LabelID `json:"label_id"`
	SKU      canon.SKU     `json:"sku,omitempty"`
	Sequence int64         `json:"sequence"`
	Reason   string        `json:"reason"`
	KeyID    string        `json:"key_id,omitempty"`
	At       time.Time     `json:"at"`
	// HeldPrice is what the label kept showing, which is the fact that matters:
	// the shelf did not go blank and did not show an unauthorised price.
	HeldPrice canon.Money `json:"held_price"`
	// Verdict is why verification failed, when the refusal came from a label.
	// It is the field that separates "the ring is stale, redistribute it" from
	// "somebody rewrote a price in flight", and those have different pagers.
	Verdict string `json:"verdict,omitempty"`
	// Tampering marks the verdicts that mean the price on the wire is not the
	// price that was signed.
	Tampering bool `json:"tampering,omitempty"`
	// RefusedBy names which tier refused: this controller, or the glass.
	RefusedBy string `json:"refused_by"`
}

// OperationalAlert records a fault that needs an engineer but is not a
// compliance incident.
//
// It is a separate list from ComplianceAlert on purpose. A label running
// end-to-end attestation that is sent a legacy frame refuses every price in its
// zone; filing those as compliance incidents would generate one per label per
// update and bury the handful that mean a shopper could have been charged the
// wrong price. The fault is real and needs fixing — it is just a deployment
// fault, and it belongs in a different queue.
type OperationalAlert struct {
	LabelID  canon.LabelID `json:"label_id"`
	Sequence int64         `json:"sequence"`
	Kind     string        `json:"kind"`
	Detail   string        `json:"detail"`
	Action   string        `json:"action"`
	At       time.Time     `json:"at"`
}

// Config parameterises a Shelf Edge Controller.
type Config struct {
	SECID   canon.SECID
	StoreID canon.StoreID
	Scope   canon.TopicScope
	// Bus is the client connected to the store gateway's broker. The controller
	// never talks to the cloud.
	Bus msgbus.Client
	// Store is the durable label cache and update queue.
	Store *kvstore.Store
	// KeyRing verifies price attestations. Without one the controller refuses
	// every update, which is the correct failure: an unverifiable price is not
	// displayed.
	KeyRing *pki.KeyRing
	// Coordinator drives the zone's radio.
	Coordinator *Coordinator
	// Sched is the controller's clock.
	Sched Scheduler
	// Labels is the roster of labels in this zone.
	Labels []LabelSpec
	// TelemetryInterval is how often the controller publishes one aggregated
	// telemetry message for its whole zone. Forwarding per label would be 13
	// million messages a second across the fleet; batched per controller it is
	// under one a second per store.
	TelemetryInterval time.Duration
	// HeartbeatInterval is the controller's own liveness publication.
	HeartbeatInterval time.Duration
	// MeshReportInterval is how often the mesh topology is republished even when
	// nothing changed, so a gateway that restarted picks it up.
	MeshReportInterval time.Duration
	// Partial bounds when a partial refresh is attempted.
	Partial PartialThresholds
	// Attestation selects the air frame the controller emits. The zero value
	// carries the signed tuple end to end so the label can verify for itself,
	// which is what the firmware requires by default and therefore what the
	// platform ships.
	Attestation AttestationDelivery
	// MaxComplianceAlerts bounds the in-memory alert ring.
	MaxComplianceAlerts int
	Log                 *obs.Logger
	Registry            *obs.Registry
}

func (c Config) withDefaults() Config {
	if c.TelemetryInterval == 0 {
		c.TelemetryInterval = time.Minute
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.MeshReportInterval == 0 {
		c.MeshReportInterval = 2 * time.Minute
	}
	if c.Partial == (PartialThresholds{}) {
		c.Partial = DefaultPartialThresholds()
	}
	if c.MaxComplianceAlerts == 0 {
		c.MaxComplianceAlerts = 256
	}
	if c.Log == nil {
		c.Log = obs.NopLogger()
	}
	if c.Sched == nil {
		c.Sched = RealScheduler()
	}
	return c
}

// AttestationDelivery selects how much of the attestation crosses the mesh.
type AttestationDelivery int

const (
	// AttestEndToEnd emits frame type 4: the whole signed tuple travels to the
	// label, which verifies it against its own key ring before driving a pixel.
	//
	// It is the default, and it is a deliberate departure from
	// INTERFACE-CONTRACTS section 5, which stops the attestation at this tier.
	// Section 5's threat model is an attacker with write access to the store's
	// broker, and against that, controller-side verification is sufficient. It
	// is not sufficient against a controller that has itself been rooted or
	// swapped — which is inside the trust boundary the guarantee rests on, and
	// is a Linux box in a ceiling void above a shop floor. Verifying in both
	// places costs a couple of 802.15.4 fragments per hop and thirteen
	// milliseconds of the label's cell per update; see the package tests for
	// the measured effect on the latency budget.
	AttestEndToEnd AttestationDelivery = iota
	// AttestControllerOnly emits frame type 1: the controller verifies and the
	// label trusts it. It is the contract's own posture and exists for
	// interoperability with label firmware that predates frame type 4.
	AttestControllerOnly
)

// String names the mode for configuration and logs.
func (d AttestationDelivery) String() string {
	if d == AttestControllerOnly {
		return "controller-only"
	}
	return "end-to-end"
}

// WillFor builds the last-will message a controller registers when it connects.
//
// The will is how a gateway learns a controller has died in under thirty
// seconds without polling twenty-five of them. It is retained so that a gateway
// which itself restarts sees the dead controller immediately rather than
// waiting for a heartbeat that will never come.
func WillFor(scope canon.TopicScope, sec canon.SECID) *msgbus.Will {
	payload, _ := json.Marshal(map[string]any{
		"sec_id": sec,
		"status": "offline",
		"reason": "connection lost without a clean disconnect",
	})
	return &msgbus.Will{
		Topic:   scope.SECTopic(sec, canon.LeafStatus),
		Payload: payload,
		QoS:     msgbus.AtLeastOnce,
		Retain:  true,
	}
}

// Stats is the controller's own accounting.
type Stats struct {
	Received          uint64
	Applied           uint64
	AttestationFailed uint64
	SequenceDiscarded uint64
	RenderFailed      uint64
	UnknownLabel      uint64
	PartialRefreshes  uint64
	FullRefreshes     uint64
	// LabelRefused counts updates a label refused end to end after this
	// controller had already verified them. It should be zero: a non-zero value
	// means the two verifiers disagree, which is either a key ring that has
	// drifted or something between here and the glass rewriting frames.
	LabelRefused uint64
	// ConfigMismatch counts labels that refused a legacy frame because they
	// require end-to-end attestation. It is a deployment fault, not a
	// compliance one, and it is counted separately so that neither number
	// hides the other.
	ConfigMismatch   uint64
	DeliveryFailed   uint64
	TelemetryBatches uint64
	Labels           int
}

// Controller is one Shelf Edge Controller.
type Controller struct {
	cfg    Config
	labels map[canon.LabelID]LabelSpec

	mu       sync.Mutex
	records  map[canon.LabelID]*LabelRecord
	images   map[canon.LabelID]*Framebuffer
	telemBuf map[canon.LabelID]canon.Telemetry
	alerts   []ComplianceAlert
	opAlerts []OperationalAlert
	stats    Stats
	timers   []Timer
	started  bool
	stopped  bool

	onDelivery func(DeliveryResult)

	mUpdates    *obs.CounterVec
	mCompliance *obs.CounterVec
	mRefresh    *obs.CounterVec
	mRenderTime *obs.HistogramVec
}

// New creates a controller. It does not touch the network; call Start.
func New(cfg Config) (*Controller, error) {
	cfg = cfg.withDefaults()
	switch {
	case cfg.SECID == "":
		return nil, errors.New("sec: controller needs a SEC id")
	case cfg.Store == nil:
		return nil, errors.New("sec: controller needs a durable store; a controller with no cache cannot cold-start without the cloud")
	case cfg.Coordinator == nil:
		return nil, errors.New("sec: controller needs a zone coordinator")
	case cfg.KeyRing == nil:
		return nil, errors.New("sec: controller needs a key ring; without one no price could ever be verified and none would ever be displayed")
	}
	if err := cfg.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("sec: controller topic scope: %w", err)
	}
	c := &Controller{
		cfg:      cfg,
		labels:   make(map[canon.LabelID]LabelSpec, len(cfg.Labels)),
		records:  make(map[canon.LabelID]*LabelRecord),
		images:   make(map[canon.LabelID]*Framebuffer),
		telemBuf: make(map[canon.LabelID]canon.Telemetry),
	}
	for _, l := range cfg.Labels {
		if l.Node == "" {
			l.Node = mesh.NodeID(l.ID)
		}
		c.labels[l.ID] = l
		cfg.Coordinator.Register(l.ID, l.Node)
	}
	if r := cfg.Registry; r != nil {
		c.mUpdates = r.Counter("sec_updates_total", "Price updates by outcome.", "sec", "outcome")
		c.mCompliance = r.Counter("sec_compliance_alerts_total", "Updates refused for compliance reasons.", "sec", "reason")
		c.mRefresh = r.Counter("sec_refreshes_total", "Panel refreshes by waveform.", "sec", "waveform")
		c.mRenderTime = r.Histogram("sec_render_seconds", "Time to render one label's framebuffer.",
			[]float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1}, "sec")
	}
	return c, nil
}

// OnDelivery registers an observer for every completed delivery.
//
// It exists because the numbers a delivery carries — measured controller-to-
// label time, hop count, the waveform the label actually ran — are the
// platform's own SLO evidence, and something other than a log line has to be
// able to consume them: the gateway's diagnostics page, a load test, a
// site-survey tool run by an engineer standing in the aisle.
func (c *Controller) OnDelivery(fn func(DeliveryResult)) {
	c.mu.Lock()
	c.onDelivery = fn
	c.mu.Unlock()
}

// Start restores the durable cache, subscribes to the zone and begins the
// periodic publications.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.started = true
	c.mu.Unlock()

	if err := c.restore(); err != nil {
		return fmt.Errorf("sec: restoring label cache: %w", err)
	}
	if err := c.cfg.Coordinator.Start(); err != nil {
		return err
	}
	c.cfg.Coordinator.OnTelemetry(c.handleTelemetry)
	c.cfg.Coordinator.OnLinkEvent(c.handleLinkEvent)
	c.cfg.Coordinator.OnTopologyChange(func() { c.publishMesh(context.Background()) })

	if c.cfg.Bus != nil {
		filter := c.cfg.Scope.SubscribeZoneLabels(c.cfg.SECID, canon.LeafPrice)
		if err := c.cfg.Bus.Subscribe(ctx, filter, canon.QoSPrice, c.handlePrice); err != nil {
			return fmt.Errorf("sec: subscribing to %s: %w", filter, err)
		}
		zone := c.cfg.Scope.ZoneTopic(c.cfg.SECID, canon.LeafZonePrice)
		if err := c.cfg.Bus.Subscribe(ctx, zone, canon.QoSPrice, c.handlePrice); err != nil {
			return fmt.Errorf("sec: subscribing to %s: %w", zone, err)
		}
		c.publishStatus(ctx, "online", "")
	}

	c.every(c.cfg.HeartbeatInterval, func() { c.publishHeartbeat(context.Background()) })
	c.every(c.cfg.TelemetryInterval, func() { c.publishTelemetry(context.Background()) })
	c.every(c.cfg.MeshReportInterval, func() { c.publishMesh(context.Background()) })
	return nil
}

// every schedules a repeating callback on the controller's clock.
func (c *Controller) every(d time.Duration, fn func()) {
	if d <= 0 {
		return
	}
	var arm func()
	arm = func() {
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		t := c.cfg.Sched.AfterFunc(d, func() {
			fn()
			arm()
		})
		c.timers = append(c.timers, t)
		c.mu.Unlock()
	}
	arm()
}

// Stop publishes a clean offline status and halts the periodic work.
func (c *Controller) Stop(ctx context.Context) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	timers := c.timers
	c.timers = nil
	c.mu.Unlock()
	for _, t := range timers {
		t.Stop()
	}
	// A planned shutdown publishes its own status. The will exists for the
	// unplanned case, and a maintenance window must not look like a failure.
	c.publishTelemetry(ctx)
	c.publishStatus(ctx, "offline", "clean shutdown")
	c.cfg.Coordinator.Stop()
}

// ---------------------------------------------------------------------------
// Durable cache
// ---------------------------------------------------------------------------

func recordKey(id canon.LabelID) []byte { return []byte("label/" + string(id) + "/state") }
func imageKey(id canon.LabelID) []byte  { return []byte("label/" + string(id) + "/image") }

// restore reloads every label's state and last image from the durable store.
func (c *Controller) restore() error {
	it := c.cfg.Store.Scan([]byte("label/"))
	defer it.Close()
	for it.Next() {
		key := string(it.Key())
		if len(key) < 7 {
			continue
		}
		if !strings.HasSuffix(key, "/state") {
			continue
		}
		var rec LabelRecord
		if err := json.Unmarshal(it.Value(), &rec); err != nil {
			// A corrupted record is not a reason to refuse to start: the
			// controller re-learns the label's state from the next retained
			// price update. Refusing to boot would black out a whole aisle.
			c.cfg.Log.Warn("discarding a corrupted label record", "sec", c.cfg.SECID, "key", key, "error", err)
			continue
		}
		c.mu.Lock()
		c.records[rec.LabelID] = &rec
		c.mu.Unlock()
	}
	if err := it.Err(); err != nil {
		return err
	}
	for id := range c.labels {
		if raw, err := c.cfg.Store.Get(imageKey(id)); err == nil {
			if fb, err := DecodeRLE(raw); err == nil {
				c.mu.Lock()
				c.images[id] = fb
				c.mu.Unlock()
			}
		}
	}
	c.mu.Lock()
	c.stats.Labels = len(c.labels)
	n := len(c.records)
	c.mu.Unlock()
	c.cfg.Log.Info("restored label cache", "sec", c.cfg.SECID, "labels", len(c.labels), "records", n)
	return nil
}

// record returns the durable record for a label, creating an empty one.
func (c *Controller) recordFor(id canon.LabelID) *LabelRecord {
	if r, ok := c.records[id]; ok {
		return r
	}
	r := &LabelRecord{LabelID: id}
	c.records[id] = r
	return r
}

// persist writes a record, and optionally its image, atomically.
//
// Atomically because a record claiming sequence 42 alongside the image from
// sequence 41 would make the next partial-refresh decision wrong, and a wrong
// partial refresh is a price a shopper cannot read.
func (c *Controller) persist(rec *LabelRecord, img *Framebuffer) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("sec: encoding label record: %w", err)
	}
	b := c.cfg.Store.NewBatch()
	b.Put(recordKey(rec.LabelID), body)
	if img != nil {
		b.Put(imageKey(rec.LabelID), img.EncodeRLE())
	}
	if err := b.Write(); err != nil {
		return fmt.Errorf("sec: persisting label %s: %w", rec.LabelID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The update path
// ---------------------------------------------------------------------------

// handlePrice is the MQTT handler for a price update on this controller's zone.
func (c *Controller) handlePrice(ctx context.Context, m msgbus.Message) {
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		c.cfg.Log.Warn("undecodable envelope", "sec", c.cfg.SECID, "topic", m.Topic, "error", err)
		return
	}
	if env.EventType != canon.EvtPriceUpdated {
		return
	}
	var upd canon.PriceUpdated
	if err := env.Decode(&upd); err != nil {
		c.cfg.Log.Warn("undecodable price update", "sec", c.cfg.SECID, "topic", m.Topic, "error", err)
		return
	}
	if err := c.Apply(ctx, env, upd); err != nil && !errors.Is(err, ErrSequenceRegression) {
		c.cfg.Log.Warn("update refused", "sec", c.cfg.SECID, "label", upd.LabelID,
			"sequence", upd.Sequence, "error", err)
	}
}

// Apply is the whole update path: verify, sequence, render, decide the
// waveform, persist and transmit.
//
// It is exported because it is the behaviour worth testing directly, without an
// MQTT broker in the way, and because the store gateway's local promotion
// activation path drives the same code.
func (c *Controller) Apply(ctx context.Context, env canon.Envelope, upd canon.PriceUpdated) error {
	c.mu.Lock()
	c.stats.Received++
	c.mu.Unlock()

	spec, known := c.labels[upd.LabelID]
	if !known {
		c.count(&c.stats.UnknownLabel, "unknown_label")
		return fmt.Errorf("%w: %s", ErrUnknownLabel, upd.LabelID)
	}

	// 1. Attestation. This comes first, before rendering and before any state is
	//    touched, because an update that cannot be verified must leave no trace
	//    on this controller at all.
	//
	//    The digest is recomputed from the update being held, never read from
	//    the wire, so a valid signature lifted from a different price does not
	//    verify against this one.
	input := canon.AttestationInputFrom(env.TenantID, upd)
	if err := c.cfg.KeyRing.VerifyAt(input, upd.Attestation, c.cfg.Sched.Now()); err != nil {
		c.refuse(ctx, spec, upd, err)
		return fmt.Errorf("%w: %w", ErrAttestationRejected, err)
	}

	// 2. Monotonic sequence, per INTERFACE-CONTRACTS section 6. The label
	//    enforces this too; enforcing it here as well saves the radio time and
	//    the label's battery on a redelivery the label would only discard.
	c.mu.Lock()
	rec := c.recordFor(upd.LabelID)
	if upd.Sequence <= rec.Sequence {
		c.stats.SequenceDiscarded++
		current := rec.Sequence
		c.mu.Unlock()
		if c.mUpdates != nil {
			c.mUpdates.With(string(c.cfg.SECID), "sequence_discarded").Inc()
		}
		return fmt.Errorf("%w: %d is not greater than %d", ErrSequenceRegression, upd.Sequence, current)
	}
	prev := c.images[upd.LabelID]
	c.mu.Unlock()

	// 3. Render. The controller, not the cloud, decides pixels — which is the
	//    reason a store with no WAN can still change a price.
	renderStart := time.Now()
	fb, err := Render(RenderRequest{
		Tier: spec.Tier, Spec: upd.Render, Price: upd.Price, WasPrice: upd.WasPrice,
		UnitPrice: upd.UnitPrice, UnitMeasure: upd.UnitMeasure, SKU: upd.SKU,
		LabelID: upd.LabelID, PromotionID: upd.PromotionID,
	})
	if c.mRenderTime != nil {
		c.mRenderTime.With(string(c.cfg.SECID)).Observe(time.Since(renderStart).Seconds())
	}
	if err != nil {
		c.count(&c.stats.RenderFailed, "render_failed")
		return err
	}

	// 4. Waveform choice, from a real diff against what is on the glass.
	decision := DecidePartial(fb, prev, spec.Tier, c.cfg.Partial, upd.Render.PartialRefresh)

	// 5. Build the air frame. A partial refresh carries only the changed window.
	window := fb
	origin := Rect{}
	if decision.Partial {
		origin = decision.Diff.Bounds
		window = fb.SubImage(origin)
	}
	base := labelsim.Update{
		Sequence:   upd.Sequence,
		PriceMinor: upd.Price.Amount,
		Currency:   upd.Price.Currency,
		Flags:      partialFlag(decision.Partial) | ledFlag(upd.Render),
		Template:   TemplateCode(upd.Render.Template),
		OriginX:    uint16(origin.X0),
		OriginY:    uint16(origin.Y0),
		Image:      window.EncodeRLE(),
	}
	frame, err := c.encodeFrame(base, env.TenantID, upd)
	if err != nil {
		c.count(&c.stats.RenderFailed, "frame_encode_failed")
		return fmt.Errorf("sec: encoding air frame for %s: %w", upd.LabelID, err)
	}

	// 6. Reserve the sequence durably before it goes on the air, so a controller
	//    that restarts mid-delivery cannot re-issue a sequence it has used.
	c.mu.Lock()
	rec.Sequence = upd.Sequence
	rec.SKU = upd.SKU
	rec.Price = upd.Price
	rec.PromotionID = upd.PromotionID
	rec.Attestation = upd.Attestation
	rec.UpdatedAt = c.cfg.Sched.Now().UTC()
	rec.LastError = ""
	snapshot := *rec
	if decision.Partial {
		c.stats.PartialRefreshes++
	} else {
		c.stats.FullRefreshes++
	}
	c.mu.Unlock()
	if c.mRefresh != nil {
		waveform := "full"
		if decision.Partial {
			waveform = "partial"
		}
		c.mRefresh.With(string(c.cfg.SECID), waveform).Inc()
	}
	if err := c.persist(&snapshot, nil); err != nil {
		return err
	}

	verifyOverhead := time.Duration(0)
	if c.cfg.Attestation == AttestEndToEnd {
		verifyOverhead = labelsim.DefaultPower().VerifyDuration
	}
	c.cfg.Coordinator.Submit(Delivery{
		LabelID:        upd.LabelID,
		Node:           spec.Node,
		Sequence:       upd.Sequence,
		Payload:        frame,
		Attested:       c.cfg.Attestation == AttestEndToEnd,
		IssuedAt:       env.RecordedAt,
		Partial:        decision.Partial,
		VerifyOverhead: verifyOverhead,
		Done: func(res DeliveryResult) {
			c.onDelivered(env, upd, spec, fb, res, decision)
			c.mu.Lock()
			fn := c.onDelivery
			c.mu.Unlock()
			if fn != nil {
				fn(res)
			}
		},
	})
	return nil
}

// encodeFrame builds the air frame for an update in the configured mode.
//
// In end-to-end mode the identifiers, the effective instant, the key
// identifier, the digest and the signature all travel, so that the label can
// rebuild the canonical string itself. Nothing here is re-derived: the digest
// and signature are the ones the platform issued, decoded from their transport
// encodings, because a controller that recomputed them would be a controller
// able to author a price, which is the thing this frame exists to prevent.
func (c *Controller) encodeFrame(base labelsim.Update, tenant canon.TenantID, upd canon.PriceUpdated) ([]byte, error) {
	if c.cfg.Attestation == AttestControllerOnly {
		return labelsim.EncodeUpdate(base)
	}
	digest, err := hex.DecodeString(upd.Attestation.Digest)
	if err != nil || len(digest) != labelsim.DigestLen {
		return nil, fmt.Errorf("sec: attestation digest for %s is not %d hex-encoded bytes",
			upd.LabelID, labelsim.DigestLen)
	}
	sig, err := base64.StdEncoding.DecodeString(upd.Attestation.Signature)
	if err != nil || len(sig) != labelsim.SignatureLen {
		return nil, fmt.Errorf("sec: attestation signature for %s is not %d base64-encoded bytes",
			upd.LabelID, labelsim.SignatureLen)
	}
	att := labelsim.AttestedUpdate{
		Update:          base,
		EffectiveAtUnix: upd.EffectiveAt.UTC().Unix(),
		Alg:             labelsim.AttestAlgEd25519,
		KeyID:           upd.Attestation.KeyID,
		TenantID:        tenant,
		StoreID:         upd.StoreID,
		LabelID:         upd.LabelID,
		SKU:             upd.SKU,
		PromotionID:     upd.PromotionID,
	}
	if att.StoreID == "" {
		att.StoreID = c.cfg.StoreID
	}
	copy(att.Digest[:], digest)
	copy(att.Signature[:], sig)
	return labelsim.EncodeAttestedUpdate(att)
}

func partialFlag(partial bool) uint8 {
	if partial {
		return labelsim.FlagRequestPartial
	}
	return 0
}

func ledFlag(spec canon.RenderSpec) uint8 {
	if spec.Animation == "PULSE_BORDER" || spec.Animation == "FLASH" {
		return labelsim.FlagLEDPulse
	}
	return 0
}

// refuse handles an update that failed attestation.
//
// The only correct behaviour is to change nothing: the label keeps showing the
// last price it was given, which was verified. A blank shelf edge and an
// unverified price are both worse than a stale one.
func (c *Controller) refuse(ctx context.Context, spec LabelSpec, upd canon.PriceUpdated, cause error) {
	c.mu.Lock()
	c.stats.AttestationFailed++
	held := canon.Money{}
	if rec, ok := c.records[upd.LabelID]; ok {
		held = rec.Price
	}
	verdict := labelsim.VerdictFor(cause)
	alert := ComplianceAlert{
		LabelID: upd.LabelID, SKU: upd.SKU, Sequence: upd.Sequence,
		Reason: cause.Error(), KeyID: upd.Attestation.KeyID,
		At: c.cfg.Sched.Now().UTC(), HeldPrice: held,
		Verdict: verdict.String(), Tampering: verdict.Tampering(),
		RefusedBy: "controller",
	}
	c.alerts = append(c.alerts, alert)
	if len(c.alerts) > c.cfg.MaxComplianceAlerts {
		c.alerts = c.alerts[len(c.alerts)-c.cfg.MaxComplianceAlerts:]
	}
	c.mu.Unlock()

	if c.mCompliance != nil {
		c.mCompliance.With(string(c.cfg.SECID), "attestation").Inc()
	}
	if c.mUpdates != nil {
		c.mUpdates.With(string(c.cfg.SECID), "attestation_failed").Inc()
	}
	c.cfg.Log.Error("refusing an unverifiable price; the label keeps its previous price",
		"sec", c.cfg.SECID, "label", upd.LabelID, "sku", upd.SKU,
		"sequence", upd.Sequence, "kid", upd.Attestation.KeyID,
		"held_price", held.String(), "error", cause)

	// Report it upstream on the acknowledgement lane so the cloud sees a
	// delivery that did not happen and why. The lane is the one the contract
	// already defines for delivery outcomes; the event type distinguishes it.
	c.publishFailure(ctx, upd, "attestation rejected: "+cause.Error(), 1)
}

// onDelivered records the outcome of a delivery and reports it upstream.
func (c *Controller) onDelivered(env canon.Envelope, upd canon.PriceUpdated, spec LabelSpec,
	fb *Framebuffer, res DeliveryResult, decision PartialDecision) {

	ctx := context.Background()
	if !res.Delivered {
		c.mu.Lock()
		c.stats.DeliveryFailed++
		if rec, ok := c.records[upd.LabelID]; ok {
			if res.Err != nil {
				rec.LastError = res.Err.Error()
			} else {
				rec.LastError = "label discarded the update: " + res.Status.String()
			}
		}
		c.mu.Unlock()
		if c.mUpdates != nil {
			c.mUpdates.With(string(c.cfg.SECID), "delivery_failed").Inc()
		}
		reason := "delivery failed"
		if res.Err != nil {
			reason = res.Err.Error()
		} else if res.Status != labelsim.AckApplied {
			reason = "label reported " + res.Status.String()
		}
		// The two refusals the label reports in its own right, routed apart
		// because their runbooks are opposite.
		switch res.Status {
		case labelsim.AckRefusedAttestation:
			c.noteLabelRefusal(ctx, spec, upd, res.Verdict, false)
			return
		case labelsim.AckRefusedUnattested:
			c.noteConfigMismatch(ctx, upd, res.Sequence)
			return
		}
		if errors.Is(res.Err, ErrLabelRequiresAttestation) {
			c.noteConfigMismatch(ctx, upd, upd.Sequence)
			return
		}
		// Fallback, and deliberately secondary: a label whose firmware predates
		// the refusal status codes reports every refusal as a bad frame, which
		// on the wire is indistinguishable from a corrupted one. Inferring is
		// the best that can be done for such a device, and it is lossy in both
		// directions — a frame genuinely corrupted in flight is escalated into a
		// weights-and-measures process, and a configuration mismatch cannot be
		// seen at all. Every label that reports status 3 or 4 takes the branch
		// above instead, and this one should go away once no old firmware
		// remains in the fleet.
		if c.cfg.Attestation == AttestEndToEnd && res.Err == nil && res.Status == labelsim.AckBadFrame {
			c.noteLabelRefusal(ctx, spec, upd, labelsim.VerdictOK, true)
			return
		}
		c.publishFailure(ctx, upd, reason, res.Attempts)
		return
	}

	c.mu.Lock()
	c.stats.Applied++
	rec := c.recordFor(upd.LabelID)
	rec.DisplayedSequence = res.Sequence
	rec.DeliveredAt = c.cfg.Sched.Now().UTC()
	rec.BatteryPct = res.BatteryPct
	rec.ImageHash = fb.Hash()
	rec.LastError = ""
	snapshot := *rec
	c.images[upd.LabelID] = fb
	c.mu.Unlock()

	if c.mUpdates != nil {
		c.mUpdates.With(string(c.cfg.SECID), "applied").Inc()
	}
	// The image is persisted only now, because only now is it what is on the
	// glass, and the next partial-refresh decision diffs against exactly that.
	if err := c.persist(&snapshot, fb); err != nil {
		c.cfg.Log.Error("could not persist a delivered label state",
			"sec", c.cfg.SECID, "label", upd.LabelID, "error", err)
	}

	delivered := canon.LabelDelivered{
		LabelID:     upd.LabelID,
		StoreID:     c.cfg.StoreID,
		SECID:       c.cfg.SECID,
		Sequence:    res.Sequence,
		DeliveredAt: rec.DeliveredAt,
		LatencyMS:   res.TotalLatency.Milliseconds(),
		MeshHops:    res.Hops,
		RefreshMS:   res.RefreshMS,
		Partial:     res.Partial,
	}
	c.publishEnvelope(ctx, env, canon.EvtLabelDelivered, string(upd.LabelID), delivered,
		c.cfg.Scope.SECLabelTopic(c.cfg.SECID, upd.LabelID, canon.LeafACK), canon.QoSPrice, false)

	c.cfg.Log.Debug("label updated",
		"sec", c.cfg.SECID, "label", upd.LabelID, "sequence", res.Sequence,
		"sec_to_label_ms", res.SECToLabel.Milliseconds(), "hops", res.Hops,
		"refresh_ms", res.RefreshMS, "partial", res.Partial,
		"waveform_reason", decision.Reason, "end_to_end_ms", res.TotalLatency.Milliseconds())
}

// noteLabelRefusal records a price the label refused after this controller had
// verified it.
//
// It should never happen, and that is exactly why it is recorded loudly rather
// than counted quietly: the two verifiers ran the same canonical encoding over
// the same tuple against overlapping key rings, so a disagreement means either
// the label's ring has drifted past a rotation it missed, or something between
// this process and the glass is rewriting frames. Both need a human.
func (c *Controller) noteLabelRefusal(ctx context.Context, spec LabelSpec, upd canon.PriceUpdated,
	verdict labelsim.AttestVerdict, inferred bool) {

	reason := "the label refused the attestation end to end after this controller accepted it: " + verdict.String()
	if inferred {
		reason = "the label reported a bad frame for a price this controller had verified; " +
			"its firmware predates the refusal status codes, so this may be a refusal or a corrupted frame"
	}
	c.mu.Lock()
	c.stats.LabelRefused++
	held := canon.Money{}
	if rec, ok := c.records[upd.LabelID]; ok {
		held = rec.Price
	}
	alert := ComplianceAlert{
		LabelID: upd.LabelID, SKU: upd.SKU, Sequence: upd.Sequence, Reason: reason,
		KeyID: upd.Attestation.KeyID, At: c.cfg.Sched.Now().UTC(), HeldPrice: held,
		Verdict: verdict.String(), Tampering: verdict.Tampering(), RefusedBy: "label",
	}
	if inferred {
		alert.Verdict = ""
		alert.RefusedBy = "label (inferred)"
	}
	c.alerts = append(c.alerts, alert)
	if len(c.alerts) > c.cfg.MaxComplianceAlerts {
		c.alerts = c.alerts[len(c.alerts)-c.cfg.MaxComplianceAlerts:]
	}
	c.mu.Unlock()
	if c.mCompliance != nil {
		c.mCompliance.With(string(c.cfg.SECID), "label_refused").Inc()
	}
	c.cfg.Log.Error("a label refused a price this controller had verified",
		"sec", c.cfg.SECID, "label", upd.LabelID, "sequence", upd.Sequence,
		"kid", upd.Attestation.KeyID, "tier", spec.Tier.String(),
		"verdict", verdict.String(), "tampering", verdict.Tampering(), "inferred", inferred)
	c.publishFailure(ctx, upd, reason, 1)
}

// noteConfigMismatch records a label that will not take a legacy frame.
//
// It is an operational alert rather than a compliance one, and it names the
// remedy rather than the symptom: the price was never in doubt, the controller
// simply cannot deliver it in the form it is sending. The coordinator has
// already stopped transmitting unattested frames to this label, so the alert is
// a report of an action taken rather than a request for one.
func (c *Controller) noteConfigMismatch(ctx context.Context, upd canon.PriceUpdated, seq int64) {
	c.mu.Lock()
	c.stats.ConfigMismatch++
	alert := OperationalAlert{
		LabelID: upd.LabelID, Sequence: seq, Kind: "attestation-configuration-mismatch",
		Detail: "the label requires an end-to-end attestation and this controller sent an unattested frame",
		Action: "set this controller's attestation mode to end-to-end; " +
			"further unattested frames to this label are being suppressed rather than transmitted",
		At: c.cfg.Sched.Now().UTC(),
	}
	c.opAlerts = append(c.opAlerts, alert)
	if len(c.opAlerts) > c.cfg.MaxComplianceAlerts {
		c.opAlerts = c.opAlerts[len(c.opAlerts)-c.cfg.MaxComplianceAlerts:]
	}
	c.mu.Unlock()
	if c.mCompliance != nil {
		c.mCompliance.With(string(c.cfg.SECID), "config_mismatch").Inc()
	}
	c.cfg.Log.Error("this controller and one of its labels disagree about attestation",
		"sec", c.cfg.SECID, "label", upd.LabelID, "sequence", seq,
		"detail", alert.Detail, "action", alert.Action)
	c.publishFailure(ctx, upd, alert.Detail, 1)
}

// publishFailure reports a delivery that did not reach the glass.
func (c *Controller) publishFailure(ctx context.Context, upd canon.PriceUpdated, reason string, attempts int) {
	if c.cfg.Bus == nil {
		return
	}
	failed := canon.LabelDeliveryFailed{
		LabelID: upd.LabelID, StoreID: c.cfg.StoreID, SECID: c.cfg.SECID,
		Sequence: upd.Sequence, Reason: reason, Attempts: attempts,
	}
	env, err := canon.NewEnvelope(canon.EvtLabelDeliveryFailed, "label", string(upd.LabelID),
		c.cfg.Scope.Tenant, failed)
	if err != nil {
		return
	}
	env.StoreID = c.cfg.StoreID
	env.Region = c.cfg.Scope.Region
	env.Source = "sec/" + string(c.cfg.SECID)
	c.publish(ctx, c.cfg.Scope.SECLabelTopic(c.cfg.SECID, upd.LabelID, canon.LeafACK), env, canon.QoSPrice, false)
}

// publishEnvelope emits a child event that inherits the causing envelope's
// trace and correlation, so one price change is one trace from the POS webhook
// to the pixels settling.
func (c *Controller) publishEnvelope(ctx context.Context, parent canon.Envelope, eventType, aggregateID string,
	payload any, topic string, qos msgbus.QoS, retain bool) {
	if c.cfg.Bus == nil {
		return
	}
	env, err := parent.Caused(eventType, "label", aggregateID, payload)
	if err != nil {
		c.cfg.Log.Error("could not build an event", "sec", c.cfg.SECID, "type", eventType, "error", err)
		return
	}
	env.Source = "sec/" + string(c.cfg.SECID)
	c.publish(ctx, topic, env, qos, retain)
}

func (c *Controller) publish(ctx context.Context, topic string, env canon.Envelope, qos msgbus.QoS, retain bool) {
	body, err := json.Marshal(env)
	if err != nil {
		return
	}
	if err := c.cfg.Bus.Publish(ctx, msgbus.Message{Topic: topic, Payload: body, QoS: qos, Retain: retain}); err != nil {
		// A publish failure here is the store gateway being unreachable, which
		// is the gateway's problem to solve: it buffers. Logging at warn rather
		// than error keeps a WAN outage from filling the controller's log with
		// something it can do nothing about.
		c.cfg.Log.Warn("could not publish to the gateway", "sec", c.cfg.SECID, "topic", topic, "error", err)
	}
}

func (c *Controller) count(field *uint64, outcome string) {
	c.mu.Lock()
	*field++
	c.mu.Unlock()
	if c.mUpdates != nil {
		c.mUpdates.With(string(c.cfg.SECID), outcome).Inc()
	}
}

// ---------------------------------------------------------------------------
// Periodic publications
// ---------------------------------------------------------------------------

// handleTelemetry buffers a label's health report for the next batch.
func (c *Controller) handleTelemetry(id canon.LabelID, tel labelsim.TelemetryFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.telemBuf[id] = canon.Telemetry{
		LabelID: id, StoreID: c.cfg.StoreID, SECID: c.cfg.SECID,
		ReportedAt:    c.cfg.Sched.Now().UTC(),
		BatteryMV:     int(tel.BatteryMV),
		BatteryPct:    int(tel.BatteryPct),
		TemperatureC:  float64(tel.TemperatureCentiC) / 100,
		RSSI:          int(tel.ParentRSSI),
		LQI:           int(tel.ParentLQI),
		FirmwareVer:   "1.4.2",
		RefreshCount:  int64(tel.RefreshCount),
		NFCTapCount:   int64(tel.NFCTapCount),
		UptimeSeconds: int64(tel.UptimeSec),
		TamperFlag:    tel.Tamper,
	}
	if rec, ok := c.records[id]; ok {
		rec.BatteryPct = int(tel.BatteryPct)
	}
}

func (c *Controller) handleLinkEvent(ev LinkEvent) {
	if c.cfg.Bus == nil {
		return
	}
	env, err := canon.NewEnvelope(canon.EvtMeshLinkDegraded, "mesh", string(c.cfg.SECID), c.cfg.Scope.Tenant,
		map[string]any{
			"sec_id": c.cfg.SECID, "store_id": c.cfg.StoreID,
			"from": ev.From, "to": ev.To, "lqi": ev.LQI,
			"lqi_trend_per_minute": ev.Trend, "failure_risk": ev.Risk,
			"reason": ev.Reason, "predicted": ev.Predicted,
		})
	if err != nil {
		return
	}
	env.StoreID = c.cfg.StoreID
	env.Region = c.cfg.Scope.Region
	env.Source = "sec/" + string(c.cfg.SECID)
	c.publish(context.Background(), c.cfg.Scope.SECTopic(c.cfg.SECID, canon.LeafMesh), env, canon.QoSTelemetry, false)
}

// publishTelemetry emits one aggregated message for the whole zone.
func (c *Controller) publishTelemetry(ctx context.Context) {
	c.mu.Lock()
	if len(c.telemBuf) == 0 {
		c.mu.Unlock()
		return
	}
	batch := make([]canon.Telemetry, 0, len(c.telemBuf))
	for _, t := range c.telemBuf {
		batch = append(batch, t)
	}
	c.telemBuf = make(map[canon.LabelID]canon.Telemetry)
	c.stats.TelemetryBatches++
	c.mu.Unlock()

	sort.Slice(batch, func(i, j int) bool { return batch[i].LabelID < batch[j].LabelID })
	env, err := canon.NewEnvelope(canon.EvtDeviceTelemetry, "sec", string(c.cfg.SECID), c.cfg.Scope.Tenant, batch)
	if err != nil {
		return
	}
	env.StoreID = c.cfg.StoreID
	env.Region = c.cfg.Scope.Region
	env.Source = "sec/" + string(c.cfg.SECID)
	c.publish(ctx, c.cfg.Scope.SECTopic(c.cfg.SECID, canon.LeafTelemetry), env, canon.QoSTelemetry, false)
}

// publishMesh emits the zone topology, retained so a gateway restart picks it
// up without waiting for the next change.
func (c *Controller) publishMesh(ctx context.Context) {
	if c.cfg.Bus == nil {
		return
	}
	topo := c.cfg.Coordinator.Topology()
	env, err := canon.NewEnvelope(canon.EvtMeshTopologyChanged, "mesh", string(c.cfg.SECID), c.cfg.Scope.Tenant, topo)
	if err != nil {
		return
	}
	env.StoreID = c.cfg.StoreID
	env.Region = c.cfg.Scope.Region
	env.Source = "sec/" + string(c.cfg.SECID)
	c.publish(ctx, c.cfg.Scope.SECTopic(c.cfg.SECID, canon.LeafMesh), env, canon.QoSTelemetry, true)
}

// publishHeartbeat emits controller health, retained.
func (c *Controller) publishHeartbeat(ctx context.Context) {
	if c.cfg.Bus == nil {
		return
	}
	st := c.Stats()
	cs := c.cfg.Coordinator.Stats()
	body, err := json.Marshal(map[string]any{
		"sec_id": c.cfg.SECID, "store_id": c.cfg.StoreID, "status": "online",
		"at": c.cfg.Sched.Now().UTC(), "labels": st.Labels, "dead_labels": cs.DeadLabels,
		"queued": cs.Queued, "in_flight": cs.InFlight,
		"applied": st.Applied, "attestation_failed": st.AttestationFailed,
	})
	if err != nil {
		return
	}
	if err := c.cfg.Bus.Publish(ctx, msgbus.Message{
		Topic: c.cfg.Scope.SECTopic(c.cfg.SECID, canon.LeafHeartbeat), Payload: body,
		QoS: canon.QoSTelemetry, Retain: true,
	}); err != nil {
		c.cfg.Log.Debug("heartbeat not delivered", "sec", c.cfg.SECID, "error", err)
	}
}

// publishStatus emits the controller's lifecycle status on the same topic its
// last will names, so a clean shutdown and a crash are reported the same way and
// only the reason differs.
func (c *Controller) publishStatus(ctx context.Context, status, reason string) {
	if c.cfg.Bus == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"sec_id": c.cfg.SECID, "status": status, "reason": reason,
		"at": c.cfg.Sched.Now().UTC(),
	})
	if err != nil {
		return
	}
	_ = c.cfg.Bus.Publish(ctx, msgbus.Message{
		Topic: c.cfg.Scope.SECTopic(c.cfg.SECID, canon.LeafStatus), Payload: body,
		QoS: canon.QoSPrice, Retain: true,
	})
}

// ---------------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------------

// Stats returns the controller's counters.
func (c *Controller) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Labels = len(c.labels)
	return s
}

// Record returns a label's durable state.
func (c *Controller) Record(id canon.LabelID) (LabelRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.records[id]
	if !ok {
		return LabelRecord{}, false
	}
	return *r, true
}

// Image returns the framebuffer currently believed to be on a label's glass.
// It is what a support engineer is shown when a shopper reports a wrong price,
// and what the golden-image tests compare against.
func (c *Controller) Image(id canon.LabelID) (*Framebuffer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fb, ok := c.images[id]
	if !ok {
		return nil, false
	}
	return fb.Clone(), true
}

// ComplianceAlerts returns the recent refusals, newest last.
func (c *Controller) ComplianceAlerts() []ComplianceAlert {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ComplianceAlert(nil), c.alerts...)
}

// OperationalAlerts returns the recent deployment faults, newest last. They are
// kept apart from the compliance alerts so that neither queue hides the other.
func (c *Controller) OperationalAlerts() []OperationalAlert {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]OperationalAlert(nil), c.opAlerts...)
}

// Roster returns the labels this controller owns, sorted.
func (c *Controller) Roster() []LabelSpec {
	out := make([]LabelSpec, 0, len(c.labels))
	for _, l := range c.labels {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
