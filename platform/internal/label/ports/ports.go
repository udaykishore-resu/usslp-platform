// Package ports declares everything the Label Service's use cases need from the
// world outside them.
//
// These are the seams of the hexagon. The application layer is written against
// this package alone; every implementation — the event store, the event bus,
// the MQTT broker, the key/value read models — lives in `adapters` and is
// injected at wiring time. Two consequences justify the indirection:
//
//   - The use cases are testable without a broker. A handler test constructs
//     fakes for these six interfaces and exercises the whole price path in
//     microseconds, which is the only reason the invariant suite can afford to
//     be exhaustive.
//   - The production transports are replaceable. `pkg/eventlog` and Kafka both
//     satisfy the bus; the in-tree MQTT broker and EMQX both satisfy the device
//     publisher. Nothing above this line knows which is running.
//
// Errors declared here are the vocabulary the application layer branches on.
// Adapters translate their own sentinels (eventstore.ErrConcurrency,
// kvstore.ErrNotFound) into these, so a use case never imports infrastructure
// just to check an error.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Errors the application layer branches on.
var (
	// ErrNotFound reports an absent aggregate, placement or read-model row.
	ErrNotFound = errors.New("label/ports: not found")
	// ErrConcurrency reports that an aggregate moved on between load and
	// append. The caller must reload and re-decide; retrying the same decision
	// would apply it against state that no longer exists.
	ErrConcurrency = errors.New("label/ports: concurrency conflict")
	// ErrRateLimited reports a tenant over its fan-out budget. It is a
	// backpressure signal, not a failure: the caller waits or sheds.
	ErrRateLimited = errors.New("label/ports: tenant rate limit exceeded")
)

// Clock is the only source of time the application layer uses.
//
// Injecting it is not a testing convenience. Every temporal rule in the domain
// — the effective-at grace window, the scheduling horizon, the SLO measurement
// — has to be reproducible from an audit record, and a handler that reads
// time.Now directly cannot be replayed.
type Clock interface {
	// Now returns the current instant in UTC.
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// AppendMeta carries the envelope metadata that events inherit when they are
// persisted: tenancy for routing, trace context for the one trace that spans
// all nine hops, and an idempotency key so a redelivered stream record cannot
// append a second copy of the same decision.
type AppendMeta struct {
	// TenantID and StoreID scope the emitted envelopes.
	TenantID canon.TenantID
	StoreID  canon.StoreID
	Region   canon.Region
	// TraceID and SpanID are W3C trace context.
	TraceID string
	SpanID  string
	// CorrelationID groups every event caused by one external request;
	// CausationID names the event that directly caused these.
	CorrelationID canon.CorrelationID
	CausationID   canon.EventID
	// Source names the producing component for the audit trail.
	Source string
	// IdempotencyKey is applied to the first event of the batch. The event
	// store makes a repeat append under the same key a no-op, which is what
	// makes at-least-once stream delivery safe at the aggregate boundary.
	IdempotencyKey string
	// OccurredAt overrides the envelope's source-clock timestamp.
	OccurredAt time.Time
}

// StoredEvent is one persisted domain event with its stream coordinates.
type StoredEvent struct {
	// Position is the event's place in the store's single global order.
	Position int64
	// Version is its place within the label's stream, starting at 1.
	Version int64
	// Event is the decoded domain event.
	Event domain.Event
	// Envelope is the event as stored, carrying trace and causation.
	Envelope canon.Envelope
}

// AppendOutcome reports what an append did.
type AppendOutcome struct {
	// Duplicate is true when the idempotency key had already been used and
	// nothing new was written. Events then holds the originals, so a caller
	// whose device publish failed after a successful append can still recover
	// what it was supposed to send.
	Duplicate bool
	// Version is the label's stream version after the append.
	Version int64
	// Events are the persisted events.
	Events []StoredEvent
}

// Repository loads and persists the Label aggregate.
type Repository interface {
	// Load rebuilds a label from its snapshot plus the events since. A label
	// that has never been provisioned is returned in its zero state with
	// Version 0 rather than as an error, so that a provisioning command and a
	// price command take the same path.
	Load(ctx context.Context, id canon.LabelID) (*domain.Label, error)
	// Append persists events under optimistic concurrency control, returning
	// ErrConcurrency if the stream moved on past expectedVersion.
	Append(ctx context.Context, id canon.LabelID, expectedVersion int64, events []domain.Event, meta AppendMeta) (AppendOutcome, error)
	// History returns a label's stored events newest-first, at most limit of
	// them. It is the event-sourced price history the API serves and the
	// compliance export reads.
	History(ctx context.Context, id canon.LabelID, limit int) ([]StoredEvent, error)
}

// Placement is where a label physically sits and what it shows.
//
// The Label Service keeps its own copy of this rather than calling the Device
// Registry on the hot path. A store-wide promotion resolves 40,000 placements;
// doing that over the network inside a 120 ms budget slice is not possible, and
// making the price path depend on another service's availability would mean a
// Device Registry outage stops prices changing.
type Placement struct {
	// LabelID identifies the label.
	LabelID canon.LabelID
	// SECID is the controller that owns it. The MQTT topic is built from this.
	SECID canon.SECID
	// TenantID, StoreID and Region complete the topic scope.
	TenantID canon.TenantID
	StoreID  canon.StoreID
	Region   canon.Region
	// SKU is the product currently assigned.
	SKU canon.SKU
	// Retired marks a placement kept for history but no longer addressable.
	Retired bool
	// UpdatedAt is when the directory last saw a change for this label.
	UpdatedAt time.Time
}

// Directory is the label placement read model: the answer to "which labels show
// this product in this store, and through which controller".
type Directory interface {
	// LabelsForSKU resolves the fan-out set for a price change.
	LabelsForSKU(ctx context.Context, tenant canon.TenantID, store canon.StoreID, sku canon.SKU) ([]Placement, error)
	// Lookup resolves one label's placement, returning ErrNotFound if the
	// directory has never seen it.
	Lookup(ctx context.Context, id canon.LabelID) (Placement, error)
	// StoreLabels lists every placement in a store, for the roster endpoint and
	// for store-wide fan-out.
	StoreLabels(ctx context.Context, tenant canon.TenantID, store canon.StoreID) ([]Placement, error)
	// Upsert records a placement.
	Upsert(ctx context.Context, p Placement) error
	// Remove deletes a placement outright. It is used only by a rebuild;
	// retirement goes through Upsert with Retired set, so history survives.
	Remove(ctx context.Context, id canon.LabelID) error
	// Clear empties the directory, which is the first step of a rebuild from
	// position zero.
	Clear(ctx context.Context) error
}

// Attestor signs the canonical price digest.
//
// It is the narrowest possible view of pki.PriceAuthority: the Label Service
// needs to sign and to name the key it signed with, and must not be able to
// export, rotate or retire the platform's most consequential secret from the
// hot path.
type Attestor interface {
	// Sign produces the attestation that authorises a price.
	Sign(input canon.AttestationInput) (canon.Attestation, error)
	// KeyID names the active signing key.
	KeyID() string
}

// DevicePublisher pushes an authorised update down to the store.
type DevicePublisher interface {
	// PublishPrice sends an update to the label's controller. It must publish
	// QoS 1 and retained: retained so a controller rebooting after a power cut
	// recovers the current price of its whole zone from the local broker
	// without a round trip to a cloud that may be unreachable.
	PublishPrice(ctx context.Context, p Placement, env canon.Envelope) error
	// Connected reports link state for the readiness check.
	Connected() bool
}

// StreamPublisher emits envelopes onto the platform's event streams.
type StreamPublisher interface {
	// Publish appends one envelope to a stream, returning only once it is
	// durable.
	Publish(ctx context.Context, stream string, env canon.Envelope) error
}

// LabelState is the query-side row for one label.
type LabelState struct {
	// LabelID and placement.
	LabelID  canon.LabelID  `json:"label_id"`
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	Region   canon.Region   `json:"region,omitempty"`
	SECID    canon.SECID    `json:"sec_id"`
	SKU      canon.SKU      `json:"sku,omitempty"`
	// Price is what the glass is showing.
	Price canon.Money `json:"price"`
	// BasePrice is the last non-promotional price: what a promotion discounts
	// from and what its expiry reverts to. A row with no base price has never
	// held an everyday price and cannot be reverted.
	BasePrice canon.Money `json:"base_price"`
	// Category and Brand are the merchandising attributes learned from price
	// changes. They are what lets a category-scoped promotion resolve its label
	// set from this read model instead of from a synchronous catalogue call.
	Category string `json:"category,omitempty"`
	Brand    string `json:"brand,omitempty"`
	// Sequence is the per-label monotonic counter.
	Sequence int64 `json:"sequence"`
	// State is the lifecycle position.
	State string `json:"state"`
	// Template is the render template on the glass.
	Template string `json:"template"`
	// EffectiveAt is when the displayed price took effect.
	EffectiveAt time.Time `json:"effective_at"`
	// PromotionID is the promotion behind it, if any.
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	// PendingSequence is non-zero while an update is published but
	// unconfirmed. It is what usslp_labels_pending_delivery counts.
	PendingSequence int64 `json:"pending_sequence,omitempty"`
	// PendingSince is when that update was authorised.
	PendingSince time.Time `json:"pending_since,omitempty"`
	// LastDeliveredSequence, LastDeliveredAt and LastLatencyMS describe the
	// most recent confirmed display.
	LastDeliveredSequence int64     `json:"last_delivered_sequence,omitempty"`
	LastDeliveredAt       time.Time `json:"last_delivered_at,omitempty"`
	LastLatencyMS         int64     `json:"last_latency_ms,omitempty"`
	// LastFailureReason is the edge's explanation of the most recent terminal
	// delivery failure.
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	// RejectedCount is a lifetime count of refused price changes.
	RejectedCount int64 `json:"rejected_count,omitempty"`
	// ScheduledCount is how many future-dated changes are waiting.
	ScheduledCount int `json:"scheduled_count,omitempty"`
	// Version is the aggregate version this row reflects, so a caller can tell
	// a stale read model from a stale aggregate.
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Healthy reports whether the label is in a state an operator would call good:
// reachable, and with nothing outstanding.
func (s LabelState) Healthy() bool {
	return s.State == string(domain.StateActive) && s.PendingSequence == 0 && s.LastFailureReason == ""
}

// StateStore is the query-side read model.
type StateStore interface {
	// Put writes one row.
	Put(ctx context.Context, s LabelState) error
	// Get reads one row, returning ErrNotFound if the projection has not seen
	// the label.
	Get(ctx context.Context, id canon.LabelID) (LabelState, error)
	// ListByStore returns every row for a store, ordered by label id.
	ListByStore(ctx context.Context, tenant canon.TenantID, store canon.StoreID) ([]LabelState, error)
	// Stores lists the stores a tenant has labels in, ordered.
	//
	// It exists for the promotion fan-out, whose candidate set is "every store
	// this tenant operates" whenever a rule names no stores of its own — which
	// is what a national promotion looks like. Deriving it from the read model
	// rather than asking the Device Registry keeps the fan-out on the same
	// local-data-only footing as the price path.
	Stores(ctx context.Context, tenant canon.TenantID) ([]canon.StoreID, error)
	// Clear empties the read model ahead of a rebuild.
	Clear(ctx context.Context) error
}

// ScheduleEntry is one future-dated change waiting to be activated.
type ScheduleEntry struct {
	// ScheduleID identifies the entry within its label's aggregate.
	ScheduleID string `json:"schedule_id"`
	// LabelID is the label to activate it on.
	LabelID canon.LabelID `json:"label_id"`
	// TenantID and StoreID scope the activation.
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	// EffectiveAt is when the runner should activate it.
	EffectiveAt time.Time `json:"effective_at"`
}

// ScheduleStore is the due-index the scheduled price runner walks.
//
// It is a separate index rather than a scan over aggregates because activating
// on time means asking "what is due in the next tick" across millions of
// labels, and the only affordable shape for that question is an index ordered
// by effective time.
type ScheduleStore interface {
	// Add records a scheduled change.
	Add(ctx context.Context, e ScheduleEntry) error
	// Remove drops one, whether it fired or was cancelled.
	Remove(ctx context.Context, label canon.LabelID, scheduleID string) error
	// Due returns entries whose effective time has arrived, at most limit of
	// them, in effective-time order.
	Due(ctx context.Context, at time.Time, limit int) ([]ScheduleEntry, error)
	// Clear empties the index ahead of a rebuild.
	Clear(ctx context.Context) error
}

// RateLimiter bounds a tenant's fan-out rate.
//
// One tenant's overnight repricing of 40,000 lines must not delay another
// tenant's single urgent change by more than its own share of the service. The
// limiter is per tenant rather than global for exactly that reason: a global
// limit would let the loudest tenant consume all of it.
type RateLimiter interface {
	// Wait blocks until n units of budget are available for the tenant, or ctx
	// is done. It returns ErrRateLimited only when waiting would exceed the
	// caller's deadline, so that a caller can distinguish backpressure from
	// cancellation.
	Wait(ctx context.Context, tenant canon.TenantID, n int) error
}
