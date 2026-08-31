package domain

import (
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Event names the registry produces that are not already in canon.
//
// canon owns the names every service shares — device.label.provisioned,
// device.label.assigned, device.status.online/offline, device.battery.critical.
// The four below describe lifecycle edges that only the registry knows about,
// and they follow the same dotted convention so a consumer subscribing to
// `device.*` on the `device-events` stream picks them up without a code change.
// They are additive: nothing in the platform is required to understand them,
// and a consumer that does not skips them like any unknown type.
const (
	// EvtDeviceStateChanged is emitted for every accepted lifecycle transition,
	// including the ones that also emit a more specific canon event. A consumer
	// that wants the whole timeline subscribes to this one and nothing else.
	EvtDeviceStateChanged = "device.state.changed"
	// EvtDeviceQuarantined is emitted when the platform stops trusting a
	// device's identity. It is separated from the generic state change because
	// it is the event a SIEM alerts on, and an alert rule should not have to
	// parse a payload field to find its trigger.
	EvtDeviceQuarantined = "device.security.quarantined"
	// EvtDeviceRetired is emitted when a device is decommissioned, which is what
	// tells the Label Service to drop it from the fan-out directory and the OTA
	// service to stop counting it in a cohort denominator.
	EvtDeviceRetired = "device.lifecycle.retired"
	// EvtLabelUnassigned is the counterpart of canon.EvtLabelAssigned: a label
	// that a planogram upload orphaned. Without it the Label Service's directory
	// would only ever grow, and a label moved to another store would keep
	// receiving prices for a SKU it no longer faces.
	EvtLabelUnassigned = "device.label.unassigned"
)

// AggregateDevice is the aggregate type for a device's event stream. It is used
// to build the eventstore stream name and appears in every envelope.
const AggregateDevice = "device"

// AggregatePlanogram is the aggregate type for a store's planogram stream.
const AggregatePlanogram = "planogram"

// AggregateManifest is the aggregate type for a tenant's manufacturing manifest
// stream.
const AggregateManifest = "manifest"

// DeviceStateChanged is the payload of EvtDeviceStateChanged.
//
// It carries the edge (From → To) rather than only the destination, so that a
// consumer replaying `device-events` reconstructs a timeline without having to
// remember every device's previous state itself.
type DeviceStateChanged struct {
	DeviceID  string         `json:"device_id"`
	Kind      DeviceKind     `json:"kind"`
	TenantID  canon.TenantID `json:"tenant_id"`
	StoreID   canon.StoreID  `json:"store_id,omitempty"`
	SECID     canon.SECID    `json:"sec_id,omitempty"`
	From      DeviceState    `json:"from"`
	To        DeviceState    `json:"to"`
	Reason    string         `json:"reason,omitempty"`
	ChangedAt time.Time      `json:"changed_at"`
}

// QuarantineReason classifies why a device's identity stopped being trusted.
// The set is small and closed so that a security dashboard can group by it
// without parsing free text.
type QuarantineReason string

// The reasons the registry quarantines a device.
const (
	// ReasonDuplicateIdentity means the same manufacturing record was presented
	// from two different placements. Either the certificate and key have been
	// cloned or the physical unit was stolen and re-sited; the registry cannot
	// tell which, and both mean the identity is no longer evidence of anything.
	ReasonDuplicateIdentity QuarantineReason = "duplicate-identity"
	// ReasonCertificateRevoked means the device's certificate appears on a CRL.
	ReasonCertificateRevoked QuarantineReason = "certificate-revoked"
	// ReasonManifestMismatch means the certificate authenticated but the device
	// it identifies does not match the manufacturing record: a different EUI-64,
	// a different hardware tier, or a public key the factory never recorded.
	ReasonManifestMismatch QuarantineReason = "manifest-mismatch"
	// ReasonTamperDetected means the device reported its tamper flag. On a
	// battery-powered label that flag is the enclosure being opened, which is
	// the physical precondition for extracting a key.
	ReasonTamperDetected QuarantineReason = "tamper-detected"
	// ReasonOperator means a human quarantined the device.
	ReasonOperator QuarantineReason = "operator"
)

// DeviceQuarantined is the payload of EvtDeviceQuarantined.
//
// It carries enough identity to stand on its own — kind, serial, radio address
// — because the commonest quarantine of all happens to a device the registry
// has never successfully enrolled. An identity that fails its manifest check on
// first contact has no prior record to join against, and an alert that named
// only an identifier nobody can look up would be an alert nobody could act on.
type DeviceQuarantined struct {
	DeviceID string           `json:"device_id"`
	Kind     DeviceKind       `json:"kind,omitempty"`
	Serial   string           `json:"serial,omitempty"`
	EUI64    string           `json:"eui64,omitempty"`
	TenantID canon.TenantID   `json:"tenant_id"`
	StoreID  canon.StoreID    `json:"store_id,omitempty"`
	Reason   QuarantineReason `json:"reason"`
	// Detail is the human-readable specifics: which store the duplicate came
	// from, which field of the manifest disagreed.
	Detail string `json:"detail,omitempty"`
	// ObservedStoreID is the placement the rejected request claimed, present for
	// duplicate-identity so an investigator has both ends of the conflict.
	ObservedStoreID canon.StoreID `json:"observed_store_id,omitempty"`
	At              time.Time     `json:"at"`
}

// DeviceRetired is the payload of EvtDeviceRetired.
type DeviceRetired struct {
	DeviceID string         `json:"device_id"`
	Kind     DeviceKind     `json:"kind"`
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id,omitempty"`
	Serial   string         `json:"serial,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	At       time.Time      `json:"at"`
}

// LabelAssigned is the payload of canon.EvtLabelAssigned and, with the
// Unassigned flag set, of EvtLabelUnassigned.
//
// # This struct is a cross-service contract
//
// The Label Service builds its fan-out directory — the map from (store, SKU) to
// the labels that must be repriced — purely from this event stream. It never
// queries the registry. That is what lets a price update fan out in 120 ms
// without a synchronous lookup, and it is why the fields below are load-bearing
// rather than informational:
//
//   - LabelID, StoreID and SECID are the address. SECID in particular decides
//     which zone topic the update is published to, so a stale one sends a price
//     to a controller that no longer owns the label.
//   - PreviousSECID is set when a reassignment moved the label between
//     controllers. It is what lets the Label Service — and this registry —
//     clear the retained message on the old zone topic. Without it a rebooting
//     controller would replay a price for a label that left its zone.
//   - SKU, Facings and Template are what the Label Service renders. A facing
//     count of two means the same price is drawn on two labels, which is a
//     different fan-out, not a different render.
//   - Sequence is the assignment's monotonic version for this label. A consumer
//     that receives assignments out of order — at-least-once delivery across
//     512 partitions makes that a matter of time — keeps the highest and
//     discards the rest.
//
// The JSON field names are frozen. Renaming one is a breaking change to a
// service the registry does not own.
type LabelAssigned struct {
	LabelID  canon.LabelID  `json:"label_id"`
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	SECID    canon.SECID    `json:"sec_id"`
	// PreviousSECID is the controller the label was on before this assignment,
	// empty when the label did not move between controllers.
	PreviousSECID canon.SECID `json:"previous_sec_id,omitempty"`
	Zone          string      `json:"zone,omitempty"`

	SKU      canon.SKU `json:"sku,omitempty"`
	Facings  int       `json:"facings,omitempty"`
	Template string    `json:"display_template,omitempty"`

	// Shelf, Rail and Position are the planogram coordinates, carried so that a
	// picking or audit tool can reconstruct the shelf without a second call.
	Shelf    string `json:"shelf,omitempty"`
	Rail     string `json:"rail,omitempty"`
	Position int    `json:"position,omitempty"`

	// Unassigned marks the label as removed from the planogram. The rest of the
	// fields then describe what it used to be, so a consumer can undo exactly
	// the directory entry it made.
	Unassigned bool `json:"unassigned,omitempty"`
	// Sequence is monotonic per label across assignments and unassignments.
	Sequence   int64     `json:"sequence"`
	AssignedAt time.Time `json:"assigned_at"`
}

// PlanogramUpdated is the payload of canon.EvtPlanogramUpdated: the summary of
// one bulk upload, published once per upload so that an operator can see the
// blast radius of a merchandising change without diffing two files.
type PlanogramUpdated struct {
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	// Revision is the planogram's monotonic version for the store.
	Revision int64 `json:"revision"`
	Added    int   `json:"added"`
	Moved    int   `json:"moved"`
	Changed  int   `json:"changed"`
	Removed  int   `json:"removed"`
	Orphaned int   `json:"orphaned"`
	// Positions is the total number of positions after the upload.
	Positions int       `json:"positions"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BatteryCritical is the payload of canon.EvtBatteryCritical.
//
// RunwayHours is included because the threshold crossing alone does not tell an
// operations manager anything schedulable: a label at 9% that loses a point a
// month and a label at 9% that loses a point a day are the same alert and very
// different jobs.
type BatteryCritical struct {
	LabelID     canon.LabelID  `json:"label_id"`
	TenantID    canon.TenantID `json:"tenant_id"`
	StoreID     canon.StoreID  `json:"store_id"`
	SECID       canon.SECID    `json:"sec_id,omitempty"`
	Zone        string         `json:"zone,omitempty"`
	BatteryPct  int            `json:"battery_pct"`
	BatteryMV   int            `json:"battery_mv"`
	RunwayHours float64        `json:"runway_hours,omitempty"`
	At          time.Time      `json:"at"`
}

// ProvisionedEventType is the name of the event that announces this device
// joining the fleet: one name per tier, so that a consumer of `device-events`
// routes on the `usslp-event-type` header instead of decoding a payload it may
// have no use for.
func (d *Device) ProvisionedEventType() string {
	return canon.ProvisionedEventFor(string(d.Kind))
}

// ProvisionedEnvelopePayload builds the canon.DeviceProvisioned payload for a
// device. It lives here so that the one place that decides which registry
// fields become the platform-wide provisioning fact is next to the model those
// fields come from.
func (d *Device) ProvisionedEnvelopePayload() canon.DeviceProvisioned {
	return canon.DeviceProvisioned{
		LabelID:       canon.LabelID(d.ID),
		Kind:          string(d.Kind),
		Serial:        d.Serial,
		EUI64:         d.EUI64,
		StoreID:       d.Placement.StoreID,
		SECID:         d.Placement.SECID,
		HardwareTier:  d.HardwareTier,
		FirmwareVer:   d.FirmwareVersion,
		CertSerial:    d.CertSerial,
		CertNotAfter:  d.CertNotAfter,
		ProvisionedAt: d.ProvisionedAt,
	}
}
