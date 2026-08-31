// Package domain is the OTA service's pure model: firmware artifacts and their
// signatures, the binary delta codec, deterministic cohort selection, quiet
// hours, bandwidth budgeting and the staged-rollout state machine.
//
// # The property this package exists to guarantee
//
// A shelf label that does not boot has to be retrieved by hand. There is no
// remote console, no recovery partition anyone can reach, and no way to tell a
// bricked label apart from a flat one without walking to it. At fifty million
// devices, a rollout that bricks one in a thousand is fifty thousand service
// visits. Every design decision below follows from that: signatures verified
// before a job can exist, cohorts that start at one percent, gates that watch
// silence as well as failure, and a rollback that fires without waiting for a
// human to notice.
//
// Nothing here performs I/O. The rollout state machine is a function from
// (job state, cohort tallies, clock) to a verdict, which is what lets a
// four-stage rollout with an automatic rollback be tested in microseconds
// instead of being discovered in a store.
package domain

import (
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Event names the OTA service produces that are not already in canon.
//
// canon owns the four the platform shares — ota.job.created,
// ota.cohort.advanced, ota.device.updated and ota.rollback.triggered. The two
// below describe the rollout's own lifecycle, which nothing outside this
// service has to understand, and follow the same dotted convention so a
// consumer subscribing to `ota.*` picks them up.
const (
	// EvtJobStateChanged records every rollout state transition: paused,
	// resumed, aborted, completed. It is what makes a job's history
	// reconstructable after the fact, which is the first thing anybody asks for
	// after a rollout goes wrong.
	EvtJobStateChanged = "ota.job.state.changed"
	// EvtDeviceDispatched records that a trigger was published to a device.
	// It is separate from ota.device.updated because dispatch and outcome are
	// different facts separated by minutes, and a controller restarting between
	// them must be able to tell which devices are already downloading.
	EvtDeviceDispatched = "ota.device.dispatched"
)

// AggregateJob is the aggregate type for a rollout's event stream.
const AggregateJob = "ota-job"

// JobCreated is the payload of canon.EvtOTAJobCreated.
//
// It carries the whole job specification rather than a reference to one,
// because the rollout's parameters are the thing an auditor asks about after an
// incident — which cohorts, which thresholds, which quiet window — and a
// reference to a mutable record would let those change after the fact.
type JobCreated struct {
	Job *Job `json:"job"`
}

// JobStateChanged is the payload of EvtJobStateChanged.
type JobStateChanged struct {
	JobID  string    `json:"job_id"`
	From   JobState  `json:"from"`
	To     JobState  `json:"to"`
	Reason string    `json:"reason,omitempty"`
	Actor  string    `json:"actor,omitempty"`
	At     time.Time `json:"at"`
}

// CohortAdvanced is the payload of canon.EvtOTACohortAdvanced.
type CohortAdvanced struct {
	JobID string `json:"job_id"`
	// From and To are wave indices; From is -1 when the first wave starts.
	From int `json:"from_wave"`
	To   int `json:"to_wave"`
	// ToPercent is the cumulative fleet percentage the new wave reaches.
	ToPercent int `json:"to_percent"`
	// Metrics is the tally of the cohort that just passed, so the event carries
	// the evidence for the decision and not only the decision.
	Metrics WaveProgress `json:"completed_metrics"`
	Reason  string       `json:"reason,omitempty"`
	At      time.Time    `json:"at"`
}

// DeviceUpdate is the payload of canon.EvtOTADeviceUpdated, in both directions.
//
// The same struct is the trigger published to a device's OTA topic and the
// outcome recorded on the event stream, distinguished by Status. Interface
// contract §3 specifies that the downstream `…/ota` topic carries an
// `Envelope{ota.device.updated}`, so the trigger and the result share an event
// name; sharing the payload type as well means the two can never drift into
// describing the same rollout differently.
type DeviceUpdate struct {
	JobID    string         `json:"job_id"`
	DeviceID string         `json:"device_id"`
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	SECID    canon.SECID    `json:"sec_id,omitempty"`
	Wave     int            `json:"wave"`
	Status   DeviceStatus   `json:"status"`

	FromVersion Version `json:"from_version,omitempty"`
	ToVersion   Version `json:"to_version"`

	// ArtifactID, SHA256 and Signature let the device verify what it downloaded
	// without asking anything else. A label that cannot verify an image does not
	// install it — the same rule as a price it cannot verify.
	ArtifactID string `json:"artifact_id,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Signature  string `json:"signature,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	// Delta, when true, means the payload is a patch against DeltaFromVersion
	// rather than a whole image. DeltaSHA256 is the digest of the patch; SHA256
	// remains the digest of the *reconstructed* image, which is what the device
	// checks after applying it.
	Delta            bool    `json:"delta,omitempty"`
	DeltaFromVersion Version `json:"delta_from_version,omitempty"`
	DeltaSHA256      string  `json:"delta_sha256,omitempty"`
	DeltaSize        int64   `json:"delta_size,omitempty"`

	// Error carries the device's own account of a failure.
	Error string `json:"error,omitempty"`
	// BatteryPctBefore and BatteryPctAfter bracket the update, which is what the
	// battery-drain gate compares.
	BatteryPctBefore int `json:"battery_pct_before,omitempty"`
	BatteryPctAfter  int `json:"battery_pct_after,omitempty"`
	// DurationMS is how long the device took from trigger to running.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Attempt counts retries for this device within the job.
	Attempt int       `json:"attempt,omitempty"`
	At      time.Time `json:"at"`
}

// DeviceDispatchBatch is the payload of EvtDeviceDispatched.
//
// Dispatch is recorded as a batch rather than one event per device on purpose.
// A wave of a 50-million-device fleet is millions of triggers, and the fact
// worth persisting is not "this label was told" but "the controller got this
// far" — enough that a restart resumes rather than re-dispatching a cohort that
// is already downloading. Outcomes stay per device, because an outcome is
// genuinely per-device information that arrives at its own time.
type DeviceDispatchBatch struct {
	JobID   string             `json:"job_id"`
	Wave    int                `json:"wave"`
	Devices []DispatchedDevice `json:"devices"`
	At      time.Time          `json:"at"`
}

// DispatchedDevice is one entry of a dispatch batch.
//
// It carries the device's placement as well as its identifier because the
// placement is what the controller needs before any outcome arrives: releasing
// a controller's download slot requires knowing which controller, and a
// rollout that lost that would keep a saturated mesh saturated until the
// silence window expired. The firmware version the device was running is
// recorded for the same reason a rollback needs it — to know what to put back.
type DispatchedDevice struct {
	DeviceID    string        `json:"device_id"`
	StoreID     canon.StoreID `json:"store_id,omitempty"`
	SECID       canon.SECID   `json:"sec_id,omitempty"`
	FromVersion Version       `json:"from_version,omitempty"`
}

// RollbackTriggered is the payload of canon.EvtOTARolledBack.
//
// It names the measurement, the threshold and the cohort, because the event is
// what pages a human at three in the morning and the first thing they need is
// enough to decide whether this is a bad image or a bad store.
type RollbackTriggered struct {
	JobID string `json:"job_id"`
	Wave  int    `json:"wave"`
	// Reason is the human sentence.
	Reason string `json:"reason"`
	// Metrics is the cohort tally that failed the gate.
	Metrics WaveProgress `json:"metrics"`
	// ToVersion is the version being rolled back from; FromVersion is the one
	// devices are being returned to.
	ToVersion   Version `json:"to_version"`
	FromVersion Version `json:"from_version,omitempty"`
	// Affected is how many devices had already taken the new firmware when the
	// rollback fired, which is the size of the problem.
	Affected int       `json:"affected"`
	At       time.Time `json:"at"`
}
