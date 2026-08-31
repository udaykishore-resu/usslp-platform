package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Event names that are internal to the label aggregate's own stream.
//
// canon fixes the names of every event that crosses a service boundary and is
// explicit that they are never renamed. These three never leave the aggregate
// stream — nothing outside the Label Service subscribes to them — so they are
// defined here rather than added to the shared kernel, keeping canon's public
// contract to the events that genuinely are contracts.
const (
	// EvtPriceScheduled records a future-dated price accepted but not yet
	// displayed.
	EvtPriceScheduled = "label.price.scheduled"
	// EvtScheduleCancelled records a scheduled price superseded before its
	// effective time.
	EvtScheduleCancelled = "label.price.schedule_cancelled"
	// EvtLabelRetired records a label permanently removed from service.
	EvtLabelRetired = "device.label.retired"
)

// Event is one immutable fact about a label.
//
// Domain events are plain values with no behaviour beyond naming themselves and
// declaring when they happened. Replay logic lives on the aggregate rather than
// on the event so that adding a field to the aggregate does not require
// touching every historical event type.
type Event interface {
	// EventType is the canonical dotted name under which the event is stored
	// and published.
	EventType() string
	// At is when the fact became true.
	At() time.Time
}

// LabelProvisioned records a label joining the fleet at a placement. It is
// always the first event on a label's stream.
type LabelProvisioned struct {
	LabelID      canon.LabelID  `json:"label_id"`
	TenantID     canon.TenantID `json:"tenant_id"`
	StoreID      canon.StoreID  `json:"store_id"`
	Region       canon.Region   `json:"region"`
	SECID        canon.SECID    `json:"sec_id"`
	Currency     string         `json:"currency"`
	Template     string         `json:"template"`
	Locale       string         `json:"locale,omitempty"`
	OccurredAt   time.Time      `json:"occurred_at"`
	HardwareTier string         `json:"hardware_tier,omitempty"`
}

// EventType implements Event.
func (e LabelProvisioned) EventType() string { return canon.EvtLabelProvisioned }

// At implements Event.
func (e LabelProvisioned) At() time.Time { return e.OccurredAt }

// LabelAssigned records the label being placed against a product. A label with
// no assignment displays nothing and rejects every price.
type LabelAssigned struct {
	LabelID    canon.LabelID `json:"label_id"`
	SKU        canon.SKU     `json:"sku"`
	SECID      canon.SECID   `json:"sec_id"`
	StoreID    canon.StoreID `json:"store_id"`
	Template   string        `json:"template,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e LabelAssigned) EventType() string { return canon.EvtLabelAssigned }

// At implements Event.
func (e LabelAssigned) At() time.Time { return e.OccurredAt }

// PriceApplied is the accepted, attested price for one label: the event the
// edge tier acts on and the compliance archive retains.
type PriceApplied struct {
	LabelID       canon.LabelID     `json:"label_id"`
	StoreID       canon.StoreID     `json:"store_id"`
	SECID         canon.SECID       `json:"sec_id"`
	SKU           canon.SKU         `json:"sku"`
	Price         canon.Money       `json:"price"`
	PreviousPrice *canon.Money      `json:"previous_price,omitempty"`
	WasPrice      *canon.Money      `json:"was_price,omitempty"`
	UnitPrice     *canon.Money      `json:"unit_price,omitempty"`
	UnitMeasure   string            `json:"unit_measure,omitempty"`
	EffectiveAt   time.Time         `json:"effective_at"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	PromotionID   canon.PromotionID `json:"promotion_id,omitempty"`
	// PromotionPriority is the activating rule's priority, recorded so that the
	// audit trail explains which promotion the platform was told to display.
	// The Label Service does not arbitrate between overlapping promotions — see
	// PromotionHandler for why that stays with the Promotion Service — but it
	// does record what it was handed.
	PromotionPriority int `json:"promotion_priority,omitempty"`
	// Category and Brand are the merchandising attributes this price change
	// carried. They are stored on the event rather than only folded into state
	// so that a replay reconstructs the attribute the label was matched on at
	// the time, which is what an auditor asking "why was this line in the dairy
	// promotion" actually needs.
	Category string           `json:"category,omitempty"`
	Brand    string           `json:"brand,omitempty"`
	Render   canon.RenderSpec `json:"render"`
	// Sequence is the per-label monotonic counter the label uses to discard
	// duplicated and reordered mesh frames.
	Sequence int64 `json:"sequence"`
	// Attestation is the detached signature that authorises this exact price
	// for this exact label.
	//
	// It is filled in by the application layer, not by the aggregate: the
	// domain must never hold key material, and an aggregate that could sign
	// would be an aggregate a unit test could be made to sign with. It is
	// nevertheless stored on the label's own stream rather than only on the
	// wire, because clause 4 of the attestation contract requires the signature
	// to be retained for the statutory period, and the label stream is the only
	// record that survives every stream retention policy.
	Attestation canon.Attestation `json:"attestation"`
	// SourceEventID is the price-updates envelope that caused this, so an
	// auditor can walk from a shelf back to a POS webhook.
	SourceEventID canon.EventID `json:"source_event_id,omitempty"`
	InitiatedBy   string        `json:"initiated_by,omitempty"`
	OccurredAt    time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e PriceApplied) EventType() string { return canon.EvtPriceUpdated }

// At implements Event.
func (e PriceApplied) At() time.Time { return e.OccurredAt }

// PriceScheduled records a future-dated price change accepted for later
// activation. Nothing reaches the glass until ScheduledPriceRunner activates it
// at EffectiveAt, and the sequence is deliberately not allocated here: an
// urgent change landing in the meantime must be able to take a lower sequence
// and still win at the label.
type PriceScheduled struct {
	LabelID     canon.LabelID     `json:"label_id"`
	StoreID     canon.StoreID     `json:"store_id"`
	SKU         canon.SKU         `json:"sku"`
	Price       canon.Money       `json:"price"`
	WasPrice    *canon.Money      `json:"was_price,omitempty"`
	UnitPrice   *canon.Money      `json:"unit_price,omitempty"`
	UnitMeasure string            `json:"unit_measure,omitempty"`
	EffectiveAt time.Time         `json:"effective_at"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	ScheduleID  string            `json:"schedule_id"`
	InitiatedBy string            `json:"initiated_by,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

// EventType implements Event.
func (e PriceScheduled) EventType() string { return EvtPriceScheduled }

// At implements Event.
func (e PriceScheduled) At() time.Time { return e.OccurredAt }

// ScheduleCancelled records a scheduled price that will never be displayed,
// because a later decision superseded it.
type ScheduleCancelled struct {
	LabelID    canon.LabelID `json:"label_id"`
	ScheduleID string        `json:"schedule_id"`
	Reason     string        `json:"reason"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e ScheduleCancelled) EventType() string { return EvtScheduleCancelled }

// At implements Event.
func (e ScheduleCancelled) At() time.Time { return e.OccurredAt }

// PriceRejected records a price the platform refused to display, with the
// reason. It is stored on the label's own stream rather than only logged
// because "why is this shelf still showing the old price" is a question asked
// weeks later, by someone who cannot read logs that have since rotated.
type PriceRejected struct {
	LabelID       canon.LabelID     `json:"label_id"`
	StoreID       canon.StoreID     `json:"store_id"`
	SKU           canon.SKU         `json:"sku"`
	RequestedSKU  canon.SKU         `json:"requested_sku,omitempty"`
	Price         canon.Money       `json:"price"`
	CurrentPrice  canon.Money       `json:"current_price"`
	EffectiveAt   time.Time         `json:"effective_at"`
	PromotionID   canon.PromotionID `json:"promotion_id,omitempty"`
	Reason        string            `json:"reason"`
	Detail        string            `json:"detail"`
	SourceEventID canon.EventID     `json:"source_event_id,omitempty"`
	OccurredAt    time.Time         `json:"occurred_at"`
}

// EventType implements Event.
func (e PriceRejected) EventType() string { return canon.EvtPriceRejected }

// At implements Event.
func (e PriceRejected) At() time.Time { return e.OccurredAt }

// DeliveryConfirmed records a label acknowledging an update after its pixels
// settled. It is the event that closes the three-second SLO.
type DeliveryConfirmed struct {
	LabelID     canon.LabelID `json:"label_id"`
	StoreID     canon.StoreID `json:"store_id"`
	SECID       canon.SECID   `json:"sec_id"`
	Sequence    int64         `json:"sequence"`
	DeliveredAt time.Time     `json:"delivered_at"`
	LatencyMS   int64         `json:"latency_ms"`
	MeshHops    int           `json:"mesh_hops"`
	RefreshMS   int           `json:"refresh_ms"`
	Partial     bool          `json:"partial_refresh"`
	OccurredAt  time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e DeliveryConfirmed) EventType() string { return canon.EvtLabelDelivered }

// At implements Event.
func (e DeliveryConfirmed) At() time.Time { return e.OccurredAt }

// DeliveryFailed records a terminal delivery failure after the edge exhausted
// its retries. The label keeps showing the previous price, which is why this is
// a compliance event and not merely an error.
type DeliveryFailed struct {
	LabelID    canon.LabelID `json:"label_id"`
	StoreID    canon.StoreID `json:"store_id"`
	SECID      canon.SECID   `json:"sec_id"`
	Sequence   int64         `json:"sequence"`
	Reason     string        `json:"reason"`
	Attempts   int           `json:"attempts"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e DeliveryFailed) EventType() string { return canon.EvtLabelDeliveryFailed }

// At implements Event.
func (e DeliveryFailed) At() time.Time { return e.OccurredAt }

// LabelWentOffline records the label dropping out of the mesh. The price it is
// showing is still the authorised price; only the ability to change it is lost.
type LabelWentOffline struct {
	LabelID    canon.LabelID `json:"label_id"`
	StoreID    canon.StoreID `json:"store_id"`
	SECID      canon.SECID   `json:"sec_id"`
	Reason     string        `json:"reason,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e LabelWentOffline) EventType() string { return canon.EvtDeviceOffline }

// At implements Event.
func (e LabelWentOffline) At() time.Time { return e.OccurredAt }

// LabelCameOnline records the label rejoining the mesh.
type LabelCameOnline struct {
	LabelID    canon.LabelID `json:"label_id"`
	StoreID    canon.StoreID `json:"store_id"`
	SECID      canon.SECID   `json:"sec_id"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e LabelCameOnline) EventType() string { return canon.EvtDeviceOnline }

// At implements Event.
func (e LabelCameOnline) At() time.Time { return e.OccurredAt }

// LabelRetired records a label permanently removed from service. Its stream is
// kept — a retired label's price history is exactly what a weights-and-measures
// audit asks for — but it accepts no further prices.
type LabelRetired struct {
	LabelID    canon.LabelID `json:"label_id"`
	Reason     string        `json:"reason,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// EventType implements Event.
func (e LabelRetired) EventType() string { return EvtLabelRetired }

// At implements Event.
func (e LabelRetired) At() time.Time { return e.OccurredAt }

// EncodeEvent renders a domain event as the JSON payload of an envelope.
func EncodeEvent(e Event) ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("label: encode %s: %w", e.EventType(), err)
	}
	return body, nil
}

// DecodeEvent rebuilds a domain event from a stored payload.
//
// An unknown event type is an error rather than a skip. A label stream written
// by a newer deployment and replayed by an older one would otherwise rebuild an
// aggregate that silently omits a price change, and the first symptom would be
// a shelf disagreeing with a till.
func DecodeEvent(eventType string, payload []byte) (Event, error) {
	decode := func(dst Event) (Event, error) {
		if err := json.Unmarshal(payload, dst); err != nil {
			return nil, fmt.Errorf("label: decode %s: %w", eventType, err)
		}
		return dst, nil
	}
	switch eventType {
	case canon.EvtLabelProvisioned:
		return decode(&LabelProvisioned{})
	case canon.EvtLabelAssigned:
		return decode(&LabelAssigned{})
	case canon.EvtPriceUpdated:
		return decode(&PriceApplied{})
	case EvtPriceScheduled:
		return decode(&PriceScheduled{})
	case EvtScheduleCancelled:
		return decode(&ScheduleCancelled{})
	case canon.EvtPriceRejected:
		return decode(&PriceRejected{})
	case canon.EvtLabelDelivered:
		return decode(&DeliveryConfirmed{})
	case canon.EvtLabelDeliveryFailed:
		return decode(&DeliveryFailed{})
	case canon.EvtDeviceOffline:
		return decode(&LabelWentOffline{})
	case canon.EvtDeviceOnline:
		return decode(&LabelCameOnline{})
	case EvtLabelRetired:
		return decode(&LabelRetired{})
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownEvent, eventType)
}

// deref normalises the pointer forms DecodeEvent produces back to values, so
// that replay sees one shape per event type regardless of whether the event
// came from a command or from storage.
func deref(e Event) Event {
	switch v := e.(type) {
	case *LabelProvisioned:
		return *v
	case *LabelAssigned:
		return *v
	case *PriceApplied:
		return *v
	case *PriceScheduled:
		return *v
	case *ScheduleCancelled:
		return *v
	case *PriceRejected:
		return *v
	case *DeliveryConfirmed:
		return *v
	case *DeliveryFailed:
		return *v
	case *LabelWentOffline:
		return *v
	case *LabelCameOnline:
		return *v
	case *LabelRetired:
		return *v
	}
	return e
}
