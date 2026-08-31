package sgu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// ---------------------------------------------------------------------------
// WAN link detection
//
// Deciding that a store is offline is a consequential act: it stops the bridge,
// starts buffering, and emits a store.mode.autonomous event that will appear on
// somebody's operations dashboard. Deciding it wrongly, twice a minute, because
// a DSL line reset for two seconds, is worse than not detecting anything.
//
// So the detector combines two signals and applies hysteresis to their
// combination:
//
//   - Link state, from the MQTT client's own connection lifecycle. It is
//     immediate and it is not sufficient: a TCP connection to a broker behind a
//     load balancer stays "up" long after the path behind it has gone.
//   - A round-trip probe: a QoS 1 publish to the cloud that must be
//     acknowledged. This is what catches a black-holed path, and it is the only
//     signal that actually proves the store can still tell the cloud something.
//
// Hysteresis is asymmetric on purpose. Entering autonomy is cheap and reversible
// and should happen quickly once the evidence is unambiguous; leaving it means
// replaying a buffer and running a merge, so it waits for the link to prove
// itself for longer than it took to fail.
// ---------------------------------------------------------------------------

// Mode is the store's operating mode. The strings are the ones that appear in
// canon.StoreModeChanged.
type Mode string

// The two store modes.
const (
	// ModeConnected means the cloud bridge is running.
	ModeConnected Mode = "connected"
	// ModeAutonomous means the store is running on its own: local broker, local
	// schedule, local rules, buffered upstream.
	ModeAutonomous Mode = "autonomous"
)

// DetectorConfig parameterises WAN detection.
type DetectorConfig struct {
	// Interval is how often the probe runs.
	Interval time.Duration
	// Timeout bounds one probe.
	Timeout time.Duration
	// FailThreshold is how many consecutive failed probes are needed to go
	// autonomous. Three probes at five seconds is fifteen seconds of unambiguous
	// evidence, which is long enough that a two-second blip cannot trigger it
	// and short enough that a store notices a real outage before the first
	// scheduled promotion of the morning.
	FailThreshold int
	// FailFor is the minimum wall time the failures must span, so shortening
	// Interval for a test does not accidentally shorten the hysteresis.
	FailFor time.Duration
	// RecoverThreshold is how many consecutive successful probes are needed to
	// come back. Higher than FailThreshold because a flapping link that is
	// declared healthy causes a reconciliation, and a reconciliation that is
	// interrupted halfway is the one genuinely messy state in this design.
	RecoverThreshold int
	// RecoverFor is the minimum wall time the successes must span.
	RecoverFor time.Duration
	Log        *obs.Logger
}

func (c DetectorConfig) withDefaults() DetectorConfig {
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.Timeout == 0 {
		c.Timeout = 3 * time.Second
	}
	if c.FailThreshold == 0 {
		c.FailThreshold = 3
	}
	if c.FailFor == 0 {
		c.FailFor = 12 * time.Second
	}
	if c.RecoverThreshold == 0 {
		c.RecoverThreshold = 4
	}
	if c.RecoverFor == 0 {
		c.RecoverFor = 15 * time.Second
	}
	if c.Log == nil {
		c.Log = obs.NopLogger()
	}
	return c
}

// Detector watches the WAN link and reports mode transitions.
type Detector struct {
	cfg DetectorConfig
	// LinkUp reports the transport's own view of the connection.
	linkUp func() bool
	// Probe performs one round trip to the cloud. Returning nil means the store
	// can still be heard.
	probe func(ctx context.Context) error

	mu            sync.Mutex
	mode          Mode
	consecFail    int
	consecOK      int
	firstFail     time.Time
	firstOK       time.Time
	lastChange    time.Time
	lastErr       error
	probesRun     uint64
	probesFailed  uint64
	transitions   uint64
	onChange      func(Mode, string)
	now           func() time.Time
	started       bool
	stopped       bool
	stop          chan struct{}
	transitionsWG sync.WaitGroup
}

// NewDetector builds a detector over a link-state function and a probe.
func NewDetector(linkUp func() bool, probe func(context.Context) error, cfg DetectorConfig) *Detector {
	if linkUp == nil {
		linkUp = func() bool { return true }
	}
	if probe == nil {
		probe = func(context.Context) error { return nil }
	}
	return &Detector{
		cfg: cfg.withDefaults(), linkUp: linkUp, probe: probe,
		mode: ModeConnected, now: time.Now, stop: make(chan struct{}),
	}
}

// SetClock replaces the detector's time source, which a test uses to make
// hysteresis assertions without waiting out the real intervals.
func (d *Detector) SetClock(now func() time.Time) {
	d.mu.Lock()
	if now != nil {
		d.now = now
	}
	d.mu.Unlock()
}

// OnChange registers the transition callback. It runs on the detector's own
// goroutine, so it must not block for long.
func (d *Detector) OnChange(fn func(Mode, string)) {
	d.mu.Lock()
	d.onChange = fn
	d.mu.Unlock()
}

// Mode returns the current mode.
func (d *Detector) Mode() Mode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mode
}

// DetectorStats is what the diagnostics page shows about link health.
type DetectorStats struct {
	Mode             Mode          `json:"mode"`
	Since            time.Time     `json:"since"`
	ProbesRun        uint64        `json:"probes_run"`
	ProbesFailed     uint64        `json:"probes_failed"`
	Transitions      uint64        `json:"transitions"`
	ConsecutiveFail  int           `json:"consecutive_failures"`
	ConsecutiveOK    int           `json:"consecutive_successes"`
	LastError        string        `json:"last_error,omitempty"`
	FailThreshold    int           `json:"fail_threshold"`
	RecoverThreshold int           `json:"recover_threshold"`
	Interval         time.Duration `json:"probe_interval"`
}

// Stats returns the detector's counters.
func (d *Detector) Stats() DetectorStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := DetectorStats{
		Mode: d.mode, Since: d.lastChange, ProbesRun: d.probesRun, ProbesFailed: d.probesFailed,
		Transitions: d.transitions, ConsecutiveFail: d.consecFail, ConsecutiveOK: d.consecOK,
		FailThreshold: d.cfg.FailThreshold, RecoverThreshold: d.cfg.RecoverThreshold,
		Interval: d.cfg.Interval,
	}
	if d.lastErr != nil {
		s.LastError = d.lastErr.Error()
	}
	return s
}

// Run drives the detector until ctx is cancelled or Stop is called.
func (d *Detector) Run(ctx context.Context) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	if d.lastChange.IsZero() {
		d.lastChange = d.now()
	}
	d.mu.Unlock()

	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case <-ticker.C:
			d.Check(ctx)
		}
	}
}

// Stop halts a running detector.
func (d *Detector) Stop() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.stop)
	d.mu.Unlock()
}

// Check runs one probe and applies the hysteresis rules.
//
// It is exported so a test can step the detector deterministically instead of
// waiting out real intervals, and so an operator can force an immediate
// evaluation from the diagnostics page after plugging a cable back in.
func (d *Detector) Check(ctx context.Context) Mode {
	linkUp := d.linkUp()
	var err error
	if linkUp {
		pctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
		err = d.probe(pctx)
		cancel()
	} else {
		err = errors.New("transport reports the link is down")
	}

	d.mu.Lock()
	now := d.now()
	d.probesRun++
	var fire func()

	if err != nil {
		d.probesFailed++
		d.lastErr = err
		d.consecOK = 0
		d.firstOK = time.Time{}
		if d.consecFail == 0 {
			d.firstFail = now
		}
		d.consecFail++
		if d.mode == ModeConnected &&
			d.consecFail >= d.cfg.FailThreshold &&
			now.Sub(d.firstFail) >= d.cfg.FailFor {
			reason := fmt.Sprintf("%d consecutive failed cloud probes over %v: %v",
				d.consecFail, now.Sub(d.firstFail).Round(time.Second), err)
			fire = d.transitionLocked(ModeAutonomous, reason, now)
		}
	} else {
		d.lastErr = nil
		d.consecFail = 0
		d.firstFail = time.Time{}
		if d.consecOK == 0 {
			d.firstOK = now
		}
		d.consecOK++
		if d.mode == ModeAutonomous &&
			d.consecOK >= d.cfg.RecoverThreshold &&
			now.Sub(d.firstOK) >= d.cfg.RecoverFor {
			reason := fmt.Sprintf("%d consecutive successful cloud probes over %v",
				d.consecOK, now.Sub(d.firstOK).Round(time.Second))
			fire = d.transitionLocked(ModeConnected, reason, now)
		}
	}
	mode := d.mode
	d.mu.Unlock()

	if fire != nil {
		fire()
	}
	return mode
}

// transitionLocked records a mode change and returns the notification to fire
// once the lock is released.
func (d *Detector) transitionLocked(to Mode, reason string, now time.Time) func() {
	d.mode = to
	d.lastChange = now
	d.transitions++
	d.consecFail = 0
	d.consecOK = 0
	fn := d.onChange
	d.cfg.Log.Warn("store mode changed", "mode", string(to), "reason", reason)
	if fn == nil {
		return nil
	}
	return func() { fn(to, reason) }
}

// ForceMode sets the mode without probing. It exists for the operator override
// on the diagnostics page — an engineer who knows the WAN is about to be cut
// for maintenance can put a store into autonomy deliberately rather than let it
// discover the outage — and for tests.
func (d *Detector) ForceMode(to Mode, reason string) {
	d.mu.Lock()
	if d.mode == to {
		d.mu.Unlock()
		return
	}
	fire := d.transitionLocked(to, "forced: "+reason, d.now())
	d.mu.Unlock()
	if fire != nil {
		fire()
	}
}
