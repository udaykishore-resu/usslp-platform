package sec

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sim"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// ErrDeliveryAbandoned means every attempt to put an update on a label failed.
// It becomes a canon.LabelDeliveryFailed upstream and, after three of them, a
// device.status.offline.
var ErrDeliveryAbandoned = errors.New("sec: delivery abandoned after retries")

// ErrLabelRequiresAttestation means the destination has told this controller it
// will not display an unattested frame, so the update was never transmitted.
//
// It is deliberately not an ErrDeliveryAbandoned: nothing was attempted and
// nothing about the radio is wrong. Retrying is not merely futile, it is
// futile in a way the label has already explained, and spending a zone's
// airtime on it once per update per label is how a configuration mismatch
// turns into a performance incident.
var ErrLabelRequiresAttestation = errors.New("sec: the label requires an end-to-end attestation and this update carries none")

// Transport is the radio the controller drives.
//
// It is an interface rather than a concrete *mesh.Network so that the
// controller's scheduling, retry and healing logic is testable without a radio
// and substitutable for a real 802.15.4 host controller interface. Everything
// in it is something a real coordinator can do.
type Transport interface {
	Coordinator() mesh.NodeID
	Send(mesh.TxRequest)
	SetReceiver(id mesh.NodeID, fn func(mesh.Frame)) error
	SampleLinks(id mesh.NodeID) []mesh.LinkSample
	Topology() []mesh.NodeStatus
	ParentOf(id mesh.NodeID) (mesh.NodeID, bool)
	Avoid(a, b mesh.NodeID, avoid bool)
	RecomputeRoutes()
	Route(dst mesh.NodeID) []mesh.NodeID
	Alive(id mesh.NodeID) bool
	OnTopologyChange(fn func())
}

// Timer is a pending scheduled callback.
type Timer interface{ Stop() }

// Scheduler is the controller's clock and timer source.
//
// The controller must measure latency and schedule retries on the same clock
// the radio uses, or the numbers it reports are meaningless. In production both
// are the wall clock; under simulation both are the virtual one. Threading it
// through an interface rather than calling time.Now is what makes a
// 5,000-label latency measurement mean anything.
type Scheduler interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) Timer
}

// SimScheduler adapts a simulation engine to the Scheduler interface.
func SimScheduler(eng *sim.Engine) Scheduler { return simScheduler{eng} }

type simScheduler struct{ eng *sim.Engine }

func (s simScheduler) Now() time.Time { return s.eng.Now() }

func (s simScheduler) AfterFunc(d time.Duration, fn func()) Timer {
	return simTimer{s.eng, s.eng.At(d, fn)}
}

type simTimer struct {
	eng *sim.Engine
	ev  *sim.Event
}

func (t simTimer) Stop() { t.eng.Cancel(t.ev) }

// RealScheduler is the production clock.
func RealScheduler() Scheduler { return realScheduler{} }

type realScheduler struct{}

func (realScheduler) Now() time.Time { return time.Now() }
func (realScheduler) AfterFunc(d time.Duration, fn func()) Timer {
	return realTimer{time.AfterFunc(d, fn)}
}

// realTimer adapts time.Timer, whose Stop reports whether it beat the timer,
// to the Timer interface, where that answer is never useful: every caller here
// is cancelling a timer whose work is idempotent.
type realTimer struct{ t *time.Timer }

func (t realTimer) Stop() { t.t.Stop() }

// Delivery is one update to put on one label's glass.
type Delivery struct {
	LabelID canon.LabelID
	Node    mesh.NodeID
	// Sequence is the per-label monotonic counter carried in the air frame.
	Sequence int64
	// Payload is the encoded air frame, already rendered and compressed.
	Payload []byte
	// Attested records whether Payload carries its own proof. The coordinator
	// needs it because a label that has told us it requires end-to-end
	// attestation will refuse an unattested frame every time, and transmitting
	// one to it again is airtime spent on something that cannot succeed.
	Attested bool
	// IssuedAt is the envelope's RecordedAt: the moment USSLP took durable
	// responsibility for this price. The platform's SLO is measured from here,
	// because it is the only point a retailer can point at.
	IssuedAt time.Time
	// Partial records what was asked of the panel, for the delivery record.
	Partial bool
	// VerifyOverhead is time the label spends between taking the frame and
	// starting the waveform, which for an attested frame is one Ed25519
	// verification. The acknowledgement has no field for it — the wire protocol
	// predates end-to-end attestation and the firmware did not add one — so the
	// caller supplies what it knows it asked the label to do. Omitting it would
	// under-report the end-to-end latency by exactly that much.
	VerifyOverhead time.Duration
	// Done is called exactly once with the outcome.
	Done func(DeliveryResult)
}

// DeliveryResult is what happened to one update, with every timing the platform
// reports.
type DeliveryResult struct {
	LabelID  canon.LabelID
	Sequence int64
	// Delivered is true only when the label acknowledged that the pixels
	// changed. A frame that reached the radio and was then discarded by the
	// sequence rule is not a delivery.
	Delivered bool
	// Hops is the measured radio hop count.
	Hops int
	// SECToLabel is the measured time from the controller deciding to transmit
	// to the frame arriving at the label. This is the hop INTERFACE-CONTRACTS
	// §4 budgets — 400 ms since end-to-end attestation lengthened the frame.
	SECToLabel time.Duration
	// RefreshMS is what the label measured its own waveform taking.
	RefreshMS int
	// Partial is what the label actually did, which is not always what was asked.
	Partial bool
	// Verdict is why the label refused an attestation, when Status is
	// labelsim.AckRefusedAttestation.
	Verdict labelsim.AttestVerdict
	// ForcedFull records that the label overrode a partial request to clear
	// ghosting.
	ForcedFull bool
	// TotalLatency is IssuedAt to pixels settled: the number the three-second
	// SLO is written against.
	TotalLatency time.Duration
	// Attempts counts application-layer transmissions, not MAC retries.
	Attempts int
	// MACAttempts counts frame transmissions on the air, including MAC retries.
	MACAttempts int
	Status      labelsim.AckStatus
	BatteryMV   int
	BatteryPct  int
	Err         error
}

// CoordinatorConfig parameterises the zone coordinator.
type CoordinatorConfig struct {
	SECID   canon.SECID
	StoreID canon.StoreID
	// Healing selects predictive, reactive or no link-quality-driven rerouting.
	Healing HealingMode
	// RiskThreshold is the predicted failure probability at which the model
	// acts. Zero means 0.5.
	RiskThreshold float64
	// SampleInterval is the link-quality sampling cadence. The platform's figure
	// is 30 seconds: often enough to see a link degrade over minutes, rare
	// enough that the beacon exchanges it rides on cost nothing measurable.
	SampleInterval time.Duration
	// HistorySamples is how many samples the trend is fitted over. Ten samples
	// at 30 seconds is five minutes of history, matching the prediction horizon.
	HistorySamples int
	// BeaconMissLimit is how many consecutive missed reports mark a label dead.
	// Three is the platform's rule.
	BeaconMissLimit int
	// LabelReportInterval is how often a label is expected to be heard from. It
	// is the label's own telemetry cadence, not the controller's link-sampling
	// cadence, and conflating the two is how a healthy fleet gets declared dead:
	// a label reporting every five minutes will always look absent to a check
	// that expects to hear from it every thirty seconds. Zero means five
	// minutes, so a dead label is noticed inside fifteen.
	LabelReportInterval time.Duration
	// MaxInflight bounds concurrent transmissions. The channel serialises them
	// anyway; the limit exists so that waiting for five hundred sleeping labels'
	// receive windows does not consume five hundred pending jobs' worth of
	// state at once.
	MaxInflight int
	// Retry is the application-layer retry policy for a failed delivery.
	Retry retry.Policy
	// AckTimeout bounds the wait for a label's acknowledgement. It has to cover
	// the slowest waveform plus the return path, which for the colour panel is
	// fifteen seconds of refresh alone.
	AckTimeout time.Duration
	Log        *obs.Logger
	Registry   *obs.Registry
}

func (c CoordinatorConfig) withDefaults() CoordinatorConfig {
	if c.RiskThreshold == 0 {
		c.RiskThreshold = 0.5
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = 30 * time.Second
	}
	if c.HistorySamples == 0 {
		c.HistorySamples = 10
	}
	if c.BeaconMissLimit == 0 {
		c.BeaconMissLimit = 3
	}
	if c.LabelReportInterval == 0 {
		c.LabelReportInterval = 5 * time.Minute
	}
	if c.MaxInflight == 0 {
		c.MaxInflight = 8
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry = retry.Policy{MaxAttempts: 3, Base: 500 * time.Millisecond, Max: 5 * time.Second, Multiplier: 2, Jitter: true}
	}
	if c.AckTimeout == 0 {
		c.AckTimeout = 25 * time.Second
	}
	if c.Log == nil {
		c.Log = obs.NopLogger()
	}
	return c
}

// LinkEvent reports a link the controller has decided to route around.
type LinkEvent struct {
	From, To mesh.NodeID
	Risk     float64
	LQI      int
	Trend    float64
	Reason   string
	At       time.Time
	// Predicted is true when the model acted before the reactive threshold was
	// crossed. It is the field that makes the platform's claim measurable.
	Predicted bool
}

// CoordinatorStats is what the controller knows about its own zone.
type CoordinatorStats struct {
	Sent          uint64
	Delivered     uint64
	Failed        uint64
	Retried       uint64
	AckTimeouts   uint64
	StaleDiscards uint64
	// RefusedAttestation counts labels refusing a price whose attestation did
	// not verify. Non-zero is a compliance incident, not a statistic.
	RefusedAttestation uint64
	// RefusedUnattested counts labels refusing a legacy frame because they
	// require end-to-end attestation. Non-zero means this controller and that
	// label are running incompatible configurations.
	RefusedUnattested uint64
	// SuppressedUnattested counts transmissions not made because the
	// destination had already said it would refuse them.
	SuppressedUnattested uint64
	Reroutes             uint64
	PredictedHeals       uint64
	ReactiveHeals        uint64
	DeadLabels           int
	InFlight             int
	Queued               int
}

// pendingJob is one delivery in progress.
type pendingJob struct {
	del      Delivery
	attempt  int
	macTotal int
	startTx  time.Time
	arrived  time.Time
	hops     int
	ackTimer Timer
}

// labelState is the controller's mesh-level view of one label.
type labelState struct {
	node        mesh.NodeID
	missed      int
	lastSeen    time.Time
	battery     float64
	lastLQI     int
	lastRSSI    int
	dead        bool
	failureRisk float64
	// requiresAttestation is set when the label has told us, with a
	// labelsim.AckRefusedUnattested, that it will not display a legacy frame.
	// It is a fact about the device rather than a policy of ours, which is why
	// it is learned rather than configured.
	requiresAttestation bool
}

// Coordinator owns the Zigbee zone: it schedules transmissions, retries them,
// tracks who acknowledged, samples link quality and moves routes off links that
// are about to fail.
//
// Safe for concurrent use: the controller's MQTT handlers submit deliveries
// from the message dispatch pool while the radio completes them on the
// simulation or driver goroutine.
type Coordinator struct {
	cfg   CoordinatorConfig
	tp    Transport
	sched Scheduler

	mu      sync.Mutex
	pending map[canon.LabelID]*pendingJob
	queue   []Delivery
	labels  map[canon.LabelID]*labelState
	history map[linkPair]*linkHistory
	avoided map[linkPair]bool
	stats   CoordinatorStats
	started bool
	stopped bool
	tickers []Timer

	onAck       func(canon.LabelID, labelsim.Ack)
	onTelemetry func(canon.LabelID, labelsim.TelemetryFrame)
	onLinkEvent func(LinkEvent)
	onTopology  func()

	mSent      *obs.CounterVec
	mDelivered *obs.CounterVec
	mLatency   *obs.HistogramVec
	mHops      *obs.HistogramVec
	mReroutes  *obs.CounterVec
	mRisk      *obs.GaugeVec
}

type linkPair struct{ a, b mesh.NodeID }

func makePair(a, b mesh.NodeID) linkPair {
	if a > b {
		a, b = b, a
	}
	return linkPair{a, b}
}

// NewCoordinator builds a zone coordinator over a transport and a clock.
func NewCoordinator(tp Transport, sched Scheduler, cfg CoordinatorConfig) *Coordinator {
	cfg = cfg.withDefaults()
	c := &Coordinator{
		cfg:     cfg,
		tp:      tp,
		sched:   sched,
		pending: make(map[canon.LabelID]*pendingJob),
		labels:  make(map[canon.LabelID]*labelState),
		history: make(map[linkPair]*linkHistory),
		avoided: make(map[linkPair]bool),
	}
	if r := cfg.Registry; r != nil {
		c.mSent = r.Counter("sec_mesh_transmissions_total", "Application-layer transmissions attempted.", "sec")
		c.mDelivered = r.Counter("sec_mesh_deliveries_total", "Deliveries by outcome.", "sec", "outcome")
		c.mLatency = r.Histogram("sec_label_delivery_seconds",
			"Measured controller-to-label delivery latency.", obs.LatencyBuckets, "sec", "phase")
		c.mHops = r.Histogram("sec_mesh_hops", "Radio hops per delivery.",
			[]float64{1, 2, 3, 4, 5, 6}, "sec")
		c.mReroutes = r.Counter("sec_mesh_reroutes_total", "Routes moved off a link, by trigger.", "sec", "trigger")
		c.mRisk = r.Gauge("sec_mesh_link_failure_risk", "Predicted probability a link fails within the horizon.", "sec", "peer")
	}
	return c
}

// Register adds a label to the controller's roster so it is tracked for
// liveness and included in topology reports.
func (c *Coordinator) Register(id canon.LabelID, node mesh.NodeID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.labels[id]; ok {
		return
	}
	c.labels[id] = &labelState{node: node, battery: 1, lastSeen: c.sched.Now()}
}

// OnAck registers an observer for label acknowledgements.
func (c *Coordinator) OnAck(fn func(canon.LabelID, labelsim.Ack)) {
	c.mu.Lock()
	c.onAck = fn
	c.mu.Unlock()
}

// OnTelemetry registers an observer for uplink health reports.
func (c *Coordinator) OnTelemetry(fn func(canon.LabelID, labelsim.TelemetryFrame)) {
	c.mu.Lock()
	c.onTelemetry = fn
	c.mu.Unlock()
}

// OnLinkEvent registers an observer for rerouting decisions, which the
// controller publishes upstream as mesh.link.degraded.
func (c *Coordinator) OnLinkEvent(fn func(LinkEvent)) {
	c.mu.Lock()
	c.onLinkEvent = fn
	c.mu.Unlock()
}

// OnTopologyChange registers an observer for tree changes.
func (c *Coordinator) OnTopologyChange(fn func()) {
	c.mu.Lock()
	c.onTopology = fn
	c.mu.Unlock()
}

// Start attaches the coordinator to the radio and begins link sampling.
func (c *Coordinator) Start() error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.started = true
	c.mu.Unlock()

	if err := c.tp.SetReceiver(c.tp.Coordinator(), c.receive); err != nil {
		return fmt.Errorf("sec: coordinator %s: %w", c.cfg.SECID, err)
	}
	c.tp.OnTopologyChange(func() {
		c.mu.Lock()
		fn := c.onTopology
		c.mu.Unlock()
		if fn != nil {
			fn()
		}
	})
	c.scheduleSample()
	return nil
}

// Stop halts sampling and abandons queued work.
func (c *Coordinator) Stop() {
	c.mu.Lock()
	c.stopped = true
	timers := c.tickers
	c.tickers = nil
	c.mu.Unlock()
	for _, t := range timers {
		t.Stop()
	}
}

// Submit queues an update for delivery.
//
// Submission never blocks: the controller's MQTT handlers must not stall the
// dispatch pool waiting for a sleeping label's receive window, which can be
// thirty seconds away.
func (c *Coordinator) Submit(d Delivery) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		c.finish(d, DeliveryResult{LabelID: d.LabelID, Sequence: d.Sequence, Err: ErrDeliveryAbandoned})
		return
	}
	c.queue = append(c.queue, d)
	c.stats.Queued = len(c.queue)
	c.mu.Unlock()
	c.pump()
}

// pump starts as many queued deliveries as the in-flight limit allows.
//
// At most one update per label is ever in flight. A second would race the first
// onto the same panel and the sequence rule would discard whichever arrived
// second, regardless of which was newer — so the queue is scanned for the first
// label that is free rather than rotated, which would spin forever in the
// entirely normal case where every queued label is already busy.
func (c *Coordinator) pump() {
	for {
		c.mu.Lock()
		if c.stopped || len(c.pending) >= c.cfg.MaxInflight {
			c.stats.Queued = len(c.queue)
			c.stats.InFlight = len(c.pending)
			c.mu.Unlock()
			return
		}
		idx := -1
		for i, d := range c.queue {
			if _, busy := c.pending[d.LabelID]; !busy {
				idx = i
				break
			}
		}
		if idx < 0 {
			c.stats.Queued = len(c.queue)
			c.stats.InFlight = len(c.pending)
			c.mu.Unlock()
			return
		}
		d := c.queue[idx]
		c.queue = append(c.queue[:idx], c.queue[idx+1:]...)
		job := &pendingJob{del: d}
		c.pending[d.LabelID] = job
		c.stats.Queued = len(c.queue)
		c.stats.InFlight = len(c.pending)
		c.mu.Unlock()
		c.transmit(job)
	}
}

// transmit puts one attempt of a job on the air.
func (c *Coordinator) transmit(job *pendingJob) {
	c.mu.Lock()
	if st, ok := c.labels[job.del.LabelID]; ok && st.requiresAttestation && !job.del.Attested {
		c.stats.SuppressedUnattested++
		c.mu.Unlock()
		c.complete(job, DeliveryResult{
			LabelID: job.del.LabelID, Sequence: job.del.Sequence,
			Attempts: job.attempt, MACAttempts: job.macTotal,
			Err: ErrLabelRequiresAttestation,
		})
		return
	}
	job.attempt++
	job.startTx = c.sched.Now()
	c.stats.Sent++
	attempt := job.attempt
	c.mu.Unlock()
	if c.mSent != nil {
		c.mSent.With(string(c.cfg.SECID)).Inc()
	}

	c.tp.Send(mesh.TxRequest{
		Dst:     job.del.Node,
		Payload: job.del.Payload,
		Done: func(r mesh.TxResult) {
			c.mu.Lock()
			job.macTotal += r.Attempts
			c.mu.Unlock()
			if !r.Delivered {
				c.handleTxFailure(job, r, attempt)
				return
			}
			c.mu.Lock()
			job.arrived = c.sched.Now()
			job.hops = r.Hops
			latency := r.Elapsed
			job.ackTimer = c.sched.AfterFunc(c.cfg.AckTimeout, func() { c.handleAckTimeout(job) })
			c.mu.Unlock()
			if c.mLatency != nil {
				c.mLatency.With(string(c.cfg.SECID), "sec_to_label").Observe(latency.Seconds())
			}
			if c.mHops != nil {
				c.mHops.With(string(c.cfg.SECID)).Observe(float64(r.Hops))
			}
		},
	})
}

// handleTxFailure retries a delivery, repairing the mesh first when the radio
// told us which link gave up.
func (c *Coordinator) handleTxFailure(job *pendingJob, r mesh.TxResult, attempt int) {
	if errors.Is(r.Err, mesh.ErrLinkFailed) && r.FailedFrom != "" && r.FailedTo != "" {
		// The MAC gave up on a specific link. Route around it before retrying,
		// or the retry takes the same broken path.
		c.routeAround(r.FailedFrom, r.FailedTo, LinkEvent{
			From: r.FailedFrom, To: r.FailedTo, Reason: "MAC retries exhausted on this hop",
			At: c.sched.Now(),
		}, "link-failure")
	}
	if c.cfg.Retry.MaxAttempts > 0 && attempt >= c.cfg.Retry.MaxAttempts {
		c.complete(job, DeliveryResult{
			LabelID: job.del.LabelID, Sequence: job.del.Sequence, Attempts: attempt,
			MACAttempts: job.macTotal, Hops: r.Hops,
			Err: fmt.Errorf("%w: %d attempts: %w", ErrDeliveryAbandoned, attempt, r.Err),
		})
		return
	}
	c.mu.Lock()
	c.stats.Retried++
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		c.complete(job, DeliveryResult{LabelID: job.del.LabelID, Sequence: job.del.Sequence, Err: ErrDeliveryAbandoned})
		return
	}
	c.sched.AfterFunc(c.cfg.Retry.Delay(attempt+1), func() { c.transmit(job) })
}

// handleAckTimeout fires when a label took the frame but never confirmed.
func (c *Coordinator) handleAckTimeout(job *pendingJob) {
	c.mu.Lock()
	if _, live := c.pending[job.del.LabelID]; !live {
		c.mu.Unlock()
		return
	}
	c.stats.AckTimeouts++
	attempt := job.attempt
	stopped := c.stopped
	c.mu.Unlock()

	if stopped || (c.cfg.Retry.MaxAttempts > 0 && attempt >= c.cfg.Retry.MaxAttempts) {
		c.complete(job, DeliveryResult{
			LabelID: job.del.LabelID, Sequence: job.del.Sequence, Attempts: attempt,
			MACAttempts: job.macTotal, Hops: job.hops,
			Err: fmt.Errorf("%w: label took the frame but never acknowledged", ErrDeliveryAbandoned),
		})
		return
	}
	c.mu.Lock()
	c.stats.Retried++
	c.mu.Unlock()
	c.sched.AfterFunc(c.cfg.Retry.Delay(attempt+1), func() { c.transmit(job) })
}

// receive dispatches an uplink frame arriving at the coordinator.
func (c *Coordinator) receive(f mesh.Frame) {
	kind, ok := labelsim.FrameKind(f.Payload)
	if !ok {
		return
	}
	id := canon.LabelID(f.Src)
	c.markSeen(id)
	switch kind {
	case labelsim.FrameAck:
		ack, err := labelsim.DecodeAck(f.Payload)
		if err != nil {
			c.cfg.Log.Warn("undecodable acknowledgement", "sec", c.cfg.SECID, "label", id, "error", err)
			return
		}
		c.handleAck(id, ack, f.Hops)
	case labelsim.FrameTelemetry:
		tel, err := labelsim.DecodeTelemetry(f.Payload)
		if err != nil {
			c.cfg.Log.Warn("undecodable telemetry", "sec", c.cfg.SECID, "label", id, "error", err)
			return
		}
		c.handleTelemetry(id, tel)
	}
}

func (c *Coordinator) markSeen(id canon.LabelID) {
	c.mu.Lock()
	st, ok := c.labels[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	st.lastSeen = c.sched.Now()
	st.missed = 0
	st.dead = false
	c.mu.Unlock()
}

func (c *Coordinator) handleAck(id canon.LabelID, ack labelsim.Ack, hops int) {
	c.mu.Lock()
	st := c.labels[id]
	if st != nil {
		st.battery = float64(ack.BatteryPct) / 100
	}
	job, ok := c.pending[id]
	if !ok || job.del.Sequence != ack.Sequence {
		// An acknowledgement for something we are no longer waiting on: a
		// duplicate, or the answer to a transmission we already retried. Not an
		// error, and specifically not something to complete a live job with.
		c.noteAckStatusLocked(id, ack)
		fn := c.onAck
		c.mu.Unlock()
		if fn != nil {
			fn(id, ack)
		}
		return
	}
	if job.ackTimer != nil {
		job.ackTimer.Stop()
	}
	c.noteAckStatusLocked(id, ack)
	res := DeliveryResult{
		LabelID:     id,
		Sequence:    ack.Sequence,
		Delivered:   ack.Status == labelsim.AckApplied,
		Hops:        job.hops,
		SECToLabel:  job.arrived.Sub(job.startTx),
		RefreshMS:   int(ack.RefreshMS),
		Partial:     ack.Partial,
		ForcedFull:  ack.ForcedFull,
		Attempts:    job.attempt,
		MACAttempts: job.macTotal,
		Status:      ack.Status,
		Verdict:     ack.Verdict,
		BatteryMV:   int(ack.BatteryMV),
		BatteryPct:  int(ack.BatteryPct),
	}
	// Pixels settled when the waveform finished, which is the arrival time plus
	// whatever the label did before starting it plus the refresh it measured —
	// not now, which also includes the acknowledgement's own trip back through
	// the mesh.
	settled := job.arrived.Add(job.del.VerifyOverhead).
		Add(time.Duration(ack.RefreshMS) * time.Millisecond)
	if !job.del.IssuedAt.IsZero() {
		res.TotalLatency = settled.Sub(job.del.IssuedAt)
	}
	fn := c.onAck
	c.mu.Unlock()
	if fn != nil {
		fn(id, ack)
	}
	c.complete(job, res)
}

// noteAckStatusLocked records what a label said about an update, including the
// one fact worth remembering across deliveries: that it will not accept an
// unattested frame.
//
// Learning it from the device rather than inferring it from configuration is
// the point. A zone can be half-upgraded, and a controller that assumed every
// label matched its own setting would either keep transmitting frames a
// upgraded label refuses, or refuse to transmit to one that would have taken
// them.
func (c *Coordinator) noteAckStatusLocked(id canon.LabelID, ack labelsim.Ack) {
	switch ack.Status {
	case labelsim.AckStaleSequence:
		c.stats.StaleDiscards++
	case labelsim.AckRefusedAttestation:
		c.stats.RefusedAttestation++
	case labelsim.AckRefusedUnattested:
		c.stats.RefusedUnattested++
		if st, ok := c.labels[id]; ok && !st.requiresAttestation {
			st.requiresAttestation = true
			c.cfg.Log.Error("a label refused an unattested price and will refuse every one after it",
				"sec", c.cfg.SECID, "label", id, "sequence", ack.Sequence,
				"action", "this controller will stop sending it legacy frames; deploy end-to-end attestation for this zone")
		}
	}
}

// RequiresAttestation reports whether a label has told this controller it will
// not display an unattested frame.
func (c *Coordinator) RequiresAttestation(id canon.LabelID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.labels[id]
	return ok && st.requiresAttestation
}

func (c *Coordinator) handleTelemetry(id canon.LabelID, tel labelsim.TelemetryFrame) {
	c.mu.Lock()
	st := c.labels[id]
	if st != nil {
		st.battery = float64(tel.BatteryPct) / 100
		st.lastLQI = int(tel.ParentLQI)
		st.lastRSSI = int(tel.ParentRSSI)
	}
	// A label's own measurement of its uplink is half the mesh picture: the
	// controller can see how well it hears the label, but only the label knows
	// how well it hears its parent.
	if parent, ok := c.tp.ParentOf(mesh.NodeID(id)); ok && parent != "" && tel.ParentLQI > 0 {
		c.recordSampleLocked(mesh.NodeID(id), parent, float64(tel.ParentLQI), float64(tel.ParentRSSI))
	}
	fn := c.onTelemetry
	c.mu.Unlock()
	if fn != nil {
		fn(id, tel)
	}
}

// complete finishes a job, releases its slot and starts the next queued one.
func (c *Coordinator) complete(job *pendingJob, res DeliveryResult) {
	c.mu.Lock()
	if cur, ok := c.pending[job.del.LabelID]; !ok || cur != job {
		c.mu.Unlock()
		return
	}
	delete(c.pending, job.del.LabelID)
	if res.Delivered {
		c.stats.Delivered++
	} else {
		c.stats.Failed++
	}
	c.stats.InFlight = len(c.pending)
	c.mu.Unlock()

	if c.mDelivered != nil {
		outcome := "failed"
		switch {
		case res.Delivered:
			outcome = "delivered"
		case errors.Is(res.Err, ErrLabelRequiresAttestation):
			outcome = "suppressed-unattested"
		case res.Status != labelsim.AckApplied && res.Err == nil:
			outcome = res.Status.String()
		}
		c.mDelivered.With(string(c.cfg.SECID), outcome).Inc()
	}
	if c.mLatency != nil && res.TotalLatency > 0 {
		c.mLatency.With(string(c.cfg.SECID), "end_to_end").Observe(res.TotalLatency.Seconds())
	}
	c.finish(job.del, res)
	c.pump()
}

func (c *Coordinator) finish(d Delivery, res DeliveryResult) {
	if d.Done != nil {
		d.Done(res)
	}
}

// ---------------------------------------------------------------------------
// Link sampling and self-healing
// ---------------------------------------------------------------------------

// scheduleSample arms the next link-quality sampling tick.
func (c *Coordinator) scheduleSample() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	t := c.sched.AfterFunc(c.cfg.SampleInterval, func() {
		c.SampleLinks()
		c.scheduleSample()
	})
	c.tickers = append(c.tickers, t)
	c.mu.Unlock()
}

// SampleLinks takes one round of link-quality measurements and applies the
// healing policy.
//
// The controller samples its own neighbours and those of every relay it can
// reach, which is the set of links downstream traffic actually traverses. It
// does not poll five hundred battery labels: waking them to ask how they are
// would cost more energy than the answer is worth, which is why a label's own
// uplink measurement rides on the telemetry it was going to send anyway.
func (c *Coordinator) SampleLinks() {
	topo := c.tp.Topology()
	batteries := make(map[mesh.NodeID]float64, len(topo))
	depths := make(map[mesh.NodeID]int, len(topo))
	relays := make(map[mesh.NodeID]bool, len(topo))
	var samplers []mesh.NodeID
	coord := c.tp.Coordinator()
	relays[coord] = true
	for _, st := range topo {
		batteries[st.ID] = st.Battery
		depths[st.ID] = st.Depth
		if st.Kind == mesh.KindCoordinator || st.Kind == mesh.KindRouter {
			relays[st.ID] = true
		}
		if st.Online && (st.Kind == mesh.KindCoordinator || st.Kind == mesh.KindRouter) {
			samplers = append(samplers, st.ID)
		}
	}

	type action struct {
		from, to mesh.NodeID
		ev       LinkEvent
		trigger  string
	}
	var acts []action
	var clears []linkPair

	c.mu.Lock()
	now := c.sched.Now()
	for _, from := range samplers {
		for _, s := range c.tp.SampleLinks(from) {
			if depths[s.Peer] < depths[from] && s.Peer != coord {
				continue // sample each link from its upstream end only
			}
			c.recordSampleLocked(from, s.Peer, float64(s.LQI), s.RSSI)
			pair := makePair(from, s.Peer)
			h := c.history[pair]
			a := assess(c.cfg.Healing, h, s.Peer, batteries[s.Peer], depths[s.Peer], c.cfg.RiskThreshold)
			if st, ok := c.labels[canon.LabelID(s.Peer)]; ok {
				st.failureRisk = a.Risk
				st.lastLQI = s.LQI
				st.lastRSSI = int(s.RSSI)
			}
			// Rerouting is only meaningful for links that carry transit traffic.
			// The last hop to a battery label is the only way to reach it, so
			// marking it avoided achieves nothing except to distort every route
			// that happens to pass nearby — and if enough of them are marked, the
			// zone loses paths it still needs. The risk is still recorded and
			// still reported upward, because a label whose own link is failing is
			// something an operator wants to know about; it just is not something
			// routing can fix.
			transit := relays[s.Peer]
			switch {
			case a.Act && transit && !c.avoided[pair]:
				trigger := "reactive"
				predicted := false
				if a.Why != "link quality below the reroute threshold" {
					trigger, predicted = "predictive", true
				}
				acts = append(acts, action{from, s.Peer, LinkEvent{
					From: from, To: s.Peer, Risk: a.Risk, LQI: int(a.Features.LQI),
					Trend: a.Features.LQITrendPerMinute, Reason: a.Why, At: now, Predicted: predicted,
				}, trigger})
			case !a.Act && transit && c.avoided[pair] && a.Features.LQI > RerouteThreshold+30 && a.Risk < c.cfg.RiskThreshold/2:
				// Recovered with margin. Clearing the avoidance matters: a
				// transient obstruction must not permanently poison a link, or
				// the mesh slowly loses every path it ever doubted.
				clears = append(clears, pair)
			}
		}
	}
	c.mu.Unlock()

	for _, a := range acts {
		c.routeAround(a.from, a.to, a.ev, a.trigger)
	}
	for _, p := range clears {
		c.mu.Lock()
		delete(c.avoided, p)
		c.mu.Unlock()
		c.tp.Avoid(p.a, p.b, false)
	}
	c.checkDeadLabels()
}

// recordSampleLocked appends one measurement to a link's history.
func (c *Coordinator) recordSampleLocked(a, b mesh.NodeID, lqi, rssi float64) {
	pair := makePair(a, b)
	h := c.history[pair]
	if h == nil {
		h = newLinkHistory(c.cfg.HistorySamples)
		c.history[pair] = h
	}
	// The history stores sample times as a duration since the Unix epoch. Only
	// differences are ever taken from them, so the origin is arbitrary; what
	// matters is that it is the same clock the rest of the controller measures
	// on, which under simulation is the virtual one.
	h.add(time.Duration(c.sched.Now().UnixNano()), lqi, rssi)
	if c.mRisk != nil {
		c.mRisk.With(string(c.cfg.SECID), string(b)).Set(FailureRisk(LinkFeatures{
			LQI: lqi, LQITrendPerMinute: h.trendPerMinute(), RSSIStdDev: h.rssiStdDev(), BatteryFraction: 1,
		}))
	}
}

// routeAround asks the radio to prefer any path that avoids a link, and reports
// the decision upstream.
func (c *Coordinator) routeAround(a, b mesh.NodeID, ev LinkEvent, trigger string) {
	pair := makePair(a, b)
	c.mu.Lock()
	if c.avoided[pair] {
		c.mu.Unlock()
		return
	}
	c.avoided[pair] = true
	c.stats.Reroutes++
	if ev.Predicted {
		c.stats.PredictedHeals++
	} else {
		c.stats.ReactiveHeals++
	}
	fn := c.onLinkEvent
	c.mu.Unlock()

	c.tp.Avoid(a, b, true)
	if c.mReroutes != nil {
		c.mReroutes.With(string(c.cfg.SECID), trigger).Inc()
	}
	c.cfg.Log.Info("routing around a degraded link",
		"sec", c.cfg.SECID, "from", a, "to", b, "lqi", ev.LQI,
		"trend_per_min", ev.Trend, "risk", ev.Risk, "reason", ev.Reason, "predicted", ev.Predicted)
	if fn != nil {
		fn(ev)
	}
}

// checkDeadLabels marks labels that have missed their reporting window.
//
// Three missed beacons is the platform's rule (INTERFACE-CONTRACTS section 5:
// suppression is "visible within three missed heartbeats"), and it is a
// deliberately blunt one — a label that has stopped answering is either flat,
// stolen or behind something metal, and all three need a human.
func (c *Coordinator) checkDeadLabels() {
	c.mu.Lock()
	now := c.sched.Now()
	window := c.cfg.LabelReportInterval * time.Duration(c.cfg.BeaconMissLimit)
	dead := 0
	for _, st := range c.labels {
		if !c.tp.Alive(st.node) {
			st.dead = true
		} else if now.Sub(st.lastSeen) > window {
			st.missed++
			st.lastSeen = now
			if st.missed >= c.cfg.BeaconMissLimit {
				st.dead = true
			}
		}
		if st.dead {
			dead++
		}
	}
	c.stats.DeadLabels = dead
	c.mu.Unlock()
}

// DeadLabels returns the labels the controller believes are off the air.
func (c *Coordinator) DeadLabels() []canon.LabelID {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []canon.LabelID
	for id, st := range c.labels {
		if st.dead {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Stats returns the coordinator's counters.
func (c *Coordinator) Stats() CoordinatorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.InFlight = len(c.pending)
	s.Queued = len(c.queue)
	return s
}

// Topology renders the zone as the canonical mesh report, with the predicted
// failure risk the cloud's store health map is drawn from.
func (c *Coordinator) Topology() canon.MeshTopology {
	raw := c.tp.Topology()
	c.mu.Lock()
	risks := make(map[mesh.NodeID]float64, len(c.labels))
	for id, st := range c.labels {
		risks[mesh.NodeID(id)] = st.failureRisk
	}
	now := c.sched.Now()
	c.mu.Unlock()

	nodes := make([]canon.MeshNode, 0, len(raw))
	for _, st := range raw {
		if st.Kind == mesh.KindCoordinator {
			continue // the coordinator is not a node in its own topology report
		}
		nodes = append(nodes, canon.MeshNode{
			LabelID:     canon.LabelID(st.ID),
			ParentID:    canon.LabelID(st.Parent),
			Depth:       st.Depth,
			LQI:         st.LQI,
			RSSI:        st.RSSI,
			Router:      st.Kind == mesh.KindRouter,
			Online:      st.Online,
			FailureRisk: risks[st.ID],
		})
	}
	return canon.MeshTopology{
		SECID:     c.cfg.SECID,
		StoreID:   c.cfg.StoreID,
		Nodes:     nodes,
		UpdatedAt: now.UTC(),
	}
}
