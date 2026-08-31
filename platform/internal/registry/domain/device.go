package domain

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// DeviceKind is the tier of hardware a registry entry describes. The registry
// holds all three in one model because they share a lifecycle, a certificate
// hierarchy and a health story; splitting them into three services would mean
// three copies of the state machine below and three chances to disagree about
// what "offline" means.
type DeviceKind string

// The three device tiers USSLP tracks.
const (
	// KindLabel is a Tier 1 E-Ink shelf label: battery powered, Zigbee, the
	// thing a customer actually reads a price from.
	KindLabel DeviceKind = "label"
	// KindSEC is a Tier 2 Shelf Edge Controller: mains powered, Zigbee
	// coordinator for roughly one 8 m shelf section.
	KindSEC DeviceKind = "sec"
	// KindSGU is a Tier 3 Store Gateway Unit: one per store, runs the local
	// MQTT broker and the offline brain.
	KindSGU DeviceKind = "sgu"
)

// Valid reports whether the kind is one of the three the platform ships.
func (k DeviceKind) Valid() bool {
	return k == KindLabel || k == KindSEC || k == KindSGU
}

// String renders the kind.
func (k DeviceKind) String() string { return string(k) }

// DeviceState is where a device sits in its operational lifecycle.
//
// The set is closed and the transitions between members are enumerated in
// [legalTransitions]. Two of the states carry meaning that is easy to conflate
// and must not be: "offline" is a fact about radio contact and reverses itself
// the moment a beacon arrives, whereas "quarantined" is a security decision
// that only an operator can reverse. A device that stops answering is offline;
// a device whose identity the platform no longer trusts is quarantined, and it
// stays that way until a human says otherwise.
type DeviceState string

// The eight lifecycle states.
const (
	// StateManufactured is a device that exists in a manufacturing manifest and
	// has never been seen by the platform. It has a certificate but no store.
	StateManufactured DeviceState = "manufactured"
	// StateProvisioned is a device whose certificate has been authenticated and
	// which has been bound to a tenant, store and controller, but which has no
	// shelf position yet.
	StateProvisioned DeviceState = "provisioned"
	// StateAssigned is a label that a planogram has bound to a SKU at a shelf
	// position. It is the state in which the Label Service will address it.
	StateAssigned DeviceState = "assigned"
	// StateActive is a device that has been heard from inside the heartbeat
	// window. Only an active device counts towards a store's health score.
	StateActive DeviceState = "active"
	// StateDegraded is a device that is still reachable but is failing a health
	// criterion — a weak mesh link, a critical battery, a rising refresh
	// failure rate. It is the state that schedules a truck roll before a
	// customer sees a blank shelf edge.
	StateDegraded DeviceState = "degraded"
	// StateOffline is a device that has missed its beacon budget. It is a
	// derived, self-reversing state, not a judgement.
	StateOffline DeviceState = "offline"
	// StateQuarantined is a device whose identity is no longer trusted:
	// duplicate provisioning, a revoked certificate, a tamper flag. The
	// platform will not address it and will not accept its telemetry until an
	// operator releases it.
	StateQuarantined DeviceState = "quarantined"
	// StateRetired is terminal. A retired device is never re-provisioned under
	// the same identity; a refurbished unit comes back with a new certificate
	// and a new manifest entry.
	StateRetired DeviceState = "retired"
)

// String renders the state.
func (s DeviceState) String() string { return string(s) }

// Terminal reports whether no transition out of the state exists.
func (s DeviceState) Terminal() bool { return s == StateRetired }

// Addressable reports whether the platform may send this device traffic. It is
// the single predicate the Label Service contract and the OTA targeting rules
// both hang on, so that "do not talk to a quarantined device" cannot be
// implemented two different ways.
func (s DeviceState) Addressable() bool {
	switch s {
	case StateAssigned, StateActive, StateDegraded, StateOffline:
		return true
	default:
		return false
	}
}

// legalTransitions is the whole lifecycle. Read it as "from → the set of states
// it may move to".
//
// Three properties are deliberate. Retired is terminal, so a decommissioned
// serial can never be resurrected by a stray provisioning request. Quarantined
// is reachable from everywhere except retired, because a security decision must
// never be blocked by the device's current operational state. And provisioned
// is reachable from quarantined but nothing else is, so releasing a quarantined
// device puts it back at the start of its operational life rather than
// restoring whatever it was doing when it was seized.
var legalTransitions = map[DeviceState]map[DeviceState]bool{
	StateManufactured: {
		StateProvisioned: true, StateQuarantined: true, StateRetired: true,
	},
	StateProvisioned: {
		StateAssigned: true, StateActive: true, StateOffline: true,
		StateQuarantined: true, StateRetired: true,
	},
	StateAssigned: {
		StateActive: true, StateDegraded: true, StateOffline: true,
		StateAssigned: true, StateQuarantined: true, StateRetired: true,
	},
	StateActive: {
		StateAssigned: true, StateDegraded: true, StateOffline: true,
		StateQuarantined: true, StateRetired: true,
	},
	StateDegraded: {
		StateActive: true, StateAssigned: true, StateOffline: true,
		StateQuarantined: true, StateRetired: true,
	},
	StateOffline: {
		StateActive: true, StateAssigned: true, StateDegraded: true,
		StateQuarantined: true, StateRetired: true,
	},
	StateQuarantined: {
		StateProvisioned: true, StateRetired: true,
	},
	StateRetired: {},
}

// CanTransition reports whether from → to is a legal lifecycle move. A
// self-transition is legal only for assigned, where it models a label being
// re-assigned to a different SKU or controller without leaving the state.
func CanTransition(from, to DeviceState) bool {
	return legalTransitions[from][to]
}

// AllStates returns every lifecycle state in lifecycle order. Enumerating it
// rather than hand-listing the states keeps the fleet summary and the admin
// surface from silently omitting one when a state is added.
func AllStates() []DeviceState {
	return []DeviceState{
		StateManufactured, StateProvisioned, StateAssigned, StateActive,
		StateDegraded, StateOffline, StateQuarantined, StateRetired,
	}
}

// Errors the device model returns. They are sentinels because the HTTP surface
// maps each to a different status code and the OTA service branches on them.
var (
	// ErrIllegalTransition means the requested lifecycle move is not in the
	// state machine. It is a 409, never a retry: repeating it will fail
	// identically.
	ErrIllegalTransition = errors.New("registry: illegal lifecycle transition")
	// ErrNotFound means no device, manifest record or planogram exists for the
	// identifier given.
	ErrNotFound = errors.New("registry: not found")
	// ErrAlreadyExists means an identifier that must be unique is already taken.
	ErrAlreadyExists = errors.New("registry: already exists")
	// ErrInvalid means the caller supplied a structurally unusable value.
	ErrInvalid = errors.New("registry: invalid argument")
	// ErrQuarantined means the operation targets a device whose identity the
	// platform has stopped trusting.
	ErrQuarantined = errors.New("registry: device is quarantined")
)

// Placement is where a device physically lives. Zone is the controller's own
// subdivision of its shelf section and is what the MQTT zone topic addresses;
// it is stored separately from the shelf/rail/position of the planogram because
// a label keeps its zone when a merchandiser moves it one rail to the left.
type Placement struct {
	// StoreID is the physical retail location.
	StoreID canon.StoreID `json:"store_id"`
	// SECID is the controller that owns this device's radio. Empty for a SEC or
	// SGU, which own themselves.
	SECID canon.SECID `json:"sec_id,omitempty"`
	// Zone is the controller's addressing subdivision, e.g. "aisle-04-bay-2".
	Zone string `json:"zone,omitempty"`
}

// BatterySample is one observation of a device's battery, retained so that a
// runway can be fitted rather than guessed. Millivolts are kept alongside the
// percentage because a coin cell's percentage is itself a firmware estimate and
// the voltage is the measurement.
type BatterySample struct {
	At         time.Time `json:"at"`
	MilliVolts int       `json:"mv"`
	Percent    int       `json:"pct"`
}

// maxBatterySamples bounds the retained history per device.
//
// Thirty-two samples at the five-minute telemetry cadence is a little under
// three hours, which is enough to fit a slope that is not dominated by the
// temperature swing of a chiller aisle, and small enough that 50 million
// devices' worth of history is a bounded number rather than a data lake. The
// runway is a scheduling input — "replace this aisle on Tuesday" — not a
// forecast anyone should trust to the hour.
const maxBatterySamples = 32

// Device is one entry in the fleet.
//
// It is a read model as much as an aggregate: the fields below are what every
// query in the service answers from, and each is written by exactly one code
// path. Identity fields come from provisioning, placement from provisioning and
// the planogram, and everything under "health" from telemetry.
type Device struct {
	// ID is the platform identifier: a LabelID, SECID or SGUID depending on
	// Kind. It is the aggregate key and the MQTT topic segment.
	ID string `json:"id"`
	// Kind is label, sec or sgu.
	Kind DeviceKind `json:"kind"`
	// TenantID is the isolation boundary the device was issued into.
	TenantID canon.TenantID `json:"tenant_id"`
	// Placement is where the device sits.
	Placement Placement `json:"placement"`

	// Serial is the factory serial printed on the unit and scanned by a
	// technician.
	Serial string `json:"serial"`
	// EUI64 is the IEEE 802.15.4 extended address the Zigbee mesh routes to,
	// rendered as 16 upper-case hex characters.
	EUI64 string `json:"eui64"`
	// HardwareTier names the display and radio generation, e.g. "esl-2.9-bw".
	// It is what an OTA job targets, so a wrong value here bricks a cohort.
	HardwareTier string `json:"hardware_tier"`
	// FirmwareVersion is the last version the device reported.
	FirmwareVersion string `json:"firmware_version,omitempty"`

	// CertSerial is the lower-case hex serial of the device certificate that
	// authenticated the most recent provisioning.
	CertSerial string `json:"cert_serial"`
	// CertNotAfter is that certificate's expiry, kept so the fleet summary can
	// answer "how many labels stop being able to authenticate this quarter".
	CertNotAfter time.Time `json:"cert_not_after,omitempty"`

	// State is the lifecycle state.
	State DeviceState `json:"state"`
	// StateReason records why the last transition happened, in the words the
	// operator will read in the audit stream.
	StateReason string `json:"state_reason,omitempty"`
	// StateChangedAt is when the device entered State.
	StateChangedAt time.Time `json:"state_changed_at"`
	// ProvisionedAt is when the device first joined the fleet.
	ProvisionedAt time.Time `json:"provisioned_at,omitempty"`

	// LastSeen is the most recent moment any evidence of life arrived:
	// telemetry, a heartbeat, a mesh report or an OTA result.
	LastSeen time.Time `json:"last_seen,omitempty"`
	// LastTelemetry is the most recent telemetry report verbatim. Keeping the
	// whole record rather than extracted fields means a new dashboard question
	// does not require a schema change.
	LastTelemetry *canon.Telemetry `json:"last_telemetry,omitempty"`
	// BatteryHistory is the retained sample window used to fit a runway.
	BatteryHistory []BatterySample `json:"battery_history,omitempty"`
	// BatteryCriticalRaised records that device.battery.critical has already
	// been emitted for this battery, so a device sitting at 4% does not emit an
	// alert every five minutes for a month.
	BatteryCriticalRaised bool `json:"battery_critical_raised,omitempty"`

	// Assignment is the planogram binding for a label, nil when unassigned.
	Assignment *Assignment `json:"assignment,omitempty"`
	// AssignmentSequence is the monotonic counter stamped into every
	// LabelAssigned event for this label. It keeps increasing across
	// unassignment, which is what lets a consumer that receives an assignment
	// and its removal out of order tell which one is current — the Assignment
	// field alone cannot, because an unassignment has nothing to compare.
	AssignmentSequence int64 `json:"assignment_sequence,omitempty"`

	// Version is the aggregate version, incremented on every recorded
	// transition. It is what the event store's optimistic concurrency check is
	// made against.
	Version int64 `json:"version"`
}

// Clone returns a deep copy. The registry hands devices out to HTTP handlers
// and projections that run on other goroutines; returning the live struct would
// make every read a data race with the next telemetry batch.
func (d *Device) Clone() *Device {
	if d == nil {
		return nil
	}
	out := *d
	if d.LastTelemetry != nil {
		t := *d.LastTelemetry
		out.LastTelemetry = &t
	}
	if d.Assignment != nil {
		a := *d.Assignment
		out.Assignment = &a
	}
	if len(d.BatteryHistory) > 0 {
		out.BatteryHistory = append([]BatterySample(nil), d.BatteryHistory...)
	}
	return &out
}

// LabelID returns the device identifier as a LabelID; meaningful only for
// KindLabel.
func (d *Device) LabelID() canon.LabelID { return canon.LabelID(d.ID) }

// SECID returns the device identifier as a SECID; meaningful only for KindSEC.
func (d *Device) SECID() canon.SECID { return canon.SECID(d.ID) }

// Transition moves the device to a new state, returning the state-change fact
// the caller must persist and publish.
//
// It is the only way a device's state changes. The returned payload carries the
// previous state as well as the new one, because a consumer rebuilding a fleet
// timeline from `device-events` needs the edge, not the node: "went offline at
// 03:12" is actionable, "was offline at 03:12" is not.
func (d *Device) Transition(to DeviceState, reason string, at time.Time) (DeviceStateChanged, error) {
	if !CanTransition(d.State, to) {
		return DeviceStateChanged{}, fmt.Errorf("%w: %s cannot move from %s to %s",
			ErrIllegalTransition, d.ID, d.State, to)
	}
	from := d.State
	d.State = to
	d.StateReason = reason
	d.StateChangedAt = at.UTC()
	d.Version++
	return DeviceStateChanged{
		DeviceID:  d.ID,
		Kind:      d.Kind,
		TenantID:  d.TenantID,
		StoreID:   d.Placement.StoreID,
		SECID:     d.Placement.SECID,
		From:      from,
		To:        to,
		Reason:    reason,
		ChangedAt: at.UTC(),
	}, nil
}

// RecordBattery appends a battery observation, keeping the retained window
// bounded and ordered.
//
// Samples that arrive out of order — and they do, because a Store Gateway
// buffers telemetry to disk through a WAN outage and replays it — are inserted
// in time order rather than appended, so the runway fit is never computed over
// a series that goes backwards.
func (d *Device) RecordBattery(s BatterySample) {
	if s.At.IsZero() {
		return
	}
	s.At = s.At.UTC()
	n := len(d.BatteryHistory)
	if n > 0 && !s.At.After(d.BatteryHistory[n-1].At) {
		d.BatteryHistory = append(d.BatteryHistory, s)
		sort.SliceStable(d.BatteryHistory, func(i, j int) bool {
			return d.BatteryHistory[i].At.Before(d.BatteryHistory[j].At)
		})
	} else {
		d.BatteryHistory = append(d.BatteryHistory, s)
	}
	if excess := len(d.BatteryHistory) - maxBatterySamples; excess > 0 {
		d.BatteryHistory = append(d.BatteryHistory[:0], d.BatteryHistory[excess:]...)
	}
}

// BatteryPercent returns the latest reported battery percentage and whether one
// has ever been reported.
func (d *Device) BatteryPercent() (int, bool) {
	if n := len(d.BatteryHistory); n > 0 {
		return d.BatteryHistory[n-1].Percent, true
	}
	return 0, false
}

// Validate rejects a device the registry could not address safely. It is
// applied at provisioning, where the values arrive from a manifest file and a
// device announcement, both of which are attacker-adjacent input that ends up
// inside an MQTT topic.
func (d *Device) Validate() error {
	switch {
	case !canon.ValidID(d.ID):
		return fmt.Errorf("%w: device id %q", ErrInvalid, d.ID)
	case !d.Kind.Valid():
		return fmt.Errorf("%w: device kind %q", ErrInvalid, d.Kind)
	case !canon.ValidID(string(d.TenantID)):
		return fmt.Errorf("%w: tenant id %q", ErrInvalid, d.TenantID)
	case d.Placement.StoreID != "" && !canon.ValidID(string(d.Placement.StoreID)):
		return fmt.Errorf("%w: store id %q", ErrInvalid, d.Placement.StoreID)
	case d.Placement.SECID != "" && !canon.ValidID(string(d.Placement.SECID)):
		return fmt.Errorf("%w: sec id %q", ErrInvalid, d.Placement.SECID)
	case d.Serial == "":
		return fmt.Errorf("%w: serial is required", ErrInvalid)
	case d.HardwareTier == "":
		return fmt.Errorf("%w: hardware tier is required", ErrInvalid)
	}
	return nil
}
