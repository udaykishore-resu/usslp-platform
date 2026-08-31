package sgu

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// ---------------------------------------------------------------------------
// The local promotion schedule
//
// A promotion that starts at 08:00 has to start at 08:00 whether or not the
// store can reach the cloud. This is the part of autonomy that retailers
// actually care about: a WAN outage on the morning of a national campaign must
// not mean four hundred stores are still showing yesterday's prices at nine.
//
// The design constraint that shapes everything here is
// INTERFACE-CONTRACTS section 5: a label never displays a price it cannot
// verify, and the gateway holds no signing key by default. So the cloud does
// not schedule a promotion by telling the store "raise these prices at 08:00";
// it pushes the *already attested* price updates ahead of time, with a future
// activation instant, and the gateway's job at 08:00 is simply to publish
// signatures it was given hours earlier. The store gains the ability to act on
// its own clock without gaining the ability to author a price.
//
// The residual risk is the clock, and it is real: the store activates on local
// time, and local time is exactly what has been undisciplined for the length of
// the outage. Every activation therefore records the clock skew measured at the
// time, and the reconciliation report carries it, so that "the promotion started
// four minutes early" is a fact somebody can find rather than one they have to
// deduce from till receipts.
// ---------------------------------------------------------------------------

// ErrScheduleInvalid rejects a promotion that cannot be activated safely.
var ErrScheduleInvalid = errors.New("sgu: scheduled promotion is not usable")

// ScheduledPromotion is a pre-attested set of price updates with an activation
// time.
type ScheduledPromotion struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	// ActivateAt is when the store should publish these updates, on its own
	// clock.
	ActivateAt time.Time `json:"activate_at"`
	// ExpireAt is when the promotion ends. Zero means it runs until replaced.
	ExpireAt time.Time `json:"expire_at,omitempty"`
	// Updates are the attested price updates, one per label. They were signed by
	// the cloud's price authority when the promotion was published, which is why
	// the store can act on them without a key of its own.
	Updates []canon.PriceUpdated `json:"updates"`
	// Envelope is the causing event, kept so that the activation inherits its
	// trace and correlation and one promotion remains one traceable story.
	Envelope canon.Envelope `json:"envelope"`
	// ActivatedAt records that this promotion has fired, so a gateway restart
	// during a promotion window does not fire it twice.
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	// ActivationSkew is the clock skew measured at activation.
	ActivationSkew time.Duration `json:"activation_skew,omitempty"`
	// ReceivedAt is when the store took delivery of the schedule.
	ReceivedAt time.Time `json:"received_at"`
}

// Validate rejects a promotion the gateway could not honour.
func (p ScheduledPromotion) Validate() error {
	switch {
	case p.PromotionID == "":
		return fmt.Errorf("%w: no promotion id", ErrScheduleInvalid)
	case p.ActivateAt.IsZero():
		return fmt.Errorf("%w: promotion %s has no activation time", ErrScheduleInvalid, p.PromotionID)
	case len(p.Updates) == 0:
		return fmt.Errorf("%w: promotion %s carries no price updates", ErrScheduleInvalid, p.PromotionID)
	case !p.ExpireAt.IsZero() && !p.ExpireAt.After(p.ActivateAt):
		return fmt.Errorf("%w: promotion %s expires before it starts", ErrScheduleInvalid, p.PromotionID)
	}
	for _, u := range p.Updates {
		if u.Attestation.Signature == "" {
			// A promotion pushed without attestations is one the store could
			// never display. Refusing it at ingest turns a silent failure at
			// 08:00 into a loud one now.
			return fmt.Errorf("%w: promotion %s has an unattested update for label %s",
				ErrScheduleInvalid, p.PromotionID, u.LabelID)
		}
	}
	return nil
}

const schedulePrefix = "schedule/"

// Schedule is the store's durable promotion calendar.
type Schedule struct {
	store *kvstore.Store
	mu    sync.RWMutex
	items map[canon.PromotionID]ScheduledPromotion
}

// NewSchedule opens the schedule, restoring anything a previous process held.
func NewSchedule(store *kvstore.Store) (*Schedule, error) {
	if store == nil {
		return nil, errors.New("sgu: the promotion schedule needs a durable store")
	}
	s := &Schedule{store: store, items: map[canon.PromotionID]ScheduledPromotion{}}
	it := store.Scan([]byte(schedulePrefix))
	defer it.Close()
	for it.Next() {
		var p ScheduledPromotion
		if err := json.Unmarshal(it.Value(), &p); err != nil {
			continue
		}
		s.items[p.PromotionID] = p
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("sgu: restoring the promotion schedule: %w", err)
	}
	return s, nil
}

// Add stores a promotion, replacing any earlier version of it.
func (s *Schedule) Add(p ScheduledPromotion) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.ReceivedAt.IsZero() {
		p.ReceivedAt = time.Now().UTC()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("sgu: encoding promotion %s: %w", p.PromotionID, err)
	}
	if err := s.store.Put([]byte(schedulePrefix+string(p.PromotionID)), body); err != nil {
		return fmt.Errorf("sgu: persisting promotion %s: %w", p.PromotionID, err)
	}
	s.mu.Lock()
	s.items[p.PromotionID] = p
	s.mu.Unlock()
	return nil
}

// Due returns the promotions whose activation time has arrived and which have
// not fired, oldest first.
//
// Oldest first matters: if a gateway was down over two consecutive activation
// windows for the same product, the later promotion has to be applied after the
// earlier one, or the shelf ends up showing the price that expired first.
func (s *Schedule) Due(now time.Time) []ScheduledPromotion {
	s.mu.RLock()
	var out []ScheduledPromotion
	for _, p := range s.items {
		if p.ActivatedAt != nil {
			continue
		}
		if now.Before(p.ActivateAt) {
			continue
		}
		if !p.ExpireAt.IsZero() && !now.Before(p.ExpireAt) {
			// The window closed while the store was down. Activating it now would
			// put an expired promotional price on a shelf, which is worse than
			// having missed it.
			continue
		}
		out = append(out, p)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ActivateAt.Equal(out[j].ActivateAt) {
			return out[i].ActivateAt.Before(out[j].ActivateAt)
		}
		return out[i].PromotionID < out[j].PromotionID
	})
	return out
}

// Missed returns promotions whose whole window elapsed while the store could
// not act on them. They are reported rather than silently forgotten, because a
// promotion that never ran is a commercial fact somebody has to know.
func (s *Schedule) Missed(now time.Time) []ScheduledPromotion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ScheduledPromotion
	for _, p := range s.items {
		if p.ActivatedAt == nil && !p.ExpireAt.IsZero() && !now.Before(p.ExpireAt) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PromotionID < out[j].PromotionID })
	return out
}

// MarkActivated records that a promotion has fired, with the clock skew
// measured at the time.
func (s *Schedule) MarkActivated(id canon.PromotionID, at time.Time, skew time.Duration) error {
	s.mu.Lock()
	p, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("sgu: promotion %s is not in the schedule", id)
	}
	stamp := at.UTC()
	p.ActivatedAt = &stamp
	p.ActivationSkew = skew
	s.items[id] = p
	s.mu.Unlock()

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("sgu: encoding promotion %s: %w", id, err)
	}
	if err := s.store.Put([]byte(schedulePrefix+string(id)), body); err != nil {
		return fmt.Errorf("sgu: recording activation of %s: %w", id, err)
	}
	return nil
}

// Pending returns every promotion still waiting to fire, sorted by activation
// time, for the diagnostics page.
func (s *Schedule) Pending() []ScheduledPromotion {
	s.mu.RLock()
	var out []ScheduledPromotion
	for _, p := range s.items {
		if p.ActivatedAt == nil {
			out = append(out, p)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ActivateAt.Before(out[j].ActivateAt) })
	return out
}

// All returns every promotion the store holds.
func (s *Schedule) All() []ScheduledPromotion {
	s.mu.RLock()
	out := make([]ScheduledPromotion, 0, len(s.items))
	for _, p := range s.items {
		out = append(out, p)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ActivateAt.Before(out[j].ActivateAt) })
	return out
}
