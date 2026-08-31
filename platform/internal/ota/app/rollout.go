package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/ota/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// JobSpec is a request to create a rollout.
type JobSpec struct {
	// JobID may be supplied to make creation idempotent across a retried API
	// call; empty generates one.
	JobID    string          `json:"job_id,omitempty"`
	TenantID canon.TenantID  `json:"tenant_id"`
	Stores   []canon.StoreID `json:"stores,omitempty"`
	// ArtifactID names the image to roll out. It must already be stored and
	// verified: the signature check happens at upload, and this is the only
	// handle a job can be created from, so there is no path to a rollout of an
	// image nobody verified.
	ArtifactID string `json:"artifact_id"`
	// FromVersion restricts the rollout to devices running it and enables a
	// delta against it.
	FromVersion domain.Version `json:"from_version,omitempty"`
	// Cohorts overrides the default 1/5/25/100 schedule.
	Cohorts []int `json:"cohort_percentages,omitempty"`
	// QuietHours is the delivery window, in each store's own local time.
	QuietHours domain.QuietHours `json:"quiet_hours"`
	// Gates overrides the default health thresholds.
	Gates domain.HealthGates `json:"health_gates"`
	// MaxConcurrentPerSEC overrides the per-controller download cap.
	MaxConcurrentPerSEC int `json:"max_concurrent_per_sec,omitempty"`
	// UseDelta asks the planner to ship a patch when one is smaller. It has no
	// effect without FromVersion, because a patch needs a base.
	UseDelta  bool   `json:"use_delta,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	// Start begins the rollout immediately rather than leaving it pending.
	Start bool `json:"start,omitempty"`
}

// CreateJob validates a rollout, prepares its delta if one is worth shipping,
// and records it.
//
// The artifact is looked up rather than uploaded here, and the lookup can only
// succeed for an image that passed [Controller.UploadFirmware]. That is the
// structural half of the guarantee that an unsigned image cannot be rolled out:
// not "we check the signature again", but "there is no way to name an image
// that was never checked".
func (c *Controller) CreateJob(ctx context.Context, spec JobSpec) (*domain.Job, error) {
	artifact, err := c.Artifact(spec.ArtifactID)
	if err != nil {
		return nil, err
	}
	// Re-verify against the ring rather than trusting the stored record alone.
	// The record and the image live in the same store; an attacker who could
	// rewrite one could rewrite the other, and this check costs microseconds
	// once per rollout.
	image, err := c.cfg.Artifacts.Get(artifact.ArtifactID)
	if err != nil {
		return nil, err
	}
	if _, err := domain.VerifyArtifact(c.cfg.Keys, artifact, image); err != nil {
		c.countRejection(rejectionReason(err))
		return nil, fmt.Errorf("ota: refusing to roll out %s: %w", artifact.ArtifactID, err)
	}

	cohorts := spec.Cohorts
	if len(cohorts) == 0 {
		cohorts = append([]int(nil), domain.DefaultCohorts...)
	}
	jobID := spec.JobID
	if jobID == "" {
		jobID = canon.NewULID()
	}
	perSEC := spec.MaxConcurrentPerSEC
	if perSEC <= 0 {
		perSEC = c.cfg.MaxConcurrentPerSEC
	}
	now := c.Now()

	job := &domain.Job{
		JobID:               jobID,
		TenantID:            spec.TenantID,
		Stores:              append([]canon.StoreID(nil), spec.Stores...),
		HardwareTier:        artifact.HardwareTier,
		FromVersion:         spec.FromVersion,
		ToVersion:           artifact.Version,
		ArtifactID:          artifact.ArtifactID,
		SHA256:              artifact.SHA256,
		Signature:           artifact.Signature,
		ArtifactSize:        artifact.Size,
		Cohorts:             cohorts,
		QuietHours:          spec.QuietHours,
		Gates:               spec.Gates.WithDefaults(),
		MaxConcurrentPerSEC: perSEC,
		State:               domain.JobPending,
		CurrentWave:         0,
		CreatedBy:           spec.CreatedBy,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	job.InitWaves()

	if spec.UseDelta && spec.FromVersion != "" {
		plan, err := c.PlanDelta(spec.FromVersion, artifact.Version, artifact.HardwareTier)
		if err != nil {
			return nil, err
		}
		if plan.Use {
			job.DeltaArtifactID = plan.ArtifactID
			job.DeltaFromVersion = plan.FromVersion
			job.DeltaSize = int64(plan.DeltaBytes)
		}
	}

	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	env, err := c.newEvent(canon.EvtOTAJobCreated, jobID, job.TenantID, "", domain.JobCreated{Job: job})
	if err != nil {
		return nil, err
	}
	env.IdempotencyKey = "ota-job:" + jobID
	if err := c.commit(ctx, jobID, eventstore.ExpectedNoStream, env); err != nil {
		return nil, err
	}
	if c.met != nil {
		c.met.jobs.With(string(domain.JobPending)).Inc()
	}
	c.log.Info("rollout created",
		"job_id", jobID, "tenant_id", string(job.TenantID),
		"hardware_tier", job.HardwareTier, "to_version", string(job.ToVersion),
		"from_version", string(job.FromVersion), "cohorts", fmt.Sprint(job.Cohorts),
		"delta", job.DeltaArtifactID != "", "quiet_hours", job.QuietHours.String())

	if spec.Start {
		if err := c.transitionLocked(ctx, jobID, domain.JobRunning, "created with immediate start", spec.CreatedBy); err != nil {
			return nil, err
		}
	}
	return c.Job(jobID)
}

// Start begins a pending or resumes a paused rollout.
func (c *Controller) Start(ctx context.Context, jobID, actor string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	return c.transitionLocked(ctx, jobID, domain.JobRunning, "started", actor)
}

// Pause stops dispatching without losing position.
//
// Devices already downloading finish; nothing new is sent. Resuming continues
// from the same wave with the same membership, because membership is a hash and
// not a list.
func (c *Controller) Pause(ctx context.Context, jobID, actor string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	return c.transitionLocked(ctx, jobID, domain.JobPaused, "paused by operator", actor)
}

// Resume restarts a paused rollout.
func (c *Controller) Resume(ctx context.Context, jobID, actor string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	return c.transitionLocked(ctx, jobID, domain.JobRunning, "resumed by operator", actor)
}

// Abort ends a rollout permanently. Devices already updated stay updated; use
// Rollback to put them back.
func (c *Controller) Abort(ctx context.Context, jobID, actor, reason string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	if reason == "" {
		reason = "aborted by operator"
	}
	return c.transitionLocked(ctx, jobID, domain.JobAborted, reason, actor)
}

// transitionLocked records a rollout state change. Must be called with cmdMu
// held.
func (c *Controller) transitionLocked(ctx context.Context, jobID string, to domain.JobState, reason, actor string) error {
	st := c.jobStateFor(jobID)
	if st == nil {
		return fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	job := st.job
	if job.State == to {
		return nil
	}
	if !domain.CanTransitionJob(job.State, to) {
		return fmt.Errorf("%w: rollout %s cannot move from %s to %s",
			domain.ErrIllegalTransition, jobID, job.State, to)
	}
	now := c.Now()
	env, err := c.newEvent(domain.EvtJobStateChanged, jobID, job.TenantID, "", domain.JobStateChanged{
		JobID: jobID, From: job.State, To: to, Reason: reason, Actor: actor, At: now,
	})
	if err != nil {
		return err
	}
	if err := c.commit(ctx, jobID, job.Version, env); err != nil {
		return err
	}
	if c.met != nil {
		c.met.jobs.With(string(to)).Inc()
	}
	c.log.Info("rollout state changed",
		"job_id", jobID, "from", string(job.State), "to", string(to),
		"reason", reason, "actor", actor)
	return nil
}

// jobStateFor returns the live projection under the read lock.
func (c *Controller) jobStateFor(jobID string) *jobState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.jobs[jobID]
}

// ---------------------------------------------------------------------------
// The control loop
// ---------------------------------------------------------------------------

// TickResult summarises one pass of the control loop over one rollout.
type TickResult struct {
	JobID string `json:"job_id"`
	// Dispatched is how many triggers were published this pass.
	Dispatched int `json:"dispatched"`
	// Suppressed counts devices held back and why, which is what makes a
	// rollout that appears stuck explainable without reading logs.
	SuppressedQuietHours int `json:"suppressed_quiet_hours"`
	SuppressedBandwidth  int `json:"suppressed_bandwidth"`
	SuppressedBattery    int `json:"suppressed_battery"`
	// MarkedSilent is how many dispatched devices crossed the silence window.
	MarkedSilent int `json:"marked_silent"`
	// Verdict is what the gates said about the current cohort.
	Verdict domain.Verdict  `json:"verdict"`
	Reason  string          `json:"reason,omitempty"`
	State   domain.JobState `json:"state"`
	Wave    int             `json:"wave"`
}

// Tick advances every active rollout by one step.
//
// It is the whole control loop and it is idempotent: everything it does is
// recomputed from durable state, so calling it twice in a row, or calling it
// after a restart, produces the same rollout. That property is what makes the
// loop safe to drive from a timer, from an HTTP request and from a test without
// three different code paths.
func (c *Controller) Tick(ctx context.Context) ([]TickResult, error) {
	c.mu.RLock()
	ids := make([]string, 0, len(c.jobs))
	for id, st := range c.jobs {
		if st.job.State.Active() || st.job.State == domain.JobPending {
			ids = append(ids, id)
		}
	}
	c.mu.RUnlock()
	sort.Strings(ids)

	out := make([]TickResult, 0, len(ids))
	for _, id := range ids {
		res, err := c.TickJob(ctx, id)
		if err != nil {
			return out, err
		}
		if res != nil {
			out = append(out, *res)
		}
	}
	return out, nil
}

// TickJob advances one rollout by one step.
func (c *Controller) TickJob(ctx context.Context, jobID string) (*TickResult, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	st := c.jobStateFor(jobID)
	if st == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	job := st.job
	if !job.State.Active() {
		return &TickResult{JobID: jobID, State: job.State, Wave: job.CurrentWave,
			Verdict: domain.VerdictWait, Reason: "rollout is not running"}, nil
	}
	if job.State == domain.JobPending {
		if err := c.transitionLocked(ctx, jobID, domain.JobRunning, "first tick", "controller"); err != nil {
			return nil, err
		}
		st = c.jobStateFor(jobID)
		job = st.job
	}

	now := c.Now()
	targets, err := c.eligibleTargets(ctx, job)
	if err != nil {
		return nil, err
	}
	res := &TickResult{JobID: jobID, State: job.State, Wave: job.CurrentWave}

	// Recompute the current wave's target count from the live fleet. It is
	// derived rather than stored because the fleet changes underneath a rollout
	// that runs for days: labels are provisioned, retired and quarantined, and a
	// stored count would go stale in exactly the direction that makes a cohort
	// look finished when it is not.
	c.mu.Lock()
	if w := job.Wave(); w != nil {
		w.Targeted = 0
		for _, t := range targets {
			if domain.InWave(jobID, t.DeviceID, job.Cohorts, job.CurrentWave) {
				w.Targeted++
			}
		}
	}
	c.mu.Unlock()

	res.MarkedSilent, err = c.markSilent(ctx, jobID, now)
	if err != nil {
		return nil, err
	}

	dispatched, sup, err := c.dispatchWave(ctx, jobID, targets, now)
	if err != nil {
		return nil, err
	}
	res.Dispatched = dispatched
	res.SuppressedQuietHours = sup.quietHours
	res.SuppressedBandwidth = sup.bandwidth
	res.SuppressedBattery = sup.battery

	// Re-read: dispatch changed the wave tallies.
	st = c.jobStateFor(jobID)
	job = st.job
	gate := job.EvaluateGates(now)
	res.Verdict = gate.Verdict
	res.Reason = gate.Reason

	switch gate.Verdict {
	case domain.VerdictRollback:
		if err := c.haltAndRollback(ctx, jobID, gate, now); err != nil {
			return nil, err
		}
	case domain.VerdictAdvance:
		if err := c.advanceCohort(ctx, jobID, gate, now); err != nil {
			return nil, err
		}
	case domain.VerdictComplete:
		if err := c.completeJob(ctx, jobID, gate, now); err != nil {
			return nil, err
		}
	}
	st = c.jobStateFor(jobID)
	res.State = st.job.State
	res.Wave = st.job.CurrentWave
	if c.met != nil {
		c.met.inflight.With(jobID).Set(float64(st.budget.Total()))
	}
	return res, nil
}

// eligibleTargets asks the registry for the devices a rollout may address and
// filters out the ones it should not.
func (c *Controller) eligibleTargets(ctx context.Context, job *domain.Job) ([]ports.Target, error) {
	if c.cfg.Fleet == nil {
		return nil, nil
	}
	all, err := c.cfg.Fleet.Targets(ctx, job.TenantID, job.Stores, job.HardwareTier)
	if err != nil {
		return nil, fmt.Errorf("ota: list rollout targets: %w", err)
	}
	out := make([]ports.Target, 0, len(all))
	for _, t := range all {
		if t.HardwareTier != job.HardwareTier {
			continue
		}
		// Already on the target version: not a failure, not a target.
		if domain.Version(t.FirmwareVersion) == job.ToVersion {
			continue
		}
		if job.FromVersion != "" && domain.Version(t.FirmwareVersion) != job.FromVersion {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out, nil
}

// suppression counts the reasons devices were held back this pass.
type suppression struct {
	quietHours int
	bandwidth  int
	battery    int
}

// minBatteryForUpdate is the charge below which a label is not asked to take a
// firmware download.
//
// A download is the most expensive thing a label ever does with its radio, and
// a label that runs out of power partway through a flash is the one failure
// mode that produces a device which is neither on the old firmware nor the new
// one. Twenty percent is chosen so that the update costs at most half of what
// is left even in the worst case; below it the right action is to replace the
// cell, and the rollout will pick the label up on a later pass once someone
// has.
const minBatteryForUpdate = 20

// dispatchWave publishes triggers to the devices in the current cohort that
// have not been dispatched, subject to quiet hours, bandwidth and battery.
func (c *Controller) dispatchWave(ctx context.Context, jobID string, targets []ports.Target, now time.Time) (int, suppression, error) {
	var sup suppression
	st := c.jobStateFor(jobID)
	if st == nil {
		return 0, sup, fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	job := st.job
	if job.State != domain.JobRunning {
		return 0, sup, nil
	}

	var batch []domain.DispatchedDevice
	var messages []dispatchMessage
	for _, t := range targets {
		if !domain.InWave(jobID, t.DeviceID, job.Cohorts, job.CurrentWave) {
			continue
		}
		c.mu.RLock()
		d := st.devices[t.DeviceID]
		c.mu.RUnlock()
		if d != nil && d.Status != "" && d.Status != domain.StatusPending {
			continue
		}
		// Quiet hours are evaluated in the store's own local time. A rollout
		// created once for a whole estate means two o'clock in each store's
		// morning, not two o'clock in head office's.
		window := job.QuietHours.InZone(t.TimeZone)
		if !window.Allows(now) {
			sup.quietHours++
			continue
		}
		if t.BatteryPct > 0 && t.BatteryPct < minBatteryForUpdate {
			sup.battery++
			continue
		}
		if !st.budget.Acquire(t.SECID, canon.LabelID(t.DeviceID)) {
			sup.bandwidth++
			continue
		}
		batch = append(batch, domain.DispatchedDevice{
			DeviceID:    t.DeviceID,
			StoreID:     t.StoreID,
			SECID:       t.SECID,
			FromVersion: domain.Version(t.FirmwareVersion),
		})
		messages = append(messages, dispatchMessage{target: t, update: c.buildUpdate(job, t, now)})
	}
	if len(batch) == 0 {
		if c.met != nil {
			if sup.quietHours > 0 {
				c.met.suppressed.With("quiet-hours").Add(uint64(sup.quietHours))
			}
			if sup.bandwidth > 0 {
				c.met.suppressed.With("bandwidth").Add(uint64(sup.bandwidth))
			}
			if sup.battery > 0 {
				c.met.suppressed.With("battery").Add(uint64(sup.battery))
			}
		}
		return 0, sup, nil
	}

	// Record the dispatch before publishing. A crash between the two re-sends a
	// trigger the device has already seen, which QoS 2 and the device's own
	// sequence check make a no-op; the opposite order would lose the record of a
	// download that is already consuming mesh bandwidth.
	env, err := c.newEvent(domain.EvtDeviceDispatched, jobID, job.TenantID, "", domain.DeviceDispatchBatch{
		JobID: jobID, Wave: job.CurrentWave, Devices: batch, At: now,
	})
	if err != nil {
		return 0, sup, err
	}
	if err := c.commit(ctx, jobID, job.Version, env); err != nil {
		return 0, sup, err
	}

	for _, m := range messages {
		c.publishTrigger(ctx, job, m)
	}
	if c.met != nil {
		c.met.dispatched.With().Add(uint64(len(batch)))
		if sup.quietHours > 0 {
			c.met.suppressed.With("quiet-hours").Add(uint64(sup.quietHours))
		}
		if sup.bandwidth > 0 {
			c.met.suppressed.With("bandwidth").Add(uint64(sup.bandwidth))
		}
		if sup.battery > 0 {
			c.met.suppressed.With("battery").Add(uint64(sup.battery))
		}
	}
	c.log.Info("firmware cohort dispatched",
		"job_id", jobID, "wave", job.CurrentWave, "devices", len(batch),
		"suppressed_quiet_hours", sup.quietHours, "suppressed_bandwidth", sup.bandwidth,
		"suppressed_battery", sup.battery)
	return len(batch), sup, nil
}

type dispatchMessage struct {
	target ports.Target
	update domain.DeviceUpdate
}

// buildUpdate assembles the trigger a device receives.
func (c *Controller) buildUpdate(job *domain.Job, t ports.Target, now time.Time) domain.DeviceUpdate {
	u := domain.DeviceUpdate{
		JobID:       job.JobID,
		DeviceID:    t.DeviceID,
		TenantID:    job.TenantID,
		StoreID:     t.StoreID,
		SECID:       t.SECID,
		Wave:        job.CurrentWave,
		Status:      domain.StatusDispatched,
		FromVersion: domain.Version(t.FirmwareVersion),
		ToVersion:   job.ToVersion,
		ArtifactID:  job.ArtifactID,
		SHA256:      job.SHA256,
		Signature:   job.Signature,
		SizeBytes:   job.ArtifactSize,
		At:          now,
	}
	// A patch is offered only to a device actually running the base it applies
	// to. Everything else takes the whole image, which is why the base version
	// travels with the trigger rather than being assumed.
	if job.DeltaArtifactID != "" && domain.Version(t.FirmwareVersion) == job.DeltaFromVersion {
		u.Delta = true
		u.DeltaFromVersion = job.DeltaFromVersion
		u.DeltaSize = job.DeltaSize
		u.ArtifactID = job.DeltaArtifactID
		u.DeltaSHA256 = job.DeltaArtifactID[len("sha256:"):]
	}
	return u
}

// publishTrigger sends one OTA command to a device's zone topic.
//
// Interface contract §3: the `…/sec/{sec}/labels/{label}/ota` topic is QoS 2 and
// not retained, and carries an `Envelope{ota.device.updated}`. QoS 2 is the only
// place in the platform that uses it, and the reason is battery: a duplicated
// price update is a no-op the label discards on its sequence number, but a
// duplicated OTA trigger is a second firmware download, and a firmware download
// is days of a coin cell's budget. Not retaining it matters just as much — a
// retained trigger would be replayed to every label that joins that zone
// afterwards, including replacements that shipped with newer firmware.
func (c *Controller) publishTrigger(ctx context.Context, job *domain.Job, m dispatchMessage) {
	if c.cfg.Messenger == nil {
		return
	}
	env, err := c.newEvent(canon.EvtOTADeviceUpdated, job.JobID, job.TenantID, m.target.StoreID, m.update)
	if err != nil {
		c.log.Warn("ota could not build a device trigger", "device_id", m.target.DeviceID, "error", err)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		c.log.Warn("ota could not encode a device trigger", "device_id", m.target.DeviceID, "error", err)
		return
	}
	scope := canon.TopicScope{Tenant: job.TenantID, Region: c.cfg.Region, Store: m.target.StoreID}
	topic := scope.SECLabelTopic(m.target.SECID, canon.LabelID(m.target.DeviceID), canon.LeafOTA)
	if err := c.cfg.Messenger.Publish(ctx, msgbus.Message{
		Topic:   topic,
		Payload: body,
		QoS:     msgbus.QoS(canon.QoSOTA),
		Retain:  false,
	}); err != nil {
		c.log.Warn("ota could not publish a device trigger",
			"device_id", m.target.DeviceID, "topic", topic, "error", err)
	}
}

// markSilent moves dispatched devices past the silence window to silent.
//
// This is the gate that catches the worst kind of bad image: one that installs,
// bricks the radio, and reports nothing. Without it a cohort of devices that
// were all killed by the update looks identical to a cohort that has simply not
// answered yet, and the rollout advances.
func (c *Controller) markSilent(ctx context.Context, jobID string, now time.Time) (int, error) {
	st := c.jobStateFor(jobID)
	if st == nil {
		return 0, nil
	}
	job := st.job
	window := job.Gates.WithDefaults().SilenceWindow

	var silent []domain.DeviceUpdate
	c.mu.RLock()
	for _, d := range st.devices {
		if d.Status != domain.StatusDispatched {
			continue
		}
		if d.DispatchedAt.IsZero() || now.Sub(d.DispatchedAt) < window {
			continue
		}
		silent = append(silent, domain.DeviceUpdate{
			JobID: jobID, DeviceID: d.DeviceID, TenantID: job.TenantID,
			StoreID: d.StoreID, SECID: d.SECID, Wave: d.Wave,
			Status: domain.StatusSilent, ToVersion: job.ToVersion,
			Error: fmt.Sprintf("no report within %s of dispatch", window),
			At:    now,
		})
	}
	c.mu.RUnlock()
	if len(silent) == 0 {
		return 0, nil
	}
	sort.Slice(silent, func(i, j int) bool { return silent[i].DeviceID < silent[j].DeviceID })

	envs := make([]canon.Envelope, 0, len(silent))
	for _, u := range silent {
		env, err := c.newEvent(canon.EvtOTADeviceUpdated, jobID, job.TenantID, u.StoreID, u)
		if err != nil {
			return 0, err
		}
		env.IdempotencyKey = fmt.Sprintf("silent:%s:%s", jobID, u.DeviceID)
		envs = append(envs, env)
	}
	if err := c.commit(ctx, jobID, job.Version, envs...); err != nil {
		return 0, err
	}
	c.log.Warn("devices went silent after a firmware update",
		"job_id", jobID, "devices", len(silent), "window", window.String())
	return len(silent), nil
}

// advanceCohort widens the rollout to the next wave.
func (c *Controller) advanceCohort(ctx context.Context, jobID string, gate domain.GateResult, now time.Time) error {
	st := c.jobStateFor(jobID)
	job := st.job
	next := job.CurrentWave + 1
	env, err := c.newEvent(canon.EvtOTACohortAdvanced, jobID, job.TenantID, "", domain.CohortAdvanced{
		JobID: jobID, From: job.CurrentWave, To: next,
		ToPercent: job.Cohorts[next], Metrics: gate.Metrics, Reason: gate.Reason, At: now,
	})
	if err != nil {
		return err
	}
	env.IdempotencyKey = fmt.Sprintf("cohort:%s:%d", jobID, next)
	if err := c.commit(ctx, jobID, job.Version, env); err != nil {
		return err
	}
	c.log.Info("rollout cohort advanced",
		"job_id", jobID, "from_wave", job.CurrentWave, "to_wave", next,
		"to_percent", job.Cohorts[next], "reason", gate.Reason)
	return nil
}

// completeJob closes a rollout that passed its last cohort.
func (c *Controller) completeJob(ctx context.Context, jobID string, gate domain.GateResult, now time.Time) error {
	st := c.jobStateFor(jobID)
	job := st.job
	env, err := c.newEvent(canon.EvtOTACohortAdvanced, jobID, job.TenantID, "", domain.CohortAdvanced{
		JobID: jobID, From: job.CurrentWave, To: job.CurrentWave,
		ToPercent: job.Cohorts[job.CurrentWave], Metrics: gate.Metrics,
		Reason: "final cohort passed", At: now,
	})
	if err != nil {
		return err
	}
	env.IdempotencyKey = fmt.Sprintf("cohort-final:%s", jobID)
	if err := c.commit(ctx, jobID, job.Version, env); err != nil {
		return err
	}
	return c.transitionLocked(ctx, jobID, domain.JobCompleted, gate.Reason, "controller")
}

// haltAndRollback stops a rollout whose cohort failed a health gate.
//
// It halts first and emits the rollback event second, in that order, because
// the halt is what stops more devices being harmed and the event is what tells
// a human. Halting is automatic and unconditional; putting the previous
// firmware back is a separate, explicit step ([Controller.Rollback]), because
// re-flashing a cohort is itself a firmware rollout and doing it automatically
// on top of a failure nobody has looked at yet is how one bad image becomes two.
func (c *Controller) haltAndRollback(ctx context.Context, jobID string, gate domain.GateResult, now time.Time) error {
	st := c.jobStateFor(jobID)
	job := st.job

	affected := 0
	c.mu.RLock()
	for _, d := range st.devices {
		if d.Status == domain.StatusSucceeded || d.Status == domain.StatusBootFailed {
			affected++
		}
	}
	c.mu.RUnlock()

	if err := c.transitionLocked(ctx, jobID, domain.JobHalted, gate.Reason, "controller"); err != nil {
		return err
	}
	st = c.jobStateFor(jobID)
	env, err := c.newEvent(canon.EvtOTARolledBack, jobID, job.TenantID, "", domain.RollbackTriggered{
		JobID: jobID, Wave: gate.Wave, Reason: gate.Reason, Metrics: gate.Metrics,
		ToVersion: job.ToVersion, FromVersion: job.FromVersion, Affected: affected, At: now,
	})
	if err != nil {
		return err
	}
	env.IdempotencyKey = fmt.Sprintf("rollback:%s:%d", jobID, gate.Wave)
	if err := c.commit(ctx, jobID, st.job.Version, env); err != nil {
		return err
	}
	if c.met != nil {
		c.met.rollbacks.With().Inc()
	}
	c.log.Error("rollout halted and rollback triggered",
		"job_id", jobID, "wave", gate.Wave, "reason", gate.Reason,
		"affected_devices", affected, "to_version", string(job.ToVersion))
	return nil
}

// Rollback puts the previous firmware back on the devices that took the new one.
//
// It is explicit rather than automatic: see [Controller.haltAndRollback]. The
// devices are re-dispatched with the source version's artifact, which must be
// stored — a rollout created with no FromVersion has nothing to roll back to,
// and saying so is better than guessing at a version.
func (c *Controller) Rollback(ctx context.Context, jobID, actor string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	st := c.jobStateFor(jobID)
	if st == nil {
		return fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	job := st.job
	if job.FromVersion == "" {
		return fmt.Errorf("%w: rollout %s has no source version to return devices to",
			domain.ErrInvalid, jobID)
	}
	if _, err := c.artifactIDForVersion(job.HardwareTier, job.FromVersion); err != nil {
		return fmt.Errorf("ota: cannot roll back %s: %w", jobID, err)
	}
	if err := c.transitionLocked(ctx, jobID, domain.JobRollingBack, "operator initiated rollback", actor); err != nil {
		return err
	}

	now := c.Now()
	st = c.jobStateFor(jobID)
	var updates []domain.DeviceUpdate
	c.mu.RLock()
	for _, d := range st.devices {
		if d.Status != domain.StatusSucceeded {
			continue
		}
		updates = append(updates, domain.DeviceUpdate{
			JobID: jobID, DeviceID: d.DeviceID, TenantID: job.TenantID,
			StoreID: d.StoreID, SECID: d.SECID, Wave: d.Wave,
			Status: domain.StatusRolledBack, FromVersion: job.ToVersion,
			ToVersion: job.FromVersion, At: now,
		})
	}
	c.mu.RUnlock()
	sort.Slice(updates, func(i, j int) bool { return updates[i].DeviceID < updates[j].DeviceID })

	if len(updates) > 0 {
		envs := make([]canon.Envelope, 0, len(updates))
		for _, u := range updates {
			env, err := c.newEvent(canon.EvtOTADeviceUpdated, jobID, job.TenantID, u.StoreID, u)
			if err != nil {
				return err
			}
			env.IdempotencyKey = fmt.Sprintf("rolled-back:%s:%s", jobID, u.DeviceID)
			envs = append(envs, env)
		}
		if err := c.commit(ctx, jobID, st.job.Version, envs...); err != nil {
			return err
		}
	}
	st = c.jobStateFor(jobID)
	if err := c.transitionLocked(ctx, jobID, domain.JobRolledBack,
		fmt.Sprintf("%d devices returned to %s", len(updates), job.FromVersion), actor); err != nil {
		return err
	}
	c.log.Warn("rollout rolled back",
		"job_id", jobID, "devices", len(updates), "to_version", string(job.FromVersion))
	return nil
}

// RecordOutcome folds a device's own report of an update into the rollout.
//
// It is idempotent per device and status: a controller that misses an
// acknowledgement re-sends, and a duplicate report must not be counted twice
// against a cohort's error rate.
func (c *Controller) RecordOutcome(ctx context.Context, u domain.DeviceUpdate) error {
	if u.JobID == "" || u.DeviceID == "" {
		return fmt.Errorf("%w: outcome needs a job and a device", domain.ErrInvalid)
	}
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	st := c.jobStateFor(u.JobID)
	if st == nil {
		return fmt.Errorf("%w: %s", ErrJobNotFound, u.JobID)
	}
	job := st.job
	c.mu.RLock()
	existing := st.devices[u.DeviceID]
	c.mu.RUnlock()
	if existing != nil {
		if existing.Status == u.Status {
			return nil
		}
		if u.Wave == 0 {
			u.Wave = existing.Wave
		}
		if u.SECID == "" {
			u.SECID = existing.SECID
		}
		if u.StoreID == "" {
			u.StoreID = existing.StoreID
		}
	}
	if u.At.IsZero() {
		u.At = c.Now()
	}
	if u.TenantID == "" {
		u.TenantID = job.TenantID
	}
	if u.ToVersion == "" {
		u.ToVersion = job.ToVersion
	}

	env, err := c.newEvent(canon.EvtOTADeviceUpdated, u.JobID, job.TenantID, u.StoreID, u)
	if err != nil {
		return err
	}
	env.IdempotencyKey = fmt.Sprintf("outcome:%s:%s:%s", u.JobID, u.DeviceID, u.Status)
	if err := c.commit(ctx, u.JobID, job.Version, env); err != nil {
		return err
	}
	if c.met != nil {
		c.met.outcomes.With(string(u.Status)).Inc()
	}
	return nil
}
