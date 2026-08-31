package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
)

// LabelAssignment is the payload of the Device Registry's
// `device.label.assigned` event.
//
// canon does not name it because assignment is the Device Registry's own
// vocabulary rather than a four-tier contract, so the shape is mirrored here at
// the boundary where the Label Service consumes it. Fields are additive only:
// an unknown field in a newer producer's payload is ignored, which is what
// makes a rolling upgrade across two hundred consumer nodes possible.
type LabelAssignment struct {
	// LabelID is the label being assigned.
	LabelID canon.LabelID `json:"label_id"`
	// SKU is the product. An empty SKU unassigns the label, which is what a
	// planogram removal looks like.
	SKU canon.SKU `json:"sku"`
	// SECID and StoreID carry a placement change made in the same event, which
	// is what a planogram reset that moves a label between bays produces.
	SECID   canon.SECID   `json:"sec_id,omitempty"`
	StoreID canon.StoreID `json:"store_id,omitempty"`
	// Template overrides the label's default display template.
	Template string `json:"template,omitempty"`
}

// DeviceStatus is the payload of `device.status.online` / `.offline`.
type DeviceStatus struct {
	// LabelID is the device.
	LabelID canon.LabelID `json:"label_id"`
	// StoreID and SECID locate it.
	StoreID canon.StoreID `json:"store_id,omitempty"`
	SECID   canon.SECID   `json:"sec_id,omitempty"`
	// Reason explains an offline transition: "missed_heartbeats",
	// "parent_lost", "battery_critical".
	Reason string `json:"reason,omitempty"`
}

// CurrencyResolver answers "what currency does this store trade in".
//
// Currency is a property of the retailer's store configuration, not of a
// device, so it cannot come from the `device.label.provisioned` event that
// otherwise carries everything the placement needs. It is resolved here, at the
// point a label enters the write side, and then frozen on the aggregate — which
// is what makes the currency invariant checkable without a lookup on the hot
// path.
type CurrencyResolver func(tenant canon.TenantID, store canon.StoreID) string

// FixedCurrency returns a resolver that answers the same code everywhere. It is
// the right choice for a single-currency deployment and the wrong one for a
// multi-region tenant, which supplies a real lookup.
func FixedCurrency(code string) CurrencyResolver {
	return func(canon.TenantID, canon.StoreID) string { return code }
}

// DirectoryProjection maintains the label placement read model and, with it,
// the write side's view of which labels exist.
//
// It consumes `device-events` and answers the question the price path asks
// forty thousand times during a store-wide promotion: which labels show this
// product in this store, and through which controller. Answering it from a
// local read model rather than by calling the Device Registry is not an
// optimisation — a synchronous call per label inside a 120 ms budget slice is
// not achievable, and a price path that depends on another service's
// availability means a Device Registry outage stops prices changing.
//
// The same events also drive the label aggregate's lifecycle, because a label
// the write side has never seen provisioned cannot be priced. Keeping both in
// one consumer means the directory and the aggregate can never disagree about
// whether a label exists.
//
// `device-events` carries all three tiers of hardware, because a controller or
// a gateway joining the fleet is a fact the OTA service and monitoring need. It
// is not a fact this projection has any use for, and the Device Registry names
// each tier's enrolment separately — `device.label.provisioned`,
// `device.sec.provisioned`, `device.sgu.provisioned` — precisely so that this
// consumer can decide on the `usslp-event-type` header. Anything that is not a
// label is skipped, not failed: a record another consumer owns must never
// become this consumer's dead-letter.
type DirectoryProjection struct {
	deps     Deps
	currency CurrencyResolver
}

// NewDirectoryProjection builds the projection. A nil resolver defaults every
// store to USD, which is correct for a single-currency deployment and is
// overridden in any real one.
func NewDirectoryProjection(deps Deps, currency CurrencyResolver) (*DirectoryProjection, error) {
	deps = deps.withDefaults()
	if deps.Directory == nil {
		return nil, fmt.Errorf("%w: Directory", ErrMissingDependency)
	}
	if deps.Repo == nil {
		return nil, fmt.Errorf("%w: Repo", ErrMissingDependency)
	}
	if currency == nil {
		currency = FixedCurrency("USD")
	}
	return &DirectoryProjection{deps: deps, currency: currency}, nil
}

// HandleMessage adapts the projection to an event-bus subscription.
func (p *DirectoryProjection) HandleMessage(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		return fmt.Errorf("%w: device-events record at %s/%d/%d: %v",
			canon.ErrEnvelopeInvalid, m.Topic, m.Partition, m.Offset, err)
	}
	return p.HandleEnvelope(ctx, env)
}

// HandleEnvelope folds one device event into the directory and the write side.
//
// Every branch is idempotent: provisioning an already-provisioned label,
// assigning the SKU it already holds and marking an offline label offline all
// produce no events. That is what makes "rebuild from zero" produce exactly the
// state a live run produced.
func (p *DirectoryProjection) HandleEnvelope(ctx context.Context, env canon.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	switch env.EventType {
	case canon.EvtSECProvisioned, canon.EvtSGUProvisioned:
		// A Shelf Edge Controller or a Store Gateway Unit joining the fleet is a
		// real event and other consumers — OTA, monitoring — act on it. It is
		// simply not a label, and this is a directory of labels: there is no
		// placement to record and no aggregate to provision. Skipping it by name
		// is the point of the Device Registry emitting one name per tier, and
		// naming the two cases here rather than letting them fall through the
		// default keeps the agreement visible from the consumer's side.
		return nil
	case canon.EvtLabelProvisioned:
		var payload canon.DeviceProvisioned
		if err := env.Decode(&payload); err != nil {
			return err
		}
		if !describesALabel(payload) {
			return nil
		}
		return p.provisioned(ctx, env, payload)
	case canon.EvtLabelAssigned:
		var payload LabelAssignment
		if err := env.Decode(&payload); err != nil {
			return err
		}
		return p.assigned(ctx, env, payload)
	case canon.EvtDeviceOnline, canon.EvtDeviceOffline:
		var payload DeviceStatus
		if err := env.Decode(&payload); err != nil {
			return err
		}
		return p.status(ctx, env, payload, env.EventType == canon.EvtDeviceOnline)
	}
	return nil
}

// describesALabel reports whether a `device.label.provisioned` payload is about
// a Tier-1 label, which is the only tier this projection has any use for.
//
// A current Device Registry states the kind and announces the other two tiers
// under their own event names, so the first branch is the whole answer for
// anything written since the names were split. The second exists for the
// records written before it, when every tier travelled under the label's name
// with no kind at all: a controller and a gateway are their own radio authority
// and never carry a parent SECID, and a label on a shelf always does, so the
// absence of one identifies exactly the records that used to be decoded as
// labels and then rejected for having no SECID.
//
// The residue of that heuristic is a pre-split record for a label that was
// enrolled with no parent controller. It is skipped rather than dead-lettered,
// which costs nothing: the aggregate would have refused it either way, and the
// label is provisioned from its assignment event instead — the same ordering
// the assigned branch already handles.
func describesALabel(d canon.DeviceProvisioned) bool {
	if d.Kind != "" {
		return d.Kind == canon.DeviceKindLabel
	}
	return d.SECID != ""
}

func (p *DirectoryProjection) provisioned(ctx context.Context, env canon.Envelope, d canon.DeviceProvisioned) error {
	if d.LabelID == "" {
		return fmt.Errorf("%w: device.label.provisioned without a label_id", canon.ErrEnvelopeInvalid)
	}
	store := firstStore(d.StoreID, env.StoreID)
	agg, err := p.deps.Repo.Load(ctx, d.LabelID)
	if err != nil {
		return fmt.Errorf("label: loading %s: %w", d.LabelID, err)
	}
	events, err := agg.Provision(domain.Provision{
		TenantID: env.TenantID, StoreID: store, Region: env.Region, SECID: d.SECID,
		Currency: p.currency(env.TenantID, store), HardwareTier: d.HardwareTier,
		Now: provisionTime(d, env),
	})
	if err != nil {
		return err
	}
	if err := p.append(ctx, agg, events, env, "provision"); err != nil {
		return err
	}
	return p.deps.Directory.Upsert(ctx, ports.Placement{
		LabelID: d.LabelID, SECID: d.SECID, TenantID: env.TenantID,
		StoreID: store, Region: env.Region, SKU: agg.SKU,
		UpdatedAt: env.OccurredAt,
	})
}

func (p *DirectoryProjection) assigned(ctx context.Context, env canon.Envelope, a LabelAssignment) error {
	if a.LabelID == "" {
		return fmt.Errorf("%w: device.label.assigned without a label_id", canon.ErrEnvelopeInvalid)
	}
	agg, err := p.deps.Repo.Load(ctx, a.LabelID)
	if err != nil {
		return fmt.Errorf("label: loading %s: %w", a.LabelID, err)
	}
	if !agg.Exists() {
		// An assignment for a label whose provisioning has not been consumed
		// yet, which is a genuinely reachable ordering rather than a corrupt
		// stream: canon.Envelope.PartitionKey derives its key from the payload,
		// so a provisioning event (no SKU) and an assignment (a SKU) for the
		// same device land on different partitions and are unordered with
		// respect to each other.
		//
		// The assignment carries everything a placement needs, so the label is
		// provisioned from it rather than the record being retried until the
		// other partition catches up. The later provisioning event is then a
		// no-op, which is what makes the projection order independent and lets
		// a rebuild from zero land on the same state whatever order it replays.
		store := firstStore(a.StoreID, env.StoreID)
		if a.SECID == "" || store == "" {
			return fmt.Errorf("label: assignment for unprovisioned label %s carries no placement: %w",
				a.LabelID, ports.ErrNotFound)
		}
		seed, perr := agg.Provision(domain.Provision{
			TenantID: env.TenantID, StoreID: store, Region: env.Region, SECID: a.SECID,
			Currency: p.currency(env.TenantID, store), Now: eventTime(env),
		})
		if perr != nil {
			return perr
		}
		// The seed append and the assignment append below come from one
		// envelope, so they must not share an idempotency key: the event store
		// treats a batch whose keys have all been seen as a no-op, and the
		// second append would be silently dropped, leaving a provisioned label
		// with no SKU that then quietly declines every price.
		if perr := p.append(ctx, agg, seed, env, "provision"); perr != nil {
			return perr
		}
	}
	events, err := agg.Assign(domain.Assign{
		SKU: a.SKU, SECID: a.SECID, StoreID: a.StoreID,
		Template: a.Template, Now: eventTime(env),
	})
	if err != nil {
		return err
	}
	if err := p.append(ctx, agg, events, env, "assign"); err != nil {
		return err
	}
	return p.deps.Directory.Upsert(ctx, ports.Placement{
		LabelID: a.LabelID, SECID: agg.SECID, TenantID: agg.TenantID,
		StoreID: agg.StoreID, Region: agg.Region, SKU: agg.SKU,
		UpdatedAt: env.OccurredAt,
	})
}

func (p *DirectoryProjection) status(ctx context.Context, env canon.Envelope, d DeviceStatus, online bool) error {
	if d.LabelID == "" {
		return nil
	}
	agg, err := p.deps.Repo.Load(ctx, d.LabelID)
	if err != nil {
		return fmt.Errorf("label: loading %s: %w", d.LabelID, err)
	}
	if !agg.Exists() {
		return nil
	}
	var events []domain.Event
	if online {
		events, err = agg.MarkOnline(eventTime(env))
	} else {
		events, err = agg.MarkOffline(d.Reason, eventTime(env))
	}
	if err != nil {
		return err
	}
	return p.append(ctx, agg, events, env, "status")
}

// append persists lifecycle events, reloading once on a concurrency conflict.
// A price change landing on the same label while a device event is being
// projected is routine, and the correct resolution is to re-decide against the
// state the winner left behind.
func (p *DirectoryProjection) append(ctx context.Context, agg *domain.Label, events []domain.Event, env canon.Envelope, purpose string) error {
	if len(events) == 0 {
		return nil
	}
	meta := ports.AppendMeta{
		TenantID: env.TenantID, StoreID: agg.StoreID, Region: env.Region,
		TraceID: env.TraceID, CorrelationID: env.CorrelationID,
		CausationID: env.EventID, Source: SourceName,
		IdempotencyKey: scopedIdempotencyKey(env, agg.ID, purpose),
	}
	var lastErr error
	for attempt := 1; attempt <= concurrencyAttempts; attempt++ {
		_, err := p.deps.Repo.Append(ctx, agg.ID, agg.Version, events, meta)
		if err == nil {
			for _, e := range events {
				agg.Apply(e)
				agg.Version++
			}
			if p.deps.State != nil {
				if perr := p.deps.State.Put(ctx, StateFromLabel(agg)); perr != nil {
					p.deps.Log.FromContext(ctx).Warn("updating label read model failed",
						"label_id", string(agg.ID), "error", perr)
				}
			}
			return nil
		}
		if !errors.Is(err, ports.ErrConcurrency) {
			return err
		}
		lastErr = err
		reloaded, lerr := p.deps.Repo.Load(ctx, agg.ID)
		if lerr != nil {
			return lerr
		}
		*agg = *reloaded
	}
	return lastErr
}

// Rebuild empties the directory so that a replay from the beginning of
// `device-events` reconstructs it. The aggregates are untouched: they are the
// write side and are not derived from anything.
func (p *DirectoryProjection) Rebuild(ctx context.Context) error {
	return p.deps.Directory.Clear(ctx)
}

func firstStore(vals ...canon.StoreID) canon.StoreID {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func eventTime(env canon.Envelope) time.Time {
	if !env.OccurredAt.IsZero() {
		return env.OccurredAt
	}
	if !env.RecordedAt.IsZero() {
		return env.RecordedAt
	}
	return time.Now().UTC()
}

func provisionTime(d canon.DeviceProvisioned, env canon.Envelope) time.Time {
	if !d.ProvisionedAt.IsZero() {
		return d.ProvisionedAt
	}
	return eventTime(env)
}
