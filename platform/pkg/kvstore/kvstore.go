// Package kvstore is USSLP's embedded, durable, ordered key/value store.
//
// It exists because the edge tier has to keep working when the cloud does not.
// A Store Gateway Unit that loses its WAN link must go on serving prices,
// accepting queued updates and recording what it did, for hours, on hardware
// with no database server and no operator. RocksDB is the usual answer to that
// shape of problem; this package is the same shape of answer with no cgo, no
// external dependency and no build toolchain requirement beyond the Go standard
// library, which is what makes it deployable to a fleet of embedded gateways
// and simultaneously usable as the read-model store behind `make dev`.
//
// The design is a write-ahead log plus an in-memory ordered index plus periodic
// snapshots:
//
//   - Every mutation is appended to a CRC-32C checksummed, length-framed WAL
//     before it is acknowledged, so an acknowledged price change survives a
//     power cut (subject to SyncPolicy, see below).
//   - The index is a skip list carrying MVCC version chains, so reads are
//     ordered, range scans are cheap, and Snapshot() is a true point-in-time
//     view rather than a lock held over a long scan. See index.go for why a
//     skip list rather than a copy-on-write sorted slice.
//   - Snapshots (checkpoints) fold the live key set into one file and start a
//     fresh WAL, which is what keeps recovery time bounded: without them, a
//     gateway that has run for six months replays six months of price changes
//     at boot.
//
// Durability, stated precisely, because "durable" is a compliance word here:
//
//   - A write is only acknowledged after its record has been handed to the
//     kernel with a single write(2). There is no user-space buffering, so a
//     crash of the *process* never loses an acknowledged write under any sync
//     policy.
//   - SyncAlways fsyncs before acknowledging. An acknowledged write survives a
//     power cut. This is the correct setting for the pricing queue on a store
//     gateway, where losing an acknowledged price change is a
//     weights-and-measures problem.
//   - SyncEvery fsyncs on a timer. A power cut loses at most the writes of the
//     last interval. Correct for telemetry and read models, which are
//     reconstructable from the cloud.
//   - SyncNever never fsyncs explicitly and leaves flushing to the operating
//     system. A power cut may lose everything written since the last
//     checkpoint. Correct only for caches and for tests.
//
// A Store is safe for concurrent use by any number of goroutines.
package kvstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// ErrNotFound is returned by Get for a key that is absent, deleted or expired.
// It is a sentinel rather than a (nil, nil) return so that a caller cannot
// silently treat "no price on file" as "price of zero".
var ErrNotFound = errors.New("kvstore: key not found")

// ErrClosed is returned by every operation on a closed store. Closing is
// terminal; a store is not reopenable in place because its temporary directory
// (if any) is gone and its background goroutines have exited.
var ErrClosed = errors.New("kvstore: store is closed")

// ErrForeignBatch is returned when a batch created by one store is handed to
// another. Applying it would write to a WAL that never framed its records.
var ErrForeignBatch = errors.New("kvstore: batch belongs to a different store")

// ErrEmptyKey rejects the empty key. Permitting it would make the empty prefix
// ambiguous between "scan everything" and "the one key that is nothing".
var ErrEmptyKey = errors.New("kvstore: empty key")

// SyncPolicy selects how aggressively the write-ahead log is flushed to stable
// storage. See the package documentation for the exact guarantee each one
// provides.
type SyncPolicy int

const (
	// SyncAlways fsyncs the WAL before every write is acknowledged.
	SyncAlways SyncPolicy = iota
	// SyncEvery fsyncs the WAL on the interval given by Options.SyncEvery.
	SyncEvery
	// SyncNever leaves flushing entirely to the operating system.
	SyncNever
)

// String renders the policy for logs and metrics labels.
func (p SyncPolicy) String() string {
	switch p {
	case SyncAlways:
		return "always"
	case SyncEvery:
		return "interval"
	case SyncNever:
		return "never"
	}
	return "unknown"
}

// Options configures a store. The zero value is usable via OpenWith once Dir is
// set; every duration and threshold has a defensible default for an edge node.
type Options struct {
	// Dir is the data directory. An empty Dir creates a temporary directory
	// that is removed on Close, which is what tests and `make dev` use.
	Dir string
	// Sync selects the durability policy.
	Sync SyncPolicy
	// SyncEvery is the fsync interval when Sync is SyncEvery. Default 200ms,
	// chosen to sit well inside the platform's 500ms ingress budget so a
	// gateway restart loses less than one budget's worth of work.
	SyncEvery time.Duration
	// CheckpointEvery is how often a background checkpoint runs. Default 5
	// minutes. Zero disables time-based checkpointing.
	CheckpointEvery time.Duration
	// CheckpointBytes forces a checkpoint once the active WAL exceeds this many
	// bytes. Default 64 MiB. This is the knob that actually bounds recovery
	// time: replay work is proportional to WAL size, not to uptime.
	CheckpointBytes int64
	// ExpireEvery is how often expired keys are swept and tombstoned. Default
	// 30 seconds. Expiry is also evaluated lazily on every read, so this
	// controls memory reclamation rather than correctness.
	ExpireEvery time.Duration
	// Registry, when non-nil, receives the store's metrics.
	Registry *obs.Registry
	// MetricNamespace prefixes the registered metric names. Default "kvstore".
	// obs.Registry rejects duplicate metric names, so two stores sharing one
	// registry must be given distinct namespaces.
	MetricNamespace string
}

func (o *Options) applyDefaults() {
	if o.SyncEvery <= 0 {
		o.SyncEvery = 200 * time.Millisecond
	}
	if o.CheckpointEvery == 0 {
		o.CheckpointEvery = 5 * time.Minute
	}
	if o.CheckpointBytes <= 0 {
		o.CheckpointBytes = 64 << 20
	}
	if o.ExpireEvery <= 0 {
		o.ExpireEvery = 30 * time.Second
	}
	if o.MetricNamespace == "" {
		o.MetricNamespace = "kvstore"
	}
}

// Store is an embedded ordered key/value store.
type Store struct {
	opts    Options
	dir     string
	tempDir bool

	// writeMu serialises all writers. It is held across the WAL append and the
	// index apply so that WAL order and index order can never diverge, and
	// across the read-modify-write of PutIfAbsent so that two concurrent
	// idempotency claims cannot both succeed. It is deliberately *not* the same
	// lock as mu: an fsync under SyncAlways must not block readers.
	writeMu sync.Mutex
	wal     *walWriter

	// mu guards the index, the sequence counter and the snapshot pin registry.
	mu       sync.RWMutex
	idx      *skiplist
	seq      uint64
	liveKeys int64
	pins     map[uint64]int
	minPin   uint64

	snapSeq  atomic.Uint64
	snapAt   atomic.Int64 // unix nanos of last successful checkpoint
	closed   atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	ckptReq  chan struct{}
	wg       sync.WaitGroup

	counters counters
	metrics  *storeMetrics
}

type counters struct {
	puts     atomic.Uint64
	deletes  atomic.Uint64
	gets     atomic.Uint64
	misses   atomic.Uint64
	expired  atomic.Uint64
	ckpts    atomic.Uint64
	walBytes atomic.Int64
}

// Open opens or creates a store in dir. An empty dir creates a temporary
// directory that Close removes, so a test or a `make dev` process gets a real
// durable store with real recovery semantics and no cleanup obligation.
func Open(dir string) (*Store, error) {
	return OpenWith(Options{Dir: dir})
}

// OpenWith opens a store with explicit options, replaying the newest valid
// checkpoint and then any write-ahead log written after it. A torn final WAL
// record — the normal result of losing power mid-write — is discarded and the
// log truncated back to the last intact boundary.
func OpenWith(opts Options) (*Store, error) {
	opts.applyDefaults()
	dir := opts.Dir
	temp := false
	if dir == "" {
		d, err := os.MkdirTemp("", "usslp-kvstore-")
		if err != nil {
			return nil, fmt.Errorf("kvstore: create temp dir: %w", err)
		}
		dir, temp = d, true
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("kvstore: create dir %s: %w", dir, err)
	}

	s := &Store{
		opts:    opts,
		dir:     dir,
		tempDir: temp,
		idx:     newSkiplist(),
		pins:    make(map[uint64]int),
		stop:    make(chan struct{}),
		ckptReq: make(chan struct{}, 1),
	}
	if err := s.recover(); err != nil {
		if temp {
			os.RemoveAll(dir)
		}
		return nil, err
	}
	if opts.Registry != nil {
		s.metrics = newStoreMetrics(opts.Registry, opts.MetricNamespace)
	}
	s.snapAt.Store(time.Now().UnixNano())
	s.startBackground()
	return s, nil
}

// Dir returns the directory backing the store, which for a temporary store is
// the generated path. Operators need it to size a volume; tests need it to
// simulate a crash.
func (s *Store) Dir() string { return s.dir }

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

const (
	snapPrefix = "snap-"
	snapSuffix = ".dat"
	walPrefix  = "wal-"
	walSuffix  = ".log"
)

func (s *Store) snapPath(seq uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s%020d%s", snapPrefix, seq, snapSuffix))
}

func (s *Store) walPath(seq uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s%020d%s", walPrefix, seq, walSuffix))
}

// listSeqs returns the sequence numbers of the files in the data directory with
// the given prefix/suffix, ascending.
func (s *Store) listSeqs(prefix, suffix string) ([]uint64, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("kvstore: read dir %s: %w", s.dir, err)
	}
	var out []uint64
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		body := n[len(prefix) : len(n)-len(suffix)]
		v, err := strconv.ParseUint(body, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// recover rebuilds the index from the newest usable checkpoint plus the logs
// written after it, and leaves the store ready to append.
func (s *Store) recover() error {
	snaps, err := s.listSeqs(snapPrefix, snapSuffix)
	if err != nil {
		return err
	}
	// Walk newest first: a checkpoint that failed its checksum (killed mid
	// rename on a filesystem that does not give us atomic renames, or a bad
	// block) must not take the store down when an older one is still intact.
	var loaded uint64
	for i := len(snaps) - 1; i >= 0; i-- {
		if err := s.loadCheckpoint(s.snapPath(snaps[i]), snaps[i]); err != nil {
			continue
		}
		loaded = snaps[i]
		break
	}
	s.seq = loaded
	s.minPin = loaded
	s.snapSeq.Store(loaded)

	wals, err := s.listSeqs(walPrefix, walSuffix)
	if err != nil {
		return err
	}
	var active uint64
	var activeSet bool
	for _, base := range wals {
		if base < loaded {
			// Superseded by the checkpoint; its records are already folded in.
			continue
		}
		path := s.walPath(base)
		good, err := replayWAL(path, func(rec record) error {
			if rec.seq <= loaded {
				return nil
			}
			s.applyLocked(rec)
			return nil
		})
		if err != nil {
			return err
		}
		if err := truncateTo(path, good); err != nil {
			return err
		}
		active, activeSet = base, true
	}
	if !activeSet {
		active = loaded
	}
	w, err := openWALWriter(s.walPath(active), active)
	if err != nil {
		return err
	}
	s.wal = w
	s.counters.walBytes.Store(w.bytes)
	return nil
}

// ---------------------------------------------------------------------------
// Write path
// ---------------------------------------------------------------------------

// applyLocked folds one record into the index. The caller must hold writeMu
// (or be single-threaded, as during recovery); it takes mu itself.
//
// Each key's new version is linked to the history it should keep — already
// trimmed of what no reader can reach — and only then published with a single
// store to the node's head. Publishing last is what makes the version chain a
// reader has already loaded immutable, and immutability is what lets that
// reader walk it with no lock held. See index.go.
func (s *Store) applyLocked(rec record) {
	s.mu.Lock()
	for i := range rec.entries {
		e := &rec.entries[i]
		n := s.idx.upsert(e.key)
		prev := n.head.Load()
		wasLive := prev != nil && !prev.tomb
		v := &version{seq: rec.seq, expiresAt: e.expiresAt}
		if e.op == opPut {
			v.val = e.val
		} else {
			v.tomb = true
		}
		v.next.Store(trimmedTail(prev, s.minPin))
		n.head.Store(v)
		switch {
		case e.op == opPut && !wasLive:
			s.liveKeys++
		case e.op == opDelete && wasLive:
			s.liveKeys--
		}
	}
	if rec.seq > s.seq {
		s.seq = rec.seq
	}
	if len(s.pins) == 0 {
		s.minPin = s.seq
	}
	s.mu.Unlock()
}

// write is the single funnel every mutation passes through: reserve a sequence,
// append one WAL record, honour the sync policy, then publish to the index.
// Entries are applied under one sequence number, which is precisely what makes
// a Batch atomic to readers and to recovery.
func (s *Store) write(entries []entry) error {
	if s.closed.Load() {
		return ErrClosed
	}
	if len(entries) == 0 {
		return nil
	}
	s.writeMu.Lock()
	err := s.writeLocked(entries)
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	s.maybeRequestCheckpoint()
	return nil
}

// writeLocked performs a write with writeMu already held. PutIfAbsent and the
// TTL sweeper use it so their read-modify-write stays atomic.
func (s *Store) writeLocked(entries []entry) error {
	s.mu.RLock()
	seq := s.seq + 1
	s.mu.RUnlock()

	rec := record{seq: seq, entries: entries}
	if err := s.wal.append(rec); err != nil {
		return err
	}
	s.counters.walBytes.Store(s.wal.bytes)
	if s.opts.Sync == SyncAlways {
		if err := s.wal.sync(); err != nil {
			return err
		}
	}
	s.applyLocked(rec)
	for _, e := range entries {
		if e.op == opPut {
			s.counters.puts.Add(1)
		} else {
			s.counters.deletes.Add(1)
		}
	}
	return nil
}

func (s *Store) maybeRequestCheckpoint() {
	if s.counters.walBytes.Load() < s.opts.CheckpointBytes {
		return
	}
	select {
	case s.ckptReq <- struct{}{}:
	default:
	}
}

// Put stores value under key, replacing any previous value.
func (s *Store) Put(key, value []byte) error {
	return s.PutTTL(key, value, 0)
}

// PutTTL stores value under key with a time to live. A ttl of zero means the
// key never expires. TTL is how promotional prices clean themselves up on a
// gateway that may never hear from the cloud again before the promotion ends.
func (s *Store) PutTTL(key, value []byte, ttl time.Duration) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	e := entry{op: opPut, key: string(key), val: append([]byte(nil), value...)}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl).UnixNano()
	}
	return s.write([]entry{e})
}

// Delete removes key. Deleting an absent key is not an error, because the
// caller's intent — "this key must not be present" — is satisfied either way.
func (s *Store) Delete(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	return s.write([]entry{{op: opDelete, key: string(key)}})
}

// PutIfAbsent atomically stores value only when key is absent, deleted or
// expired, reporting whether it did. It is the primitive the idempotency guard
// at the integration gateway is built on: two simultaneous redeliveries of the
// same SAP IDoc race here, and exactly one of them wins.
func (s *Store) PutIfAbsent(key, value []byte, ttl time.Duration) (bool, error) {
	if len(key) == 0 {
		return false, ErrEmptyKey
	}
	if s.closed.Load() {
		return false, ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, ok := s.lookup(string(key), seqLatest); ok {
		return false, nil
	}
	e := entry{op: opPut, key: string(key), val: append([]byte(nil), value...)}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl).UnixNano()
	}
	if err := s.writeLocked([]entry{e}); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Read path
// ---------------------------------------------------------------------------

// seqLatest asks lookup for the newest committed value rather than a
// point-in-time one.
//
// It is deliberately a sequence no store can ever reach rather than zero. Zero
// is a perfectly ordinary sequence — it is what every store has before its
// first write, and what a store recovered from a checkpoint at sequence zero
// has — so a snapshot or iterator pinned there is a real point-in-time view
// with a real answer for every key: nothing exists yet. Spelling "latest" as
// zero silently turned exactly those views into live reads, which meant a
// snapshot taken on a store that had not been written to yet was not a snapshot
// at all: two Gets for one key through one pinned snapshot could return two
// different answers.
const seqLatest = ^uint64(0)

// lookup returns the value visible at seq, honouring TTL against the current
// wall clock. seqLatest asks for the newest committed value.
//
// The read lock covers resolving the sequence and loading the key's chain head,
// which is what ties the two together: the chain reachable from that head is
// guaranteed to still hold the version visible at that sequence, because a
// concurrent trim cannot have run between the two. Past that point the chain is
// immutable (see index.go), so the walk itself — which for a snapshot pinned
// well behind the write frontier can be thousands of versions long — runs with
// no lock held and cannot delay a writer.
func (s *Store) lookup(key string, seq uint64) ([]byte, bool) {
	now := time.Now().UnixNano()
	s.mu.RLock()
	if seq == seqLatest {
		seq = s.seq
	}
	var head *version
	if n := s.idx.get(key); n != nil {
		head = n.head.Load()
	}
	s.mu.RUnlock()
	if head == nil {
		return nil, false
	}
	return liveIn(head, seq, now)
}

// Get returns the value stored under key. The returned slice is a copy, so a
// caller that scribbles on it cannot corrupt the index.
func (s *Store) Get(key []byte) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	s.counters.gets.Add(1)
	v, ok := s.lookup(string(key), seqLatest)
	if !ok {
		s.counters.misses.Add(1)
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Has reports whether key holds a live value, without copying it.
func (s *Store) Has(key []byte) (bool, error) {
	if s.closed.Load() {
		return false, ErrClosed
	}
	if len(key) == 0 {
		return false, ErrEmptyKey
	}
	s.counters.gets.Add(1)
	_, ok := s.lookup(string(key), seqLatest)
	if !ok {
		s.counters.misses.Add(1)
	}
	return ok, nil
}

// ---------------------------------------------------------------------------
// Snapshot pinning
// ---------------------------------------------------------------------------

// pin registers a reader at the current sequence and returns it. Every pinned
// sequence holds back version trimming, which is why every pin must be released.
func (s *Store) pin() uint64 {
	s.mu.Lock()
	seq := s.seq
	s.pins[seq]++
	if len(s.pins) == 1 || seq < s.minPin {
		s.minPin = seq
	}
	s.mu.Unlock()
	return seq
}

func (s *Store) unpin(seq uint64) {
	s.mu.Lock()
	if n := s.pins[seq]; n > 1 {
		s.pins[seq] = n - 1
	} else {
		delete(s.pins, seq)
		s.recomputeMinPin()
	}
	s.mu.Unlock()
}

// recomputeMinPin must be called with mu held for writing.
func (s *Store) recomputeMinPin() {
	if len(s.pins) == 0 {
		s.minPin = s.seq
		return
	}
	min := ^uint64(0)
	for k := range s.pins {
		if k < min {
			min = k
		}
	}
	s.minPin = min
}

// ---------------------------------------------------------------------------
// Range helpers
// ---------------------------------------------------------------------------

// prefixEnd returns the exclusive upper bound of the key range covered by
// prefix, or nil when the prefix is all 0xFF bytes and therefore unbounded.
func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// collect materialises the nodes in [start, end) under the read lock. The
// iterator then reads values with no lock at all, so a full-catalogue scan for
// a compliance export never blocks the price hot path. See index.go.
func (s *Store) collect(start, end []byte) []*node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n *node
	if len(start) == 0 {
		n = s.idx.first()
	} else {
		n = s.idx.seek(string(start))
	}
	var out []*node
	for ; n != nil; n = n.tower[0] {
		if end != nil && n.key >= string(end) {
			break
		}
		out = append(out, n)
	}
	return out
}

// Scan returns an iterator over every key with the given prefix, in ascending
// key order. An empty prefix scans the whole store. The iterator must be closed.
func (s *Store) Scan(prefix []byte) *Iterator {
	return s.Range(prefix, prefixEnd(prefix))
}

// Range returns an iterator over [start, end) in ascending key order. A nil or
// empty start begins at the first key; a nil end runs to the last. The iterator
// must be closed, and reads a consistent view taken at the moment it was made.
func (s *Store) Range(start, end []byte) *Iterator {
	if s.closed.Load() {
		return &Iterator{err: ErrClosed}
	}
	seq := s.pin()
	return &Iterator{
		store: s,
		seq:   seq,
		nodes: s.collect(start, end),
		now:   time.Now().UnixNano(),
		pos:   -1,
	}
}

// ---------------------------------------------------------------------------
// Stats and lifecycle
// ---------------------------------------------------------------------------

// Stats is a point-in-time view of store health. It is what the SGU's local
// diagnostics page shows a field engineer who has driven to a store because
// "the labels are wrong".
type Stats struct {
	// Dir is the backing directory.
	Dir string
	// Keys counts keys whose newest version is not a tombstone. Keys that have
	// passed their TTL but not yet been swept are still counted; reads already
	// treat them as absent.
	Keys int64
	// Sequence is the latest committed write sequence, i.e. the number of
	// atomic write groups the store has applied since it was created.
	Sequence uint64
	// WALBytes is the size of the active write-ahead log. It is the direct
	// predictor of recovery time.
	WALBytes int64
	// SnapshotSequence is the sequence the last checkpoint captured.
	SnapshotSequence uint64
	// SnapshotAge is how long ago that checkpoint was taken.
	SnapshotAge time.Duration
	// Checkpoints counts successful checkpoint (compaction) runs.
	Checkpoints uint64
	// ActiveSnapshots counts open snapshots and iterators holding history back.
	ActiveSnapshots int
	// Puts, Deletes, Gets, Misses and Expired are lifetime counters.
	Puts    uint64
	Deletes uint64
	Gets    uint64
	Misses  uint64
	Expired uint64
	// Sync is the configured durability policy.
	Sync SyncPolicy
}

// Stats returns current store statistics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	keys := s.liveKeys
	seq := s.seq
	pins := len(s.pins)
	s.mu.RUnlock()
	snapAt := time.Unix(0, s.snapAt.Load())
	st := Stats{
		Dir:              s.dir,
		Keys:             keys,
		Sequence:         seq,
		WALBytes:         s.counters.walBytes.Load(),
		SnapshotSequence: s.snapSeq.Load(),
		SnapshotAge:      time.Since(snapAt),
		Checkpoints:      s.counters.ckpts.Load(),
		ActiveSnapshots:  pins,
		Puts:             s.counters.puts.Load(),
		Deletes:          s.counters.deletes.Load(),
		Gets:             s.counters.gets.Load(),
		Misses:           s.counters.misses.Load(),
		Expired:          s.counters.expired.Load(),
		Sync:             s.opts.Sync,
	}
	s.publish(st)
	return st
}

func (s *Store) publish(st Stats) {
	if s.metrics == nil {
		return
	}
	s.metrics.keys.Set(float64(st.Keys))
	s.metrics.walBytes.Set(float64(st.WALBytes))
	s.metrics.snapshotAge.Set(st.SnapshotAge.Seconds())
	s.metrics.activeSnaps.Set(float64(st.ActiveSnapshots))
}

func (s *Store) startBackground() {
	if s.opts.Sync == SyncEvery {
		s.wg.Add(1)
		go s.syncLoop()
	}
	s.wg.Add(1)
	go s.expireLoop()
	s.wg.Add(1)
	go s.checkpointLoop()
}

func (s *Store) syncLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.opts.SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.writeMu.Lock()
			// A failed background fsync is reported through the next
			// foreground write rather than swallowed silently: the next
			// append to the same file will fail the same way.
			_ = s.wal.sync()
			s.writeMu.Unlock()
		}
	}
}

func (s *Store) expireLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.opts.ExpireEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			_ = s.expireOnce()
			// Stats refreshes the gauges; doing it on the sweep tick means an
			// idle store still reports a growing snapshot age.
			s.Stats()
		}
	}
}

// expireOnce tombstones every key whose TTL has passed, so the memory and the
// on-disk checkpoint both shrink. Reads already ignore expired keys, so this is
// reclamation, not correctness — which is why a failure here is not fatal.
func (s *Store) expireOnce() error {
	if s.closed.Load() {
		return ErrClosed
	}
	now := time.Now().UnixNano()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	seq := s.seq
	var dead []entry
	for n := s.idx.first(); n != nil; n = n.tower[0] {
		v := n.visible(seq)
		if v != nil && !v.tomb && v.expiresAt != 0 && now >= v.expiresAt {
			dead = append(dead, entry{op: opDelete, key: n.key})
		}
	}
	s.mu.RUnlock()
	if len(dead) == 0 {
		return nil
	}
	if err := s.writeLocked(dead); err != nil {
		return err
	}
	s.counters.expired.Add(uint64(len(dead)))
	return nil
}

func (s *Store) checkpointLoop() {
	defer s.wg.Done()
	var tc <-chan time.Time
	if s.opts.CheckpointEvery > 0 {
		t := time.NewTicker(s.opts.CheckpointEvery)
		defer t.Stop()
		tc = t.C
	}
	for {
		select {
		case <-s.stop:
			return
		case <-s.ckptReq:
			_ = s.Checkpoint()
		case <-tc:
			_ = s.Checkpoint()
		}
	}
}

// Close flushes and closes the store, stops its background goroutines, and
// removes the directory if it was a temporary one. Operations after Close
// return ErrClosed rather than panicking, because a shutdown race in a gateway
// must degrade, not crash.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()

	s.writeMu.Lock()
	err := s.wal.sync()
	if cerr := s.wal.close(); err == nil {
		err = cerr
	}
	s.writeMu.Unlock()

	if s.tempDir {
		if rerr := os.RemoveAll(s.dir); err == nil {
			err = rerr
		}
	}
	return err
}
