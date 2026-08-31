package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// PlanogramResult is what a bulk upload reports back.
type PlanogramResult struct {
	// Revision is the store's new planogram revision.
	Revision int64 `json:"revision"`
	// Diff is the change set against the previous revision.
	Diff domain.Diff `json:"diff"`
	// Assigned counts the labels the registry actually re-bound. It can be
	// lower than the diff's total when a planogram references labels that have
	// not been provisioned yet, which is normal for a store fitting out: the
	// spreadsheet is uploaded before the hardware is clipped on.
	Assigned int `json:"assigned"`
	// Unassigned counts the labels whose binding was removed.
	Unassigned int `json:"unassigned"`
	// Pending lists the labels the planogram binds that the registry has never
	// seen. They are bound automatically when they provision.
	Pending []canon.LabelID `json:"pending,omitempty"`
}

// UploadPlanogram replaces a store's layout and re-binds its labels.
//
// The upload is diffed rather than applied wholesale, which is what makes a
// nightly full-file export from a space-planning system safe to accept. A
// merchandiser exports all 40,000 rows every night and changes three of them;
// applying the file would emit 40,000 assignment events, and the Label Service
// would rebuild its entire directory — and reprice an entire store — because
// somebody moved a jar of olives. Diffing means three events.
//
// Orphans are handled explicitly and are the reason the diff distinguishes them
// from removals: a label the new layout does not mention anywhere is still
// physically clipped to a rail, and until its binding is withdrawn the Label
// Service will keep repricing it for a SKU that is no longer in front of it.
func (s *Service) UploadPlanogram(ctx context.Context, pg *domain.Planogram) (*PlanogramResult, error) {
	if pg == nil {
		return nil, fmt.Errorf("%w: nil planogram", domain.ErrInvalid)
	}
	if err := pg.Validate(); err != nil {
		return nil, err
	}
	pg.Sort()
	now := s.Now()
	pg.UpdatedAt = now

	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	old := s.planogramFor(pg.StoreID)
	diff := domain.DiffPlanograms(old, pg)
	pg.Revision = 1
	if old != nil {
		pg.Revision = old.Revision + 1
	}

	result := &PlanogramResult{Revision: pg.Revision, Diff: diff}

	// Persist the document before emitting any consequence. If the process dies
	// between the two, the next upload of the same file produces an empty diff
	// and no events, whereas the opposite order would emit assignments for a
	// layout nobody can look up.
	body, err := json.Marshal(pg)
	if err != nil {
		return nil, fmt.Errorf("registry: encode planogram for %s: %w", pg.StoreID, err)
	}
	if err := s.kv.Put(planogramKey(pg.StoreID), body); err != nil {
		return nil, fmt.Errorf("registry: store planogram for %s: %w", pg.StoreID, err)
	}
	s.mu.Lock()
	s.planograms[pg.StoreID] = pg
	s.mu.Unlock()

	// Bindings first, withdrawals second. Doing it in this order means a label
	// that moved from one coordinate to another is never momentarily unbound in
	// a consumer's directory.
	rebind := make([]domain.Change, 0, len(diff.Added)+len(diff.Moved)+len(diff.Changed))
	rebind = append(rebind, diff.Added...)
	rebind = append(rebind, diff.Moved...)
	rebind = append(rebind, diff.Changed...)
	for _, ch := range rebind {
		if ch.To == nil {
			continue
		}
		bound, err := s.assignLocked(ctx, *ch.To, pg.TenantID, pg.StoreID, now)
		if err != nil {
			return nil, err
		}
		if bound {
			result.Assigned++
		} else {
			result.Pending = append(result.Pending, ch.To.LabelID)
		}
	}
	for _, label := range diff.Orphaned {
		removed, err := s.unassignLocked(ctx, label, "orphaned by planogram revision", now)
		if err != nil {
			return nil, err
		}
		if removed {
			result.Unassigned++
		}
	}

	summary := domain.PlanogramUpdated{
		TenantID:  pg.TenantID,
		StoreID:   pg.StoreID,
		Revision:  pg.Revision,
		Added:     len(diff.Added),
		Moved:     len(diff.Moved),
		Changed:   len(diff.Changed),
		Removed:   len(diff.Removed),
		Orphaned:  len(diff.Orphaned),
		Positions: len(pg.Positions),
		UpdatedAt: now,
	}
	env, err := s.newEvent(canon.EvtPlanogramUpdated, domain.AggregatePlanogram,
		string(pg.StoreID), pg.TenantID, pg.StoreID, summary)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, planogramStream(pg.StoreID), pg.Revision-1, env); err != nil {
		return nil, err
	}

	// Push the layout to the store, retained, so a Store Gateway that comes back
	// after a WAN outage has the current planogram without asking the cloud.
	s.publishPlanogram(ctx, pg)

	s.log.Info("planogram applied",
		"store_id", string(pg.StoreID), "revision", pg.Revision,
		"added", len(diff.Added), "moved", len(diff.Moved), "changed", len(diff.Changed),
		"removed", len(diff.Removed), "orphaned", len(diff.Orphaned),
		"assigned", result.Assigned, "pending", len(result.Pending))
	return result, nil
}

// planogramStream is the event-store stream carrying a store's planogram
// revision summaries.
func planogramStream(store canon.StoreID) eventstore.StreamID {
	return eventstore.Stream(domain.AggregatePlanogram, string(store))
}

// Planogram returns a store's current layout, or nil when it has none.
func (s *Service) Planogram(store canon.StoreID) *domain.Planogram {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pg := s.planograms[store]
	if pg == nil {
		return nil
	}
	out := *pg
	out.Positions = append([]domain.Position(nil), pg.Positions...)
	return &out
}

// planogramFor returns the live pointer under the read lock, for use while
// holding cmdMu.
func (s *Service) planogramFor(store canon.StoreID) *domain.Planogram {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.planograms[store]
}

// assignLocked binds one label to one planogram position, emitting the
// cross-service assignment event and clearing any retained state left on a
// controller the label has moved away from.
//
// It reports false when the label is not on record: the position is stored, and
// the binding is applied at provisioning time by bindFromPlanogramLocked. That
// is the ordering a store fit-out actually has — the planogram is uploaded from
// head office days before an engineer clips the hardware on — and treating it
// as an error would make the common case a failure.
//
// Must be called with cmdMu held.
func (s *Service) assignLocked(ctx context.Context, pos domain.Position, tenant canon.TenantID, store canon.StoreID, now time.Time) (bool, error) {
	dev := s.device(string(pos.LabelID))
	if dev == nil {
		return false, nil
	}
	if dev.State == domain.StateRetired || dev.State == domain.StateQuarantined {
		// Neither is an error for an upload: the file describes a shelf, and the
		// registry is telling the truth about a device that must not be
		// addressed. The position stays in the planogram so the binding takes
		// effect if the device is ever released or replaced.
		return false, nil
	}
	previousSEC := dev.Placement.SECID
	assigned := domain.LabelAssigned{
		LabelID:       pos.LabelID,
		TenantID:      tenant,
		StoreID:       store,
		SECID:         pos.SECID,
		PreviousSECID: previousSEC,
		Zone:          pos.Zone,
		SKU:           pos.SKU,
		Facings:       pos.Facings,
		Template:      pos.Template,
		Shelf:         pos.Shelf,
		Rail:          pos.Rail,
		Position:      pos.Position,
		Sequence:      dev.AssignmentSequence + 1,
		AssignedAt:    now,
	}
	if previousSEC == pos.SECID {
		assigned.PreviousSECID = ""
	}

	envs := make([]canon.Envelope, 0, 2)
	env, err := s.newEvent(canon.EvtLabelAssigned, domain.AggregateDevice, dev.ID, tenant, store, assigned)
	if err != nil {
		return false, err
	}
	envs = append(envs, env)

	// An assigned label that was merely provisioned moves state as well, so that
	// "assigned" in the fleet view means what it says.
	if domain.CanTransition(dev.State, domain.StateAssigned) && dev.State != domain.StateAssigned {
		change, err := deviceTransition(dev, domain.StateAssigned, "planogram assignment", now)
		if err != nil {
			return false, err
		}
		envChange, err := s.newEvent(domain.EvtDeviceStateChanged, domain.AggregateDevice, dev.ID, tenant, store, change)
		if err != nil {
			return false, err
		}
		envs = append(envs, envChange)
	}
	if err := s.commit(ctx, deviceStream(dev.ID), dev.Version, envs...); err != nil {
		return false, err
	}

	// Interface contract §3: a label reassigned to a different controller leaves
	// a stale retained message behind on the old zone topic. Clearing it is the
	// registry's job because the registry is the only component that knows the
	// reassignment happened, and a controller rebooting after a power cut would
	// otherwise replay a price for a label that is no longer in its zone.
	if assigned.PreviousSECID != "" {
		scope := s.scopeFor(tenant, store)
		s.pushRetained(ctx, scope.SECLabelTopic(previousSEC, pos.LabelID, canon.LeafPrice), nil, canon.QoSPrice)
		s.pushRetained(ctx, scope.SECLabelTopic(previousSEC, pos.LabelID, canon.LeafConfig), nil, canon.QoSConfig)
		if updated := s.Device(dev.ID); updated != nil {
			s.pushConfig(ctx, updated, s.buildConfig(updated))
		}
	}
	if s.met != nil {
		s.met.assignments.With("assigned").Inc()
	}
	return true, nil
}

// unassignLocked withdraws a label's binding. Must be called with cmdMu held.
func (s *Service) unassignLocked(ctx context.Context, label canon.LabelID, reason string, now time.Time) (bool, error) {
	dev := s.device(string(label))
	if dev == nil || dev.Assignment == nil {
		return false, nil
	}
	env, err := s.unassignEvent(dev, reason, now)
	if err != nil {
		return false, err
	}
	if err := s.commit(ctx, deviceStream(dev.ID), dev.Version, env); err != nil {
		return false, err
	}
	if s.met != nil {
		s.met.assignments.With("unassigned").Inc()
	}
	return true, nil
}

// unassignEvent builds the withdrawal event for a bound label. The payload
// repeats what the binding *was* rather than leaving the fields empty, because
// a consumer undoing a directory entry needs to know which entry to undo.
func (s *Service) unassignEvent(dev *domain.Device, reason string, now time.Time) (canon.Envelope, error) {
	a := dev.Assignment
	payload := domain.LabelAssigned{
		LabelID:    dev.LabelID(),
		TenantID:   dev.TenantID,
		StoreID:    dev.Placement.StoreID,
		SECID:      dev.Placement.SECID,
		Zone:       dev.Placement.Zone,
		Unassigned: true,
		Sequence:   dev.AssignmentSequence + 1,
		AssignedAt: now,
	}
	if a != nil {
		payload.SKU = a.SKU
		payload.Facings = a.Facings
		payload.Template = a.Template
		payload.Shelf = a.Shelf
		payload.Rail = a.Rail
		payload.Position = a.Position
	}
	env, err := s.newEvent(domain.EvtLabelUnassigned, domain.AggregateDevice, dev.ID,
		dev.TenantID, dev.Placement.StoreID, payload)
	if err != nil {
		return canon.Envelope{}, err
	}
	env.IdempotencyKey = fmt.Sprintf("unassign:%s:%d:%s", dev.ID, payload.Sequence, reason)
	return env, nil
}

// bindFromPlanogramLocked applies a stored planogram position to a device that
// has just provisioned. Must be called with cmdMu held.
func (s *Service) bindFromPlanogramLocked(ctx context.Context, dev *domain.Device, now time.Time) error {
	pg := s.planogramFor(dev.Placement.StoreID)
	if pg == nil {
		return nil
	}
	pos, ok := pg.ByLabel()[dev.LabelID()]
	if !ok {
		return nil
	}
	_, err := s.assignLocked(ctx, pos, dev.TenantID, dev.Placement.StoreID, now)
	return err
}

// publishPlanogram pushes the layout to the store, retained, on the store-wide
// planogram topic from interface contract §3.
func (s *Service) publishPlanogram(ctx context.Context, pg *domain.Planogram) {
	if s.cfg.Messenger == nil {
		return
	}
	env, err := s.newEvent(canon.EvtPlanogramUpdated, domain.AggregatePlanogram,
		string(pg.StoreID), pg.TenantID, pg.StoreID, pg)
	if err != nil {
		s.log.Warn("registry could not build a planogram envelope", "store_id", string(pg.StoreID), "error", err)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		s.log.Warn("registry could not encode a planogram envelope", "store_id", string(pg.StoreID), "error", err)
		return
	}
	scope := s.scopeFor(pg.TenantID, pg.StoreID)
	s.pushRetained(ctx, scope.StoreTopic(canon.LeafPlanogram), body, msgbus.QoS(canon.QoSConfig))
}
