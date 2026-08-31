package eventlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// partition is one ordered sequence of records: a directory of segments plus
// the offset bookkeeping that makes them look like a single infinite file.
//
// A partition is the unit of both ordering and parallelism. Exactly one member
// of a consumer group owns a partition at a time, which is what makes per-key
// ordering deliverable; and there are many partitions, which is what makes a
// group scale past one core.
type partition struct {
	id       int
	dir      string
	opts     *options
	lg       *obs.Logger
	mu       sync.RWMutex
	segs     []*segment
	next     atomic.Int64 // offset the next appended record will take
	base     atomic.Int64 // earliest offset still on disk
	notifyMu sync.Mutex
	notify   chan struct{}
}

func newPartition(dir string, id int, opts *options, lg *obs.Logger) *partition {
	return &partition{dir: dir, id: id, opts: opts, lg: lg, notify: make(chan struct{})}
}

// openPartition loads an existing partition directory, recovering every
// segment. A directory that does not exist is not an error: partition
// directories are created on first append, so that a 2,048-partition telemetry
// stream costs nothing until somebody actually writes to it.
func openPartition(dir string, id int, opts *options, lg *obs.Logger) (*partition, error) {
	p := newPartition(dir, id, opts, lg)
	bases, err := segmentBases(dir)
	if err != nil {
		return nil, err
	}
	for i, base := range bases {
		last := i == len(bases)-1
		s, err := openSegment(dir, base, opts.indexInterval, last, lg)
		if err != nil {
			return nil, fmt.Errorf("eventlog: partition %s segment %d: %w", dir, base, err)
		}
		if i > 0 && s.baseOffset != p.segs[i-1].nextOffset {
			// A hole means an earlier segment lost its tail to truncation. The
			// records are gone either way; say so loudly rather than let a
			// consumer silently skip them.
			p.lg.Warn("eventlog: offset hole between segments",
				"partition", dir, "expected", p.segs[i-1].nextOffset, "found", s.baseOffset)
		}
		p.segs = append(p.segs, s)
	}
	if len(p.segs) > 0 {
		p.base.Store(p.segs[0].baseOffset)
		p.next.Store(p.segs[len(p.segs)-1].nextOffset)
	}
	return p, nil
}

// segmentBases lists the base offsets of the segments in dir, in offset order.
func segmentBases(dir string) ([]int64, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bases []int64
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		base, err := strconv.ParseInt(strings.TrimSuffix(name, segmentSuffix), 10, 64)
		if err != nil {
			continue
		}
		bases = append(bases, base)
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
	return bases, nil
}

// waitCh returns a channel closed when the next append lands. Consumers take it
// before reading so that a record appended between the read and the wait cannot
// be missed; the alternative, polling, either burns CPU at idle or adds latency
// the 3-second propagation budget cannot spare.
func (p *partition) waitCh() <-chan struct{} {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	return p.notify
}

func (p *partition) wake() {
	p.notifyMu.Lock()
	close(p.notify)
	p.notify = make(chan struct{})
	p.notifyMu.Unlock()
}

// append writes recs and returns the offset of the first one and the bytes
// written. The whole batch lands in one segment and one write, so a reader
// never observes half a batch.
func (p *partition) append(recs []record, now int64) (first int64, written int64, err error) {
	buf := make([]byte, 0, 256*len(recs))
	marks := make([]recordMark, 0, len(recs))
	for i := range recs {
		if recs[i].timestamp == 0 {
			recs[i].timestamp = now
		}
		marks = append(marks, recordMark{rel: len(buf), ts: recs[i].timestamp})
		buf = encodeRecord(buf, recs[i])
	}

	p.mu.Lock()
	if err := p.ensureActiveLocked(); err != nil {
		p.mu.Unlock()
		return 0, 0, err
	}
	act := p.segs[len(p.segs)-1]
	if act.size > 0 && act.size+int64(len(buf)) > p.opts.segmentBytes {
		if err := p.rollLocked(); err != nil {
			p.mu.Unlock()
			return 0, 0, err
		}
		act = p.segs[len(p.segs)-1]
	}
	first = act.nextOffset
	if err := act.appendBatch(buf, marks); err != nil {
		p.mu.Unlock()
		return 0, 0, err
	}
	p.next.Store(act.nextOffset)
	p.mu.Unlock()

	p.wake()
	return first, int64(len(buf)), nil
}

// ensureActiveLocked materialises the partition directory and its first segment
// on first use.
func (p *partition) ensureActiveLocked() error {
	if len(p.segs) > 0 && p.segs[len(p.segs)-1].wf != nil {
		return nil
	}
	if len(p.segs) > 0 {
		// Recovered a partition whose last segment was opened read-only.
		s := p.segs[len(p.segs)-1]
		wf, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		s.wf = wf
		return nil
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return err
	}
	s, err := createSegment(p.dir, p.next.Load(), p.opts.indexInterval, p.lg)
	if err != nil {
		return err
	}
	p.segs = append(p.segs, s)
	p.base.Store(s.baseOffset)
	return nil
}

// rollLocked seals the active segment and starts a new one.
func (p *partition) rollLocked() error {
	act := p.segs[len(p.segs)-1]
	if err := act.seal(); err != nil {
		return err
	}
	s, err := createSegment(p.dir, act.nextOffset, p.opts.indexInterval, p.lg)
	if err != nil {
		return err
	}
	p.segs = append(p.segs, s)
	return nil
}

// read returns up to max records with offset >= from, plus the offset
// immediately after the last record examined.
//
// The second return value is not simply the last record's offset plus one:
// compaction leaves gap records that are counted but not delivered, and a poll
// that lands entirely inside a compacted region must still make progress.
func (p *partition) read(from int64, max int) ([]record, int64, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	end := p.next.Load()
	if from >= end || len(p.segs) == 0 {
		return nil, from, nil
	}
	if base := p.segs[0].baseOffset; from < base {
		p.lg.Warn("eventlog: consumer fell off the log, skipping to earliest retained offset",
			"partition", p.dir, "requested", from, "earliest", base)
		from = base
	}
	i := sort.Search(len(p.segs), func(i int) bool { return p.segs[i].baseOffset > from })
	if i > 0 {
		i--
	}
	out := make([]record, 0, min(max, 64))
	cur := from
	for ; i < len(p.segs) && len(out) < max; i++ {
		s := p.segs[i]
		next, err := int64(0), error(nil)
		out, next, err = s.read(cur, max, out)
		if err != nil {
			// Damage inside an already-validated region (bit rot, or a segment
			// edited underneath us). Skip the rest of this segment rather than
			// spin on it forever; the records are unrecoverable either way.
			p.lg.Error("eventlog: unreadable record mid-segment, skipping remainder",
				"segment", s.path(), "offset", next, "error", err)
			next = s.nextOffset
		}
		cur = next
		if cur < s.nextOffset {
			break // hit max inside this segment
		}
	}
	return out, cur, nil
}

// enforceRetention deletes every closed segment whose newest record is older
// than cutoff, returning how many were removed.
//
// Deletion is by whole segment because that is the only granularity at which
// space is actually reclaimed on a log-structured store; the consequence, which
// operators need to know, is that a stream's real retention is its configured
// retention plus however long the active segment takes to fill.
func (p *partition) enforceRetention(cutoff int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.segs) == 0 {
		return 0
	}
	act := p.segs[len(p.segs)-1]
	if act.size > 0 && act.maxTS > 0 && act.maxTS < cutoff {
		// Everything written so far is expired. Roll so the data becomes
		// deletable in this same sweep instead of lingering until the next
		// write happens to fill the segment.
		if err := p.rollLocked(); err != nil {
			p.lg.Error("eventlog: rolling expired active segment", "partition", p.dir, "error", err)
			return 0
		}
	}
	deleted := 0
	for deleted < len(p.segs)-1 {
		s := p.segs[deleted]
		if s.maxTS == 0 || s.maxTS >= cutoff {
			break
		}
		if err := s.remove(); err != nil {
			p.lg.Error("eventlog: deleting expired segment", "segment", s.path(), "error", err)
			break
		}
		deleted++
	}
	if deleted > 0 {
		p.segs = p.segs[deleted:]
		p.base.Store(p.segs[0].baseOffset)
	}
	return deleted
}

// compact rewrites closed segments so that only the newest record for each key
// survives, returning how many segments were rewritten.
//
// Superseded records are replaced by fixed-size gap records rather than removed
// outright. That is the central trade-off of this implementation: offsets here
// are positional (a record's offset is its index within its segment, which is
// what keeps the frame small and the index sparse), so physically removing a
// record would renumber every later one and invalidate offsets that consumers
// have already committed. A gap costs ~50 bytes and reclaims the entire value
// and header payload, which for the label-state stream — small keys, whole
// rendered display states as values — is the overwhelming majority of the file.
//
// The active segment is never rewritten, matching Kafka: the newest version of
// a key is usually there, and rewriting a file that is being appended to is how
// you lose data.
//
// A rewrite buffers one segment in memory, so segment size doubles as the bound
// on compaction's working set — another reason not to raise it without thought.
func (p *partition) compact() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.segs) < 2 {
		return 0
	}
	// The newest copy of a key may live in the active segment, so the survey
	// covers every segment even though only closed ones are rewritten.
	latest := make(map[string]int64)
	for _, s := range p.segs {
		err := s.forEach(func(off int64, rec record) error {
			if !rec.gap && len(rec.key) > 0 {
				latest[string(rec.key)] = off
			}
			return nil
		})
		if err != nil {
			p.lg.Error("eventlog: compaction survey failed, skipping partition",
				"segment", s.path(), "error", err)
			return 0
		}
	}
	rewritten := 0
	for _, s := range p.segs[:len(p.segs)-1] {
		ok, err := p.compactSegmentLocked(s, latest)
		if err != nil {
			p.lg.Error("eventlog: compacting segment", "segment", s.path(), "error", err)
			continue
		}
		if ok {
			rewritten++
		}
	}
	return rewritten
}

// compactSegmentLocked rewrites one segment, reporting whether anything
// changed. A segment with nothing to drop is left untouched, which is what
// stops the retention sweep rewriting the whole log every time it runs.
func (p *partition) compactSegmentLocked(s *segment, latest map[string]int64) (bool, error) {
	var buf []byte
	marks := make([]recordMark, 0, 64)
	changed := false
	err := s.forEach(func(off int64, rec record) error {
		marks = append(marks, recordMark{rel: len(buf), ts: rec.timestamp})
		switch {
		case rec.gap:
			buf = encodeRecord(buf, gapRecord(rec.timestamp))
		case len(rec.key) > 0 && latest[string(rec.key)] != off:
			changed = true
			buf = encodeRecord(buf, gapRecord(rec.timestamp))
		default:
			// Unkeyed records cannot be compacted by key and are kept. A
			// compacted stream should never carry them; keeping them is the
			// safe reading of an ambiguous situation.
			buf = encodeRecord(buf, rec)
		}
		return nil
	})
	if err != nil || !changed {
		return false, err
	}
	if err := writeFileAtomic(s.path(), buf); err != nil {
		return false, err
	}
	s.entries, s.indexed, s.maxTS = nil, 0, 0
	s.size, s.nextOffset = 0, s.baseOffset
	for i, m := range marks {
		s.noteIndex(s.baseOffset+int64(i), int64(m.rel))
		s.noteTimestamp(m.ts)
	}
	s.size = int64(len(buf))
	s.nextOffset = s.baseOffset + int64(len(marks))
	if err := s.reopenRead(); err != nil {
		return false, err
	}
	return true, s.writeIndex()
}

// forEach walks every record in the segment, gaps included, in offset order.
func (s *segment) forEach(fn func(off int64, rec record) error) error {
	sr := io.NewSectionReader(s.rf, 0, s.size)
	br := newSegmentReader(sr)
	off := s.baseOffset
	for off < s.nextOffset {
		rec, _, err := readRecord(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(off, rec); err != nil {
			return err
		}
		off++
	}
	return nil
}

// sync flushes the active segment.
func (p *partition) sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.segs) == 0 {
		return nil
	}
	return p.segs[len(p.segs)-1].sync()
}

// close flushes and releases every file handle held by the partition.
func (p *partition) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var first error
	for _, s := range p.segs {
		if err := s.close(); err != nil && first == nil {
			first = err
		}
	}
	p.segs = nil
	return first
}

// segmentCount reports how many segments back this partition.
func (p *partition) segmentCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.segs)
}

// partitionDir is the on-disk name of a partition inside its topic directory.
func partitionDir(topicDir string, id int) string {
	return filepath.Join(topicDir, strconv.Itoa(id))
}
