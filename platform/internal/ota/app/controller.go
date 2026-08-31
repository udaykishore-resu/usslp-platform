// Package app is the OTA service's application layer: firmware artifact
// management and the staged rollout controller.
//
// # Why the controller is event-sourced
//
// A rollout runs for days. In that time the OTA service will be redeployed, a
// pod will be rescheduled, and at least one node will go away without warning.
// A controller that held its cohort position in memory would, after any of
// those, either re-dispatch a wave that is already downloading — wasting mesh
// bandwidth that price updates need — or advance past a cohort that was still
// soaking, which is how a bad image reaches the second wave without anyone
// deciding that it should.
//
// So the controller keeps nothing that matters in memory alone. Job creation,
// every state change, every cohort advance, every dispatch batch and every
// device outcome is an event in the job's stream, and start-up is a replay.
// [Controller.Tick] is idempotent by construction: it recomputes what should
// happen from durable state and does only the part that has not happened yet.
//
// # Why cohort membership is not stored
//
// It is a hash of (job, device) — see domain.Bucket. Nothing to write, nothing
// to reconcile, and a device provisioned halfway through a rollout lands in the
// wave it would always have been in.
package app

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/ota/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Config assembles a Controller from its ports.
type Config struct {
	// Store is the event store the rollout's history is written to. Its
	// underlying kvstore also holds the artifact records.
	Store *eventstore.Store
	// Artifacts holds the firmware images. Required.
	Artifacts ports.ArtifactStore
	// Keys are the firmware signing keys the platform trusts. An empty ring
	// makes every upload fail closed, which is the correct behaviour for a
	// misconfigured deployment: no signature can be verified, so no image is
	// accepted.
	Keys domain.KeyRing
	// Fleet is the Device Registry. Required for a rollout to have targets.
	Fleet ports.FleetDirectory
	// Events publishes to `ota-commands`. Nil disables publishing.
	Events ports.EventStreamPublisher
	// Messenger is the MQTT path to devices. Nil disables device triggers,
	// which is how the controller runs in an environment with no broker.
	Messenger ports.DeviceMessenger
	// Clock is the time source; nil means ports.SystemClock.
	Clock ports.Clock
	// Region is the geographic shard, and becomes the region segment of every
	// MQTT topic the service publishes to.
	Region canon.Region
	// Log receives operational events; nil is silent.
	Log *obs.Logger
	// Metrics, when non-nil, receives the service's counters and gauges.
	Metrics *obs.Registry
	// Source names this component in every envelope it produces.
	Source string
	// MaxConcurrentPerSEC overrides the default per-controller download cap.
	MaxConcurrentPerSEC int
}

// deviceState is one device's position in one rollout.
type deviceState struct {
	DeviceID string              `json:"device_id"`
	StoreID  canon.StoreID       `json:"store_id"`
	SECID    canon.SECID         `json:"sec_id"`
	Wave     int                 `json:"wave"`
	Status   domain.DeviceStatus `json:"status"`
	// FromVersion is what the device was running when it was dispatched, kept
	// so a rollback knows what to put back.
	FromVersion  domain.Version `json:"from_version,omitempty"`
	DispatchedAt time.Time      `json:"dispatched_at,omitempty"`
	ReportedAt   time.Time      `json:"reported_at,omitempty"`
	Attempt      int            `json:"attempt,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// jobState is a rollout's full in-memory projection.
type jobState struct {
	job     *domain.Job
	devices map[string]*deviceState
	budget  *domain.BandwidthBudget
}

// metrics holds the service's instrumentation.
type metrics struct {
	artifacts  *obs.CounterVec
	rejected   *obs.CounterVec
	jobs       *obs.CounterVec
	dispatched *obs.CounterVec
	outcomes   *obs.CounterVec
	rollbacks  *obs.CounterVec
	suppressed *obs.CounterVec
	inflight   *obs.GaugeVec
}

func newMetrics(r *obs.Registry) *metrics {
	if r == nil {
		return nil
	}
	return &metrics{
		artifacts: r.Counter("ota_artifacts_accepted_total",
			"Firmware artifacts accepted, by hardware tier.", "hardware_tier"),
		rejected: r.Counter("ota_artifacts_rejected_total",
			"Firmware uploads refused, by reason.", "reason"),
		jobs: r.Counter("ota_jobs_total",
			"Rollout state transitions, by destination state.", "state"),
		dispatched: r.Counter("ota_devices_dispatched_total",
			"Firmware triggers published to devices."),
		outcomes: r.Counter("ota_device_outcomes_total",
			"Device update outcomes, by status.", "status"),
		rollbacks: r.Counter("ota_rollbacks_total",
			"Automatic rollbacks triggered."),
		suppressed: r.Counter("ota_dispatch_suppressed_total",
			"Dispatch attempts held back, by reason.", "reason"),
		inflight: r.Gauge("ota_downloads_in_flight",
			"Firmware downloads currently in flight, by job.", "job_id"),
	}
}

// Controller is the OTA service application.
//
// It is safe for concurrent use. cmdMu serialises the decide-persist-apply
// sequence so two callers cannot both decide against the same stale job state;
// mu guards the read model that queries and the HTTP surface read.
type Controller struct {
	cfg   Config
	store *eventstore.Store
	kv    *kvstore.Store
	clock ports.Clock
	log   *obs.Logger
	met   *metrics

	cmdMu sync.Mutex

	mu   sync.RWMutex
	jobs map[string]*jobState

	publishMu sync.Mutex
	published int64
}

// Errors returned by the application layer.
var (
	// ErrNotConfigured means a required port was not supplied.
	ErrNotConfigured = errors.New("ota: controller is not configured")
	// ErrJobNotFound means no rollout exists under that identifier.
	ErrJobNotFound = errors.New("ota: rollout not found")
)

var outboxKey = []byte("A\x00outbox\x00pos")

// Open builds a Controller and rebuilds its rollout state by replaying the
// event store.
func Open(ctx context.Context, cfg Config) (*Controller, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: event store is required", ErrNotConfigured)
	}
	if cfg.Artifacts == nil {
		return nil, fmt.Errorf("%w: artifact store is required", ErrNotConfigured)
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Source == "" {
		cfg.Source = "ota-service"
	}
	c := &Controller{
		cfg:   cfg,
		store: cfg.Store,
		kv:    cfg.Store.KV(),
		clock: cfg.Clock,
		log:   cfg.Log,
		met:   newMetrics(cfg.Metrics),
		jobs:  make(map[string]*jobState),
	}
	if err := c.replay(ctx); err != nil {
		return nil, err
	}
	if err := c.loadOutbox(); err != nil {
		return nil, err
	}
	if err := c.drainOutbox(ctx); err != nil {
		c.log.Warn("ota could not republish pending events at start-up", "error", err)
	}
	return c, nil
}

// Now returns the controller's current time.
func (c *Controller) Now() time.Time { return c.clock.Now().UTC() }

// Close releases the controller. It does not close the event store, which the
// caller owns.
func (c *Controller) Close() error { return nil }

func (c *Controller) replay(ctx context.Context) error {
	const page = 4096
	from := int64(1)
	for {
		recs, err := c.store.ReadAll(ctx, from, page)
		if err != nil {
			return fmt.Errorf("ota: replay from position %d: %w", from, err)
		}
		if len(recs) == 0 {
			return nil
		}
		for _, rec := range recs {
			if err := c.applyLocked(rec.Event, rec.Version); err != nil {
				return fmt.Errorf("ota: replay event %s at position %d: %w",
					rec.Event.EventID, rec.Position, err)
			}
		}
		from = recs[len(recs)-1].Position + 1
	}
}

func (c *Controller) loadOutbox() error {
	v, err := c.kv.Get(outboxKey)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ota: read publish cursor: %w", err)
	}
	if len(v) != 8 {
		return errors.New("ota: publish cursor is corrupt")
	}
	c.published = int64(binary.BigEndian.Uint64(v))
	return nil
}

func (c *Controller) drainOutbox(ctx context.Context) error {
	if c.cfg.Events == nil {
		return nil
	}
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	return c.drainFrom(ctx)
}

func (c *Controller) drainFrom(ctx context.Context) error {
	for {
		recs, err := c.store.ReadAll(ctx, c.published+1, 512)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		envs := make([]canon.Envelope, 0, len(recs))
		for _, r := range recs {
			envs = append(envs, r.Event)
		}
		if err := c.cfg.Events.PublishEvents(ctx, canon.StreamOTA.Name, envs...); err != nil {
			return err
		}
		c.published = recs[len(recs)-1].Position
		if err := c.persistCursor(c.published); err != nil {
			return err
		}
	}
}

func (c *Controller) persistCursor(pos int64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(pos))
	return c.kv.Put(outboxKey, b[:])
}

// jobStream returns a rollout's event-store stream.
func jobStream(jobID string) eventstore.StreamID {
	return eventstore.Stream(domain.AggregateJob, jobID)
}

// newEvent builds an envelope in the service's house style.
func (c *Controller) newEvent(eventType, jobID string, tenant canon.TenantID, store canon.StoreID, payload any) (canon.Envelope, error) {
	env, err := canon.NewEnvelope(eventType, domain.AggregateJob, jobID, tenant, payload)
	if err != nil {
		return canon.Envelope{}, err
	}
	env.StoreID = store
	env.Region = c.cfg.Region
	env.Source = c.cfg.Source
	env.CorrelationID = canon.CorrelationID(jobID)
	now := c.Now()
	env.OccurredAt = now
	env.RecordedAt = now
	return env, nil
}

// commit appends events to a rollout's stream, applies them and publishes them.
// It must be called with cmdMu held.
func (c *Controller) commit(ctx context.Context, jobID string, expectedVersion int64, envs ...canon.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	res, err := c.store.AppendWithResult(ctx, jobStream(jobID), expectedVersion, envs...)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for _, rec := range res.Events {
		if err := c.applyLocked(rec.Event, rec.Version); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("ota: apply committed event %s: %w", rec.Event.EventID, err)
		}
	}
	c.mu.Unlock()
	if res.Duplicate {
		return nil
	}
	c.publish(ctx)
	return nil
}

// publish hands committed events to `ota-commands`. A failure is logged rather
// than returned: the events are durable and the cursor means the next commit or
// the next start-up resends them, whereas failing the caller would turn a
// stream blip into a halted rollout.
func (c *Controller) publish(ctx context.Context) {
	if c.cfg.Events == nil {
		return
	}
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	if err := c.drainFrom(ctx); err != nil {
		c.log.Warn("ota could not publish rollout events", "error", err, "pending_from", c.published+1)
	}
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

// applyLocked folds one event into the rollout read model. Every write goes
// through it, from a live command or from replay, so recovery cannot build a
// different model from the same events.
func (c *Controller) applyLocked(env canon.Envelope, version int64) error {
	switch env.EventType {
	case canon.EvtOTAJobCreated:
		var p domain.JobCreated
		if err := env.Decode(&p); err != nil {
			return err
		}
		if p.Job == nil {
			return fmt.Errorf("ota: job-created event %s carries no job", env.EventID)
		}
		job := p.Job.Clone()
		job.Version = version
		c.jobs[job.JobID] = &jobState{
			job:     job,
			devices: make(map[string]*deviceState),
			budget:  domain.NewBandwidthBudget(job.MaxConcurrentPerSEC),
		}

	case domain.EvtJobStateChanged:
		var p domain.JobStateChanged
		if err := env.Decode(&p); err != nil {
			return err
		}
		st := c.jobs[p.JobID]
		if st == nil {
			return nil
		}
		st.job.State = p.To
		st.job.UpdatedAt = p.At
		st.job.Version = version
		switch p.To {
		case domain.JobRunning:
			if st.job.StartedAt.IsZero() {
				st.job.StartedAt = p.At
			}
		case domain.JobHalted, domain.JobRollingBack:
			st.job.HaltReason = p.Reason
		case domain.JobCompleted, domain.JobAborted, domain.JobRolledBack:
			st.job.CompletedAt = p.At
			if p.To != domain.JobCompleted {
				st.job.HaltReason = p.Reason
			}
			st.budget.Reset()
		}

	case canon.EvtOTACohortAdvanced:
		var p domain.CohortAdvanced
		if err := env.Decode(&p); err != nil {
			return err
		}
		st := c.jobs[p.JobID]
		if st == nil {
			return nil
		}
		if p.From >= 0 && p.From < len(st.job.Waves) {
			st.job.Waves[p.From].CompletedAt = p.At
		}
		st.job.CurrentWave = p.To
		if p.To >= 0 && p.To < len(st.job.Waves) {
			st.job.Waves[p.To].StartedAt = p.At
		}
		st.job.UpdatedAt = p.At
		st.job.Version = version

	case domain.EvtDeviceDispatched:
		var p domain.DeviceDispatchBatch
		if err := env.Decode(&p); err != nil {
			return err
		}
		st := c.jobs[p.JobID]
		if st == nil {
			return nil
		}
		for _, entry := range p.Devices {
			d := st.devices[entry.DeviceID]
			if d == nil {
				d = &deviceState{DeviceID: entry.DeviceID, Wave: p.Wave}
				st.devices[entry.DeviceID] = d
			}
			d.StoreID = entry.StoreID
			d.SECID = entry.SECID
			if entry.FromVersion != "" {
				d.FromVersion = entry.FromVersion
			}
			if d.Status == domain.StatusDispatched {
				continue
			}
			d.Wave = p.Wave
			d.Status = domain.StatusDispatched
			d.DispatchedAt = p.At
			d.Attempt++
			if p.Wave >= 0 && p.Wave < len(st.job.Waves) {
				st.job.Waves[p.Wave].Dispatched++
			}
			// Rebuilding after a restart must restore the controller's download
			// budget too, or a saturated mesh would look idle and the rollout
			// would double it up.
			if entry.SECID != "" {
				st.budget.Acquire(entry.SECID, canon.LabelID(entry.DeviceID))
			}
		}
		st.job.Version = version

	case canon.EvtOTADeviceUpdated:
		var p domain.DeviceUpdate
		if err := env.Decode(&p); err != nil {
			return err
		}
		c.applyOutcome(p, version)

	case canon.EvtOTARolledBack:
		var p domain.RollbackTriggered
		if err := env.Decode(&p); err != nil {
			return err
		}
		if st := c.jobs[p.JobID]; st != nil {
			st.job.HaltReason = p.Reason
			st.job.Version = version
		}

	default:
		// An unknown event type on a rollout stream is tolerated: a rolling
		// upgrade may put a newer build's events in a stream an older build is
		// replaying, and the envelope contract requires skipping rather than
		// failing.
	}
	return nil
}

// applyOutcome folds one device's result into its wave's tally.
//
// A device that reports twice — and it will, because a controller that misses
// an acknowledgement re-sends — must not be counted twice. The guard is the
// device's recorded status: once an outcome is terminal, a second report of the
// same outcome is ignored, and a *different* one moves the device and adjusts
// both tallies.
func (c *Controller) applyOutcome(p domain.DeviceUpdate, version int64) {
	st := c.jobs[p.JobID]
	if st == nil {
		return
	}
	st.job.Version = version
	d := st.devices[p.DeviceID]
	if d == nil {
		d = &deviceState{DeviceID: p.DeviceID, Wave: p.Wave}
		st.devices[p.DeviceID] = d
	}
	if d.Status == p.Status {
		return
	}
	wave := d.Wave
	if wave < 0 || wave >= len(st.job.Waves) {
		wave = p.Wave
	}
	if wave < 0 || wave >= len(st.job.Waves) {
		return
	}
	w := &st.job.Waves[wave]
	adjust(w, d.Status, -1)
	adjust(w, p.Status, +1)
	if p.Status == domain.StatusSucceeded && batteryAnomaly(p) {
		w.BatteryAnomalies++
	}
	d.Status = p.Status
	d.StoreID = p.StoreID
	d.SECID = p.SECID
	d.ReportedAt = p.At
	d.Error = p.Error
	if p.FromVersion != "" {
		d.FromVersion = p.FromVersion
	}
	// A device that has reached a terminal outcome releases its download slot,
	// which is what lets the next label on that controller start.
	if p.Status != domain.StatusDispatched && p.SECID != "" {
		st.budget.Release(p.SECID, canon.LabelID(p.DeviceID))
	}
	// Record the moment a cohort first meets its success threshold; the soak
	// timer runs from here rather than from dispatch, because a cohort that is
	// still failing has not started soaking.
	if w.SatisfiedAt.IsZero() && w.Dispatched > 0 &&
		w.Observed() >= w.Dispatched &&
		w.SuccessRate() >= st.job.Gates.WithDefaults().MinSuccessRate {
		w.SatisfiedAt = p.At
	}
}

// adjust moves a wave's tally by delta for a status.
func adjust(w *domain.WaveProgress, status domain.DeviceStatus, delta int) {
	switch status {
	case domain.StatusSucceeded:
		w.Succeeded += delta
	case domain.StatusFailed:
		w.Failed += delta
	case domain.StatusBootFailed:
		w.BootFailed += delta
	case domain.StatusSilent:
		w.Silent += delta
	case domain.StatusSkipped:
		w.Skipped += delta
	}
}

// batteryAnomaly reports whether a successful update cost materially more
// battery than an update should.
//
// A firmware flash legitimately costs a few percent of a coin cell: the radio
// is on for the whole download and the flash write itself is expensive. Beyond
// about eight points, something in the image is wrong — a sleep path that never
// sleeps, a radio that never idles — and the fleet will discover it as a wave
// of dead labels months after the rollout is closed and forgotten. Catching it
// here, in the first cohort, is the only cheap opportunity.
func batteryAnomaly(p domain.DeviceUpdate) bool {
	const maxUpdateCostPct = 8
	if p.BatteryPctBefore <= 0 || p.BatteryPctAfter <= 0 {
		return false
	}
	return p.BatteryPctBefore-p.BatteryPctAfter > maxUpdateCostPct
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// Job returns a copy of a rollout's current state.
func (c *Controller) Job(id string) (*domain.Job, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := c.jobs[id]
	if st == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	return st.job.Clone(), nil
}

// Jobs returns every rollout, newest first.
func (c *Controller) Jobs() []*domain.Job {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*domain.Job, 0, len(c.jobs))
	for _, st := range c.jobs {
		out = append(out, st.job.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].JobID < out[j].JobID
	})
	return out
}

// DeviceReport is one device's position in a rollout, as the job's device
// listing returns it.
type DeviceReport struct {
	DeviceID     string              `json:"device_id"`
	StoreID      canon.StoreID       `json:"store_id,omitempty"`
	SECID        canon.SECID         `json:"sec_id,omitempty"`
	Wave         int                 `json:"wave"`
	Status       domain.DeviceStatus `json:"status"`
	FromVersion  domain.Version      `json:"from_version,omitempty"`
	DispatchedAt time.Time           `json:"dispatched_at,omitempty"`
	ReportedAt   time.Time           `json:"reported_at,omitempty"`
	Attempt      int                 `json:"attempt,omitempty"`
	Error        string              `json:"error,omitempty"`
}

// JobDevices returns the devices in a rollout, optionally filtered by status.
//
// The failed filter is the endpoint an operator actually uses: after a rollback
// the question is never "how many" — the job page says that — it is "which
// ones, and are they all in the same store", because the answer decides whether
// the image is bad or one building's mesh is.
func (c *Controller) JobDevices(id string, status domain.DeviceStatus) ([]DeviceReport, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := c.jobs[id]
	if st == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	out := make([]DeviceReport, 0, len(st.devices))
	for _, d := range st.devices {
		if status != "" && d.Status != status {
			continue
		}
		out = append(out, DeviceReport{
			DeviceID: d.DeviceID, StoreID: d.StoreID, SECID: d.SECID,
			Wave: d.Wave, Status: d.Status, FromVersion: d.FromVersion,
			DispatchedAt: d.DispatchedAt, ReportedAt: d.ReportedAt,
			Attempt: d.Attempt, Error: d.Error,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out, nil
}

// countRejection increments the artifact rejection counter.
func (c *Controller) countRejection(reason string) {
	if c.met != nil {
		c.met.rejected.With(reason).Inc()
	}
}
