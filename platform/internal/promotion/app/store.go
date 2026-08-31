package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Errors the store returns.
var (
	// ErrNotFound is returned for an unknown promotion.
	ErrNotFound = errors.New("promotion: not found")
	// ErrConcurrency is returned when an edit lost a race.
	ErrConcurrency = errors.New("promotion: version conflict")
	// ErrDuplicate is returned when creating a promotion whose id is taken.
	ErrDuplicate = errors.New("promotion: already exists")
)

// Record is a stored promotion with its lifecycle state.
type Record struct {
	Rule domain.Rule `json:"rule"`
	// State is the lifecycle position.
	State domain.State `json:"state"`
	// ActivatedAt and EndedAt bracket the promotion's real life, as opposed to
	// its scheduled one. A promotion cancelled mid-flight has an EndedAt well
	// before its scheduled end, and the lift measurement must use the real
	// dates or it measures a period the promotion was not running in.
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// CancelledBy and CancelReason record who pulled it and why.
	CancelledBy  string `json:"cancelled_by,omitempty"`
	CancelReason string `json:"cancel_reason,omitempty"`
	// UpdatedAt is the last edit.
	UpdatedAt time.Time `json:"updated_at"`
	// ActiveStores records which stores have entered the promotion, since a
	// national promotion goes live store by store as local windows open.
	ActiveStores []canon.StoreID `json:"active_stores,omitempty"`
}

// Store persists promotions over a key/value store.
//
// # Why a mutex over a store that is already safe for concurrent use
//
// Every state transition here is a read-modify-write of one record, and the
// kvstore's atomicity is per key, not per sequence. Two operators cancelling
// and editing the same promotion at the same moment would otherwise interleave.
// Promotions are edited by people at human rates, so a single mutex costs
// nothing measurable and removes the whole class of race; the optimistic
// version check on top of it is what protects against the *other* kind of
// conflict, where two operators each read, think, and then write.
type Store struct {
	kv *kvstore.Store
	mu sync.Mutex
}

// NewStore builds a promotion store.
func NewStore(kv *kvstore.Store) (*Store, error) {
	if kv == nil {
		return nil, errors.New("promotion: a key/value store is required")
	}
	return &Store{kv: kv}, nil
}

func recordKey(tenant canon.TenantID, id canon.PromotionID) []byte {
	return []byte("pr\x00" + string(tenant) + "\x00" + string(id))
}

func tenantPrefix(tenant canon.TenantID) []byte {
	return []byte("pr\x00" + string(tenant) + "\x00")
}

// Create stores a new promotion in the draft state.
func (s *Store) Create(rule domain.Rule, now time.Time) (Record, error) {
	if err := rule.Validate(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ok, err := s.kv.Has(recordKey(rule.TenantID, rule.ID)); err != nil {
		return Record{}, err
	} else if ok {
		return Record{}, fmt.Errorf("%w: %s", ErrDuplicate, rule.ID)
	}
	rule.Version = 1
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = now
	}
	rec := Record{Rule: rule, State: domain.StateDraft, UpdatedAt: now}
	return rec, s.put(rec)
}

// Get returns one promotion.
func (s *Store) Get(tenant canon.TenantID, id canon.PromotionID) (Record, error) {
	raw, err := s.kv.Get(recordKey(tenant, id))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("promotion: corrupt record %s: %w", id, err)
	}
	return rec, nil
}

// List returns a tenant's promotions, optionally filtered by state.
func (s *Store) List(tenant canon.TenantID, states []domain.State) ([]Record, error) {
	want := map[domain.State]bool{}
	for _, st := range states {
		want[st] = true
	}
	it := s.kv.Scan(tenantPrefix(tenant))
	defer it.Close()
	out := make([]Record, 0, 32)
	for it.Next() {
		var rec Record
		if err := json.Unmarshal(it.Value(), &rec); err != nil {
			// A corrupt record must not hide every other promotion from an
			// operator trying to find out what is live.
			continue
		}
		if len(want) > 0 && !want[rec.State] {
			continue
		}
		out = append(out, rec)
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule.ID < out[j].Rule.ID })
	return out, nil
}

// Update replaces a promotion's rule, enforcing the version check.
//
// Only a draft may be edited. A scheduled or active promotion that needs
// changing is cancelled and re-authored, so that the audit trail shows two
// documents rather than one that changed under a store's feet — a promotion
// whose terms changed while it was running is a dispute nobody can settle.
func (s *Store) Update(rule domain.Rule, expectedVersion int64, now time.Time) (Record, error) {
	if err := rule.Validate(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getLocked(rule.TenantID, rule.ID)
	if err != nil {
		return Record{}, err
	}
	if rec.State != domain.StateDraft {
		return Record{}, fmt.Errorf("%w: a %s promotion cannot be edited; cancel it and author a new one",
			domain.ErrTransition, rec.State)
	}
	if expectedVersion != 0 && rec.Rule.Version != expectedVersion {
		return Record{}, fmt.Errorf("%w: stored version is %d, caller expected %d",
			ErrConcurrency, rec.Rule.Version, expectedVersion)
	}
	rule.Version = rec.Rule.Version + 1
	rule.CreatedAt = rec.Rule.CreatedAt
	rec.Rule = rule
	rec.UpdatedAt = now
	return rec, s.put(rec)
}

// SetState moves a promotion through its lifecycle, enforcing the state
// machine.
func (s *Store) SetState(tenant canon.TenantID, id canon.PromotionID, to domain.State, now time.Time,
	by, reason string) (Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getLocked(tenant, id)
	if err != nil {
		return Record{}, err
	}
	if rec.State == to {
		// Idempotent: a retried activation must succeed rather than fail, since
		// the scheduler is at-least-once like everything else on the bus.
		return rec, nil
	}
	if err := domain.Transition(rec.State, to); err != nil {
		return Record{}, err
	}
	rec.State = to
	rec.UpdatedAt = now
	switch to {
	case domain.StateActive:
		t := now
		rec.ActivatedAt = &t
	case domain.StateExpired, domain.StateCancelled:
		t := now
		rec.EndedAt = &t
		if to == domain.StateCancelled {
			rec.CancelledBy, rec.CancelReason = by, reason
		}
	}
	return rec, s.put(rec)
}

// MarkStoreActive records that a store has entered the promotion.
//
// It is separate from the promotion's own state because they answer different
// questions: the promotion is active from the moment its earliest store opens
// until its latest store closes, while any individual store is in or out on its
// own local clock.
func (s *Store) MarkStoreActive(tenant canon.TenantID, id canon.PromotionID, store canon.StoreID, now time.Time) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.getLocked(tenant, id)
	if err != nil {
		return Record{}, err
	}
	for _, existing := range rec.ActiveStores {
		if existing == store {
			return rec, nil
		}
	}
	rec.ActiveStores = append(rec.ActiveStores, store)
	sort.Slice(rec.ActiveStores, func(i, j int) bool { return rec.ActiveStores[i] < rec.ActiveStores[j] })
	rec.UpdatedAt = now
	return rec, s.put(rec)
}

// Delete removes a promotion. Only a draft may be deleted; anything that has
// been scheduled has a compliance trail attached to it and is cancelled
// instead.
func (s *Store) Delete(tenant canon.TenantID, id canon.PromotionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.getLocked(tenant, id)
	if err != nil {
		return err
	}
	if rec.State != domain.StateDraft {
		return fmt.Errorf("%w: a %s promotion is retained for audit; cancel it instead",
			domain.ErrTransition, rec.State)
	}
	return s.kv.Delete(recordKey(tenant, id))
}

func (s *Store) getLocked(tenant canon.TenantID, id canon.PromotionID) (Record, error) {
	raw, err := s.kv.Get(recordKey(tenant, id))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("promotion: corrupt record %s: %w", id, err)
	}
	return rec, nil
}

func (s *Store) put(rec Record) error {
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.kv.Put(recordKey(rec.Rule.TenantID, rec.Rule.ID), blob)
}
