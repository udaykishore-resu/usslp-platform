package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// topicMetaName holds a stream's definition next to its data, so that reopening
// a directory recovers the partition count, retention and compaction settings
// without the caller having to hand them back. A log directory is
// self-describing: copy it to another machine and it still knows what it is.
const topicMetaName = "topic.json"

// offsetFlushInterval bounds how long a committed offset can sit only in
// memory. It is short enough that a normal restart re-delivers a handful of
// records and long enough that a group committing every record does not turn
// into a write-amplification problem.
const offsetFlushInterval = 200 * time.Millisecond

// ErrPartitionsChanged is returned by EnsureStreams when a stream is redefined
// with a different partition count. Changing it would re-map every key to a
// different partition, silently destroying the per-key ordering that the whole
// platform is built on, so it is refused rather than applied.
var ErrPartitionsChanged = errors.New("eventlog: partition count cannot be changed")

// ErrInvalidTopic marks a stream name that cannot be a directory.
var ErrInvalidTopic = errors.New("eventlog: invalid topic name")

// topicMeta is the on-disk form of canon.Stream.
type topicMeta struct {
	Name           string `json:"name"`
	Partitions     int    `json:"partitions"`
	RetentionHours int    `json:"retention_hours"`
	Compacted      bool   `json:"compacted"`
	Description    string `json:"description"`
}

// topic is one stream: a fixed set of partitions plus the definition that says
// how they are retained.
type topic struct {
	stream canon.Stream
	dir    string
	parts  []*partition
	// rr round-robins records that carry no key. A counter rather than a random
	// choice so that unkeyed traffic spreads evenly rather than merely
	// uniformly at random, which at 2,048 partitions is a visibly different
	// distribution.
	rr atomic.Uint64
}

// Log is a durable, embedded, partitioned append-only log implementing
// eventbus.Bus.
type Log struct {
	dir  string
	temp bool
	opts options
	lg   *obs.Logger
	m    *metrics

	// mu guards the topic map and, crucially, the lifetime of every file handle
	// underneath it: readers and writers hold it shared, Close takes it
	// exclusively, so no goroutine can be reading a segment that Close is busy
	// closing.
	mu      sync.RWMutex
	closed  bool
	topics  map[string]*topic
	groups  map[string]*group
	offsets *offsetStore

	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ eventbus.Bus = (*Log)(nil)

// Open opens or creates a log rooted at dir.
//
// An empty dir creates a temporary directory that Close removes. That is the
// mode the test suite runs in: a test gets a genuinely durable log with real
// segments, real fsyncs and real recovery, and leaves nothing behind.
//
// Opening an existing directory recovers everything: every segment's tail is
// re-validated, every damaged tail truncated, every stream definition and every
// committed consumer offset reloaded, and appends resume at the next offset.
func Open(dir string, opts ...Option) (*Log, error) {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	if o.logger == nil {
		o.logger = defaultLogger()
	}

	temp := false
	if dir == "" {
		d, err := os.MkdirTemp("", "usslp-eventlog-*")
		if err != nil {
			return nil, fmt.Errorf("eventlog: creating temp directory: %w", err)
		}
		dir, temp = d, true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: creating %s: %w", dir, err)
	}

	offsets, err := openOffsetStore(filepath.Join(dir, offsetsDirName), o.logger)
	if err != nil {
		return nil, fmt.Errorf("eventlog: opening offsets: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	l := &Log{
		dir:     dir,
		temp:    temp,
		opts:    o,
		lg:      o.logger,
		m:       newMetrics(o.registry),
		topics:  make(map[string]*topic),
		groups:  make(map[string]*group),
		offsets: offsets,
		done:    make(chan struct{}),
		cancel:  cancel,
	}
	if err := l.loadTopics(); err != nil {
		cancel()
		return nil, err
	}

	l.wg.Add(2)
	go l.retentionLoop(ctx)
	go l.offsetLoop(ctx)
	if l.opts.sync.mode == syncModeInterval {
		l.wg.Add(1)
		go l.syncLoop(ctx)
	}
	return l, nil
}

func defaultLogger() *obs.Logger {
	// Warn and above only: this package logs damaged segments, dropped
	// dead-letters and consumers that fell off the log — events that must reach
	// an operator — and nothing routine.
	return obs.NewLogger(obs.LogConfig{Service: "eventlog", Level: "warn", Format: "text", Output: os.Stderr})
}

// Dir reports the directory backing the log, which for a temporary log is the
// path that Close will delete.
func (l *Log) Dir() string { return l.dir }

// loadTopics reconstructs every stream found on disk.
func (l *Log) loadTopics() error {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("eventlog: reading %s: %w", l.dir, err)
	}
	for _, e := range ents {
		if !e.IsDir() || e.Name() == offsetsDirName {
			continue
		}
		topicDir := filepath.Join(l.dir, e.Name())
		b, err := os.ReadFile(filepath.Join(topicDir, topicMetaName))
		if errors.Is(err, os.ErrNotExist) {
			l.lg.Warn("eventlog: directory without stream metadata ignored", "dir", topicDir)
			continue
		}
		if err != nil {
			return fmt.Errorf("eventlog: reading stream metadata in %s: %w", topicDir, err)
		}
		var meta topicMeta
		if err := json.Unmarshal(b, &meta); err != nil {
			return fmt.Errorf("eventlog: parsing stream metadata in %s: %w", topicDir, err)
		}
		t, err := l.openTopic(topicDir, meta)
		if err != nil {
			return err
		}
		l.topics[t.stream.Name] = t
	}
	return nil
}

func (l *Log) openTopic(dir string, meta topicMeta) (*topic, error) {
	if meta.Partitions <= 0 {
		return nil, fmt.Errorf("eventlog: stream %q has %d partitions", meta.Name, meta.Partitions)
	}
	t := &topic{
		stream: canon.Stream{
			Name:           meta.Name,
			Partitions:     meta.Partitions,
			RetentionHours: meta.RetentionHours,
			Description:    meta.Description,
			Compacted:      meta.Compacted,
		},
		dir:   dir,
		parts: make([]*partition, meta.Partitions),
	}
	for i := 0; i < meta.Partitions; i++ {
		p, err := openPartition(partitionDir(dir, i), i, &l.opts, l.lg)
		if err != nil {
			return nil, err
		}
		t.parts[i] = p
		l.m.setSegments(t.stream.Name, i, p.segmentCount())
	}
	return t, nil
}

// EnsureStreams creates any missing streams and is safe to call on every
// service start-up. It is idempotent for an unchanged definition, updates
// retention and compaction settings in place, and refuses a changed partition
// count.
func (l *Log) EnsureStreams(ctx context.Context, streams ...canon.Stream) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return eventbus.ErrClosed
	}
	for _, s := range streams {
		if err := validateTopicName(s.Name); err != nil {
			return err
		}
		if s.Partitions <= 0 {
			return fmt.Errorf("eventlog: stream %q: partitions must be positive", s.Name)
		}
		meta := topicMeta{
			Name:           s.Name,
			Partitions:     s.Partitions,
			RetentionHours: s.RetentionHours,
			Compacted:      s.Compacted,
			Description:    s.Description,
		}
		if existing, ok := l.topics[s.Name]; ok {
			if existing.stream.Partitions != s.Partitions {
				return fmt.Errorf("%w: stream %q is %d, requested %d",
					ErrPartitionsChanged, s.Name, existing.stream.Partitions, s.Partitions)
			}
			if existing.stream == s {
				continue
			}
			if err := writeTopicMeta(existing.dir, meta); err != nil {
				return err
			}
			existing.stream = s
			continue
		}
		dir := filepath.Join(l.dir, s.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("eventlog: creating stream %q: %w", s.Name, err)
		}
		if err := writeTopicMeta(dir, meta); err != nil {
			return err
		}
		t, err := l.openTopic(dir, meta)
		if err != nil {
			return err
		}
		l.topics[s.Name] = t
	}
	// A new stream changes the partition set a group must cover, so every
	// existing group re-plans its assignment.
	counts := l.countsLocked()
	for _, g := range l.groups {
		g.rebalance(counts)
	}
	return nil
}

func writeTopicMeta(dir string, meta topicMeta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, topicMetaName), b)
}

// validateTopicName rejects anything that would not be a single, ordinary
// directory inside the log root. Stream names come from configuration and end
// up as paths; a name containing a separator is a directory-traversal bug
// waiting to happen, and one colliding with the offsets directory would corrupt
// consumer state.
func validateTopicName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrInvalidTopic)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q", ErrInvalidTopic, name)
	case name == offsetsDirName:
		return fmt.Errorf("%w: %q is reserved", ErrInvalidTopic, name)
	case strings.ContainsAny(name, `/\`), strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidTopic, name)
	}
	return nil
}

// topicByName returns a stream, or nil. The caller must hold l.mu.
func (l *Log) topicByName(name string) *topic { return l.topics[name] }

// Streams reports the stream definitions currently known to the log.
func (l *Log) Streams() []canon.Stream {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]canon.Stream, 0, len(l.topics))
	for _, t := range l.topics {
		out = append(out, t.stream)
	}
	return out
}

// PartitionFor reports which partition a key lands in. Exposed because tests
// and operational tooling both need to answer "where did this SKU go" without
// re-deriving the hash, and a second implementation of that hash would be a
// silent correctness hazard.
func (l *Log) PartitionFor(topicName, key string) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.topicByName(topicName)
	if t == nil {
		return 0, fmt.Errorf("%w: %q", eventbus.ErrNoTopic, topicName)
	}
	if key == "" {
		return 0, errors.New("eventlog: an empty key has no stable partition: unkeyed records round-robin")
	}
	return int(fnv1a(key) % uint32(t.stream.Partitions)), nil
}

// partitionOf assigns a record to a partition.
//
// Keyed records hash with FNV-1a, which is fast, allocation-free and — the
// property that actually matters — fixed forever: the mapping from key to
// partition is part of the on-disk contract, because a group's committed
// offsets and a partition's ordering guarantee are both meaningless if the same
// key can move.
//
// Unkeyed records round-robin. They have no ordering requirement by definition
// (there is no key whose sequence could be violated), so spreading them evenly
// is strictly better than hashing an empty string into one hot partition.
func (t *topic) partitionOf(key []byte) int {
	n := uint32(t.stream.Partitions)
	if len(key) == 0 {
		return int((t.rr.Add(1) - 1) % uint64(n))
	}
	return int(fnv1aBytes(key) % n)
}

// fnv1a is the 32-bit FNV-1a hash, written out rather than taken from hash/fnv
// so that the hot path allocates nothing: hash/fnv's constructor returns an
// interface backed by a heap value, which at 52,000 records/sec is 52,000
// pointless allocations per second.
func fnv1a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

func fnv1aBytes(b []byte) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for _, c := range b {
		h ^= uint32(c)
		h *= prime32
	}
	return h
}

// Publish appends records and returns once they are durable under the
// configured SyncPolicy.
//
// Topics are resolved for the whole batch before a single byte is written, so
// that a typo'd stream name fails with ErrNoTopic having changed nothing.
// Beyond that, atomicity is per partition: each partition's share of the batch
// is one write to one segment, and a batch spanning partitions can in principle
// be half-applied if the disk fails mid-call. Callers rely on the idempotency
// key in canon.Envelope for safe retry, exactly as they must against Kafka.
func (l *Log) Publish(ctx context.Context, msgs ...eventbus.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return eventbus.ErrClosed
	}

	type dest struct {
		t  *topic
		id int
	}
	order := make([]dest, 0, 4)
	batches := make(map[dest][]record, 4)
	for _, msg := range msgs {
		t := l.topicByName(msg.Topic)
		if t == nil {
			return fmt.Errorf("%w: %q", eventbus.ErrNoTopic, msg.Topic)
		}
		key := []byte(msg.Key)
		d := dest{t: t, id: t.partitionOf(key)}
		if _, seen := batches[d]; !seen {
			order = append(order, d)
		}
		var ts int64
		if !msg.Timestamp.IsZero() {
			ts = msg.Timestamp.UnixNano()
		}
		batches[d] = append(batches[d], record{
			timestamp: ts,
			key:       key,
			headers:   msg.Headers,
			value:     msg.Value,
		})
	}

	now := time.Now().UnixNano()
	var failed []string
	for _, d := range order {
		recs := batches[d]
		p := d.t.parts[d.id]
		_, written, err := p.append(recs, now)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s/%d: %v", d.t.stream.Name, d.id, err))
			continue
		}
		if l.opts.sync.mode == syncModeAlways {
			if err := p.sync(); err != nil {
				failed = append(failed, fmt.Sprintf("%s/%d: %v", d.t.stream.Name, d.id, err))
				continue
			}
		}
		l.m.appendedRecords(d.t.stream.Name, int64(len(recs)), written)
		l.m.setSegments(d.t.stream.Name, d.id, p.segmentCount())
	}
	if len(failed) > 0 {
		return fmt.Errorf("eventlog: publish failed for %d partition(s): %s",
			len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// readPartition performs a consumer read under the shared lock, so that Close
// cannot pull a segment's file handle out from under an in-flight read.
func (l *Log) readPartition(p *partition, from int64, max int) ([]record, int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, from, eventbus.ErrClosed
	}
	return p.read(from, max)
}

// retentionLoop enforces per-stream retention and compaction.
func (l *Log) retentionLoop(ctx context.Context) {
	defer l.wg.Done()
	t := time.NewTicker(l.opts.retentionInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.enforceRetention(time.Now())
		}
	}
}

// enforceRetention runs one retention and compaction sweep. Time is a parameter
// rather than a call to time.Now so that tests can age a log deterministically
// instead of racing a ticker.
func (l *Log) enforceRetention(now time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return
	}
	for _, t := range l.topics {
		if t.stream.RetentionHours > 0 {
			cutoff := now.Add(-time.Duration(t.stream.RetentionHours) * time.Hour).UnixNano()
			for _, p := range t.parts {
				if n := p.enforceRetention(cutoff); n > 0 {
					l.m.setSegments(t.stream.Name, p.id, p.segmentCount())
				}
			}
		}
		if t.stream.Compacted {
			for _, p := range t.parts {
				p.compact()
			}
		}
	}
}

// offsetLoop persists consumer offsets, coalescing bursts of commits into one
// write per interval.
func (l *Log) offsetLoop(ctx context.Context) {
	defer l.wg.Done()
	t := time.NewTicker(offsetFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.offsets.pending():
			// Wait out the interval so a group committing every record still
			// only writes its file a few times a second.
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		case <-t.C:
		}
		if err := l.offsets.Flush(); err != nil {
			l.lg.Error("eventlog: flushing consumer offsets", "error", err)
		}
	}
}

// syncLoop implements SyncInterval.
func (l *Log) syncLoop(ctx context.Context) {
	defer l.wg.Done()
	t := time.NewTicker(l.opts.sync.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.syncAll()
		}
	}
}

func (l *Log) syncAll() {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return
	}
	for _, t := range l.topics {
		for _, p := range t.parts {
			if err := p.sync(); err != nil {
				l.lg.Error("eventlog: background fsync", "topic", t.stream.Name, "partition", p.id, "error", err)
			}
		}
	}
}

// Close stops every consumer, flushes offsets, fsyncs and closes every segment,
// and removes the directory if the log was opened with an empty dir.
//
// Close always fsyncs, whatever the sync policy: an orderly shutdown that loses
// acknowledged records would make SyncNever unusable even in the tests it
// exists for.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	// Wake every consumer before taking the exclusive lock: a Run loop blocked
	// on a read holds the shared lock, and Close would otherwise wait on it.
	close(l.done)
	l.cancel()
	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	var first error
	if err := l.offsets.Flush(); err != nil {
		first = err
	}
	for _, t := range l.topics {
		for _, p := range t.parts {
			if err := p.close(); err != nil && first == nil {
				first = err
			}
		}
	}
	l.topics = make(map[string]*topic)
	if l.temp {
		if err := os.RemoveAll(l.dir); err != nil && first == nil {
			first = err
		}
	}
	return first
}
