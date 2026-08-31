package stack

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// PromotionFanout is what an activation did, observed from the shelves.
//
// # Why this is observed rather than reported
//
// The Label Service's promotion consumer produces a rich report — labels
// resolved, applied, rejected, failed — and then discards it, because the
// consumer's contract with the stream is an offset commit and nothing else.
// That is the right design and it has a consequence for anything outside the
// process: the report is not addressable. There was, until recently, a bridge
// in this package that called the fan-out synchronously and could therefore
// return one, and its ability to hand back a tidy struct was the least
// important thing about it and the most misleading — it meant the number came
// from the workaround rather than from the product.
//
// So this is what the platform's own surfaces can actually be asked, which is
// also the thing a retailer cares about: how many shelves are showing the
// promotion, in which stores, and how long after the activation the last one
// arrived. Every field is read from the controllers, which know what is on
// their labels' glass, rather than from the component that sent it.
type PromotionFanout struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	TenantID    canon.TenantID    `json:"tenant_id"`
	// Stores are the stores with at least one shelf under this promotion.
	Stores []canon.StoreID `json:"stores"`
	// Labels is how many labels the controllers hold under this promotion.
	Labels int `json:"labels"`
	// Displayed is how many of those have confirmed the pixels changed. The
	// gap between the two is a delivery still in flight or one that failed.
	Displayed int `json:"labels_displayed"`
	// ActivatedAt is when the Promotion Service accepted the transition, and
	// DurationMS is from there to the last shelf arriving.
	ActivatedAt time.Time `json:"activated_at"`
	DurationMS  int64     `json:"duration_ms"`
}

// ActivatePromotion activates a promotion through the Promotion Service.
//
// It does nothing else, and that is the whole point. The Promotion Service
// publishes `promotion.activated` to `promotion-events`; the Label Service's
// own consumer group `label-service.promotions` picks it up, resolves the
// affected labels through the promotion domain's compiled matcher, and drives
// them through the same batch path a store-wide repricing takes — rate limits,
// attestation, sequencing, guard rails and all. Nothing in this package is on
// that path any more.
//
// The returned instant is when the service accepted the transition, which is
// where the fan-out clock starts. Use AwaitPromotion to wait for it to land.
func (s *Stack) ActivatePromotion(ctx context.Context, tenant canon.TenantID,
	id canon.PromotionID, by string) (time.Time, error) {

	if s.cloudSvcs == nil || s.cloudSvcs.promotion == nil {
		return time.Time{}, fmt.Errorf("usslpd: the promotion service is not running")
	}
	at := time.Now()
	if _, err := s.cloudSvcs.promotion.Activate(ctx, tenant, id, by); err != nil {
		return time.Time{}, fmt.Errorf("usslpd: activating %s: %w", id, err)
	}
	return at, nil
}

// AwaitPromotion blocks until at least want labels are showing prices the named
// promotion set, and reports what arrived.
//
// A count rather than quiescence, because quiescence cannot distinguish "the
// fan-out has finished" from "the fan-out has not started": between the
// Promotion Service's append and the Label Service's consumer picking it up
// there is a durable write, a poll and a batch, and during that window every
// controller in the store is legitimately idle. Waiting for a store to go quiet
// would therefore sometimes report a promotion as complete before a single
// shelf had moved, which is exactly the kind of green test that hides a
// disconnected consumer.
//
// Passing want = 0 waits for the first shelf, which is the right question when
// the caller does not know the rule's reach.
func (s *Stack) AwaitPromotion(ctx context.Context, tenant canon.TenantID, id canon.PromotionID,
	want int, activatedAt time.Time, within time.Duration) (PromotionFanout, error) {

	if want < 1 {
		want = 1
	}
	deadline := time.Now().Add(within)
	for {
		f := s.PromotionFanout(tenant, id)
		f.ActivatedAt = activatedAt.UTC()
		if f.Labels >= want {
			f.DurationMS = time.Since(activatedAt).Milliseconds()
			return f, nil
		}
		if time.Now().After(deadline) {
			return f, fmt.Errorf("usslpd: %d of %d label(s) were showing promotion %s after %s; "+
				"the promotion-events consumer did not fan it out", f.Labels, want, id, within)
		}
		select {
		case <-ctx.Done():
			return f, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// PromotionFanout reports which shelves are currently under a promotion.
//
// It reads the controllers rather than any cloud read model deliberately: the
// question "is the promotion on the shelves" is answered by the shelves, and a
// read model that said yes while the glass said otherwise would be the failure
// worth catching rather than the answer worth trusting.
func (s *Stack) PromotionFanout(tenant canon.TenantID, id canon.PromotionID) PromotionFanout {
	f := PromotionFanout{PromotionID: id, TenantID: tenant}
	stores := map[canon.StoreID]bool{}
	for _, st := range s.stores {
		if tenant != "" && st.Tenant != tenant {
			continue
		}
		for _, z := range st.Zones {
			if z.Controller == nil {
				continue
			}
			for _, labelID := range z.Labels() {
				rec, ok := z.Controller.Record(labelID)
				if !ok || rec.PromotionID != id {
					continue
				}
				f.Labels++
				stores[st.ID] = true
				if rec.DisplayedSequence >= rec.Sequence {
					f.Displayed++
				}
			}
		}
	}
	f.Stores = make([]canon.StoreID, 0, len(stores))
	for storeID := range stores {
		f.Stores = append(f.Stores, storeID)
	}
	sort.Slice(f.Stores, func(i, j int) bool { return f.Stores[i] < f.Stores[j] })
	return f
}
