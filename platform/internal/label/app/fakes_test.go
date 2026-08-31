package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// The fakes below exist for the use-case tests that are about the use case
// rather than about the infrastructure: per-tenant fairness, partial-failure
// reporting, worker-pool behaviour. The behaviours that are properties of the
// real infrastructure — durable append ordering, retained QoS 1 publishes,
// consumer-group redelivery — are tested against the real thing in the parent
// package, because a fake would only prove the fake was called.

// memRepo is an in-memory aggregate repository with real optimistic concurrency
// and real idempotency-key semantics.
type memRepo struct {
	mu      sync.Mutex
	streams map[canon.LabelID][]ports.StoredEvent
	idem    map[canon.LabelID]map[string]bool
	pos     int64
}

func newMemRepo() *memRepo {
	return &memRepo{
		streams: map[canon.LabelID][]ports.StoredEvent{},
		idem:    map[canon.LabelID]map[string]bool{},
	}
}

func (r *memRepo) Load(ctx context.Context, id canon.LabelID) (*domain.Label, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agg := domain.New(id)
	for _, se := range r.streams[id] {
		agg.Apply(se.Event)
		agg.Version = se.Version
	}
	return agg, nil
}

func (r *memRepo) Append(ctx context.Context, id canon.LabelID, expected int64, events []domain.Event, meta ports.AppendMeta) (ports.AppendOutcome, error) {
	if len(events) == 0 {
		return ports.AppendOutcome{Version: expected}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stream := r.streams[id]
	if meta.IdempotencyKey != "" && r.idem[id][meta.IdempotencyKey] {
		return ports.AppendOutcome{Duplicate: true, Version: int64(len(stream))}, nil
	}
	if int64(len(stream)) != expected {
		return ports.AppendOutcome{}, fmt.Errorf("%w: at %d, expected %d",
			ports.ErrConcurrency, len(stream), expected)
	}
	out := ports.AppendOutcome{Version: expected}
	for _, e := range events {
		r.pos++
		out.Version++
		se := ports.StoredEvent{Position: r.pos, Version: out.Version, Event: e}
		stream = append(stream, se)
		out.Events = append(out.Events, se)
	}
	r.streams[id] = stream
	if meta.IdempotencyKey != "" {
		if r.idem[id] == nil {
			r.idem[id] = map[string]bool{}
		}
		r.idem[id][meta.IdempotencyKey] = true
	}
	return out, nil
}

func (r *memRepo) History(ctx context.Context, id canon.LabelID, limit int) ([]ports.StoredEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream := r.streams[id]
	out := make([]ports.StoredEvent, 0, len(stream))
	for i := len(stream) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, stream[i])
	}
	return out, nil
}

// memDirectory is an in-memory placement read model.
type memDirectory struct {
	mu    sync.Mutex
	byID  map[canon.LabelID]ports.Placement
	bySKU map[string][]canon.LabelID
}

func newMemDirectory() *memDirectory {
	return &memDirectory{
		byID:  map[canon.LabelID]ports.Placement{},
		bySKU: map[string][]canon.LabelID{},
	}
}

func skuKey(t canon.TenantID, s canon.StoreID, sku canon.SKU) string {
	return string(t) + "\x00" + string(s) + "\x00" + string(sku)
}

func (d *memDirectory) Upsert(ctx context.Context, p ports.Placement) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byID[p.LabelID] = p
	k := skuKey(p.TenantID, p.StoreID, p.SKU)
	for _, id := range d.bySKU[k] {
		if id == p.LabelID {
			return nil
		}
	}
	d.bySKU[k] = append(d.bySKU[k], p.LabelID)
	return nil
}

func (d *memDirectory) Lookup(ctx context.Context, id canon.LabelID) (ports.Placement, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.byID[id]
	if !ok {
		return ports.Placement{}, fmt.Errorf("%w: %s", ports.ErrNotFound, id)
	}
	return p, nil
}

func (d *memDirectory) LabelsForSKU(ctx context.Context, t canon.TenantID, s canon.StoreID, sku canon.SKU) ([]ports.Placement, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.bySKU[skuKey(t, s, sku)]
	out := make([]ports.Placement, 0, len(ids))
	for _, id := range ids {
		out = append(out, d.byID[id])
	}
	return out, nil
}

func (d *memDirectory) StoreLabels(ctx context.Context, t canon.TenantID, s canon.StoreID) ([]ports.Placement, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []ports.Placement
	for _, p := range d.byID {
		if p.TenantID == t && p.StoreID == s {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LabelID < out[j].LabelID })
	return out, nil
}

func (d *memDirectory) Remove(ctx context.Context, id canon.LabelID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byID, id)
	return nil
}

func (d *memDirectory) Clear(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byID = map[canon.LabelID]ports.Placement{}
	d.bySKU = map[string][]canon.LabelID{}
	return nil
}

// memAttestor signs with a throwaway Ed25519 key.
type memAttestor struct {
	kid  string
	priv ed25519.PrivateKey
}

func newMemAttestor() *memAttestor {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return &memAttestor{kid: "test-key", priv: priv}
}

func (a *memAttestor) Sign(in canon.AttestationInput) (canon.Attestation, error) {
	return canon.Attest(in, a.kid, a.priv)
}

func (a *memAttestor) KeyID() string { return a.kid }

// memDevice records publishes and can be made to fail for chosen labels.
type memDevice struct {
	mu       sync.Mutex
	sent     map[canon.LabelID]int
	failFor  map[canon.LabelID]bool
	failWith error
}

func newMemDevice() *memDevice {
	return &memDevice{sent: map[canon.LabelID]int{}, failFor: map[canon.LabelID]bool{}}
}

func (d *memDevice) PublishPrice(ctx context.Context, p ports.Placement, env canon.Envelope) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failFor[p.LabelID] {
		if d.failWith != nil {
			return d.failWith
		}
		return errors.New("broker unreachable")
	}
	d.sent[p.LabelID]++
	return nil
}

func (d *memDevice) Connected() bool { return true }

func (d *memDevice) count(id canon.LabelID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent[id]
}

// memStreams records stream publishes.
type memStreams struct {
	mu   sync.Mutex
	sent map[string]int
}

func newMemStreams() *memStreams { return &memStreams{sent: map[string]int{}} }

func (s *memStreams) Publish(ctx context.Context, stream string, env canon.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[stream]++
	return nil
}

func (s *memStreams) count(stream string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[stream]
}

// memState is an in-memory query-side read model.
type memState struct {
	mu   sync.Mutex
	rows map[canon.LabelID]ports.LabelState
}

func newMemState() *memState { return &memState{rows: map[canon.LabelID]ports.LabelState{}} }

func (s *memState) Put(ctx context.Context, row ports.LabelState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.LabelID] = row
	return nil
}

func (s *memState) Get(ctx context.Context, id canon.LabelID) (ports.LabelState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return ports.LabelState{}, fmt.Errorf("%w: %s", ports.ErrNotFound, id)
	}
	return row, nil
}

func (s *memState) ListByStore(ctx context.Context, t canon.TenantID, store canon.StoreID) ([]ports.LabelState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.LabelState
	for _, row := range s.rows {
		if row.TenantID == t && row.StoreID == store {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LabelID < out[j].LabelID })
	return out, nil
}

func (s *memState) Stores(ctx context.Context, t canon.TenantID) ([]canon.StoreID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[canon.StoreID]bool{}
	var out []canon.StoreID
	for _, row := range s.rows {
		if row.TenantID == t && row.StoreID != "" && !seen[row.StoreID] {
			seen[row.StoreID] = true
			out = append(out, row.StoreID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (s *memState) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = map[canon.LabelID]ports.LabelState{}
	return nil
}

// fixedClock is a clock that does not move, so a decision's outcome depends
// only on the command and the policy.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// countingLimiter records every charge, per tenant, so a test can assert that
// one tenant's bulk fan-out was actually shaped.
type countingLimiter struct {
	mu      sync.Mutex
	charged map[canon.TenantID]int
	delay   time.Duration
}

func newCountingLimiter() *countingLimiter {
	return &countingLimiter{charged: map[canon.TenantID]int{}}
}

func (l *countingLimiter) Wait(ctx context.Context, tenant canon.TenantID, n int) error {
	l.mu.Lock()
	l.charged[tenant] += n
	delay := l.delay
	l.mu.Unlock()
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func (l *countingLimiter) total(tenant canon.TenantID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.charged[tenant]
}
