package labelsim

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sim"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// Config describes one label.
type Config struct {
	// ID is the platform identifier; it is also the mesh node ID.
	ID canon.LabelID
	// StoreID and SECID are carried so the label's telemetry can be attributed
	// without a lookup at the controller.
	StoreID canon.StoreID
	SECID   canon.SECID
	Tier    DisplayTier
	Power   PowerProfile
	// AmbientC is the shelf temperature. A chiller label runs at 4 degrees and
	// a freezer label at -20, and both cost real capacity.
	AmbientC float64
	// Mains is true for the mains-powered relay units. They keep their receiver
	// on, so they have no receive-window latency and no battery to spend.
	Mains bool
	// TelemetryInterval is how often the label reports health uplink. Zero
	// disables it, which is what a 5,000-label load test wants: it is measuring
	// price-update latency, not telemetry.
	TelemetryInterval time.Duration
	// FirmwareVersion appears in the device registry and in OTA cohorts.
	FirmwareVersion string
	// InitialSequence seeds the monotonic counter, so a label restored from the
	// controller's durable cache does not accept an update it already showed.
	InitialSequence int64
	// KeyRing is the price-authority ring the label verifies against. A label
	// syncs it itself rather than being told what to trust by the controller,
	// which is the whole point of end-to-end attestation.
	KeyRing *pki.KeyRing
	// Attestation selects whether the label insists on verifying for itself.
	// The zero value requires it, matching the firmware's
	// CONFIG_USSLP_REQUIRE_ATTESTATION=y default.
	Attestation AttestationMode
	// StrictClock refuses an attestation when the label has no trusted time,
	// rather than skipping the key's validity window. Off by default: a label
	// that has not yet acquired time must still be able to take a price, and
	// the signature is the real control. A deployment with a trusted time
	// source turns it on.
	StrictClock bool
}

// AttestationMode selects how much the label trusts its controller.
type AttestationMode int

const (
	// AttestEndToEnd refuses any price that does not carry its own proof, and
	// verifies that proof against the label's own key ring before driving a
	// pixel. It is the zero value because it is what the fleet ships with: a
	// controller that has been rooted or physically replaced is inside the
	// trust boundary that controller-side verification depends on, and a shelf
	// label is a device the public can reach.
	AttestEndToEnd AttestationMode = iota
	// AttestTrustController accepts an unattested type 1 frame on the basis
	// that the Shelf Edge Controller verified on the label's behalf, which is
	// what INTERFACE-CONTRACTS section 5 specifies. It exists for
	// interoperability with controllers that predate frame type 4, and a fleet
	// audit looks for labels running it.
	AttestTrustController
)

// String names the mode for configuration, logs and the fleet audit.
func (m AttestationMode) String() string {
	if m == AttestTrustController {
		return "trust-controller"
	}
	return "end-to-end"
}

func (c Config) withDefaults() Config {
	if c.Power.CapacityMAH == 0 {
		c.Power = DefaultPower()
	}
	if c.FirmwareVersion == "" {
		c.FirmwareVersion = "1.4.2"
	}
	if c.AmbientC == 0 {
		c.AmbientC = 20
	}
	return c
}

// Stats is the label's own view of itself, for tests and for the demo binary.
type Stats struct {
	Sequence         int64
	RefreshCount     int64
	FullRefreshes    int64
	PartialRefreshes int64
	ForcedFulls      int64
	Discarded        int64
	BadFrames        int64
	NFCTaps          int64
	FramesReceived   int64
	// AttestationFailures counts prices refused because the label could not
	// verify them. Non-zero is a compliance incident, not a statistic.
	AttestationFailures int64
	// UnattestedRefused counts type 1 frames refused by a label that requires
	// end-to-end attestation. It is how a zone running mismatched firmware
	// makes itself visible.
	UnattestedRefused int64
	Verifications     int64
	AcksSent          int64
	AcksLost          int64
	TelemetrySent     int64
	ChargeUsedMAH     float64
	BeaconChargeMAH   float64
	RefreshChargeMAH  float64
	RadioChargeMAH    float64
	SleepChargeMAH    float64
	BatteryPct        int
	BatteryMV         int
	BeaconWindows     int64
	FastBeaconWindows int64
}

// Label is one simulated smart shelf label.
//
// It is a struct, not a goroutine. Everything it does is a callback on the
// shared simulation engine, which is what allows a store of 40,000 of them to
// run in one process; see the package documentation and edge/sim for why the
// beacon cadence in particular is integrated rather than scheduled.
//
// Safe for concurrent use: the engine drives it, and a test or the demo's HTTP
// surface reads it from elsewhere.
type Label struct {
	cfg  Config
	disp DisplaySpec
	eng  *sim.Engine
	net  *mesh.Network

	mu                sync.Mutex
	seq               int64
	partialsSinceFull int
	stats             Stats
	usedMAH           float64
	beaconMAH         float64
	refreshMAH        float64
	radioMAH          float64
	sleepMAH          float64
	lastAccrual       time.Duration
	beaconPhase       time.Duration
	fastFrom          time.Duration
	fastUntil         time.Duration
	busyUntil         time.Duration
	bornAt            time.Duration
	lastImageCRC      uint32
	priceMinor        int64
	currency          string
	dead              bool
	tamper            bool
	telemetryEvent    *sim.Event
	onEvent           func(Event)
}

// EventKind classifies what a label did, for the demo's activity feed and for
// tests that need to observe behaviour the wire protocol does not report.
type EventKind int

// The observable label events.
const (
	// EventApplied means an update reached the glass.
	EventApplied EventKind = iota
	// EventDiscarded means an update was rejected by the monotonic sequence
	// rule. It is the expected outcome of a duplicated mesh frame.
	EventDiscarded
	// EventBadFrame means a frame did not decode.
	EventBadFrame
	// EventBatteryDead means the cell is exhausted and the label is off the air.
	EventBatteryDead
	// EventAttestationRefused means a price could not be verified on the glass.
	// The previous image stays, exactly as it does when the controller refuses
	// one.
	EventAttestationRefused
)

// Event is one observable thing a label did.
type Event struct {
	Kind     EventKind
	LabelID  canon.LabelID
	Sequence int64
	Plan     RefreshPlan
	At       time.Time
	Reason   string
	// Verdict is why an attestation failed, on an EventAttestationRefused.
	Verdict   AttestVerdict
	BatteryMV int
}

// New creates a label bound to a simulation engine.
func New(eng *sim.Engine, cfg Config) *Label {
	cfg = cfg.withDefaults()
	l := &Label{
		cfg:         cfg,
		disp:        Display(cfg.Tier),
		eng:         eng,
		seq:         cfg.InitialSequence,
		lastAccrual: eng.Elapsed(),
		bornAt:      eng.Elapsed(),
		currency:    "GBP",
	}
	// Each label's listen windows are offset by an independent phase. Without
	// it every label in a zone would wake on the same millisecond and collide,
	// which is a real commissioning failure and not one worth simulating by
	// accident.
	if cfg.Power.BeaconSlow > 0 {
		l.beaconPhase = time.Duration(eng.Rand().Duration(int64(cfg.Power.BeaconSlow)))
	}
	return l
}

// ID returns the label's platform identifier.
func (l *Label) ID() canon.LabelID { return l.cfg.ID }

// NodeID returns the label's mesh address.
func (l *Label) NodeID() mesh.NodeID { return mesh.NodeID(l.cfg.ID) }

// Display returns the panel specification.
func (l *Label) Display() DisplaySpec { return l.disp }

// Tier returns which panel is fitted. The controller needs it to render, and
// it is per label rather than per zone because a promotion end in the middle of
// an aisle carries a colour panel while its neighbours do not.
func (l *Label) Tier() DisplayTier { return l.cfg.Tier }

// OnEvent registers an observer. It is called on the engine goroutine.
func (l *Label) OnEvent(fn func(Event)) {
	l.mu.Lock()
	l.onEvent = fn
	l.mu.Unlock()
}

// Attach wires the label into a mesh network: it registers the receiver, the
// receive-window gate that models duty cycling, and — if configured — the
// periodic telemetry uplink.
func (l *Label) Attach(net *mesh.Network) error {
	l.net = net
	if err := net.SetReceiver(l.NodeID(), l.receive); err != nil {
		return fmt.Errorf("labelsim: attaching %s: %w", l.cfg.ID, err)
	}
	if err := net.SetRxGate(l.NodeID(), l.rxGate); err != nil {
		return fmt.Errorf("labelsim: attaching %s: %w", l.cfg.ID, err)
	}
	if l.cfg.TelemetryInterval > 0 {
		l.scheduleTelemetry()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Duty cycling
// ---------------------------------------------------------------------------

// beaconIntervalAt returns the listen interval in force at a virtual instant.
//
// The active window is a half-open interval rather than just an end time,
// because a zone-wide wake instruction does not take effect until the label
// hears it. Counting the intervening rest as fast listening would overstate the
// energy the wake costs by an order of magnitude, which is exactly the kind of
// error that makes a battery projection worthless.
func (l *Label) beaconIntervalAt(t time.Duration) time.Duration {
	if l.cfg.Mains {
		return 0
	}
	if l.cfg.Power.BeaconSlow <= 0 {
		return l.cfg.Power.BeaconFast
	}
	if t >= l.fastFrom && t < l.fastUntil {
		return l.cfg.Power.BeaconFast
	}
	return l.cfg.Power.BeaconSlow
}

// enterActiveWindowLocked puts the label on the fast interval from now until at
// least now+d. The caller must have accrued up to now first.
func (l *Label) enterActiveWindowLocked(now, d time.Duration) {
	l.fastFrom = now
	if until := now + d; until > l.fastUntil {
		l.fastUntil = until
	}
}

// rxGate reports how long the mesh must wait before this label can hear a
// frame.
//
// This is where most of the SEC-to-label latency in a real deployment actually
// lives, and it is why the platform's SEC-to-label budget (INTERFACE-CONTRACTS
// §4) is a statement about a zone in its active window rather than about a label
// asleep at three in the morning. A label on the 30-second resting interval is, on average, fifteen
// seconds from being reachable at all.
func (l *Label) rxGate(now time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dead {
		return -1
	}
	if l.cfg.Mains {
		return 0
	}
	interval := l.beaconIntervalAt(now)
	if interval <= 0 {
		return 0
	}
	// The next boundary of a periodic window with this label's own phase.
	rel := now - l.beaconPhase
	k := int64(math.Floor(float64(rel)/float64(interval))) + 1
	next := l.beaconPhase + time.Duration(k)*interval
	if next <= now {
		return 0
	}
	return next - now
}

// OpenActiveWindow puts the label on its fast listen interval for d, after the
// delay it takes to hear the instruction.
//
// A controller about to push prices to a zone broadcasts this in its beacon.
// Labels only hear it in their own receive window, so the instruction itself is
// up to one resting interval late — which is exactly why a price load is
// planned as a window rather than fired one label at a time.
func (l *Label) OpenActiveWindow(d time.Duration) {
	l.mu.Lock()
	now := l.eng.Elapsed()
	l.accrueLocked(now)
	// A window that is already open, or already scheduled to open, is extended
	// rather than restarted. Restarting it would be a real bug and a subtle one:
	// a controller that re-broadcasts the flag every few seconds during a price
	// load would push the opening moment forward every time and the zone would
	// never wake at all.
	if l.cfg.Mains || now < l.fastUntil {
		if until := now + d; until > l.fastUntil {
			l.fastUntil = until
		}
		l.mu.Unlock()
		return
	}
	// Resting with no window pending: the label will not hear the flag until its
	// next receive window, up to one resting interval away, and keeps resting
	// until then.
	from := now + l.cfg.Power.BeaconSlow
	l.fastFrom = from
	l.fastUntil = from + d
	l.mu.Unlock()
}

// InActiveWindow reports whether the label is currently on its fast interval.
func (l *Label) InActiveWindow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg.Mains || l.eng.Elapsed() < l.fastUntil
}

// ---------------------------------------------------------------------------
// Power accounting
// ---------------------------------------------------------------------------

// accrueLocked integrates the standing draw — deep sleep and listen windows —
// from the last accounting point up to now.
//
// Listen windows are counted arithmetically rather than simulated. Over a
// decade a label wakes something like ten million times; scheduling that many
// events would make the projection impossible to compute and would add nothing,
// because nothing happens in a window where no frame arrives.
func (l *Label) accrueLocked(now time.Duration) {
	if now <= l.lastAccrual {
		return
	}
	from, to := l.lastAccrual, now
	l.lastAccrual = now
	p := l.cfg.Power

	l.sleepMAH += chargeMAH(p.DeepSleepUA/1000, to-from)

	if l.cfg.Mains {
		// A mains-powered relay keeps its receiver on. It draws far more than a
		// label, and it is wired, so the charge is tracked but never limits it.
		return
	}

	count := func(a, b, interval time.Duration) int64 {
		if interval <= 0 || b <= a {
			return 0
		}
		fa := math.Floor(float64(a-l.beaconPhase) / float64(interval))
		fb := math.Floor(float64(b-l.beaconPhase) / float64(interval))
		n := int64(fb - fa)
		if n < 0 {
			return 0
		}
		return n
	}
	// The interval splits into at most three segments: resting before the
	// active window opens, fast inside it, resting again after it closes.
	clamp := func(t time.Duration) time.Duration {
		if t < from {
			return from
		}
		if t > to {
			return to
		}
		return t
	}
	fastStart, fastEnd := clamp(l.fastFrom), clamp(l.fastUntil)
	if fastEnd < fastStart {
		fastEnd = fastStart
	}
	fast := count(fastStart, fastEnd, p.BeaconFast)
	slow := count(from, fastStart, p.BeaconSlow) + count(fastEnd, to, p.BeaconSlow)
	windows := fast + slow
	l.beaconMAH += float64(windows) * chargeMAH(p.BeaconRXMA, p.BeaconRXDuration)
	l.stats.BeaconWindows += windows
	l.stats.FastBeaconWindows += fast
}

// spendLocked adds the charge of one explicit event and kills the label if the
// cell is exhausted.
func (l *Label) spendLocked(bucket *float64, currentMA float64, d time.Duration) {
	c := chargeMAH(currentMA, d)
	*bucket += c
}

// totalMAHLocked is the charge drawn so far.
func (l *Label) totalMAHLocked() float64 {
	return l.sleepMAH + l.beaconMAH + l.refreshMAH + l.radioMAH + l.usedMAH
}

// usableCapacity is the cell derated for this label's shelf temperature.
func (l *Label) usableCapacity() float64 {
	return l.cfg.Power.CapacityMAH * CapacityDerating(l.cfg.AmbientC)
}

// checkDeadLocked marks the label exhausted, reporting whether it just died.
//
// It deliberately performs no side effects: taking a node off the mesh calls
// back into the network, which calls back into topology observers, and doing
// that with the label's own lock held — or worse, dropping and retaking it
// mid-update — is how a simulator acquires a deadlock that only shows up under
// -race at 5,000 labels. The caller drops the lock and calls announceDeath.
func (l *Label) checkDeadLocked() (justDied bool) {
	if l.dead || l.cfg.Mains {
		return false
	}
	if l.totalMAHLocked() < l.usableCapacity() {
		return false
	}
	l.dead = true
	return true
}

// announceDeath takes an exhausted label off the mesh and notifies observers.
// It must be called with the lock released.
func (l *Label) announceDeath() {
	l.mu.Lock()
	fn, id, at, net := l.onEvent, l.cfg.ID, l.eng.Now(), l.net
	l.mu.Unlock()
	if net != nil {
		net.KillNode(mesh.NodeID(id))
	}
	if fn != nil {
		fn(Event{Kind: EventBatteryDead, LabelID: id, At: at, Reason: "cell exhausted", BatteryMV: 1800})
	}
}

// depthOfDischargeLocked is the fraction of usable capacity consumed.
func (l *Label) depthOfDischargeLocked() float64 {
	cap := l.usableCapacity()
	if cap <= 0 {
		return 1
	}
	d := l.totalMAHLocked() / cap
	if d < 0 {
		return 0
	}
	if d > 1 {
		return 1
	}
	return d
}

// Battery returns the label's current cell state.
func (l *Label) Battery() (millivolts, percent int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accrueLocked(l.eng.Elapsed())
	d := l.depthOfDischargeLocked()
	return batteryMillivolts(d), int(math.Round(100 * (1 - d)))
}

// ChargeUsedMAH returns the charge drawn from the cell so far. It is the
// quantity the analytic projection in PowerProfile.Project is validated
// against.
func (l *Label) ChargeUsedMAH() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accrueLocked(l.eng.Elapsed())
	return l.totalMAHLocked()
}

// Stats returns a snapshot of the label's counters.
func (l *Label) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accrueLocked(l.eng.Elapsed())
	s := l.stats
	s.Sequence = l.seq
	s.ChargeUsedMAH = l.totalMAHLocked()
	s.BeaconChargeMAH = l.beaconMAH
	s.RefreshChargeMAH = l.refreshMAH
	s.RadioChargeMAH = l.radioMAH
	s.SleepChargeMAH = l.sleepMAH
	d := l.depthOfDischargeLocked()
	s.BatteryMV = batteryMillivolts(d)
	s.BatteryPct = int(math.Round(100 * (1 - d)))
	return s
}

// SetKeyRing replaces the price-authority ring the label verifies against.
//
// A label syncs its own ring rather than being handed one per update, so this
// is the operation a key rotation performs — and, in a test, the operation that
// simulates a label that missed one.
func (l *Label) SetKeyRing(ring *pki.KeyRing) {
	l.mu.Lock()
	l.cfg.KeyRing = ring
	l.mu.Unlock()
}

// AttestationMode reports whether this label verifies for itself. It is the
// cluster attribute a fleet audit reads to find labels running the weaker mode.
func (l *Label) AttestationMode() AttestationMode { return l.cfg.Attestation }

// Dead reports whether the cell is exhausted.
func (l *Label) Dead() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dead
}

// SetTamper sets the tamper flag, which the label reports until it is cleared
// by a technician. It exists so the anomaly-detection path has something real
// to consume.
func (l *Label) SetTamper(v bool) {
	l.mu.Lock()
	l.tamper = v
	l.mu.Unlock()
}

// NFCTap models a shopper holding a phone against the label. The MCU wakes to
// serve the dynamic NDEF record, which costs charge and puts the label into its
// active window — a label someone is standing in front of is a label worth
// being able to update quickly.
func (l *Label) NFCTap() {
	l.mu.Lock()
	now := l.eng.Elapsed()
	l.accrueLocked(now)
	l.spendLocked(&l.radioMAH, l.cfg.Power.NFCMA, l.cfg.Power.NFCTapDuration)
	l.stats.NFCTaps++
	l.enterActiveWindowLocked(now, l.cfg.Power.ActiveWindow)
	died := l.checkDeadLocked()
	l.mu.Unlock()
	if died {
		l.announceDeath()
	}
}

// ---------------------------------------------------------------------------
// Radio behaviour
// ---------------------------------------------------------------------------

// receive handles a frame arriving from the mesh. It runs on the engine
// goroutine.
func (l *Label) receive(f mesh.Frame) {
	kind, ok := FrameKind(f.Payload)
	if !ok {
		l.badFrame("unrecognised protocol version")
		return
	}
	switch kind {
	case FrameAttestedUpdate:
		a, err := DecodeAttestedUpdate(f.Payload)
		if err != nil {
			l.badFrame(err.Error())
			return
		}
		l.apply(a.Update, len(f.Payload), &a)
	case FrameUpdate:
		u, err := DecodeUpdate(f.Payload)
		if err != nil {
			l.badFrame(err.Error())
			return
		}
		if l.cfg.Attestation == AttestEndToEnd {
			// This label does not trust its controller to have verified on its
			// behalf. The frame is decoded first anyway, purely so the refusal
			// can name the sequence it is refusing: an acknowledgement carrying
			// sequence zero tells the controller only that something went
			// wrong, while one carrying the sequence lets it correlate the
			// refusal with the update it sent and stop retrying that one.
			//
			// The status is AckRefusedUnattested, not AckRefusedAttestation.
			// This is a fleet configuration mismatch — the controller speaks
			// frame type 1 to a label that requires type 4 — and raising a
			// compliance alert for it would bury the real ones under every
			// label in the zone.
			l.mu.Lock()
			l.stats.UnattestedRefused++
			fn, id, at := l.onEvent, l.cfg.ID, l.eng.Now()
			l.mu.Unlock()
			if fn != nil {
				fn(Event{Kind: EventAttestationRefused, LabelID: id, Sequence: u.Sequence, At: at,
					Reason: "this label requires end-to-end attestation and the controller sent an unattested frame"})
			}
			l.sendAck(Ack{Sequence: u.Sequence, Status: AckRefusedUnattested})
			return
		}
		l.apply(u, len(f.Payload), nil)
	default:
		l.badFrame(fmt.Sprintf("frame type %d is not a downstream price frame", kind))
	}
}

// badFrame records a frame that did not decode and tells any observer why.
func (l *Label) badFrame(reason string) {
	l.mu.Lock()
	l.stats.BadFrames++
	fn, id, at := l.onEvent, l.cfg.ID, l.eng.Now()
	l.mu.Unlock()
	if fn != nil {
		fn(Event{Kind: EventBadFrame, LabelID: id, At: at, Reason: reason})
	}
}

// verify checks an attested frame against the label's own key ring.
//
// The order of the checks, and the fact that the digest is recomputed from the
// fields about to be rendered rather than read from the wire, are canon.Verify's
// and pki.KeyRing.VerifyAt's. Reusing them rather than reimplementing is
// deliberate: the firmware ports the same logic to C precisely because there
// must be exactly one definition of what a valid price is, and a third
// definition in the simulator would be a third thing to keep in step.
func (l *Label) verify(a *AttestedUpdate) error {
	if a.Alg != AttestAlgEd25519 {
		return fmt.Errorf("%w: algorithm %d is not Ed25519", canon.ErrAttestationInvalid, a.Alg)
	}
	if err := a.ValidateIdentifiers(); err != nil {
		return err
	}
	l.mu.Lock()
	ring := l.cfg.KeyRing
	strict := l.cfg.StrictClock
	l.mu.Unlock()
	if ring == nil {
		return ErrNoKeyRing
	}
	now := l.eng.Now()
	if now.IsZero() && !strict {
		now = time.Unix(a.EffectiveAtUnix, 0).UTC()
	}
	return ring.VerifyAt(a.AttestationInput(), a.Attestation(), now)
}

// apply runs the sequence check, verifies, drives the panel and acknowledges.
//
// att is nil for a legacy type 1 frame accepted in compatibility mode.
func (l *Label) apply(u Update, frameBytes int, att *AttestedUpdate) {
	l.mu.Lock()
	now := l.eng.Elapsed()
	l.accrueLocked(now)
	l.stats.FramesReceived++

	// Receiving the frame costs receiver-on time whatever happens to it: the
	// duplicate that the sequence rule is about to discard was still paid for.
	l.spendLocked(&l.radioMAH, l.cfg.Power.DataRXMA, mesh.Airtime(frameBytes))
	l.enterActiveWindowLocked(now, l.cfg.Power.ActiveWindow)

	// INTERFACE-CONTRACTS section 6: a label discards any update whose sequence
	// is not greater than the one it is displaying. This is what makes
	// at-least-once mesh delivery safe — a duplicated frame is a no-op and a
	// reordered one cannot roll a price backwards.
	if u.Sequence <= l.seq {
		l.stats.Discarded++
		current := l.seq
		died := l.checkDeadLocked()
		fn, id, at := l.onEvent, l.cfg.ID, l.eng.Now()
		l.mu.Unlock()
		if died {
			l.announceDeath()
		}
		if fn != nil {
			fn(Event{Kind: EventDiscarded, LabelID: id, Sequence: u.Sequence, At: at,
				Reason: fmt.Sprintf("sequence %d is not greater than the displayed %d", u.Sequence, current)})
		}
		l.sendAck(Ack{Sequence: u.Sequence, Status: AckStaleSequence})
		return
	}

	// The attestation, after the sequence rule and before anything touches the
	// panel. After, because a duplicate is the common case under at-least-once
	// delivery and an Ed25519 verification is thirteen milliseconds of a coin
	// cell's life; checking the free invariant first costs nothing in safety.
	// Before the panel, because a price that cannot be verified must leave no
	// trace at all.
	if att != nil {
		l.spendLocked(&l.radioMAH, l.cfg.Power.VerifyMA, l.cfg.Power.VerifyDuration)
		l.stats.Verifications++
		l.mu.Unlock()
		err := l.verify(att)
		l.mu.Lock()
		if err != nil {
			l.stats.AttestationFailures++
			died := l.checkDeadLocked()
			fn, id, at := l.onEvent, l.cfg.ID, l.eng.Now()
			l.mu.Unlock()
			if died {
				l.announceDeath()
			}
			verdict := VerdictFor(err)
			if fn != nil {
				fn(Event{Kind: EventAttestationRefused, LabelID: id, Sequence: u.Sequence,
					At: at, Reason: err.Error(), Verdict: verdict})
			}
			// The refusal says so in its own right, and says which way the
			// verification failed. That is what lets the controller tell a
			// stale key ring from actual tampering without asking, and what
			// keeps a genuinely corrupted frame from being escalated into a
			// weights-and-measures process.
			l.sendAck(Ack{Sequence: u.Sequence, Status: AckRefusedAttestation, Verdict: verdict})
			return
		}
	}

	plan := planRefresh(l.disp, u.Flags&FlagRequestPartial != 0, l.partialsSinceFull)
	if plan.Partial {
		l.partialsSinceFull++
		l.stats.PartialRefreshes++
	} else {
		l.partialsSinceFull = 0
		l.stats.FullRefreshes++
		if plan.ForcedFull {
			l.stats.ForcedFulls++
		}
	}
	l.spendLocked(&l.refreshMAH, l.disp.RefreshCurrentMA, plan.Duration)
	l.stats.RefreshCount++
	// Verification runs before the waveform starts, so the pixels settle that
	// much later. The acknowledgement has no field for it — see the comment on
	// the refusal path — so a controller that wants an accurate end-to-end
	// figure adds VerifyDuration itself, which sec.Controller does.
	verifyDelay := time.Duration(0)
	if att != nil {
		verifyDelay = l.cfg.Power.VerifyDuration
	}
	l.seq = u.Sequence
	l.priceMinor = u.PriceMinor
	l.currency = u.Currency
	l.lastImageCRC = u.ImageCRC
	l.busyUntil = now + verifyDelay + plan.Duration
	net := l.net
	died := l.checkDeadLocked()
	dead := l.dead
	fn, id, at := l.onEvent, l.cfg.ID, l.eng.Now()
	l.mu.Unlock()
	if died {
		l.announceDeath()
	}

	if net != nil {
		// The panel is being driven: the radio is off and any frame arriving in
		// this window is lost, not queued.
		net.SetBusyUntil(l.NodeID(), now+verifyDelay+plan.Duration)
	}
	if dead {
		return
	}
	l.eng.At(verifyDelay+plan.Duration, func() {
		if fn != nil {
			fn(Event{Kind: EventApplied, LabelID: id, Sequence: u.Sequence, Plan: plan, At: at})
		}
		mv, pct := l.Battery()
		l.sendAck(Ack{
			Sequence:          u.Sequence,
			Status:            AckApplied,
			RefreshMS:         uint16(plan.Duration / time.Millisecond),
			Partial:           plan.Partial,
			ForcedFull:        plan.ForcedFull,
			BatteryMV:         uint16(mv),
			BatteryPct:        uint8(pct),
			TemperatureCentiC: l.temperatureCentiC(),
		})
	})
}

// sendAck transmits an acknowledgement back to the coordinator.
func (l *Label) sendAck(a Ack) {
	l.mu.Lock()
	net := l.net
	if net == nil || l.dead {
		l.mu.Unlock()
		return
	}
	payload := EncodeAck(a)
	l.accrueLocked(l.eng.Elapsed())
	l.spendLocked(&l.radioMAH, l.cfg.Power.TXMA, mesh.Airtime(len(payload)))
	l.stats.AcksSent++
	died := l.checkDeadLocked()
	coord := net.Coordinator()
	l.mu.Unlock()
	if died {
		l.announceDeath()
		return
	}

	net.Send(mesh.TxRequest{Src: l.NodeID(), Dst: coord, Payload: payload, Done: func(r mesh.TxResult) {
		if !r.Delivered {
			l.mu.Lock()
			l.stats.AcksLost++
			l.mu.Unlock()
		}
	}})
}

// temperatureCentiC reports the on-die temperature. It tracks ambient with a
// couple of degrees of self-heating, which is what a sensor on the same die as
// the MCU actually reads.
func (l *Label) temperatureCentiC() int16 {
	return int16(math.Round((l.cfg.AmbientC + 1.5) * 100))
}

// scheduleTelemetry arranges the next uplink health report.
func (l *Label) scheduleTelemetry() {
	l.mu.Lock()
	if l.dead || l.cfg.TelemetryInterval <= 0 {
		l.mu.Unlock()
		return
	}
	// Jitter keeps a zone's labels from all reporting on the same second, which
	// would put a five-minute spike into a channel that is otherwise idle.
	d := time.Duration(l.eng.Rand().Jitter(int64(l.cfg.TelemetryInterval), 0.2))
	l.mu.Unlock()
	l.telemetryEvent = l.eng.At(d, l.sendTelemetry)
}

func (l *Label) sendTelemetry() {
	l.mu.Lock()
	net := l.net
	if net == nil || l.dead {
		l.mu.Unlock()
		return
	}
	now := l.eng.Elapsed()
	l.accrueLocked(now)
	d := l.depthOfDischargeLocked()
	frame := TelemetryFrame{
		BatteryMV:         uint16(batteryMillivolts(d)),
		BatteryPct:        uint8(math.Round(100 * (1 - d))),
		TemperatureCentiC: l.temperatureCentiC(),
		RefreshCount:      uint32(l.stats.RefreshCount),
		NFCTapCount:       uint32(l.stats.NFCTaps),
		UptimeSec:         uint32((now - l.bornAt) / time.Second),
		Tamper:            l.tamper,
	}
	l.mu.Unlock()

	// The label reports what it measured on its own uplink, which is the half of
	// the mesh picture the controller cannot see for itself.
	if parent, ok := net.ParentOf(l.NodeID()); ok && parent != "" {
		if rssi, ok := net.RSSI(l.NodeID(), parent); ok {
			frame.ParentLQI = uint8(mesh.LQIFromRSSI(rssi))
			frame.ParentRSSI = int8(clampInt(int(math.Round(rssi)), -128, 127))
		}
	}
	payload := EncodeTelemetry(frame)

	l.mu.Lock()
	l.spendLocked(&l.radioMAH, l.cfg.Power.TXMA, mesh.Airtime(len(payload)))
	l.stats.TelemetrySent++
	died := l.checkDeadLocked()
	l.mu.Unlock()
	if died {
		l.announceDeath()
		return
	}

	net.Send(mesh.TxRequest{Src: l.NodeID(), Dst: net.Coordinator(), Payload: payload})
	l.scheduleTelemetry()
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
