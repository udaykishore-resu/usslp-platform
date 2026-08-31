// Package eventstore is the CQRS write side of USSLP: an append-only,
// optimistically concurrent event store built on the embedded kvstore.
//
// USSLP's Label and Pricing aggregates are event sourced because pricing is a
// regulated activity. A price on a shelf edge has to be explainable months
// later — who changed it, from what, on whose authority, and in what order
// relative to every other change — and a mutable row in a table cannot answer
// that. Here the events *are* the record: current state is a projection that
// can be thrown away and rebuilt, while the stream is immutable and is what the
// weights-and-measures audit reads.
//
// Three properties carry most of the weight:
//
//   - Optimistic concurrency. Append takes the version the caller believed the
//     stream was at. If an NCR till and a Shopify webhook both try to reprice
//     the same SKU from version 41, exactly one of them lands and the other
//     gets ErrConcurrency and re-reads. Nothing is silently lost, and no
//     distributed lock is needed to get there.
//   - A global monotonic position. Every event gets a position in one total
//     order, so a projection that has consumed up to position N can resume at
//     N+1 and rebuild deterministically, on any replica, at any time.
//   - Idempotent append. A POS webhook that is redelivered — and they are all
//     redelivered eventually — carries the same idempotency key, and the second
//     append is a no-op that returns the original event rather than creating a
//     second price change.
//
// Storage layout inside the kvstore, all keys byte-ordered:
//
//	g\0<position:be8>            -> the full serialised record
//	s\0<stream>\0<version:be8>   -> the position of that event
//	h\0<stream>                  -> current version of the stream
//	i\0<stream>\0<idemkey>       -> position of the event that claimed the key
//	p\0<stream>                  -> latest aggregate snapshot
//	c\0<name>                    -> a projection's durable checkpoint
//	m\0pos                       -> the last assigned global position
//
// The full record lives under the global key and the per-stream key holds only
// a pointer. That asymmetry is deliberate: ReadAll is the high-volume path —
// every projection, every subscription and every audit export walks it — and it
// gets a single ordered scan with no indirection, while ReadStream pays one
// extra lookup per event on a path that aggregate snapshots keep short anyway.
//
// A Store is safe for concurrent use by any number of goroutines.
package eventstore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// StreamID names one aggregate's event stream, conventionally
// "<aggregate-type>/<aggregate-id>", e.g. "label/lbl-0f21".
type StreamID string

// String renders the stream identifier.
func (s StreamID) String() string { return string(s) }

// Stream builds a StreamID from an aggregate type and identifier. Using it
// rather than hand-formatting keeps the naming convention in one place, which
// matters because the stream name is baked into every stored key.
func Stream(aggregateType, aggregateID string) StreamID {
	return StreamID(aggregateType + "/" + aggregateID)
}

// Split returns the aggregate type and identifier encoded in a stream name.
func (s StreamID) Split() (aggregateType, aggregateID string) {
	str := string(s)
	if i := strings.IndexByte(str, '/'); i >= 0 {
		return str[:i], str[i+1:]
	}
	return str, ""
}

// ExpectedAny disables the optimistic concurrency check for an append.
//
// It is the right choice only where the events are independent facts rather
// than decisions derived from current state — device telemetry, mesh topology
// reports, delivery acknowledgements. Using it for a price change would defeat
// the entire point of the write side: two tills would both "succeed" and one
// price would silently win.
const ExpectedAny int64 = -1

// ExpectedNoStream asserts that the stream does not exist yet. It is how a
// label is provisioned exactly once even if the provisioning job is retried.
const ExpectedNoStream int64 = 0

// Errors returned by the store. They are sentinels so callers can branch:
// ErrConcurrency means "re-read and retry", everything else does not.
var (
	// ErrConcurrency reports that the stream moved on between the caller
	// reading it and appending to it. The caller must reload the aggregate and
	// re-decide; retrying the same command blindly would apply a decision made
	// against stale state.
	ErrConcurrency = errors.New("eventstore: concurrency conflict")
	// ErrNoSnapshot reports that a stream has no saved aggregate snapshot.
	ErrNoSnapshot = errors.New("eventstore: no snapshot")
	// ErrPartialDuplicate reports an append where some events carried
	// idempotency keys already used on the stream and some did not. That can
	// only happen if a producer retried a batch with different contents, which
	// is a producer bug; half-applying it would corrupt the audit trail.
	ErrPartialDuplicate = errors.New("eventstore: append mixes new and already-appended events")
	// ErrInvalidStream rejects a stream name that cannot be encoded as a key.
	ErrInvalidStream = errors.New("eventstore: invalid stream id")
	// ErrClosed is returned once the store has been closed.
	ErrClosed = errors.New("eventstore: store is closed")
)

// Recorded is one event as it sits in the log: the caller's envelope plus the
// two coordinates the store assigned it.
type Recorded struct {
	// Position is the event's place in the single global order. It is strictly
	// increasing across the whole store and never reused.
	Position int64 `json:"position"`
	// Stream is the aggregate stream the event belongs to.
	Stream StreamID `json:"stream"`
	// Version is the event's place within its stream, starting at 1.
	Version int64 `json:"version"`
	// Event is the immutable envelope as appended.
	Event canon.Envelope `json:"event"`
}

// AppendResult describes what an append did.
type AppendResult struct {
	// Duplicate is true when every event in the call had already been appended
	// under its idempotency key and nothing new was written.
	Duplicate bool
	// Events are the stored events: the newly written ones, or — for a
	// duplicate — the originals from the first delivery.
	Events []Recorded
	// FirstVersion and LastVersion bound the stream versions involved.
	FirstVersion int64
	// LastVersion is the stream version after the append.
	LastVersion int64
	// LastPosition is the global position of the last event involved.
	LastPosition int64
}

// Stats is a lifetime activity summary, used by tests and by the admin surface
// to answer "is the write side healthy and is anything conflicting".
type Stats struct {
	// Appends counts successful append calls that wrote something.
	Appends uint64
	// Events counts individual events written.
	Events uint64
	// Conflicts counts appends rejected by the optimistic concurrency check.
	Conflicts uint64
	// Duplicates counts appends short-circuited by an idempotency key.
	Duplicates uint64
	// EventsRead counts events materialised by ReadStream and ReadAll. It is
	// the number that shows an aggregate snapshot is actually saving work.
	EventsRead uint64
	// LastPosition is the highest assigned global position.
	LastPosition int64
	// Subscribers counts live catch-up subscriptions.
	Subscribers int
}

// Store is an append-only event store.
type Store struct {
	kv     *kvstore.Store
	ownsKV bool

	// appendMu serialises appends. It guards the assignment of global positions
	// and stream versions and is held across the durable write and the fan-out
	// to subscribers, which is what guarantees subscribers observe events in
	// exactly global position order with no reordering window.
	appendMu sync.Mutex
	lastPos  atomic.Int64

	subMu sync.RWMutex
	subs  map[*subscriber]struct{}

	closed atomic.Bool
	stats  struct {
		appends    atomic.Uint64
		events     atomic.Uint64
		conflicts  atomic.Uint64
		duplicates atomic.Uint64
		reads      atomic.Uint64
	}
}

// New builds an event store over an existing kvstore. The caller keeps
// ownership of the kvstore, which lets the event store and the read models it
// feeds share one write-ahead log and therefore one atomic write per command.
func New(kv *kvstore.Store) (*Store, error) {
	if kv == nil {
		return nil, errors.New("eventstore: nil kvstore")
	}
	s := &Store{kv: kv, subs: make(map[*subscriber]struct{})}
	pos, err := s.readInt64(metaPosKey)
	if err != nil {
		return nil, err
	}
	s.lastPos.Store(pos)
	return s, nil
}

// Open creates an event store with its own kvstore in dir, which Close then
// closes. An empty dir uses a temporary directory removed on Close.
func Open(dir string) (*Store, error) {
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
	if err != nil {
		return nil, err
	}
	s, err := New(kv)
	if err != nil {
		kv.Close()
		return nil, err
	}
	s.ownsKV = true
	return s, nil
}

// KV exposes the underlying store so a caller can put its read models in the
// same durable unit as the events that produced them.
func (s *Store) KV() *kvstore.Store { return s.kv }

// Close releases the store, waking every subscription. It closes the underlying
// kvstore only if this store created it.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.subMu.Lock()
	for sub := range s.subs {
		sub.close()
	}
	s.subs = make(map[*subscriber]struct{})
	s.subMu.Unlock()
	if s.ownsKV {
		return s.kv.Close()
	}
	return nil
}

// Stats returns lifetime activity counters.
func (s *Store) Stats() Stats {
	s.subMu.RLock()
	n := len(s.subs)
	s.subMu.RUnlock()
	return Stats{
		Appends:      s.stats.appends.Load(),
		Events:       s.stats.events.Load(),
		Conflicts:    s.stats.conflicts.Load(),
		Duplicates:   s.stats.duplicates.Load(),
		EventsRead:   s.stats.reads.Load(),
		LastPosition: s.lastPos.Load(),
		Subscribers:  n,
	}
}

// LastPosition returns the highest global position assigned so far. A
// projection that has reached it is fully caught up.
func (s *Store) LastPosition() int64 { return s.lastPos.Load() }

// ---------------------------------------------------------------------------
// Key encoding
// ---------------------------------------------------------------------------

var metaPosKey = []byte("m\x00pos")

func be8(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func gKey(pos int64) []byte {
	b := make([]byte, 10)
	b[0], b[1] = 'g', 0
	binary.BigEndian.PutUint64(b[2:], uint64(pos))
	return b
}

var gPrefix = []byte("g\x00")

func streamPrefix(tag byte, stream StreamID) []byte {
	b := make([]byte, 0, 3+len(stream))
	b = append(b, tag, 0)
	b = append(b, stream...)
	b = append(b, 0)
	return b
}

func sKey(stream StreamID, version int64) []byte {
	return append(streamPrefix('s', stream), be8(version)...)
}

func hKey(stream StreamID) []byte {
	b := make([]byte, 0, 2+len(stream))
	return append(append(b, 'h', 0), stream...)
}

func iKey(stream StreamID, idem string) []byte {
	return append(streamPrefix('i', stream), idem...)
}

func pKey(stream StreamID) []byte {
	b := make([]byte, 0, 2+len(stream))
	return append(append(b, 'p', 0), stream...)
}

func cKey(name string) []byte {
	b := make([]byte, 0, 2+len(name))
	return append(append(b, 'c', 0), name...)
}

func validStream(stream StreamID) error {
	if stream == "" {
		return fmt.Errorf("%w: empty", ErrInvalidStream)
	}
	if strings.ContainsRune(string(stream), 0) {
		return fmt.Errorf("%w: %q contains NUL", ErrInvalidStream, stream)
	}
	return nil
}

func (s *Store) readInt64(key []byte) (int64, error) {
	v, err := s.kv.Get(key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("eventstore: corrupt counter at %q", key)
	}
	return int64(binary.BigEndian.Uint64(v)), nil
}

func (s *Store) readRecord(pos int64) (Recorded, error) {
	raw, err := s.kv.Get(gKey(pos))
	if err != nil {
		return Recorded{}, err
	}
	var r Recorded
	if err := json.Unmarshal(raw, &r); err != nil {
		return Recorded{}, fmt.Errorf("eventstore: decode record at position %d: %w", pos, err)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Append
// ---------------------------------------------------------------------------

// Version returns the current version of a stream; 0 means the stream does not
// exist. It is the value a caller passes back as expectedVersion.
func (s *Store) Version(ctx context.Context, stream StreamID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.closed.Load() {
		return 0, ErrClosed
	}
	if err := validStream(stream); err != nil {
		return 0, err
	}
	return s.readInt64(hKey(stream))
}

// Append writes events to a stream under optimistic concurrency control.
//
// expectedVersion is the stream version the caller made its decision against.
// Pass ExpectedNoStream to require that the stream is new, ExpectedAny to skip
// the check, or the version returned by a previous read. A mismatch returns
// ErrConcurrency and writes nothing.
//
// Events carrying an IdempotencyKey already seen on this stream make the whole
// call a no-op; use AppendWithResult to recover the originals.
func (s *Store) Append(ctx context.Context, stream StreamID, expectedVersion int64, events ...canon.Envelope) error {
	_, err := s.AppendWithResult(ctx, stream, expectedVersion, events...)
	return err
}

// AppendWithResult is Append with the outcome reported back: the versions and
// positions assigned, and — when an idempotency key had already been used —
// the original events from the first delivery. A POS adapter answering a
// retried webhook needs those originals to return the same response it
// returned the first time.
func (s *Store) AppendWithResult(ctx context.Context, stream StreamID, expectedVersion int64, events ...canon.Envelope) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if s.closed.Load() {
		return AppendResult{}, ErrClosed
	}
	if err := validStream(stream); err != nil {
		return AppendResult{}, err
	}
	if len(events) == 0 {
		return AppendResult{}, nil
	}
	aggType, aggID := stream.Split()
	for i := range events {
		if events[i].AggregateType == "" {
			events[i].AggregateType = aggType
		}
		if events[i].AggregateID == "" {
			events[i].AggregateID = aggID
		}
		if events[i].SchemaVersion == 0 {
			events[i].SchemaVersion = canon.SchemaVersion
		}
		if err := events[i].Validate(); err != nil {
			return AppendResult{}, fmt.Errorf("eventstore: event %d: %w", i, err)
		}
	}

	s.appendMu.Lock()
	res, records, err := s.appendLocked(stream, expectedVersion, events)
	if err != nil {
		s.appendMu.Unlock()
		return AppendResult{}, err
	}
	// Fan out while still holding appendMu so that subscribers observe events
	// in exactly global-position order. Enqueueing is O(1) and never blocks on
	// a slow consumer, so this does not couple append latency to subscribers.
	if len(records) > 0 {
		s.subMu.RLock()
		for sub := range s.subs {
			sub.enqueue(records)
		}
		s.subMu.RUnlock()
	}
	s.appendMu.Unlock()
	return res, nil
}

// appendLocked performs the durable part of an append with appendMu held. It
// returns the newly written records (empty for an idempotent no-op).
func (s *Store) appendLocked(stream StreamID, expectedVersion int64, events []canon.Envelope) (AppendResult, []Recorded, error) {
	head, err := s.readInt64(hKey(stream))
	if err != nil {
		return AppendResult{}, nil, err
	}

	// Idempotency is checked before the concurrency check. A redelivered
	// webhook is not a conflict — the work was already done — and reporting it
	// as one would send an adapter into a pointless read-and-retry loop.
	dup, res, err := s.checkIdempotent(stream, events)
	if err != nil {
		return AppendResult{}, nil, err
	}
	if dup {
		s.stats.duplicates.Add(1)
		return res, nil, nil
	}

	if expectedVersion != ExpectedAny && expectedVersion != head {
		s.stats.conflicts.Add(1)
		return AppendResult{}, nil, fmt.Errorf("%w: stream %s is at version %d, caller expected %d",
			ErrConcurrency, stream, head, expectedVersion)
	}

	now := time.Now().UTC()
	pos := s.lastPos.Load()
	batch := s.kv.NewBatch()
	records := make([]Recorded, 0, len(events))
	version := head
	for i := range events {
		version++
		pos++
		env := events[i]
		env.Version = version
		if env.RecordedAt.IsZero() {
			env.RecordedAt = now
		}
		rec := Recorded{Position: pos, Stream: stream, Version: version, Event: env}
		body, err := json.Marshal(rec)
		if err != nil {
			return AppendResult{}, nil, fmt.Errorf("eventstore: encode event %d: %w", i, err)
		}
		batch.Put(gKey(pos), body)
		batch.Put(sKey(stream, version), be8(pos))
		if env.IdempotencyKey != "" {
			batch.Put(iKey(stream, env.IdempotencyKey), be8(pos))
		}
		records = append(records, rec)
	}
	batch.Put(hKey(stream), be8(version))
	batch.Put(metaPosKey, be8(pos))

	// One atomic write: either the events, the stream head, the idempotency
	// index and the global position counter all land, or none of them do.
	// Anything less and recovery could produce a stream whose head disagrees
	// with its events.
	if err := batch.Write(); err != nil {
		return AppendResult{}, nil, err
	}
	s.lastPos.Store(pos)
	s.stats.appends.Add(1)
	s.stats.events.Add(uint64(len(records)))

	return AppendResult{
		Events:       records,
		FirstVersion: head + 1,
		LastVersion:  version,
		LastPosition: pos,
	}, records, nil
}

// checkIdempotent reports whether every keyed event in the batch has already
// been appended to the stream, returning the originals if so.
func (s *Store) checkIdempotent(stream StreamID, events []canon.Envelope) (bool, AppendResult, error) {
	keyed, seen := 0, 0
	var positions []int64
	for i := range events {
		k := events[i].IdempotencyKey
		if k == "" {
			continue
		}
		keyed++
		pos, err := s.readInt64(iKey(stream, k))
		if err != nil {
			return false, AppendResult{}, err
		}
		if pos > 0 {
			seen++
			positions = append(positions, pos)
		}
	}
	if seen == 0 {
		return false, AppendResult{}, nil
	}
	if seen != keyed {
		return false, AppendResult{}, fmt.Errorf("%w: stream %s, %d of %d events already appended",
			ErrPartialDuplicate, stream, seen, keyed)
	}
	res := AppendResult{Duplicate: true}
	for _, pos := range positions {
		rec, err := s.readRecord(pos)
		if err != nil {
			return false, AppendResult{}, err
		}
		res.Events = append(res.Events, rec)
	}
	if n := len(res.Events); n > 0 {
		res.FirstVersion = res.Events[0].Version
		res.LastVersion = res.Events[n-1].Version
		res.LastPosition = res.Events[n-1].Position
	}
	return true, res, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ReadStream returns events from one stream starting at fromVersion inclusive,
// at most limit of them (limit <= 0 means no limit). Stream versions start at
// 1, so ReadStream(ctx, stream, 1, 0) is the whole stream and
// ReadStream(ctx, stream, snapshot.Version+1, 0) is everything a snapshot does
// not already cover.
func (s *Store) ReadStream(ctx context.Context, stream StreamID, fromVersion int64, limit int) ([]Recorded, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if err := validStream(stream); err != nil {
		return nil, err
	}
	if fromVersion < 1 {
		fromVersion = 1
	}
	prefix := streamPrefix('s', stream)
	start := append(append([]byte(nil), prefix...), be8(fromVersion)...)

	it := s.kv.Range(start, prefixEnd(prefix))
	defer it.Close()

	var out []Recorded
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v := it.Value()
		if len(v) != 8 {
			return nil, fmt.Errorf("eventstore: corrupt stream index for %s", stream)
		}
		rec, err := s.readRecord(int64(binary.BigEndian.Uint64(v)))
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	s.stats.reads.Add(uint64(len(out)))
	return out, nil
}

// ReadAll returns events in global position order starting at fromPosition
// inclusive, at most limit of them (limit <= 0 means no limit). Positions start
// at 1, so a projection holding checkpoint N resumes with ReadAll(ctx, N+1, …)
// and is guaranteed to see every event exactly once, in the same order, on
// every replay.
func (s *Store) ReadAll(ctx context.Context, fromPosition int64, limit int) ([]Recorded, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if fromPosition < 1 {
		fromPosition = 1
	}
	it := s.kv.Range(gKey(fromPosition), prefixEnd(gPrefix))
	defer it.Close()

	var out []Recorded
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r Recorded
		if err := json.Unmarshal(it.Value(), &r); err != nil {
			return nil, fmt.Errorf("eventstore: decode record: %w", err)
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	s.stats.reads.Add(uint64(len(out)))
	return out, nil
}

// prefixEnd returns the exclusive upper bound of the range covered by prefix.
// Every prefix this package builds ends in a NUL separator or is a fixed tag,
// so incrementing the final byte is always a valid bound.
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

// ---------------------------------------------------------------------------
// Aggregate snapshots
// ---------------------------------------------------------------------------

// Snapshot is a serialised aggregate state plus the stream version it reflects.
type Snapshot struct {
	// Stream identifies the aggregate.
	Stream StreamID `json:"stream"`
	// Version is the stream version the state includes. Replay resumes at
	// Version+1.
	Version int64 `json:"version"`
	// State is the caller's encoding of the aggregate. The store never
	// interprets it; snapshots are an optimisation, and the events remain the
	// only authority.
	State []byte `json:"state"`
	// SavedAt is when the snapshot was written.
	SavedAt time.Time `json:"saved_at"`
}

// SaveSnapshot stores the aggregate state for a stream, replacing any earlier
// snapshot.
//
// This is what keeps aggregate loading bounded. A long-lived label in a
// high-churn category accumulates tens of thousands of price changes; without a
// snapshot, every command against it replays all of them. With one, the write
// side reads a blob plus the handful of events since.
//
// Snapshots are strictly derived data. Losing or ignoring one costs time, never
// correctness, which is why they are written outside the append path and are
// not part of any concurrency check.
func (s *Store) SaveSnapshot(stream StreamID, version int64, state []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	if err := validStream(stream); err != nil {
		return err
	}
	if version < 0 {
		return fmt.Errorf("eventstore: negative snapshot version for %s", stream)
	}
	body, err := json.Marshal(Snapshot{
		Stream:  stream,
		Version: version,
		State:   state,
		SavedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("eventstore: encode snapshot for %s: %w", stream, err)
	}
	return s.kv.Put(pKey(stream), body)
}

// LoadSnapshot returns the most recent snapshot for a stream, or ErrNoSnapshot.
func (s *Store) LoadSnapshot(stream StreamID) (Snapshot, error) {
	if s.closed.Load() {
		return Snapshot{}, ErrClosed
	}
	if err := validStream(stream); err != nil {
		return Snapshot{}, err
	}
	raw, err := s.kv.Get(pKey(stream))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Snapshot{}, fmt.Errorf("%w for stream %s", ErrNoSnapshot, stream)
	}
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("eventstore: decode snapshot for %s: %w", stream, err)
	}
	return snap, nil
}

// DeleteSnapshot removes a stream's snapshot, forcing the next load to replay
// from the beginning. It is the escape hatch for a snapshot written by a
// version of the aggregate whose state encoding has since changed.
func (s *Store) DeleteSnapshot(stream StreamID) error {
	if s.closed.Load() {
		return ErrClosed
	}
	if err := validStream(stream); err != nil {
		return err
	}
	return s.kv.Delete(pKey(stream))
}
