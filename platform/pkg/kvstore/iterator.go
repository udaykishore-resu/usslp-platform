package kvstore

import "sync/atomic"

// Iterator walks a key range in ascending key order at a fixed point in time.
//
// It is created by Scan, Range, Snapshot.Scan or Snapshot.Range. The node set
// for the range is materialised under the store's read lock at construction
// and the values are then read without any lock at all, so a long scan — a
// nightly compliance export over every label in a store — never blocks the
// price write path, and a price write during the scan is simply not visible to
// it.
//
// An Iterator is not safe for concurrent use by multiple goroutines. Close it
// when done; until every iterator and snapshot is released the store must keep
// old versions alive.
type Iterator struct {
	store  *Store
	seq    uint64
	nodes  []*node
	now    int64
	pos    int
	key    []byte
	val    []byte
	err    error
	closed atomic.Bool
	// borrowed distinguishes an iterator that owns its sequence pin (Scan/Range)
	// from one borrowing a Snapshot's pin, which the Snapshot releases.
	borrowed bool
}

// Next advances to the next live key, reporting whether one was found. Keys
// that were deleted or had expired at the moment the iterator was created are
// skipped. Reaching the end releases the iterator's hold on old versions, so a
// fully drained iterator does not have to be closed to avoid leaking history —
// though closing it is still the habit to keep.
func (it *Iterator) Next() bool {
	if it.err != nil || it.closed.Load() {
		return false
	}
	for {
		it.pos++
		if it.pos >= len(it.nodes) {
			it.key, it.val = nil, nil
			it.release()
			return false
		}
		n := it.nodes[it.pos]
		v, ok := n.live(it.seq, it.now)
		if !ok {
			continue
		}
		it.key = []byte(n.key)
		it.val = append([]byte(nil), v...)
		return true
	}
}

// Key returns the current key. The slice is a fresh copy and is owned by the
// caller.
func (it *Iterator) Key() []byte { return it.key }

// Value returns the current value. The slice is a fresh copy and is owned by
// the caller, so it stays valid after the next call to Next.
func (it *Iterator) Value() []byte { return it.val }

// Err returns the error that stopped iteration, if any. An exhausted iterator
// returns nil.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator. It is safe to call more than once.
func (it *Iterator) Close() error {
	it.release()
	return it.err
}

func (it *Iterator) release() {
	if !it.closed.CompareAndSwap(false, true) {
		return
	}
	if it.store != nil && !it.borrowed {
		it.store.unpin(it.seq)
	}
	it.nodes = nil
}
