package sgu

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// ---------------------------------------------------------------------------
// The durable upstream buffer
//
// While the WAN is down, everything the store would have told the cloud has to
// go somewhere, survive a power cut, and come back in order. That is the whole
// job of this queue, and the interesting part of it is what happens when it
// fills up, because a bounded queue that never explains its own overflow is
// just a slower way to lose data silently.
// ---------------------------------------------------------------------------

// ErrQueueClosed is returned after the queue's store has been closed.
var ErrQueueClosed = errors.New("sgu: upstream queue closed")

// Class determines what happens to a buffered message when the queue is full.
type Class string

// The three classes, in the order they are sacrificed.
const (
	// ClassBulk is high-volume, low-value telemetry. Losing an hour of battery
	// readings costs an analytics dashboard some resolution and nothing else, so
	// it is dropped first and dropped oldest-first.
	ClassBulk Class = "bulk"
	// ClassLatest is state whose only useful version is the newest: a
	// controller's heartbeat, a mesh topology report. These are coalesced by
	// topic on the way in — a second heartbeat from the same controller replaces
	// the first — so they cost bounded space no matter how long the outage runs.
	ClassLatest Class = "latest"
	// ClassCritical is evidence: delivery acknowledgements, price rejections,
	// store mode transitions. Every one is a fact about what a shopper could see
	// on a shelf, and the compliance archive is meant to be complete. These are
	// dropped only when nothing else is left, and dropping one marks the whole
	// reconciliation lossy so that a human is told rather than left to infer it.
	ClassCritical Class = "critical"
)

// Entry is one buffered upstream message.
type Entry struct {
	Seq        uint64     `json:"seq"`
	Topic      string     `json:"topic"`
	Payload    []byte     `json:"payload"`
	QoS        msgbus.QoS `json:"qos"`
	Retain     bool       `json:"retain"`
	Class      Class      `json:"class"`
	EnqueuedAt time.Time  `json:"enqueued_at"`
	// IdempotencyKey is what makes the flush safe to repeat. It comes from the
	// envelope's own event identifier, so a message the gateway published
	// successfully and then failed to remove from the queue — the classic
	// crash-after-ack — is recognised on the next attempt rather than delivered
	// twice.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// TS is the hybrid clock stamp at enqueue, which is what orders the flush
	// against anything the cloud did in the meantime.
	TS HLC `json:"ts"`
}

// Message renders the entry as a publishable message.
func (e Entry) Message() msgbus.Message {
	return msgbus.Message{Topic: e.Topic, Payload: e.Payload, QoS: e.QoS, Retain: e.Retain}
}

// QueueConfig bounds the buffer.
type QueueConfig struct {
	// MaxEntries bounds the queue by count. Zero means 50,000: at the platform's
	// upstream rates that is roughly twelve hours of a busy store's
	// acknowledgements, which is longer than any outage a retailer will tolerate
	// without sending an engineer.
	MaxEntries int
	// MaxBytes bounds it by size, which is the limit that actually binds on a
	// gateway with a 32 GB industrial SD card shared with the label replica.
	// Zero means 256 MiB.
	MaxBytes int64
	// SentTTL is how long a successfully published idempotency key is
	// remembered. Zero means 24 hours, matching the platform's ingress
	// deduplication window.
	SentTTL time.Duration
}

func (c QueueConfig) withDefaults() QueueConfig {
	if c.MaxEntries == 0 {
		c.MaxEntries = 50000
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 256 << 20
	}
	if c.SentTTL == 0 {
		c.SentTTL = 24 * time.Hour
	}
	return c
}

// QueueStats is what the gateway's diagnostics page and its heartbeat report.
type QueueStats struct {
	Depth        int    `json:"depth"`
	Bytes        int64  `json:"bytes"`
	MaxEntries   int    `json:"max_entries"`
	MaxBytes     int64  `json:"max_bytes"`
	Enqueued     uint64 `json:"enqueued_total"`
	Flushed      uint64 `json:"flushed_total"`
	Coalesced    uint64 `json:"coalesced_total"`
	DroppedBulk  uint64 `json:"dropped_bulk_total"`
	DroppedOther uint64 `json:"dropped_critical_total"`
	Deduplicated uint64 `json:"deduplicated_total"`
	// Lossy latches once a critical message has been dropped. It is cleared only
	// by a successful reconciliation that reports it.
	Lossy bool `json:"lossy"`
	// Oldest is the age of the head of the queue, which is the number that tells
	// an operator how far behind the store is.
	Oldest time.Duration `json:"oldest"`
}

// Queue is the durable, bounded, ordered upstream buffer.
//
// Safe for concurrent use: the bridge enqueues from the broker's dispatch pool
// while the reconciler drains from its own goroutine.
type Queue struct {
	cfg   QueueConfig
	store *kvstore.Store

	mu    sync.Mutex
	next  uint64
	depth int
	bytes int64
	// latest maps a coalescable topic to the sequence currently holding it, so
	// a replacement can remove its predecessor in the same batch.
	latest map[string]uint64
	// order is the sequence numbers in queue order. Keeping it in memory rather
	// than re-scanning the store on every operation is what keeps enqueue O(1)
	// on a gateway that may be buffering thousands of messages an hour.
	order []queued
	stats QueueStats
}

// queued is the in-memory index entry.
type queued struct {
	seq   uint64
	class Class
	size  int64
	topic string
	at    time.Time
}

const queuePrefix = "q/up/"
const sentPrefix = "q/sent/"

// queueKey renders a sequence number as a fixed-width hexadecimal key.
//
// Fixed width because kvstore iterates in byte order: a key of "q/up/9" would
// sort after "q/up/10" and the buffer would flush out of order, which is the
// one property it exists to guarantee.
func queueKey(seq uint64) []byte {
	return fmt.Appendf(make([]byte, 0, len(queuePrefix)+16), "%s%016x", queuePrefix, seq)
}

// NewQueue opens the buffer over a durable store, restoring anything a previous
// process left behind.
//
// Restoration is the point of the type: a gateway that loses power mid-outage
// comes back with every acknowledgement it had not yet delivered, in the order
// it accepted them.
func NewQueue(store *kvstore.Store, cfg QueueConfig) (*Queue, error) {
	if store == nil {
		return nil, errors.New("sgu: upstream queue needs a durable store")
	}
	q := &Queue{cfg: cfg.withDefaults(), store: store, latest: map[string]uint64{}}
	it := store.Scan([]byte(queuePrefix))
	defer it.Close()
	for it.Next() {
		var e Entry
		if err := json.Unmarshal(it.Value(), &e); err != nil {
			// A torn record cannot be republished and cannot be ordered. Dropping
			// it and saying so is better than refusing to start a store gateway.
			continue
		}
		size := int64(len(it.Value()))
		q.order = append(q.order, queued{seq: e.Seq, class: e.Class, size: size, topic: e.Topic, at: e.EnqueuedAt})
		q.depth++
		q.bytes += size
		if e.Seq >= q.next {
			q.next = e.Seq + 1
		}
		if e.Class == ClassLatest {
			q.latest[e.Topic] = e.Seq
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("sgu: restoring the upstream queue: %w", err)
	}
	return q, nil
}

// Enqueue appends a message, applying coalescing and the overflow policy.
func (q *Queue) Enqueue(e Entry) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	e.Seq = q.next
	q.next++
	if e.EnqueuedAt.IsZero() {
		e.EnqueuedAt = time.Now().UTC()
	}
	if e.Class == "" {
		e.Class = ClassCritical
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("sgu: encoding a buffered message for %s: %w", e.Topic, err)
	}

	batch := q.store.NewBatch()
	batch.Put(queueKey(e.Seq), body)

	// Coalesce: a topic whose only useful version is the newest keeps exactly
	// one entry, however long the outage lasts.
	if e.Class == ClassLatest {
		if prev, ok := q.latest[e.Topic]; ok {
			batch.Delete(queueKey(prev))
			q.removeLocked(prev)
			q.stats.Coalesced++
		}
		q.latest[e.Topic] = e.Seq
	}

	size := int64(len(body))
	q.order = append(q.order, queued{seq: e.Seq, class: e.Class, size: size, topic: e.Topic, at: e.EnqueuedAt})
	q.depth++
	q.bytes += size
	q.stats.Enqueued++

	q.evictLocked(batch)

	if err := batch.Write(); err != nil {
		return fmt.Errorf("sgu: buffering a message for %s: %w", e.Topic, err)
	}
	return nil
}

// evictLocked enforces the bounds.
//
// The order of sacrifice is the documented overflow policy: bulk telemetry
// first, oldest first; then coalescable state; then, only if the queue is
// entirely critical evidence, the oldest critical message — which latches the
// lossy flag so the reconciliation report says plainly that the cloud's record
// of this outage has a hole in it.
func (q *Queue) evictLocked(batch *kvstore.Batch) {
	over := func() bool {
		return q.depth > q.cfg.MaxEntries || q.bytes > q.cfg.MaxBytes
	}
	for _, class := range []Class{ClassBulk, ClassLatest, ClassCritical} {
		for over() {
			idx := -1
			for i, e := range q.order {
				if e.class == class {
					idx = i
					break
				}
			}
			if idx < 0 {
				break
			}
			victim := q.order[idx]
			batch.Delete(queueKey(victim.seq))
			q.order = append(q.order[:idx], q.order[idx+1:]...)
			q.depth--
			q.bytes -= victim.size
			if victim.class == ClassLatest {
				if cur, ok := q.latest[victim.topic]; ok && cur == victim.seq {
					delete(q.latest, victim.topic)
				}
			}
			if class == ClassBulk {
				q.stats.DroppedBulk++
			} else {
				q.stats.DroppedOther++
				if class == ClassCritical {
					q.stats.Lossy = true
				}
			}
		}
		if !over() {
			return
		}
	}
}

// removeLocked drops a sequence from the in-memory index.
func (q *Queue) removeLocked(seq uint64) {
	for i, e := range q.order {
		if e.seq == seq {
			q.order = append(q.order[:i], q.order[i+1:]...)
			q.depth--
			q.bytes -= e.size
			return
		}
	}
}

// Peek returns up to n entries from the head, in order, without removing them.
func (q *Queue) Peek(n int) ([]Entry, error) {
	q.mu.Lock()
	seqs := make([]uint64, 0, n)
	for i := 0; i < len(q.order) && len(seqs) < n; i++ {
		seqs = append(seqs, q.order[i].seq)
	}
	q.mu.Unlock()

	out := make([]Entry, 0, len(seqs))
	for _, seq := range seqs {
		raw, err := q.store.Get(queueKey(seq))
		if errors.Is(err, kvstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("sgu: reading buffered message %d: %w", seq, err)
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Remove deletes an entry that has been successfully published.
func (q *Queue) Remove(seq uint64) error {
	q.mu.Lock()
	q.removeLocked(seq)
	q.stats.Flushed++
	q.mu.Unlock()
	if err := q.store.Delete(queueKey(seq)); err != nil {
		return fmt.Errorf("sgu: removing buffered message %d: %w", seq, err)
	}
	return nil
}

// MarkSent records an idempotency key as delivered.
//
// It is written durably and before the queue entry is removed, so the window in
// which a crash could cause a duplicate is the window between the broker's
// acknowledgement and this write, rather than the whole flush.
func (q *Queue) MarkSent(key string) error {
	if key == "" {
		return nil
	}
	if err := q.store.PutTTL([]byte(sentPrefix+key), []byte{1}, q.cfg.SentTTL); err != nil {
		return fmt.Errorf("sgu: recording a delivered message: %w", err)
	}
	return nil
}

// AlreadySent reports whether an idempotency key has been delivered inside the
// deduplication window.
func (q *Queue) AlreadySent(key string) bool {
	if key == "" {
		return false
	}
	ok, err := q.store.Has([]byte(sentPrefix + key))
	return err == nil && ok
}

// NoteDeduplicated records that a flush skipped a message the cloud already
// has.
func (q *Queue) NoteDeduplicated() {
	q.mu.Lock()
	q.stats.Deduplicated++
	q.mu.Unlock()
}

// Depth returns the number of buffered messages.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// Stats returns the queue's counters.
func (q *Queue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	s.Depth = q.depth
	s.Bytes = q.bytes
	s.MaxEntries = q.cfg.MaxEntries
	s.MaxBytes = q.cfg.MaxBytes
	if len(q.order) > 0 {
		s.Oldest = time.Since(q.order[0].at)
	}
	return s
}

// ClearLossy resets the lossy latch after a reconciliation has reported it.
func (q *Queue) ClearLossy() {
	q.mu.Lock()
	q.stats.Lossy = false
	q.mu.Unlock()
}
