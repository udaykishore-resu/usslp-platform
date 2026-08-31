package kvstore

import "time"

// Batch groups mutations that must take effect together or not at all.
//
// Atomicity is not a luxury here. Applying a new price without also updating
// the label-to-SKU assignment that renders it, or advancing a projection's
// checkpoint without having written the row the checkpoint claims to cover, is
// how a store ends up displaying a price it cannot justify to a trading
// standards officer. A batch is written as a single WAL record under a single
// sequence number, so readers see all of it or none of it, and recovery either
// replays all of it or discards all of it.
//
// A Batch is not safe for concurrent use by multiple goroutines.
type Batch struct {
	store *Store
	// order preserves first-touch ordering while byKey collapses repeated
	// writes to the same key, because one sequence number cannot carry two
	// versions of the same key.
	order []string
	byKey map[string]entry
}

// NewBatch returns an empty batch bound to the store.
func (s *Store) NewBatch() *Batch {
	return &Batch{store: s, byKey: make(map[string]entry)}
}

// Put stages a write. A later Put or Delete of the same key in the same batch
// replaces the earlier one, matching the last-write-wins semantics a caller
// would get from separate writes.
func (b *Batch) Put(key, value []byte) { b.PutTTL(key, value, 0) }

// PutTTL stages a write with a time to live. A ttl of zero means no expiry.
func (b *Batch) PutTTL(key, value []byte, ttl time.Duration) {
	if len(key) == 0 {
		return
	}
	e := entry{op: opPut, key: string(key), val: append([]byte(nil), value...)}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl).UnixNano()
	}
	b.stage(e)
}

// Delete stages a deletion.
func (b *Batch) Delete(key []byte) {
	if len(key) == 0 {
		return
	}
	b.stage(entry{op: opDelete, key: string(key)})
}

func (b *Batch) stage(e entry) {
	if _, seen := b.byKey[e.key]; !seen {
		b.order = append(b.order, e.key)
	}
	b.byKey[e.key] = e
}

// Len returns the number of distinct keys staged.
func (b *Batch) Len() int { return len(b.order) }

// Reset empties the batch for reuse, which keeps a hot ingest loop from
// allocating a fresh batch per POS message.
func (b *Batch) Reset() {
	b.order = b.order[:0]
	clear(b.byKey)
}

// entries renders the staged mutations in first-touch order.
func (b *Batch) entries() []entry {
	out := make([]entry, 0, len(b.order))
	for _, k := range b.order {
		out = append(out, b.byKey[k])
	}
	return out
}

// Write applies the batch atomically. On success the batch is left intact so a
// caller may inspect it; call Reset to reuse it.
func (b *Batch) Write() error { return b.store.write(b.entries()) }

// Write applies a batch atomically. It is the same operation as Batch.Write and
// exists so that code holding a *Store reads naturally.
func (s *Store) Write(b *Batch) error {
	if b.store != s {
		return ErrForeignBatch
	}
	return s.write(b.entries())
}
