package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// timeZero is the zero time, used to clear timestamp fields on a read-model row
// so that "no pending update" serialises as an absent field rather than as an
// old one.
var timeZero time.Time

// StateFromLabel renders an aggregate as its query-side row.
//
// One function, used by both writers of the read model — the hot-path
// write-through and the rebuildable projection — so that a row written by a
// live update and a row written by a replay are byte-identical. Two functions
// here would eventually disagree, and the symptom would be a read model that
// changes when it is rebuilt.
func StateFromLabel(l *domain.Label) ports.LabelState {
	s := ports.LabelState{
		LabelID: l.ID, TenantID: l.TenantID, StoreID: l.StoreID, Region: l.Region,
		SECID: l.SECID, SKU: l.SKU, Price: l.Price, BasePrice: l.BasePrice,
		Category: l.Category, Brand: l.Brand, Sequence: l.Sequence,
		State: string(l.State), Template: l.Render.Template,
		EffectiveAt: l.EffectiveAt, PromotionID: l.PromotionID,
		RejectedCount: l.RejectedCount, ScheduledCount: len(l.Scheduled),
		Version: l.Version, UpdatedAt: l.UpdatedAt,
	}
	if l.Pending != nil {
		s.PendingSequence = l.Pending.Sequence
		s.PendingSince = l.Pending.PublishedAt
	}
	if l.LastDelivery != nil {
		s.LastDeliveredSequence = l.LastDelivery.Sequence
		s.LastDeliveredAt = l.LastDelivery.At
		s.LastLatencyMS = l.LastDelivery.LatencyMS
	}
	if l.LastFailure != nil {
		s.LastFailureReason = l.LastFailure.Reason
	}
	return s
}

// LabelStateProjection is the CQRS query side: the read model the HTTP surface
// serves and the SLO endpoint aggregates.
//
// It is driven from the event store's single global order rather than from the
// event bus, for one reason that matters more than any other: a read model has
// to be rebuildable, deterministically, on any replica, at any time. A
// projection with a durable checkpoint over a totally ordered log can be
// dropped and replayed and will land on exactly the state it had; one driven by
// a partitioned stream cannot, because the interleaving across partitions is
// not reproducible.
type LabelStateProjection struct {
	state ports.StateStore
}

// NewLabelStateProjection builds the projection.
func NewLabelStateProjection(state ports.StateStore) (*LabelStateProjection, error) {
	if state == nil {
		return nil, fmt.Errorf("%w: StateStore", ErrMissingDependency)
	}
	return &LabelStateProjection{state: state}, nil
}

// Apply folds one stored event into the read model.
//
// It is idempotent by version: an event whose stream version is not ahead of
// the row's is ignored, which is what lets the projection run alongside the
// hot-path write-through without the slower of the two rolling the row back.
func (p *LabelStateProjection) Apply(ctx context.Context, se ports.StoredEvent) error {
	ev := deref(se.Event)
	id := eventLabelID(ev)
	if id == "" {
		return nil
	}
	row, err := p.state.Get(ctx, id)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		row = ports.LabelState{LabelID: id, State: string(domain.StateUnprovisioned)}
	case err != nil:
		return fmt.Errorf("label: reading state row for %s: %w", id, err)
	}
	if se.Version > 0 && row.Version >= se.Version {
		return nil
	}
	next := p.fold(row, ev)
	next.LabelID = id
	if se.Version > 0 {
		next.Version = se.Version
	}
	if at := ev.At(); at.After(next.UpdatedAt) {
		next.UpdatedAt = at
	}
	if err := p.state.Put(ctx, next); err != nil {
		return fmt.Errorf("label: writing state row for %s: %w", id, err)
	}
	return nil
}

// fold is the pure state transition. It has no side effects at all — not even a
// metric — so that a rebuild replaying six months of events cannot corrupt a
// counter, and so a table test can drive every branch without a store.
func (p *LabelStateProjection) fold(s ports.LabelState, ev domain.Event) ports.LabelState {
	switch e := ev.(type) {
	case domain.LabelProvisioned:
		s.TenantID, s.StoreID, s.Region, s.SECID = e.TenantID, e.StoreID, e.Region, e.SECID
		s.Template = e.Template
		if s.State == "" || s.State == string(domain.StateUnprovisioned) {
			s.State = string(domain.StateAssigned)
		}
	case domain.LabelAssigned:
		s.SKU = e.SKU
		if e.SECID != "" {
			s.SECID = e.SECID
		}
		if e.StoreID != "" {
			s.StoreID = e.StoreID
		}
		if s.State == string(domain.StateActive) || s.State == string(domain.StateOffline) {
			s.State = string(domain.StateAssigned)
		}
		if s.SKU != e.SKU {
			// A different product: its everyday price and its merchandising
			// attributes belong to the line that left the shelf, not the one
			// that arrived.
			s.BasePrice, s.Category, s.Brand = canon.Money{}, "", ""
		}
		s.PendingSequence, s.PendingSince = 0, timeZero
	case domain.PriceApplied:
		// The same base-price rule the aggregate applies, for the same reason:
		// an expiring promotion must fall back to the everyday price and not to
		// whatever the previous promotion charged.
		if e.PromotionID == "" {
			s.BasePrice = e.Price
		} else if s.BasePrice.Amount == 0 && s.BasePrice.Currency == "" {
			if s.Sequence > 0 {
				s.BasePrice = s.Price
			} else if e.PreviousPrice != nil {
				s.BasePrice = *e.PreviousPrice
			}
		}
		if e.Category != "" {
			s.Category = e.Category
		}
		if e.Brand != "" {
			s.Brand = e.Brand
		}
		s.SKU, s.Price, s.Sequence = e.SKU, e.Price, e.Sequence
		if e.StoreID != "" {
			s.StoreID = e.StoreID
		}
		s.EffectiveAt, s.PromotionID = e.EffectiveAt, e.PromotionID
		s.Template = e.Render.Template
		s.PendingSequence, s.PendingSince = e.Sequence, e.OccurredAt
		if e.SECID != "" {
			s.SECID = e.SECID
		}
		if s.State != string(domain.StateOffline) && s.State != string(domain.StateRetired) {
			s.State = string(domain.StateActive)
		}
		s.LastFailureReason = ""
	case domain.PriceScheduled:
		s.ScheduledCount++
	case domain.ScheduleCancelled:
		if s.ScheduledCount > 0 {
			s.ScheduledCount--
		}
	case domain.PriceRejected:
		s.RejectedCount++
	case domain.DeliveryConfirmed:
		s.LastDeliveredSequence = e.Sequence
		s.LastDeliveredAt = e.DeliveredAt
		s.LastLatencyMS = e.LatencyMS
		if s.PendingSequence != 0 && s.PendingSequence <= e.Sequence {
			s.PendingSequence, s.PendingSince = 0, timeZero
		}
		if s.State == string(domain.StateOffline) {
			s.State = string(domain.StateActive)
		}
		s.LastFailureReason = ""
	case domain.DeliveryFailed:
		s.LastFailureReason = e.Reason
		if s.PendingSequence == e.Sequence {
			s.PendingSequence, s.PendingSince = 0, timeZero
		}
	case domain.LabelWentOffline:
		if s.State == string(domain.StateActive) || s.State == string(domain.StateAssigned) {
			s.State = string(domain.StateOffline)
		}
	case domain.LabelCameOnline:
		if s.State == string(domain.StateOffline) {
			if s.Sequence > 0 {
				s.State = string(domain.StateActive)
			} else {
				s.State = string(domain.StateAssigned)
			}
		}
	case domain.LabelRetired:
		s.State = string(domain.StateRetired)
		s.PendingSequence, s.PendingSince = 0, timeZero
	}
	return s
}

// Clear empties the read model. It is the first step of a rebuild.
func (p *LabelStateProjection) Clear(ctx context.Context) error { return p.state.Clear(ctx) }

func eventLabelID(ev domain.Event) canon.LabelID {
	switch e := ev.(type) {
	case domain.LabelProvisioned:
		return e.LabelID
	case domain.LabelAssigned:
		return e.LabelID
	case domain.PriceApplied:
		return e.LabelID
	case domain.PriceScheduled:
		return e.LabelID
	case domain.ScheduleCancelled:
		return e.LabelID
	case domain.PriceRejected:
		return e.LabelID
	case domain.DeliveryConfirmed:
		return e.LabelID
	case domain.DeliveryFailed:
		return e.LabelID
	case domain.LabelWentOffline:
		return e.LabelID
	case domain.LabelCameOnline:
		return e.LabelID
	case domain.LabelRetired:
		return e.LabelID
	}
	return ""
}

// WithStore returns a copy of the projection writing through a different state
// store.
//
// The event store's projection runner hands each event a batch that is
// committed atomically with the projection's checkpoint, and a projection that
// wrote its rows outside that batch would, on a crash between the row and the
// checkpoint, either double-apply an event or skip one. Skipping one means a
// read model that has missed a price change, so the runner wraps the batch as a
// store and swaps it in here for the duration of one event.
func (p *LabelStateProjection) WithStore(s ports.StateStore) *LabelStateProjection {
	cp := *p
	cp.state = s
	return &cp
}
