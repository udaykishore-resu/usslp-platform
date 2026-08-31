package domain

import (
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// JobState is where a rollout sits in its lifecycle.
type JobState string

// The rollout states.
const (
	// JobPending is a created job that has not dispatched anything.
	JobPending JobState = "pending"
	// JobRunning is a job actively dispatching and gating cohorts.
	JobRunning JobState = "running"
	// JobPaused is a job an operator stopped. Devices already updated stay
	// updated; nothing new is dispatched. Resuming continues from the same wave
	// with the same membership.
	JobPaused JobState = "paused"
	// JobHalted is a job the controller stopped because a health gate failed.
	// It is distinct from paused because nobody chose it and because resuming
	// one requires an explicit override rather than a resume.
	JobHalted JobState = "halted"
	// JobRollingBack is a halted job actively pushing the previous firmware
	// back to the devices that took the new one.
	JobRollingBack JobState = "rolling_back"
	// JobRolledBack is a completed rollback.
	JobRolledBack JobState = "rolled_back"
	// JobCompleted is a rollout that reached 100% and passed its gates.
	JobCompleted JobState = "completed"
	// JobAborted is a job an operator cancelled. Terminal.
	JobAborted JobState = "aborted"
)

// Terminal reports whether the job will do nothing further on its own.
func (s JobState) Terminal() bool {
	return s == JobCompleted || s == JobAborted || s == JobRolledBack
}

// Active reports whether the controller should be doing work for this job.
func (s JobState) Active() bool {
	return s == JobPending || s == JobRunning || s == JobRollingBack
}

// legalJobTransitions enumerates the rollout lifecycle.
//
// Two edges are deliberately absent. There is no halted → running: a job the
// controller stopped for a failed health gate must not be restarted by the same
// "resume" button an operator uses on a job they paused themselves, because the
// two situations call for entirely different judgement. And there is no
// rolled_back → anything: a rollback that has finished is history, and the way
// to try again is a new job with a new artifact.
var legalJobTransitions = map[JobState]map[JobState]bool{
	JobPending:     {JobRunning: true, JobPaused: true, JobAborted: true},
	JobRunning:     {JobPaused: true, JobHalted: true, JobCompleted: true, JobAborted: true, JobRollingBack: true},
	JobPaused:      {JobRunning: true, JobAborted: true, JobHalted: true},
	JobHalted:      {JobRollingBack: true, JobAborted: true},
	JobRollingBack: {JobRolledBack: true, JobAborted: true},
	JobRolledBack:  {},
	JobCompleted:   {},
	JobAborted:     {},
}

// CanTransitionJob reports whether a rollout state change is legal.
func CanTransitionJob(from, to JobState) bool { return legalJobTransitions[from][to] }

// HealthGates are the thresholds a cohort must satisfy before the rollout
// widens, and the thresholds that trigger an automatic rollback.
//
// Four independent signals are watched because a bad firmware image fails in
// four different ways and any one of them alone would be missed:
//
//   - an update that fails outright reports a failure, and the failure rate is
//     the obvious signal;
//   - an update that installs and then fails to boot reports nothing at all
//     from the device — the *controller* reports it, which is why boot failures
//     are counted separately from update failures;
//   - an image with a bug in its sleep path drains a coin cell in weeks instead
//     of years, and shows up as a battery-drain anomaly long before it shows up
//     as a failure;
//   - an image that bricks the radio produces silence, which looks identical to
//     a healthy device that has not reported yet until enough time has passed.
//
// The last is the one a naive rollout controller always misses: it counts
// successes and failures, sees no failures, and advances a rollout that has
// killed every device it touched.
type HealthGates struct {
	// MaxErrorRate is the failed-update fraction that triggers a rollback. The
	// blueprint's default is 2%: high enough that a handful of labels with flat
	// batteries in one aisle do not halt a national rollout, low enough that a
	// genuinely bad image is caught in the first cohort.
	MaxErrorRate float64 `json:"max_error_rate"`
	// MaxBootFailureRate is the fraction of devices that took the image and did
	// not come back. It is lower than MaxErrorRate because a boot failure is a
	// device somebody has to physically retrieve, where a failed update is a
	// device that is still running its old firmware and can be retried.
	MaxBootFailureRate float64 `json:"max_boot_failure_rate"`
	// MaxSilenceRate is the fraction of updated devices that have reported
	// nothing within SilenceWindow.
	MaxSilenceRate float64 `json:"max_silence_rate"`
	// MaxBatteryAnomalyRate is the fraction of updated devices whose post-update
	// drain rate is materially worse than their pre-update baseline.
	MaxBatteryAnomalyRate float64 `json:"max_battery_anomaly_rate"`
	// MinSuccessRate is the fraction of a cohort that must have succeeded before
	// the rollout widens.
	MinSuccessRate float64 `json:"min_success_rate"`
	// MinCohortSamples is how many outcomes must be in before an error rate is
	// believed. Without it a first wave of three devices with one failure reads
	// as a 33% error rate and halts a rollout on no evidence.
	MinCohortSamples int `json:"min_cohort_samples"`
	// SoakDuration is how long a cohort must sit at its success threshold before
	// the next one starts. It exists because the failures that matter most —
	// battery drain, a watchdog reboot loop — take time to appear, and a
	// rollout that advances the instant the last acknowledgement lands has
	// tested nothing except the download.
	SoakDuration time.Duration `json:"soak_duration"`
	// SilenceWindow is how long a dispatched device may say nothing before it
	// is counted as silent.
	SilenceWindow time.Duration `json:"silence_window"`
}

// DefaultHealthGates returns the platform's thresholds.
func DefaultHealthGates() HealthGates {
	return HealthGates{
		MaxErrorRate:          0.02,
		MaxBootFailureRate:    0.01,
		MaxSilenceRate:        0.05,
		MaxBatteryAnomalyRate: 0.05,
		MinSuccessRate:        0.95,
		MinCohortSamples:      20,
		SoakDuration:          30 * time.Minute,
		SilenceWindow:         15 * time.Minute,
	}
}

// WithDefaults fills unset fields so a partially specified gate set is safe.
func (g HealthGates) WithDefaults() HealthGates {
	d := DefaultHealthGates()
	if g.MaxErrorRate <= 0 {
		g.MaxErrorRate = d.MaxErrorRate
	}
	if g.MaxBootFailureRate <= 0 {
		g.MaxBootFailureRate = d.MaxBootFailureRate
	}
	if g.MaxSilenceRate <= 0 {
		g.MaxSilenceRate = d.MaxSilenceRate
	}
	if g.MaxBatteryAnomalyRate <= 0 {
		g.MaxBatteryAnomalyRate = d.MaxBatteryAnomalyRate
	}
	if g.MinSuccessRate <= 0 {
		g.MinSuccessRate = d.MinSuccessRate
	}
	if g.MinCohortSamples <= 0 {
		g.MinCohortSamples = d.MinCohortSamples
	}
	if g.SoakDuration <= 0 {
		g.SoakDuration = d.SoakDuration
	}
	if g.SilenceWindow <= 0 {
		g.SilenceWindow = d.SilenceWindow
	}
	return g
}

// DeviceStatus is where one device stands in a rollout.
type DeviceStatus string

// The per-device outcomes.
const (
	// StatusPending means the device is in a cohort that has not started.
	StatusPending DeviceStatus = "pending"
	// StatusDispatched means the trigger has been published and the device is
	// expected to download.
	StatusDispatched DeviceStatus = "dispatched"
	// StatusSucceeded means the device reported the new version running.
	StatusSucceeded DeviceStatus = "succeeded"
	// StatusFailed means the device reported that the update did not apply. It
	// is still running its old firmware.
	StatusFailed DeviceStatus = "failed"
	// StatusBootFailed means the device took the image and did not come back.
	StatusBootFailed DeviceStatus = "boot_failed"
	// StatusSilent means the device has reported nothing since dispatch and the
	// silence window has elapsed.
	StatusSilent DeviceStatus = "silent"
	// StatusSkipped means the device was not eligible: wrong tier, not
	// addressable, or already on the target version.
	StatusSkipped DeviceStatus = "skipped"
	// StatusRolledBack means the device has been returned to its previous
	// firmware.
	StatusRolledBack DeviceStatus = "rolled_back"
)

// Failure reports whether a status counts against a cohort's health.
func (s DeviceStatus) Failure() bool {
	return s == StatusFailed || s == StatusBootFailed || s == StatusSilent
}

// WaveProgress is one cohort's running tally.
type WaveProgress struct {
	Wave int `json:"wave"`
	// Percent is the cumulative fleet percentage this wave reaches.
	Percent int `json:"percent"`
	// Targeted is how many eligible devices fall in this wave.
	Targeted int `json:"targeted"`
	// Dispatched is how many triggers have been published.
	Dispatched int `json:"dispatched"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	BootFailed int `json:"boot_failed"`
	Silent     int `json:"silent"`
	// BatteryAnomalies counts devices whose post-update drain is materially
	// worse than their own pre-update baseline.
	BatteryAnomalies int `json:"battery_anomalies"`
	// Skipped counts devices in the wave that were not eligible.
	Skipped     int       `json:"skipped"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	SatisfiedAt time.Time `json:"satisfied_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// Observed returns how many dispatched devices have reported an outcome, silent
// ones included.
func (w WaveProgress) Observed() int {
	return w.Succeeded + w.Failed + w.BootFailed + w.Silent
}

// Errors returns the number of outcomes that count against health.
func (w WaveProgress) Errors() int { return w.Failed + w.BootFailed + w.Silent }

// ErrorRate is the fraction of observed outcomes that failed.
func (w WaveProgress) ErrorRate() float64 { return rate(w.Errors(), w.Observed()) }

// BootFailureRate is the fraction of observed outcomes that did not come back.
func (w WaveProgress) BootFailureRate() float64 { return rate(w.BootFailed, w.Observed()) }

// SilenceRate is the fraction of dispatched devices that have said nothing.
func (w WaveProgress) SilenceRate() float64 { return rate(w.Silent, w.Dispatched) }

// BatteryAnomalyRate is the fraction of succeeded devices draining abnormally.
func (w WaveProgress) BatteryAnomalyRate() float64 { return rate(w.BatteryAnomalies, w.Succeeded) }

// SuccessRate is the fraction of dispatched devices confirmed on the new
// firmware.
func (w WaveProgress) SuccessRate() float64 { return rate(w.Succeeded, w.Dispatched) }

func rate(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Job is one staged firmware rollout.
//
// Its state is rebuilt by replaying the job's event stream, never held only in
// memory. That is not fastidiousness: a rollout runs for days, a deploy of the
// OTA service happens in the middle of one, and a controller that forgot which
// cohort it was on would either re-dispatch a wave that already succeeded or
// advance past one that was still soaking.
type Job struct {
	JobID    string         `json:"job_id"`
	TenantID canon.TenantID `json:"tenant_id"`
	// Stores restricts the rollout. Empty means every store the registry knows
	// for the tenant, which is the normal case for a fleet-wide release.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// HardwareTier is the only tier this job will address. It is checked against
	// each device as well as against the artifact, so a job cannot be created
	// for one tier and dispatched to another.
	HardwareTier string `json:"hardware_tier"`
	// FromVersion, when set, restricts the rollout to devices currently running
	// it. A release that is only safe as an upgrade from 1.2.x says so here.
	FromVersion Version `json:"from_version,omitempty"`
	ToVersion   Version `json:"to_version"`

	// ArtifactID, SHA256 and Signature identify and authenticate the image. They
	// are copied onto the job at creation so that the trigger a device receives
	// carries the digest and signature it must check, without the device ever
	// asking a service a question.
	ArtifactID   string `json:"artifact_id"`
	SHA256       string `json:"sha256"`
	Signature    string `json:"signature"`
	ArtifactSize int64  `json:"artifact_size"`
	// DeltaArtifactID and DeltaSize describe the patch shipped in preference to
	// the whole image, when one is smaller. DeltaFromVersion is the base it
	// applies to; a device on any other version takes the full image.
	DeltaArtifactID  string  `json:"delta_artifact_id,omitempty"`
	DeltaSize        int64   `json:"delta_size,omitempty"`
	DeltaFromVersion Version `json:"delta_from_version,omitempty"`

	Cohorts             []int       `json:"cohort_percentages"`
	QuietHours          QuietHours  `json:"quiet_hours"`
	Gates               HealthGates `json:"health_gates"`
	MaxConcurrentPerSEC int         `json:"max_concurrent_per_sec"`

	State JobState `json:"state"`
	// CurrentWave is the index into Cohorts the rollout is working on.
	CurrentWave int            `json:"current_wave"`
	Waves       []WaveProgress `json:"waves"`
	// HaltReason records why the controller stopped, in the words an operator
	// will read on the job page.
	HaltReason string `json:"halt_reason,omitempty"`

	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// Version is the aggregate version, used for the event store's optimistic
	// concurrency check.
	Version int64 `json:"version"`
}

// Clone returns a deep copy, so the controller can hand a job to an HTTP
// handler without the handler racing the next cohort advance.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	out := *j
	out.Stores = append([]canon.StoreID(nil), j.Stores...)
	out.Cohorts = append([]int(nil), j.Cohorts...)
	out.Waves = append([]WaveProgress(nil), j.Waves...)
	return &out
}

// Wave returns the progress record for the current cohort.
func (j *Job) Wave() *WaveProgress {
	if j.CurrentWave < 0 || j.CurrentWave >= len(j.Waves) {
		return nil
	}
	return &j.Waves[j.CurrentWave]
}

// Validate rejects a job that cannot be executed safely.
func (j *Job) Validate() error {
	switch {
	case j.JobID == "":
		return fmt.Errorf("%w: job id is required", ErrInvalid)
	case !canon.ValidID(string(j.TenantID)):
		return fmt.Errorf("%w: tenant id %q", ErrInvalid, j.TenantID)
	case j.HardwareTier == "":
		return fmt.Errorf("%w: hardware tier is required", ErrInvalid)
	case !j.ToVersion.Valid():
		return fmt.Errorf("%w: target version %q", ErrInvalid, j.ToVersion)
	case j.ArtifactID == "":
		return fmt.Errorf("%w: artifact id is required", ErrInvalid)
	case j.Signature == "":
		return ErrUnsigned
	}
	if j.FromVersion != "" && !j.FromVersion.Valid() {
		return fmt.Errorf("%w: source version %q", ErrInvalid, j.FromVersion)
	}
	// A rollout that installs an older image than the one running is a
	// downgrade, which is a legitimate operation but never an accidental one.
	// Refusing it by default means the accident cannot happen; an operator who
	// means it creates the job with FromVersion unset.
	if j.FromVersion != "" && j.ToVersion.Compare(j.FromVersion) <= 0 {
		return fmt.Errorf("%w: target version %s does not advance on source version %s",
			ErrInvalid, j.ToVersion, j.FromVersion)
	}
	if err := ValidateCohorts(j.Cohorts); err != nil {
		return err
	}
	if err := j.QuietHours.Validate(); err != nil {
		return err
	}
	for _, s := range j.Stores {
		if !canon.ValidID(string(s)) {
			return fmt.Errorf("%w: store id %q", ErrInvalid, s)
		}
	}
	return nil
}

// InitWaves builds the per-cohort progress records. It is called once, at
// creation, so that the shape of the rollout is fixed before anything is
// dispatched and cannot be changed by a later edit.
func (j *Job) InitWaves() {
	j.Waves = make([]WaveProgress, len(j.Cohorts))
	for i, pct := range j.Cohorts {
		j.Waves[i] = WaveProgress{Wave: i, Percent: pct}
	}
}

// TransitionTo moves the job to a new state.
func (j *Job) TransitionTo(to JobState, reason string, now time.Time) error {
	if !CanTransitionJob(j.State, to) {
		return fmt.Errorf("%w: rollout cannot move from %s to %s", ErrIllegalTransition, j.State, to)
	}
	j.State = to
	j.UpdatedAt = now.UTC()
	switch to {
	case JobRunning:
		if j.StartedAt.IsZero() {
			j.StartedAt = now.UTC()
		}
	case JobHalted, JobRollingBack:
		j.HaltReason = reason
	case JobCompleted, JobAborted, JobRolledBack:
		j.CompletedAt = now.UTC()
		if to != JobCompleted {
			j.HaltReason = reason
		}
	}
	return nil
}

// ErrIllegalTransition marks a rollout state change the lifecycle does not
// permit.
var ErrIllegalTransition = fmt.Errorf("ota: illegal rollout transition")

// Verdict is what the health gates say the controller should do next.
type Verdict string

// The four verdicts.
const (
	// VerdictWait means the cohort is still in flight or still soaking.
	VerdictWait Verdict = "wait"
	// VerdictAdvance means the cohort passed and the next one may start.
	VerdictAdvance Verdict = "advance"
	// VerdictComplete means the last cohort passed.
	VerdictComplete Verdict = "complete"
	// VerdictRollback means a gate failed and the rollout must stop and undo.
	VerdictRollback Verdict = "rollback"
)

// GateResult is the outcome of evaluating a cohort against its gates.
type GateResult struct {
	Verdict Verdict `json:"verdict"`
	// Reason is the sentence an operator reads on the job page and in the
	// ota.rollback.triggered event. It always names the measurement and the
	// threshold, because "health check failed" is not something anybody can act
	// on at three in the morning.
	Reason string `json:"reason"`
	Wave   int    `json:"wave"`
	// Metrics is the cohort tally the verdict was reached from.
	Metrics WaveProgress `json:"metrics"`
}

// EvaluateGates decides what to do with the current cohort at instant now.
//
// The order of the checks is the safety design. Failures are evaluated before
// success, so a cohort that is simultaneously above its success threshold and
// above its error threshold rolls back rather than advances — which is not a
// hypothetical: a 96% success rate with a 4% boot-failure rate passes a naive
// success gate and means four devices in every hundred are bricked.
func (j *Job) EvaluateGates(now time.Time) GateResult {
	gates := j.Gates.WithDefaults()
	w := j.Wave()
	if w == nil {
		return GateResult{Verdict: VerdictWait, Reason: "no cohort in progress"}
	}
	res := GateResult{Wave: w.Wave, Metrics: *w}

	if w.Dispatched == 0 {
		if w.Targeted == 0 && w.Skipped >= 0 {
			// A cohort with nothing eligible in it is passed immediately rather
			// than blocking the rollout: a hardware tier that is simply absent
			// from the first 1% of a fleet is normal.
			return j.completionVerdict(res, "cohort has no eligible devices")
		}
		res.Verdict = VerdictWait
		res.Reason = "cohort has not dispatched yet"
		return res
	}

	// Enough evidence to believe a rate: either the minimum sample size, or the
	// whole cohort has reported, whichever comes first.
	enough := w.Observed() >= gates.MinCohortSamples || w.Observed() >= w.Dispatched

	// The specific failure modes are tested before the aggregate error rate, and
	// the order is diagnostic rather than arithmetic. Silence and boot failures
	// both count towards the error rate, so a cohort killed outright by an image
	// would trip the aggregate gate first and page somebody with "error rate
	// 100%" — true, and useless. Naming the mechanism is what tells the person
	// woken up whether this is a bad image or a bad building.
	if enough {
		if r := w.BootFailureRate(); r > gates.MaxBootFailureRate {
			res.Verdict = VerdictRollback
			res.Reason = fmt.Sprintf("boot-failure rate %.2f%% exceeds the %.2f%% threshold (%d devices did not come back)",
				r*100, gates.MaxBootFailureRate*100, w.BootFailed)
			return res
		}
		if r := w.SilenceRate(); r > gates.MaxSilenceRate {
			res.Verdict = VerdictRollback
			res.Reason = fmt.Sprintf("post-update silence %.2f%% exceeds the %.2f%% threshold (%d of %d devices have not reported)",
				r*100, gates.MaxSilenceRate*100, w.Silent, w.Dispatched)
			return res
		}
		if r := w.BatteryAnomalyRate(); r > gates.MaxBatteryAnomalyRate {
			res.Verdict = VerdictRollback
			res.Reason = fmt.Sprintf("battery-drain anomaly %.2f%% exceeds the %.2f%% threshold (%d of %d updated devices)",
				r*100, gates.MaxBatteryAnomalyRate*100, w.BatteryAnomalies, w.Succeeded)
			return res
		}
		if r := w.ErrorRate(); r > gates.MaxErrorRate {
			res.Verdict = VerdictRollback
			res.Reason = fmt.Sprintf("error rate %.2f%% exceeds the %.2f%% threshold (%d failures in %d outcomes)",
				r*100, gates.MaxErrorRate*100, w.Errors(), w.Observed())
			return res
		}
	}

	if w.Observed() < w.Dispatched {
		res.Verdict = VerdictWait
		res.Reason = fmt.Sprintf("%d of %d dispatched devices have reported", w.Observed(), w.Dispatched)
		return res
	}
	if r := w.SuccessRate(); r < gates.MinSuccessRate {
		res.Verdict = VerdictWait
		res.Reason = fmt.Sprintf("success rate %.2f%% is below the %.2f%% threshold", r*100, gates.MinSuccessRate*100)
		return res
	}
	if w.SatisfiedAt.IsZero() {
		res.Verdict = VerdictWait
		res.Reason = "cohort has just met its success threshold and has not soaked"
		return res
	}
	if now.Sub(w.SatisfiedAt) < gates.SoakDuration {
		res.Verdict = VerdictWait
		res.Reason = fmt.Sprintf("soaking: %s of %s elapsed",
			now.Sub(w.SatisfiedAt).Round(time.Second), gates.SoakDuration)
		return res
	}
	return j.completionVerdict(res, fmt.Sprintf("cohort passed: %d of %d succeeded", w.Succeeded, w.Dispatched))
}

// completionVerdict picks between advancing and finishing.
func (j *Job) completionVerdict(res GateResult, reason string) GateResult {
	res.Reason = reason
	if j.CurrentWave >= len(j.Cohorts)-1 {
		res.Verdict = VerdictComplete
		return res
	}
	res.Verdict = VerdictAdvance
	return res
}

// ToCanonical renders the job as the canon.OTAJob the platform's event stream
// carries, so a consumer outside this service reads the shape §2 promises.
func (j *Job) ToCanonical() canon.OTAJob {
	return canon.OTAJob{
		JobID:        j.JobID,
		TenantID:     j.TenantID,
		FromVersion:  string(j.FromVersion),
		ToVersion:    string(j.ToVersion),
		ArtifactURL:  j.ArtifactID,
		ArtifactSize: j.ArtifactSize,
		SHA256:       j.SHA256,
		Signature:    j.Signature,
		Cohorts:      append([]int(nil), j.Cohorts...),
		QuietHours:   j.QuietHours.String(),
		CreatedAt:    j.CreatedAt,
	}
}
