// Package domain is the Label aggregate: the pure, infrastructure-free core of
// the Label Service.
//
// Everything a price change is allowed to depend on lives here — the label's
// placement, its assignment, the price on its glass, its monotonic sequence,
// and the rules that decide whether a proposed price reaches the glass at all.
// Nothing here opens a socket, reads a clock it was not given, or knows that an
// event store exists. That is not architectural neatness for its own sake: the
// rules in this package are the ones a regulator will ask about, and a rule
// that can only be exercised by standing up Kafka is a rule nobody re-verifies
// after the first release.
//
// The aggregate is event sourced. Commands are pure functions from
// (state, command, policy) to (events, error); state is rebuilt by folding
// events. A rejection is itself an event, because "why does this shelf still
// show the old price" is asked weeks later by someone who cannot read rotated
// logs.
//
// A Label value is not safe for concurrent use. Concurrency is handled one
// level up, by the event store's optimistic concurrency check: two commands
// racing on one label both load, both decide, and exactly one append lands.
package domain

import (
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// AggregateType is the event-store stream prefix for label streams.
const AggregateType = "label"

// State is the label's lifecycle position.
//
// The transitions are unprovisioned → assigned → active ⇄ offline → retired.
// Offline is not a terminal state and does not stop price changes: a label out
// of the mesh still has an authorised price, the update is still attested and
// still published retained, and the local broker delivers it the moment the
// label comes back. Refusing to price an offline label would mean a store
// coming back from a mesh fault shows yesterday's prices.
type State string

// The label lifecycle states.
const (
	// StateUnprovisioned is a label that exists only as an identifier.
	StateUnprovisioned State = "unprovisioned"
	// StateAssigned is provisioned and placed against a SKU, but has never
	// displayed a price.
	StateAssigned State = "assigned"
	// StateActive is displaying an authorised price.
	StateActive State = "active"
	// StateOffline is displaying an authorised price but is not reachable.
	StateOffline State = "offline"
	// StateRetired is permanently out of service.
	StateRetired State = "retired"
)

// Delivery is the last confirmed display of an update.
type Delivery struct {
	// Sequence is the update the label confirmed.
	Sequence int64 `json:"sequence"`
	// At is when the pixels settled.
	At time.Time `json:"at"`
	// LatencyMS is the end-to-end latency this delivery achieved, measured from
	// the moment USSLP took durable responsibility to the moment the pixels
	// settled. It is the number the SLO is written against.
	LatencyMS int64 `json:"latency_ms"`
	// MeshHops is how many Zigbee hops the update traversed.
	MeshHops int `json:"mesh_hops"`
	// RefreshMS is how long the E-Ink waveform took.
	RefreshMS int `json:"refresh_ms"`
	// Partial reports whether a partial waveform was used.
	Partial bool `json:"partial_refresh"`
}

// PendingUpdate is an update published to the device but not yet confirmed.
type PendingUpdate struct {
	// Sequence is the update's per-label sequence.
	Sequence int64 `json:"sequence"`
	// Price is what the label was told to display.
	Price canon.Money `json:"price"`
	// SKU is the product the price belongs to.
	SKU canon.SKU `json:"sku"`
	// EffectiveAt is when the price takes effect.
	EffectiveAt time.Time `json:"effective_at"`
	// PublishedAt is when the Label Service authorised it. The gap between
	// this and the confirmation is the latency the SLO measures.
	PublishedAt time.Time `json:"published_at"`
	// Render is the spec the label was given.
	Render canon.RenderSpec `json:"render"`
	// SourceEventID links back to the price-updates envelope.
	SourceEventID canon.EventID `json:"source_event_id,omitempty"`
}

// ScheduledUpdate is a future-dated price waiting for its effective time.
type ScheduledUpdate struct {
	// ScheduleID identifies the scheduled change, so a supersession can name
	// exactly which one it cancelled.
	ScheduleID string `json:"schedule_id"`
	// SKU, Price and the optional comparison prices are the change itself.
	SKU         canon.SKU         `json:"sku"`
	Price       canon.Money       `json:"price"`
	WasPrice    *canon.Money      `json:"was_price,omitempty"`
	UnitPrice   *canon.Money      `json:"unit_price,omitempty"`
	UnitMeasure string            `json:"unit_measure,omitempty"`
	EffectiveAt time.Time         `json:"effective_at"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	InitiatedBy string            `json:"initiated_by,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Label is the aggregate.
//
// Every field is exported and JSON-serialisable because the whole value is what
// gets written as an event-store snapshot; a snapshot with a private field
// would silently rebuild an incomplete aggregate.
type Label struct {
	// ID identifies the label. It is also the event-store stream identifier.
	ID canon.LabelID `json:"id"`
	// TenantID, StoreID, Region and SECID are the placement. The SEC is part of
	// the aggregate rather than looked up at publish time because the MQTT
	// topic routes through the controller that owns the label, and a topic
	// built from a stale directory entry would deliver a price into a zone that
	// discards it.
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	Region   canon.Region   `json:"region"`
	SECID    canon.SECID    `json:"sec_id"`
	// Currency is the store's configured trading currency. A price in any other
	// currency is refused; a label cannot show two.
	Currency string `json:"currency"`
	// SKU is the product currently assigned to this label.
	SKU canon.SKU `json:"sku,omitempty"`
	// Price is what the glass is showing, once State is active.
	Price canon.Money `json:"price"`
	// BasePrice is the last non-promotional price: the everyday price a
	// promotion discounts from and the price an expiry reverts to.
	//
	// It is tracked separately from PreviousPrice because the two answer
	// different questions. PreviousPrice is "what was on the glass a moment
	// ago", which after two consecutive promotions is another promotional
	// price; BasePrice is "what this product costs when nothing is running",
	// which is the only price an expiring promotion can safely fall back to.
	// Without it, a promotion ending would restore whatever the previous
	// promotion charged and the shelf would stay discounted forever.
	BasePrice canon.Money `json:"base_price"`
	// PreviousPrice is what it showed before, retained for the was/now claim.
	PreviousPrice *canon.Money `json:"previous_price,omitempty"`
	// Sequence is the per-label monotonic counter. The label discards any
	// update whose sequence does not exceed the one it is displaying, which is
	// what makes at-least-once mesh delivery safe.
	Sequence int64 `json:"sequence"`
	// Render is the spec currently on the glass.
	Render canon.RenderSpec `json:"render"`
	// PartialsSinceFull counts consecutive partial refreshes, and is what
	// forces a periodic ghost-clearing full waveform.
	PartialsSinceFull int `json:"partials_since_full"`
	// State is the lifecycle position.
	State State `json:"state"`
	// PriceOccurredAt is the source-system clock of the displayed price. It is
	// the ordering reference for out-of-order detection, because a POS batch
	// carries no sequence of its own.
	PriceOccurredAt time.Time `json:"price_occurred_at"`
	// EffectiveAt is when the displayed price took effect.
	EffectiveAt time.Time `json:"effective_at"`
	// ExpiresAt, when set, is when the displayed promotional price lapses.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// PromotionID is the promotion behind the displayed price, if any.
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	// Category and Brand are the merchandising attributes the label has learned
	// from the price changes it has been given.
	//
	// They are held here rather than fetched because a promotion scoped to a
	// category has to resolve its label set without a synchronous call to a
	// catalogue service: a national activation would otherwise become two
	// thousand stores' worth of simultaneous lookups, which is precisely the
	// fan-in the event-driven design exists to avoid. They are only ever as
	// good as the last price change that carried them, which is why a rule
	// constrained on an attribute no label has recorded resolves to nothing
	// rather than to everything.
	Category string `json:"category,omitempty"`
	Brand    string `json:"brand,omitempty"`
	// Pending is the published-but-unconfirmed update, if any.
	Pending *PendingUpdate `json:"pending,omitempty"`
	// LastDelivery is the most recent confirmed display.
	LastDelivery *Delivery `json:"last_delivery,omitempty"`
	// LastFailure describes the most recent terminal delivery failure.
	LastFailure *DeliveryFailure `json:"last_failure,omitempty"`
	// Scheduled holds accepted future-dated changes, ordered by effective time.
	Scheduled []ScheduledUpdate `json:"scheduled,omitempty"`
	// Version is the event-store stream version this state reflects. It is what
	// the caller passes back as expectedVersion on append.
	Version int64 `json:"version"`
	// UpdatedAt is when the aggregate last changed.
	UpdatedAt time.Time `json:"updated_at"`
	// RejectedCount is a lifetime counter of refused price changes, surfaced on
	// the store roster so a persistently mis-fed SKU is visible without a log
	// search.
	RejectedCount int64 `json:"rejected_count"`
}

// DeliveryFailure records the last terminal delivery failure.
type DeliveryFailure struct {
	// Sequence is the update that never landed.
	Sequence int64 `json:"sequence"`
	// Reason is the edge's explanation.
	Reason string `json:"reason"`
	// Attempts is how many times the edge tried.
	Attempts int `json:"attempts"`
	// At is when the edge gave up.
	At time.Time `json:"at"`
}

// New returns an empty aggregate for an identifier. It is the starting point
// for replay and for provisioning.
func New(id canon.LabelID) *Label {
	return &Label{ID: id, State: StateUnprovisioned}
}

// StreamID is the event-store stream name for a label.
func StreamID(id canon.LabelID) string { return AggregateType + "/" + string(id) }

// Scope returns the MQTT topic scope for the label's placement.
func (l *Label) Scope() canon.TopicScope {
	return canon.TopicScope{Tenant: l.TenantID, Region: l.Region, Store: l.StoreID}
}

// Exists reports whether the label has been provisioned.
func (l *Label) Exists() bool { return l.State != StateUnprovisioned }

// Displaying reports whether the label has a price on the glass.
func (l *Label) Displaying() bool {
	return l.State == StateActive || l.State == StateOffline
}

// Replay folds a sequence of stored events onto the aggregate, advancing
// Version once per event.
func (l *Label) Replay(events ...Event) error {
	for _, e := range events {
		if e == nil {
			return fmt.Errorf("%w: nil event during replay of %s", ErrUnknownEvent, l.ID)
		}
		l.Apply(e)
		l.Version++
	}
	return nil
}

// Apply folds one event onto the aggregate without touching Version. Command
// handlers use it to reflect the events they just decided; Replay uses it with
// the version bookkeeping.
func (l *Label) Apply(e Event) {
	switch ev := deref(e).(type) {
	case LabelProvisioned:
		l.ID = ev.LabelID
		l.TenantID = ev.TenantID
		l.StoreID = ev.StoreID
		l.Region = ev.Region
		l.SECID = ev.SECID
		l.Currency = ev.Currency
		l.Render.Template = ev.Template
		l.Render.Locale = ev.Locale
		if l.State == StateUnprovisioned {
			l.State = StateAssigned
		}
	case LabelAssigned:
		reassigned := l.SKU != ev.SKU
		l.SKU = ev.SKU
		if ev.SECID != "" {
			l.SECID = ev.SECID
		}
		if ev.StoreID != "" {
			l.StoreID = ev.StoreID
		}
		if ev.Template != "" {
			l.Render.Template = ev.Template
		}
		// Reassignment invalidates the price on the glass: the label is now
		// against a different product, and the old price is no longer a claim
		// about anything. State drops back to assigned so the guard rail does
		// not compare the new product's price against the old product's.
		if l.State == StateActive || l.State == StateOffline {
			l.State = StateAssigned
		}
		l.PreviousPrice = nil
		l.Pending = nil
		if reassigned {
			// A different product has a different everyday price and different
			// merchandising attributes. Keeping either would let a promotion
			// scoped to the old product's category catch the new one, and would
			// let an expiry revert the shelf to a price for something that is
			// no longer on it.
			l.BasePrice = canon.Money{}
			l.Category, l.Brand = "", ""
		}
	case PriceApplied:
		l.applyPrice(ev)
	case PriceScheduled:
		l.addSchedule(ev)
	case ScheduleCancelled:
		l.removeSchedule(ev.ScheduleID)
	case PriceRejected:
		l.RejectedCount++
	case DeliveryConfirmed:
		d := Delivery{
			Sequence: ev.Sequence, At: ev.DeliveredAt, LatencyMS: ev.LatencyMS,
			MeshHops: ev.MeshHops, RefreshMS: ev.RefreshMS, Partial: ev.Partial,
		}
		l.LastDelivery = &d
		if l.Pending != nil && l.Pending.Sequence <= ev.Sequence {
			l.Pending = nil
		}
		if ev.SECID != "" {
			l.SECID = ev.SECID
		}
		// A confirmation is proof of reachability, so it is also the fastest
		// signal that a label previously marked offline is back.
		if l.State == StateOffline {
			l.State = StateActive
		}
		l.LastFailure = nil
	case DeliveryFailed:
		f := DeliveryFailure{Sequence: ev.Sequence, Reason: ev.Reason, Attempts: ev.Attempts, At: ev.OccurredAt}
		l.LastFailure = &f
		if l.Pending != nil && l.Pending.Sequence == ev.Sequence {
			l.Pending = nil
		}
	case LabelWentOffline:
		if l.State == StateActive || l.State == StateAssigned {
			l.State = StateOffline
		}
	case LabelCameOnline:
		if l.State == StateOffline {
			if l.Displaying() || l.Sequence > 0 {
				l.State = StateActive
			} else {
				l.State = StateAssigned
			}
		}
	case LabelRetired:
		l.State = StateRetired
		l.Pending = nil
		l.Scheduled = nil
	}
	if at := e.At(); at.After(l.UpdatedAt) {
		l.UpdatedAt = at
	}
}

func (l *Label) applyPrice(ev PriceApplied) {
	if l.Displaying() {
		prev := l.Price
		l.PreviousPrice = &prev
	}
	// A price carrying no promotion *is* the everyday price, so it becomes the
	// base. A promotional price leaves the base alone — that is the whole point
	// of it — and seeds it from what the glass was showing if this is the first
	// price the label has ever had, so that an expiry has somewhere to fall
	// back to even when a promotion was the label's first price.
	if ev.PromotionID == "" {
		l.BasePrice = ev.Price
	} else if l.BasePrice.Amount == 0 && l.BasePrice.Currency == "" {
		if l.Displaying() {
			l.BasePrice = l.Price
		} else if ev.PreviousPrice != nil {
			l.BasePrice = *ev.PreviousPrice
		}
	}
	if ev.Category != "" {
		l.Category = ev.Category
	}
	if ev.Brand != "" {
		l.Brand = ev.Brand
	}
	l.SKU = ev.SKU
	l.Price = ev.Price
	l.Sequence = ev.Sequence
	l.EffectiveAt = ev.EffectiveAt
	l.ExpiresAt = ev.ExpiresAt
	l.PromotionID = ev.PromotionID
	l.PriceOccurredAt = ev.OccurredAt
	if ev.SECID != "" {
		l.SECID = ev.SECID
	}
	if ev.Render.PartialRefresh {
		l.PartialsSinceFull++
	} else {
		l.PartialsSinceFull = 0
	}
	l.Render = ev.Render
	l.Pending = &PendingUpdate{
		Sequence: ev.Sequence, Price: ev.Price, SKU: ev.SKU,
		EffectiveAt: ev.EffectiveAt, PublishedAt: ev.OccurredAt,
		Render: ev.Render, SourceEventID: ev.SourceEventID,
	}
	if l.State != StateOffline && l.State != StateRetired {
		l.State = StateActive
	}
	// A price that has been applied supersedes anything scheduled for the same
	// SKU at or before its effective time; the schedule is folded away rather
	// than left to fire later and roll the shelf backwards.
	l.dropSchedulesUpTo(ev.SKU, ev.EffectiveAt)
}

func (l *Label) addSchedule(ev PriceScheduled) {
	s := ScheduledUpdate{
		ScheduleID: ev.ScheduleID, SKU: ev.SKU, Price: ev.Price,
		WasPrice: ev.WasPrice, UnitPrice: ev.UnitPrice, UnitMeasure: ev.UnitMeasure,
		EffectiveAt: ev.EffectiveAt, ExpiresAt: ev.ExpiresAt,
		PromotionID: ev.PromotionID, Reason: ev.Reason, InitiatedBy: ev.InitiatedBy,
	}
	out := make([]ScheduledUpdate, 0, len(l.Scheduled)+1)
	inserted := false
	for _, existing := range l.Scheduled {
		if !inserted && s.EffectiveAt.Before(existing.EffectiveAt) {
			out = append(out, s)
			inserted = true
		}
		out = append(out, existing)
	}
	if !inserted {
		out = append(out, s)
	}
	l.Scheduled = out
}

func (l *Label) removeSchedule(id string) {
	if len(l.Scheduled) == 0 {
		return
	}
	out := l.Scheduled[:0]
	for _, s := range l.Scheduled {
		if s.ScheduleID != id {
			out = append(out, s)
		}
	}
	l.Scheduled = append([]ScheduledUpdate(nil), out...)
}

func (l *Label) dropSchedulesUpTo(sku canon.SKU, at time.Time) {
	if len(l.Scheduled) == 0 {
		return
	}
	out := make([]ScheduledUpdate, 0, len(l.Scheduled))
	for _, s := range l.Scheduled {
		if s.SKU == sku && !s.EffectiveAt.After(at) {
			continue
		}
		out = append(out, s)
	}
	l.Scheduled = out
}

// Schedule returns the scheduled update with an identifier, if it is still
// pending.
func (l *Label) Schedule(id string) (ScheduledUpdate, bool) {
	for _, s := range l.Scheduled {
		if s.ScheduleID == id {
			return s, true
		}
	}
	return ScheduledUpdate{}, false
}

// DueSchedules returns the scheduled updates whose effective time has arrived.
func (l *Label) DueSchedules(now time.Time) []ScheduledUpdate {
	var out []ScheduledUpdate
	for _, s := range l.Scheduled {
		if !s.EffectiveAt.After(now) {
			out = append(out, s)
		}
	}
	return out
}
