package canon

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Event envelope
//
// Every state change in USSLP is an immutable, append-only event. Services do
// not call each other to mutate state; they publish an event and let interested
// parties project it. The envelope is what makes that safe at 52,000 price
// updates per second: it carries the identity, ordering, tenancy and trace
// context that every downstream consumer needs without opening the payload.
// ---------------------------------------------------------------------------

// SchemaVersion is the envelope schema version. Consumers must accept any
// envelope whose version they know and skip (not fail on) higher ones, which is
// what makes rolling upgrades across 200 Kafka consumer nodes possible.
const SchemaVersion = 1

// Envelope wraps every event on the bus.
type Envelope struct {
	// EventID is unique and time-sortable.
	EventID EventID `json:"event_id"`
	// EventType is the dotted canonical name, e.g. "label.price.updated".
	EventType string `json:"event_type"`
	// AggregateType and AggregateID identify the event-sourced aggregate whose
	// stream this event belongs to.
	AggregateType string `json:"aggregate_type"`
	AggregateID   string `json:"aggregate_id"`
	// Version is the monotonic per-aggregate version used for optimistic
	// concurrency control on the write side.
	Version int64 `json:"version"`

	TenantID TenantID `json:"tenant_id"`
	StoreID  StoreID  `json:"store_id,omitempty"`
	Region   Region   `json:"region,omitempty"`

	// OccurredAt is when the fact became true in the source system (the POS
	// clock). RecordedAt is when USSLP durably accepted it. They differ during
	// backfills and after a WAN outage, and analytics must never confuse them.
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`

	// TraceID/SpanID are W3C trace context, propagated from the POS webhook all
	// the way to the label ACK so one trace shows the whole 3-second budget.
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	// CorrelationID groups every event caused by one external request.
	// CausationID is the event that directly caused this one.
	CorrelationID CorrelationID `json:"correlation_id,omitempty"`
	CausationID   EventID       `json:"causation_id,omitempty"`

	// Source names the producing component, e.g. "uig/shopify" or
	// "label-service". Audit reviewers ask this question first.
	Source string `json:"source"`
	// SchemaVersion of this envelope.
	SchemaVersion int `json:"schema_version"`
	// IdempotencyKey is the de-duplication key derived at ingress. Two
	// deliveries of the same POS webhook produce the same key.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Payload is the type-specific body.
	Payload json.RawMessage `json:"payload"`
}

// Event names. These strings are a public contract: they appear in Kafka
// payloads, in the audit log retained for 365 days, and in customer-facing
// webhooks. They are only ever added to, never renamed.
const (
	EvtPriceChangeRequested = "pricing.change.requested"
	EvtPriceUpdated         = "label.price.updated"
	EvtPriceRejected        = "label.price.rejected"
	EvtDisplayRendered      = "label.display.rendered"
	EvtLabelDelivered       = "label.update.delivered"
	EvtLabelDeliveryFailed  = "label.update.failed"
	EvtLabelProvisioned     = "device.label.provisioned"
	EvtSECProvisioned       = "device.sec.provisioned"
	EvtSGUProvisioned       = "device.sgu.provisioned"
	EvtLabelAssigned        = "device.label.assigned"
	EvtDeviceOnline         = "device.status.online"
	EvtDeviceOffline        = "device.status.offline"
	EvtDeviceTelemetry      = "device.telemetry.reported"
	EvtBatteryCritical      = "device.battery.critical"
	EvtMeshTopologyChanged  = "mesh.topology.changed"
	EvtMeshLinkDegraded     = "mesh.link.degraded"
	EvtPromotionActivated   = "promotion.activated"
	EvtPromotionExpired     = "promotion.expired"
	EvtPlanogramUpdated     = "planogram.updated"
	EvtOTAJobCreated        = "ota.job.created"
	EvtOTACohortAdvanced    = "ota.cohort.advanced"
	EvtOTARolledBack        = "ota.rollback.triggered"
	EvtOTADeviceUpdated     = "ota.device.updated"
	EvtStoreWentAutonomous  = "store.mode.autonomous"
	EvtStoreReconciled      = "store.mode.reconciled"
	EvtPriceAttested        = "compliance.price.attested"
	EvtInventoryChanged     = "inventory.level.changed"
)

// DeviceKind values carried by DeviceProvisioned.Kind and encoded into the
// provisioning event names above.
//
// The platform enrols three tiers of hardware and every one of them is a fleet
// fact somebody consumes: the Label Service wants labels, the OTA service wants
// controllers and gateways as much as labels, and monitoring wants all three.
// They therefore travel on one stream (`device-events`, interface contract §2)
// under three event names rather than one, so that a consumer routes on the
// `usslp-event-type` header — see eventbus.HeaderEventType — and never has to
// deserialise a payload to discover a record was not addressed to it.
const (
	DeviceKindLabel = "label"
	DeviceKindSEC   = "sec"
	DeviceKindSGU   = "sgu"
)

// ProvisionedEventFor returns the event name that announces a device of this
// kind joining the fleet.
//
// An unrecognised kind maps to EvtLabelProvisioned, because the label event is
// the name that predates the split and is what an older producer emitted for
// everything. A consumer must therefore still check DeviceProvisioned.Kind
// before treating a device.label.provisioned record as a label.
func ProvisionedEventFor(kind string) string {
	switch kind {
	case DeviceKindSEC:
		return EvtSECProvisioned
	case DeviceKindSGU:
		return EvtSGUProvisioned
	default:
		return EvtLabelProvisioned
	}
}

// IsDeviceProvisionedEvent reports whether an event type is a member of the
// provisioning family, whatever tier of hardware it announces. A consumer that
// wants every enrolment — an inventory of the estate, an audit sink — subscribes
// on this rather than on one name.
func IsDeviceProvisionedEvent(eventType string) bool {
	switch eventType {
	case EvtLabelProvisioned, EvtSECProvisioned, EvtSGUProvisioned:
		return true
	}
	return false
}

// ErrEnvelopeInvalid marks a structurally unusable envelope. Such a message is
// routed to the dead-letter topic rather than retried: replaying it will never
// succeed and retrying poisons the consumer group.
var ErrEnvelopeInvalid = errors.New("canon: invalid envelope")

// NewEnvelope builds a valid envelope around a payload, filling in identity and
// timestamps. Callers supply causation explicitly so the lineage of every
// derived event stays reconstructable.
func NewEnvelope(eventType, aggregateType, aggregateID string, tenant TenantID, payload any) (Envelope, error) {
	now := time.Now().UTC()
	env := Envelope{
		EventID:       NewEventID(),
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		TenantID:      tenant,
		OccurredAt:    now,
		RecordedAt:    now,
		SchemaVersion: SchemaVersion,
	}
	if payload == nil {
		return env, nil
	}
	return env.WithPayload(payload)
}

// WithPayload returns a copy of the envelope carrying the marshalled payload.
func (e Envelope) WithPayload(payload any) (Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return e, fmt.Errorf("canon: marshal payload for %s: %w", e.EventType, err)
	}
	e.Payload = body
	return e, nil
}

// Caused returns a child envelope that inherits tenancy, trace and correlation
// from e, with e recorded as the cause. This is how a single POS webhook stays
// traceable through six services and three network tiers.
func (e Envelope) Caused(eventType, aggregateType, aggregateID string, payload any) (Envelope, error) {
	child, err := NewEnvelope(eventType, aggregateType, aggregateID, e.TenantID, nil)
	if err != nil {
		return Envelope{}, err
	}
	child.StoreID = e.StoreID
	child.Region = e.Region
	child.TraceID = e.TraceID
	child.SpanID = NewSpanID()
	child.CorrelationID = e.CorrelationID
	child.CausationID = e.EventID
	child.OccurredAt = e.OccurredAt
	return child.WithPayload(payload)
}

// Validate rejects envelopes that cannot be safely routed or audited.
func (e Envelope) Validate() error {
	switch {
	case e.EventID == "":
		return fmt.Errorf("%w: missing event_id", ErrEnvelopeInvalid)
	case e.EventType == "":
		return fmt.Errorf("%w: missing event_type", ErrEnvelopeInvalid)
	case e.TenantID == "":
		return fmt.Errorf("%w: missing tenant_id", ErrEnvelopeInvalid)
	case !ValidID(string(e.TenantID)):
		return fmt.Errorf("%w: tenant_id %q contains reserved characters", ErrEnvelopeInvalid, e.TenantID)
	case e.StoreID != "" && !ValidID(string(e.StoreID)):
		return fmt.Errorf("%w: store_id %q contains reserved characters", ErrEnvelopeInvalid, e.StoreID)
	case e.OccurredAt.IsZero():
		return fmt.Errorf("%w: missing occurred_at", ErrEnvelopeInvalid)
	case e.SchemaVersion == 0:
		return fmt.Errorf("%w: missing schema_version", ErrEnvelopeInvalid)
	case len(e.Payload) == 0:
		return fmt.Errorf("%w: empty payload", ErrEnvelopeInvalid)
	}
	return nil
}

// Decode unmarshals the payload into dst.
func (e Envelope) Decode(dst any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("%w: empty payload for %s", ErrEnvelopeInvalid, e.EventType)
	}
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("canon: decode %s payload: %w", e.EventType, err)
	}
	return nil
}

// PartitionKey returns the key that determines Kafka partition assignment.
// Ordering in USSLP is guaranteed per (store, SKU): two price changes for the
// same product in the same store must never be applied out of order, while
// changes to different products are free to be processed in parallel across
// 1024 partitions.
func (e Envelope) PartitionKey() string {
	var pk PriceKeyed
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &pk); err == nil && pk.SKU != "" {
			return string(e.StoreID) + ":" + string(pk.SKU)
		}
	}
	if e.AggregateID != "" {
		return e.AggregateID
	}
	return string(e.TenantID)
}

// PriceKeyed is the minimal shape needed to compute a partition key without
// fully decoding a payload.
type PriceKeyed struct {
	SKU SKU `json:"sku"`
}

// ---------------------------------------------------------------------------
// Payload types
// ---------------------------------------------------------------------------

// PriceChangeRequested is the canonical form every POS adapter normalises to.
// Whatever NCR, SAP, Shopify or a nightly CSV drop actually sent, the rest of
// the platform only ever sees this.
type PriceChangeRequested struct {
	SKU          SKU               `json:"sku"`
	StoreID      StoreID           `json:"store_id"`
	Price        Money             `json:"price"`
	WasPrice     *Money            `json:"was_price,omitempty"`
	UnitPrice    *Money            `json:"unit_price,omitempty"`
	UnitMeasure  string            `json:"unit_measure,omitempty"`
	EffectiveAt  time.Time         `json:"effective_at"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	PromotionID  PromotionID       `json:"promotion_id,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	InitiatedBy  string            `json:"initiated_by"`
	SourceSystem string            `json:"source_system"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// Validate enforces the invariants that make a price change safe to display.
func (p PriceChangeRequested) Validate() error {
	switch {
	case p.SKU == "":
		return errors.New("price change: missing sku")
	case p.StoreID == "":
		return errors.New("price change: missing store_id")
	case !p.Price.Valid():
		return fmt.Errorf("price change: invalid currency %q", p.Price.Currency)
	case p.Price.Amount < 0:
		return errors.New("price change: negative price")
	case p.EffectiveAt.IsZero():
		return errors.New("price change: missing effective_at")
	case p.WasPrice != nil && p.WasPrice.Currency != p.Price.Currency:
		return ErrCurrencyMismatch
	case p.ExpiresAt != nil && !p.ExpiresAt.After(p.EffectiveAt):
		return errors.New("price change: expires_at must be after effective_at")
	}
	return nil
}

// PriceUpdated is the accepted, attested price for one label. It is the event
// the edge tier acts on and the compliance archive retains for seven years.
type PriceUpdated struct {
	LabelID     LabelID     `json:"label_id"`
	SKU         SKU         `json:"sku"`
	StoreID     StoreID     `json:"store_id"`
	Price       Money       `json:"price"`
	WasPrice    *Money      `json:"was_price,omitempty"`
	UnitPrice   *Money      `json:"unit_price,omitempty"`
	UnitMeasure string      `json:"unit_measure,omitempty"`
	EffectiveAt time.Time   `json:"effective_at"`
	PromotionID PromotionID `json:"promotion_id,omitempty"`
	Render      RenderSpec  `json:"render"`
	// Attestation proves this exact price for this exact label was authorised
	// by the platform. The SEC verifies it before driving the display.
	Attestation Attestation `json:"attestation"`
	// Sequence is the per-label monotonic counter. The label discards any
	// update whose sequence is not greater than the one it is showing, which is
	// what makes at-least-once mesh delivery safe.
	Sequence int64 `json:"sequence"`
}

// RenderSpec tells the label how to draw, without the cloud needing to know the
// pixel geometry of every display tier. The SEC's zone rendering engine turns
// this into a framebuffer.
type RenderSpec struct {
	Template  string            `json:"template"`            // "standard", "promo", "unit_price", "clearance"
	Badge     string            `json:"badge,omitempty"`     // "SALE", "NEW", "2 FOR £3"
	LEDColor  string            `json:"led_color,omitempty"` // "RED","GREEN","BLUE","AMBER","OFF"
	Animation string            `json:"animation,omitempty"` // "NONE","PULSE_BORDER","FLASH"
	ShowWas   bool              `json:"show_was_price"`
	Locale    string            `json:"locale,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	// PartialRefresh asks the label for a 0.3s partial waveform instead of a
	// 1.5s full refresh. Only safe when the changed region is small; a full
	// refresh is forced periodically to clear E-Ink ghosting.
	PartialRefresh bool `json:"partial_refresh"`
}

// Attestation is a detached signature over the canonical price digest.
type Attestation struct {
	Algorithm string    `json:"alg"`    // "Ed25519"
	KeyID     string    `json:"kid"`    // signing key identifier
	Digest    string    `json:"digest"` // hex SHA-256 of the canonical string
	Signature string    `json:"sig"`    // base64 raw signature
	SignedAt  time.Time `json:"signed_at"`
}

// LabelDelivered is emitted when a label has acknowledged an update and the
// pixels have actually changed. This is the event that closes the SLO: latency
// is measured from PriceChangeRequested.RecordedAt to this event.
type LabelDelivered struct {
	LabelID     LabelID   `json:"label_id"`
	StoreID     StoreID   `json:"store_id"`
	SECID       SECID     `json:"sec_id"`
	Sequence    int64     `json:"sequence"`
	DeliveredAt time.Time `json:"delivered_at"`
	// LatencyMS is measured end to end, POS ingress to confirmed display.
	LatencyMS int64 `json:"latency_ms"`
	// MeshHops is how many Zigbee hops the update traversed.
	MeshHops int `json:"mesh_hops"`
	// RefreshMS is how long the E-Ink waveform took.
	RefreshMS int  `json:"refresh_ms"`
	Partial   bool `json:"partial_refresh"`
}

// LabelDeliveryFailed records a terminal delivery failure after retries.
type LabelDeliveryFailed struct {
	LabelID  LabelID `json:"label_id"`
	StoreID  StoreID `json:"store_id"`
	SECID    SECID   `json:"sec_id"`
	Sequence int64   `json:"sequence"`
	Reason   string  `json:"reason"`
	Attempts int     `json:"attempts"`
}

// Telemetry is the periodic device health report. At 50M labels reporting every
// five minutes this is ~167,000 events per second, which is why it travels on
// its own topic and its own Kafka cluster.
type Telemetry struct {
	LabelID       LabelID   `json:"label_id"`
	StoreID       StoreID   `json:"store_id"`
	SECID         SECID     `json:"sec_id"`
	ReportedAt    time.Time `json:"reported_at"`
	BatteryMV     int       `json:"battery_mv"`
	BatteryPct    int       `json:"battery_pct"`
	TemperatureC  float64   `json:"temperature_c"`
	RSSI          int       `json:"rssi"`
	LQI           int       `json:"lqi"`
	MeshHops      int       `json:"mesh_hops"`
	ParentID      LabelID   `json:"parent_id,omitempty"`
	FirmwareVer   string    `json:"firmware_version"`
	RefreshCount  int64     `json:"refresh_count"`
	NFCTapCount   int64     `json:"nfc_tap_count"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	TamperFlag    bool      `json:"tamper"`
}

// DeviceProvisioned records a device joining the fleet.
//
// It is named for the label because the label is the tier every other service
// cares about, but all three tiers are enrolled through it and Kind is what
// says which one this is. Kind is also encoded in the event name — see
// ProvisionedEventFor — so a consumer routes on the envelope's type without
// decoding this struct at all; the field exists for the consumer that has
// decoded the payload anyway and wants to state its assumption rather than
// infer it from which fields happen to be populated.
//
// SECID is the parent controller and is meaningful for a label only. A
// controller is its own radio authority and a gateway has no radio, so both
// carry an empty SECID: a consumer that requires one must first check Kind.
type DeviceProvisioned struct {
	LabelID LabelID `json:"label_id"`
	// Kind is "label", "sec" or "sgu". It is omitted by producers older than
	// the event-name split, which emitted device.label.provisioned for every
	// tier; an empty Kind on a device.label.provisioned record therefore means
	// "unstated", not "label".
	Kind          string    `json:"kind,omitempty"`
	Serial        string    `json:"serial"`
	EUI64         string    `json:"eui64"`
	StoreID       StoreID   `json:"store_id"`
	SECID         SECID     `json:"sec_id"`
	HardwareTier  string    `json:"hardware_tier"`
	FirmwareVer   string    `json:"firmware_version"`
	CertSerial    string    `json:"cert_serial"`
	CertNotAfter  time.Time `json:"cert_not_after"`
	ProvisionedAt time.Time `json:"provisioned_at"`
}

// MeshTopology is the SEC's periodic view of its Zigbee network. The cloud uses
// it for the store health map and to predict link failures before they happen.
type MeshTopology struct {
	SECID     SECID      `json:"sec_id"`
	StoreID   StoreID    `json:"store_id"`
	Nodes     []MeshNode `json:"nodes"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// MeshNode is one label's position in the mesh.
type MeshNode struct {
	LabelID  LabelID `json:"label_id"`
	ParentID LabelID `json:"parent_id,omitempty"`
	Depth    int     `json:"depth"`
	LQI      int     `json:"lqi"`
	RSSI     int     `json:"rssi"`
	Router   bool    `json:"router"`
	Online   bool    `json:"online"`
	// FailureRisk is the routing intelligence module's predicted probability
	// that this link degrades below threshold in the next five minutes.
	FailureRisk float64 `json:"failure_risk"`
}

// StoreModeChanged records a store entering or leaving autonomous operation.
type StoreModeChanged struct {
	StoreID       StoreID   `json:"store_id"`
	SGUID         SGUID     `json:"sgu_id"`
	Mode          string    `json:"mode"` // "autonomous" | "connected"
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
	QueuedUpdates int       `json:"queued_updates,omitempty"`
	Conflicts     int       `json:"conflicts_resolved,omitempty"`
	OutageSeconds int64     `json:"outage_seconds,omitempty"`
}

// OTAJob describes a staged firmware rollout.
type OTAJob struct {
	JobID        string    `json:"job_id"`
	TenantID     TenantID  `json:"tenant_id"`
	FromVersion  string    `json:"from_version"`
	ToVersion    string    `json:"to_version"`
	ArtifactURL  string    `json:"artifact_url"`
	ArtifactSize int64     `json:"artifact_size"`
	SHA256       string    `json:"sha256"`
	Signature    string    `json:"signature"`
	Cohorts      []int     `json:"cohort_percentages"`
	QuietHours   string    `json:"quiet_hours"`
	CreatedAt    time.Time `json:"created_at"`
}
