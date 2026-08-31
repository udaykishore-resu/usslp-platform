package kvstore

import (
	"math/bits"
	"math/rand/v2"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Ordered in-memory index
//
// Index structure choice: a skip list of nodes, each holding an MVCC version
// chain.
//
// The requirement is an ordered structure that supports range scans, so a hash
// map is disqualified outright: Scan(prefix) and Range(start, end) are how the
// edge tier reads "every label in aisle 7" and how the event store reads a
// stream, and a hash map cannot answer either without a full sweep.
//
// That leaves two realistic candidates:
//
//   - A sorted slice with copy-on-write. Reads are excellent (binary search on
//     an immutable slice, no locking at all) and a consistent read view is just
//     a pointer to the current slice. The problem is insertion of a *new* key:
//     every one copies the whole slice, so loading a store's SKU catalogue —
//     100,000 keys on a Store Gateway Unit — is quadratic, roughly 5x10^9
//     pointer copies before the store is even usable. Batching amortises some
//     of it, but a device that ingests a catalogue key-by-key over a slow WAN
//     link does not get that amortisation.
//
//   - A skip list. Insertion is O(log n) with no bulk copying, deletion is a
//     tombstone write rather than a structural change, and level-0 is already a
//     sorted singly linked list, which is exactly the shape a range scan wants.
//     The cost is that a reader must cooperate with concurrent writers.
//
// The edge workload is write-heavy on a stable-ish key set (prices for the same
// SKUs churn all day) but the initial load is genuinely large, so the skip
// list's O(log n) insert is worth the extra care on the read path. That care is
// paid for as follows: the skip list *structure* (the level links) is mutated
// only under the store's write lock and read only under its read lock, so the
// links themselves need no atomics; the per-key *value* history hangs off each
// node in an atomically published version chain, so an iterator that has
// already materialised its node pointers can read values with no lock at all
// and never blocks a price update.
//
// MVCC: every write is stamped with a monotonically increasing sequence number
// and prepended to the node's chain. A read at sequence S takes the newest
// version whose sequence is <= S. That is what makes Snapshot() a genuine
// point-in-time view — a compliance export can scan 100,000 labels while the
// store keeps accepting price changes, and it will see the prices as they were
// when the export began, not a torn mixture. Versions that no live snapshot can
// still reach are trimmed off the tail of the chain on the next write, so the
// steady-state chain length for a hot key is short.
//
// The rule that makes the lock-free read path safe:
//
//	A published version chain is immutable. Once a *version has been reachable
//	from n.head, neither it nor any version it links to is ever mutated again.
//
// A reader therefore only has to load n.head inside the store's read lock —
// one atomic load — and can then walk as far down the chain as it likes with no
// lock at all, knowing the history it is walking cannot change underneath it.
// That is what lets a snapshot pinned thousands of sequences behind the write
// frontier resolve a deep chain without stalling a single price write.
//
// Trimming honours the rule by publishing a fresh, shorter chain rather than
// severing the live one: see trim. Prepending a new version honours it because
// the version being linked in is not reachable from n.head until the store that
// built it publishes it, and the chain it points at is not modified.
// ---------------------------------------------------------------------------

// maxHeight bounds the skip list tower. 16 levels with p=1/4 addresses roughly
// 4^16 (~4x10^9) keys before the expected search cost degrades, which is orders
// of magnitude beyond what an edge node holds in RAM.
const maxHeight = 16

// version is one immutable value of one key at one sequence number.
//
// Nothing in it — next included — is mutated once the version has been
// published, that is, once it has been reachable from its node's head pointer.
// next is still atomic because it is written (to link the chain being built)
// and read (by lock-free readers walking a published chain) without a common
// lock; the atomic gives the reader a well-defined value, and immutability
// after publication gives it a *correct* one.
type version struct {
	seq uint64
	// val is nil for a tombstone. Stored values are never mutated after they
	// are published, which is what lets a lock-free reader hand the bytes back
	// without copying them under a lock.
	val []byte
	// expiresAt is a wall-clock deadline in Unix nanoseconds; 0 means the key
	// never expires. TTL is deliberately evaluated against the clock at read
	// time rather than against the snapshot sequence: a promotion that expired
	// at 18:00 must stop being served at 18:00 even to a long-running scan.
	expiresAt int64
	tomb      bool
	next      atomic.Pointer[version]
}

// node is one key in the ordered index. tower holds the skip list forward
// links; it is only ever touched under the store's write lock.
type node struct {
	key   string
	head  atomic.Pointer[version]
	tower []*node
}

// visibleIn returns the newest version at or below seq in the published chain
// rooted at head, or nil if the key did not exist yet at that sequence. head
// must have been loaded from some node's head pointer, after which the chain is
// immutable, so this walk needs no lock however long it runs.
func visibleIn(head *version, seq uint64) *version {
	for v := head; v != nil; v = v.next.Load() {
		if v.seq <= seq {
			return v
		}
	}
	return nil
}

// liveIn reports whether the chain rooted at head holds a readable value at seq
// and wall-clock time nowNanos, returning that value.
func liveIn(head *version, seq uint64, nowNanos int64) ([]byte, bool) {
	v := visibleIn(head, seq)
	if v == nil || v.tomb {
		return nil, false
	}
	if v.expiresAt != 0 && nowNanos >= v.expiresAt {
		return nil, false
	}
	return v.val, true
}

// visible returns the newest version of the key at or below seq, or nil if the
// key did not exist yet at that sequence.
func (n *node) visible(seq uint64) *version { return visibleIn(n.head.Load(), seq) }

// live reports whether the key holds a readable value at seq and wall-clock
// time nowNanos, returning that value.
func (n *node) live(seq uint64, nowNanos int64) ([]byte, bool) {
	return liveIn(n.head.Load(), seq, nowNanos)
}

// skiplist is the ordered index. It is not safe for concurrent use; the store
// serialises structural access with its own lock.
type skiplist struct {
	head   *node
	height int
	length int
}

func newSkiplist() *skiplist {
	return &skiplist{
		head:   &node{tower: make([]*node, maxHeight)},
		height: 1,
	}
}

// randomHeight draws a tower height with p=1/4 per level. Counting trailing
// zero *pairs* of a random word gives the same distribution as repeated coin
// flips without a loop of RNG calls.
func randomHeight() int {
	h := 1 + bits.TrailingZeros32(rand.Uint32()|0x80000000)/2
	if h > maxHeight {
		h = maxHeight
	}
	return h
}

// findPrev fills prev with, for each level, the last node whose key is strictly
// less than key, and returns the first node at level 0 whose key is >= key
// (nil at the end of the list).
func (s *skiplist) findPrev(key string, prev *[maxHeight]*node) *node {
	x := s.head
	for i := s.height - 1; i >= 0; i-- {
		for x.tower[i] != nil && x.tower[i].key < key {
			x = x.tower[i]
		}
		prev[i] = x
	}
	for i := s.height; i < maxHeight; i++ {
		prev[i] = s.head
	}
	return x.tower[0]
}

// get returns the node for an exact key, or nil.
func (s *skiplist) get(key string) *node {
	x := s.head
	for i := s.height - 1; i >= 0; i-- {
		for x.tower[i] != nil && x.tower[i].key < key {
			x = x.tower[i]
		}
	}
	n := x.tower[0]
	if n != nil && n.key == key {
		return n
	}
	return nil
}

// seek returns the first node whose key is >= key, or nil.
func (s *skiplist) seek(key string) *node {
	x := s.head
	for i := s.height - 1; i >= 0; i-- {
		for x.tower[i] != nil && x.tower[i].key < key {
			x = x.tower[i]
		}
	}
	return x.tower[0]
}

// first returns the smallest node, or nil when the index is empty.
func (s *skiplist) first() *node { return s.head.tower[0] }

// upsert returns the node for key, creating it if absent.
func (s *skiplist) upsert(key string) *node {
	var prev [maxHeight]*node
	next := s.findPrev(key, &prev)
	if next != nil && next.key == key {
		return next
	}
	h := randomHeight()
	if h > s.height {
		s.height = h
	}
	n := &node{key: key, tower: make([]*node, h)}
	for i := 0; i < h; i++ {
		n.tower[i] = prev[i].tower[i]
		prev[i].tower[i] = n
	}
	s.length++
	return n
}

// unlink removes a node from the index entirely. It is only called during a
// checkpoint rebuild, when the caller holds the write lock and has established
// that no snapshot can still reach the node's history.
func (s *skiplist) unlink(key string) {
	var prev [maxHeight]*node
	next := s.findPrev(key, &prev)
	if next == nil || next.key != key {
		return
	}
	for i := 0; i < len(next.tower); i++ {
		if prev[i].tower[i] == next {
			prev[i].tower[i] = next.tower[i]
		}
	}
	s.length--
}

// trim drops every version in n's chain that a reader starting from now on
// cannot reach: everything past the first version whose sequence is at or below
// minPin. It is applied as a key is written, so a key that is updated a
// thousand times a day does not accumulate a thousand versions. The caller must
// hold the store's write lock, so there is never a second publisher in flight.
//
// Trimming does *not* sever the live chain by storing nil into that version's
// next pointer. A reader is entitled to walk any chain it has loaded from
// n.head long after it loaded it — it holds no lock while it walks, and it may
// be descheduled anywhere along the way — so mutating a published version is
// how a reader ends up walking a chain that has had the very version it was
// looking for cut out from under it. That is not a hypothetical: an unpinned
// read resolves its sequence and then walks, and two writes landing in that
// window are enough to make Get report ErrNotFound — "no price on file" — for a
// key that has existed the whole time.
//
// So trimming instead builds a shorter chain for the *incoming* version to
// point at, which the caller then publishes with a single store to n.head.
// Readers already walking the old chain keep walking it, complete and correct,
// until they are done and the garbage collector takes it; readers arriving
// after the store see the short one. Nothing already published is touched.
//
// trimmedTail returns the chain to hang beneath the version being written,
// given prev, the chain that version would otherwise have been prepended to. It
// returns prev itself whenever nothing needs dropping, which is the common case
// and costs one walk and no allocation.
func trimmedTail(prev *version, minPin uint64) *version {
	// Find the cut: the newest version at or below minPin. Every reader is
	// pinned at some sequence >= minPin (an unpinned read resolves the latest
	// sequence, which is >= minPin too), so every reader stops at or before the
	// cut and nothing below it will ever be read again.
	depth := 0
	cut := prev
	for ; cut != nil; cut = cut.next.Load() {
		if cut.seq <= minPin {
			break
		}
		depth++
	}
	if cut == nil || cut.next.Load() == nil {
		// Either no version is old enough to cut at — a snapshot is pinned
		// behind everything this key holds, so the whole chain is still needed —
		// or the chain already ends exactly at the cut. Nothing to do.
		return prev
	}
	// Copy [prev .. cut] tail-first, so every copy is fully linked before
	// anything can reach it. depth is the number of versions above the cut,
	// which is zero for a hot key with no snapshot open: then the only
	// allocation is the truncated copy of the cut itself.
	tail := &version{seq: cut.seq, val: cut.val, expiresAt: cut.expiresAt, tomb: cut.tomb}
	if depth > 0 {
		var buf [16]*version
		var prefix []*version
		if depth <= len(buf) {
			prefix = buf[:0]
		} else {
			prefix = make([]*version, 0, depth)
		}
		for v := prev; v != cut; v = v.next.Load() {
			prefix = append(prefix, v)
		}
		for i := len(prefix) - 1; i >= 0; i-- {
			src := prefix[i]
			cp := &version{seq: src.seq, val: src.val, expiresAt: src.expiresAt, tomb: src.tomb}
			cp.next.Store(tail)
			tail = cp
		}
	}
	return tail
}
