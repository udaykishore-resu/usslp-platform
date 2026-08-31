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
// The local state replica
//
// The gateway holds a complete copy of its store's label state, so that a
// controller rebooting at four in the morning with the WAN down can be told
// what every label in its zone should be showing, and so that reconciliation
// has something to merge against.
//
// It is a replica, not a cache: the distinction is that a cache may be empty
// and the caller falls back to the origin, while this has no origin to fall
// back to. A store gateway that cannot answer from local state during an outage
// has failed at its only job.
// ---------------------------------------------------------------------------

// LabelState is the gateway's view of one label.
type LabelState struct {
	LabelID canon.LabelID `json:"label_id"`
	SECID   canon.SECID   `json:"sec_id"`
	SKU     canon.SKU     `json:"sku"`
	Price   canon.Money   `json:"price"`
	// Sequence is the per-label monotonic counter. The gateway allocates it for
	// locally originated changes, which is why it has to be durable: a gateway
	// that restarted and reused a sequence would have its update silently
	// discarded by the label.
	Sequence    int64             `json:"sequence"`
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	Render      canon.RenderSpec  `json:"render"`
	Attestation canon.Attestation `json:"attestation"`
	EffectiveAt time.Time         `json:"effective_at"`
	// TS and Origin are what reconciliation merges on.
	TS        HLC       `json:"ts"`
	Origin    Origin    `json:"origin"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PriceUpdate rebuilds the canonical event from replicated state, which is what
// the gateway republishes to a controller asking a cold-start question.
func (s LabelState) PriceUpdate() canon.PriceUpdated {
	return canon.PriceUpdated{
		LabelID: s.LabelID, SKU: s.SKU, StoreID: "", Price: s.Price,
		EffectiveAt: s.EffectiveAt, PromotionID: s.PromotionID,
		Render: s.Render, Attestation: s.Attestation, Sequence: s.Sequence,
	}
}

// InventoryState is the store's own count of a product.
type InventoryState struct {
	SKU    canon.SKU `json:"sku"`
	OnHand int64     `json:"on_hand"`
	TS     HLC       `json:"ts"`
	Origin Origin    `json:"origin"`
	AsOf   time.Time `json:"as_of"`
}

const (
	labelPrefix = "replica/label/"
	invPrefix   = "replica/inventory/"
)

// Replica is the durable local copy of the store's state.
//
// Safe for concurrent use: the bridge writes it from the broker's dispatch
// pool, the schedule writes it from its own goroutine, and the diagnostics
// surface reads it from an HTTP handler.
type Replica struct {
	store *kvstore.Store

	mu     sync.RWMutex
	labels map[canon.LabelID]LabelState
	inv    map[canon.SKU]InventoryState
	// bySKU indexes labels by product, which is what a local point-of-sale
	// price change needs: it names a SKU, and the gateway has to find every
	// shelf edge showing it.
	bySKU map[canon.SKU][]canon.LabelID
}

// NewReplica opens the replica, restoring everything a previous process wrote.
func NewReplica(store *kvstore.Store) (*Replica, error) {
	if store == nil {
		return nil, errors.New("sgu: the state replica needs a durable store")
	}
	r := &Replica{
		store:  store,
		labels: map[canon.LabelID]LabelState{},
		inv:    map[canon.SKU]InventoryState{},
		bySKU:  map[canon.SKU][]canon.LabelID{},
	}
	it := store.Scan([]byte(labelPrefix))
	for it.Next() {
		var st LabelState
		if err := json.Unmarshal(it.Value(), &st); err != nil {
			continue
		}
		r.labels[st.LabelID] = st
		r.bySKU[st.SKU] = append(r.bySKU[st.SKU], st.LabelID)
	}
	err := it.Err()
	it.Close()
	if err != nil {
		return nil, fmt.Errorf("sgu: restoring the label replica: %w", err)
	}

	it2 := store.Scan([]byte(invPrefix))
	for it2.Next() {
		var st InventoryState
		if err := json.Unmarshal(it2.Value(), &st); err != nil {
			continue
		}
		r.inv[st.SKU] = st
	}
	err = it2.Err()
	it2.Close()
	if err != nil {
		return nil, fmt.Errorf("sgu: restoring the inventory replica: %w", err)
	}
	for sku := range r.bySKU {
		sort.Slice(r.bySKU[sku], func(i, j int) bool { return r.bySKU[sku][i] < r.bySKU[sku][j] })
	}
	return r, nil
}

// PutLabel writes a label's state.
func (r *Replica) PutLabel(st LabelState) error {
	if st.LabelID == "" {
		return errors.New("sgu: replicated label state needs a label id")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	body, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("sgu: encoding label state for %s: %w", st.LabelID, err)
	}
	if err := r.store.Put([]byte(labelPrefix+string(st.LabelID)), body); err != nil {
		return fmt.Errorf("sgu: persisting label state for %s: %w", st.LabelID, err)
	}
	r.mu.Lock()
	prev, existed := r.labels[st.LabelID]
	r.labels[st.LabelID] = st
	if !existed || prev.SKU != st.SKU {
		if existed {
			r.unindexLocked(prev.SKU, st.LabelID)
		}
		r.bySKU[st.SKU] = append(r.bySKU[st.SKU], st.LabelID)
		sort.Slice(r.bySKU[st.SKU], func(i, j int) bool { return r.bySKU[st.SKU][i] < r.bySKU[st.SKU][j] })
	}
	r.mu.Unlock()
	return nil
}

func (r *Replica) unindexLocked(sku canon.SKU, id canon.LabelID) {
	list := r.bySKU[sku]
	for i, x := range list {
		if x == id {
			r.bySKU[sku] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// Label returns one label's replicated state.
func (r *Replica) Label(id canon.LabelID) (LabelState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.labels[id]
	return st, ok
}

// LabelsForSKU returns every label showing a product, which is what a local
// point-of-sale price change fans out to.
func (r *Replica) LabelsForSKU(sku canon.SKU) []canon.LabelID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]canon.LabelID(nil), r.bySKU[sku]...)
}

// Labels returns every label, sorted, for the diagnostics page and for the
// cold-start republication.
func (r *Replica) Labels() []LabelState {
	r.mu.RLock()
	out := make([]LabelState, 0, len(r.labels))
	for _, st := range r.labels {
		out = append(out, st)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LabelID < out[j].LabelID })
	return out
}

// LabelCount returns how many labels the store knows about.
func (r *Replica) LabelCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.labels)
}

// NextSequence allocates the next monotonic sequence for a label and persists
// it before returning.
//
// Persisting first is what makes it safe: a gateway that hands out a sequence
// and then loses power must not hand out the same one again, because the label
// would discard the second update as a replay and the shelf would quietly stop
// tracking its price.
func (r *Replica) NextSequence(id canon.LabelID) (int64, error) {
	r.mu.Lock()
	st, ok := r.labels[id]
	if !ok {
		st = LabelState{LabelID: id}
	}
	st.Sequence++
	next := st.Sequence
	r.labels[id] = st
	r.mu.Unlock()

	body, err := json.Marshal(st)
	if err != nil {
		return 0, fmt.Errorf("sgu: encoding label state for %s: %w", id, err)
	}
	if err := r.store.Put([]byte(labelPrefix+string(id)), body); err != nil {
		return 0, fmt.Errorf("sgu: reserving sequence %d for %s: %w", next, id, err)
	}
	return next, nil
}

// PutInventory writes a stock level.
func (r *Replica) PutInventory(st InventoryState) error {
	if st.SKU == "" {
		return errors.New("sgu: replicated inventory needs a SKU")
	}
	body, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("sgu: encoding inventory for %s: %w", st.SKU, err)
	}
	if err := r.store.Put([]byte(invPrefix+string(st.SKU)), body); err != nil {
		return fmt.Errorf("sgu: persisting inventory for %s: %w", st.SKU, err)
	}
	r.mu.Lock()
	r.inv[st.SKU] = st
	r.mu.Unlock()
	return nil
}

// Inventory returns a product's stock level.
func (r *Replica) Inventory(sku canon.SKU) (InventoryState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.inv[sku]
	return st, ok
}

// InventoryAll returns every stock level, sorted.
func (r *Replica) InventoryAll() []InventoryState {
	r.mu.RLock()
	out := make([]InventoryState, 0, len(r.inv))
	for _, st := range r.inv {
		out = append(out, st)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out
}

// Registers renders the whole replica as mergeable registers, which is what
// reconciliation compares against the cloud's retained state.
func (r *Replica) Registers() map[string]Register {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Register, len(r.labels)+len(r.inv))
	for id, st := range r.labels {
		body, err := json.Marshal(st.Price)
		if err != nil {
			continue
		}
		out[PriceKey(id)] = Register{
			Key: PriceKey(id), Kind: KindPricing, Value: body,
			TS: st.TS, Origin: st.Origin, WrittenAt: st.UpdatedAt,
		}
	}
	for sku, st := range r.inv {
		body, err := json.Marshal(st.OnHand)
		if err != nil {
			continue
		}
		out[InventoryKey(sku)] = Register{
			Key: InventoryKey(sku), Kind: KindInventory, Value: body,
			TS: st.TS, Origin: st.Origin, WrittenAt: st.AsOf,
		}
	}
	return out
}

// PriceKey is the merge key for a label's displayed price.
func PriceKey(id canon.LabelID) string { return "price/" + string(id) }

// InventoryKey is the merge key for a product's stock level.
func InventoryKey(sku canon.SKU) string { return "inventory/" + string(sku) }
