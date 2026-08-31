package eventlog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/usslp/usslp/platform/pkg/obs"
)

const (
	// segmentSuffix and indexSuffix are the two files that make up a segment.
	segmentSuffix = ".log"
	indexSuffix   = ".index"

	// segmentNameDigits zero-pads the base offset so that a lexical sort of the
	// directory is also an offset sort — which is what lets recovery and
	// retention work off a plain ReadDir without parsing every name first.
	segmentNameDigits = 20

	readBufBytes = 64 << 10

	indexMagic   uint32 = 0x55534C49 // "USLI"
	indexVersion uint32 = 1
	// indexHeader is magic, version, baseOffset, nextOffset, indexedBytes,
	// maxTS and entryCount, followed by the entries and a trailing CRC32C.
	indexHeader   = 4 + 4 + 8 + 8 + 8 + 8 + 4
	indexEntryLen = 16
)

// newSegmentReader wraps a slice of a segment file in the package's standard
// read buffer, so every scan path (recovery, delivery, compaction) reads with
// the same syscall granularity.
func newSegmentReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, readBufBytes) }

// indexEntry maps a record offset to its byte position in the segment. The
// index is sparse: one entry per indexInterval bytes. Sparse rather than dense
// because a dense index would cost more than the data for USSLP's small
// telemetry records, and the only thing it needs to do is bound the scan a
// consumer performs when it starts at an arbitrary offset.
type indexEntry struct {
	offset int64
	pos    int64
}

// recordMark describes where a record sits inside an encoded batch, so the
// segment can index and time-stamp it without decoding what it just wrote.
type recordMark struct {
	rel int   // byte offset of the record within the batch buffer
	ts  int64 // record timestamp, unix nanoseconds
}

// segment is one append-only file plus its sparse index.
//
// Segments are the unit of retention and of compaction: whole files are deleted
// or rewritten, never individual records, because rewriting in place would put
// a torn write in the middle of history rather than at its tail where recovery
// can deal with it.
type segment struct {
	dir        string
	baseOffset int64
	nextOffset int64
	size       int64
	// maxTS is the newest record in the segment, and the only thing time-based
	// retention needs: if the newest record is expired then every record is.
	maxTS    int64
	entries  []indexEntry
	interval int64
	rf       *os.File // read handle, always open
	wf       *os.File // write handle, only on the active segment
	dirty    bool     // has unsynced data
	indexed  int64    // bytes covered by the index last written to disk
	lg       *obs.Logger
}

func segmentPath(dir string, base int64) string {
	return filepath.Join(dir, fmt.Sprintf("%0*d%s", segmentNameDigits, base, segmentSuffix))
}

func indexPath(dir string, base int64) string {
	return filepath.Join(dir, fmt.Sprintf("%0*d%s", segmentNameDigits, base, indexSuffix))
}

func (s *segment) path() string      { return segmentPath(s.dir, s.baseOffset) }
func (s *segment) indexPath() string { return indexPath(s.dir, s.baseOffset) }

// createSegment starts a new, empty segment and makes it writable.
func createSegment(dir string, base, interval int64, lg *obs.Logger) (*segment, error) {
	s := &segment{dir: dir, baseOffset: base, nextOffset: base, interval: interval, lg: lg}
	wf, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.wf = wf
	rf, err := os.Open(s.path())
	if err != nil {
		wf.Close()
		return nil, err
	}
	s.rf = rf
	// A new file is only durable once its directory entry is. Without this, a
	// power cut immediately after the first append can leave a partition whose
	// records were fsync'd into a file that does not exist.
	if err := syncDir(dir); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// openSegment opens an existing segment, recovering its tail.
//
// Recovery is the reason this is not a simple stat: the index on disk describes
// the segment as of the last clean seal, and anything appended after that
// (which is everything, if the process died) has to be re-scanned and
// re-validated. A record that fails validation ends the segment and the file is
// truncated back to the last intact record.
func openSegment(dir string, base, interval int64, forWrite bool, lg *obs.Logger) (*segment, error) {
	s := &segment{dir: dir, baseOffset: base, nextOffset: base, interval: interval, lg: lg}
	rf, err := os.Open(s.path())
	if err != nil {
		return nil, err
	}
	s.rf = rf
	st, err := rf.Stat()
	if err != nil {
		rf.Close()
		return nil, err
	}
	s.size = st.Size()

	scanFrom, scanOffset := int64(0), base
	if ok := s.loadIndex(); ok {
		scanFrom, scanOffset = s.indexed, s.nextOffset
		if forWrite && len(s.entries) > 0 {
			// Always re-validate the tail of the active segment, even when the
			// index claims to cover it. The active segment is the only one a
			// crash — or a half-flushed write-back cache — can have torn, and
			// re-reading it back to the last sparse index entry costs one
			// index interval rather than a full scan of the log. Trusting the
			// index here is what would let a torn record survive into
			// production reads.
			last := s.entries[len(s.entries)-1]
			s.entries = s.entries[:len(s.entries)-1]
			scanFrom, scanOffset = last.pos, last.offset
		}
	}
	if err := s.recoverFrom(scanFrom, scanOffset); err != nil {
		rf.Close()
		return nil, err
	}
	if forWrite {
		wf, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			rf.Close()
			return nil, err
		}
		s.wf = wf
	}
	return s, nil
}

// loadIndex reads the sparse index, reporting whether it is usable. A missing,
// short, wrong-version, mis-checksummed or over-long index is silently
// discarded rather than repaired: it is a derived artefact and rebuilding it
// from the segment is both cheap and unambiguous.
func (s *segment) loadIndex() bool {
	b, err := os.ReadFile(s.indexPath())
	if err != nil || len(b) < indexHeader+4 {
		return false
	}
	payload := b[:len(b)-4]
	if crc32.Checksum(payload, castagnoli) != binary.BigEndian.Uint32(b[len(b)-4:]) {
		return false
	}
	c := &cursor{b: payload}
	if c.uint32() != indexMagic || c.uint32() != indexVersion {
		return false
	}
	base := int64(c.uint64())
	next := int64(c.uint64())
	indexed := int64(c.uint64())
	maxTS := int64(c.uint64())
	count := c.uint32()
	if c.err != nil || base != s.baseOffset || indexed > s.size || next < base {
		return false
	}
	if int(count)*indexEntryLen != len(c.b) {
		return false
	}
	entries := make([]indexEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		off := int64(c.uint64())
		pos := int64(c.uint64())
		if c.err != nil || pos > indexed || off < base || off >= next {
			return false
		}
		entries = append(entries, indexEntry{offset: off, pos: pos})
	}
	s.entries = entries
	s.nextOffset = next
	s.indexed = indexed
	s.maxTS = maxTS
	return true
}

// recoverFrom scans the segment from pos, validating every record, extending
// the sparse index and truncating at the first damage.
func (s *segment) recoverFrom(pos, off int64) error {
	if pos > s.size {
		pos, off = 0, s.baseOffset
		s.entries, s.indexed, s.maxTS = nil, 0, 0
	}
	if pos == s.size {
		s.nextOffset = off
		return nil
	}
	sr := io.NewSectionReader(s.rf, pos, s.size-pos)
	br := newSegmentReader(sr)
	for {
		rec, n, err := readRecord(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Expected after a crash mid-append: the tail is a partial or
			// mis-checksummed record. Cut it off so the log stays readable.
			s.lg.Warn("eventlog: truncating damaged segment tail",
				"segment", s.path(), "position", pos, "kept_bytes", pos, "discarded_bytes", s.size-pos, "error", err)
			if terr := os.Truncate(s.path(), pos); terr != nil {
				return fmt.Errorf("eventlog: truncating %s: %w", s.path(), terr)
			}
			s.size = pos
			break
		}
		s.noteIndex(off, pos)
		s.noteTimestamp(rec.timestamp)
		pos += int64(n)
		off++
	}
	s.nextOffset = off
	s.size = pos
	return nil
}

// noteIndex adds a sparse index entry if this record starts a new interval.
func (s *segment) noteIndex(off, pos int64) {
	if len(s.entries) == 0 || pos-s.entries[len(s.entries)-1].pos >= s.interval {
		s.entries = append(s.entries, indexEntry{offset: off, pos: pos})
	}
}

func (s *segment) noteTimestamp(ts int64) {
	if ts > s.maxTS {
		s.maxTS = ts
	}
}

// seek returns the file position and offset of the closest indexed record at or
// before target. A consumer starting mid-segment scans forward from here rather
// than from byte zero.
func (s *segment) seek(target int64) (pos, off int64) {
	i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].offset > target })
	if i == 0 {
		return 0, s.baseOffset
	}
	e := s.entries[i-1]
	return e.pos, e.offset
}

// appendBatch writes an encoded batch as one write(2). One syscall per batch
// (rather than per record) is what keeps 52k records/sec affordable, and it
// also means a batch is either wholly in the page cache or wholly absent from
// the reader's point of view.
func (s *segment) appendBatch(data []byte, marks []recordMark) error {
	if _, err := s.wf.Write(data); err != nil {
		return fmt.Errorf("eventlog: appending to %s: %w", s.path(), err)
	}
	for i, m := range marks {
		s.noteIndex(s.nextOffset+int64(i), s.size+int64(m.rel))
		s.noteTimestamp(m.ts)
	}
	s.size += int64(len(data))
	s.nextOffset += int64(len(marks))
	s.dirty = true
	return nil
}

// read appends up to max deliverable records with offset >= from to out and
// returns the offset immediately after the last record examined. Gap records
// left by compaction are counted but never delivered, which is why the returned
// offset cannot be derived from the records alone.
func (s *segment) read(from int64, max int, out []record) ([]record, int64, error) {
	if from >= s.nextOffset || len(out) >= max {
		return out, from, nil
	}
	if from < s.baseOffset {
		from = s.baseOffset
	}
	pos, cur := s.seek(from)
	sr := io.NewSectionReader(s.rf, pos, s.size-pos)
	br := newSegmentReader(sr)
	for cur < s.nextOffset {
		if cur >= from && len(out) >= max {
			break
		}
		rec, _, err := readRecord(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, cur, err
		}
		at := cur
		cur++
		if at < from || rec.gap {
			continue
		}
		rec.offset = at
		out = append(out, rec)
	}
	return out, cur, nil
}

// sync forces this segment's data to stable storage.
func (s *segment) sync() error {
	if s.wf == nil || !s.dirty {
		return nil
	}
	if err := s.wf.Sync(); err != nil {
		return fmt.Errorf("eventlog: fsync %s: %w", s.path(), err)
	}
	s.dirty = false
	return nil
}

// writeIndex persists the sparse index atomically.
//
// The index is only written when a segment is sealed or the log is closed, not
// on every append: it is a rebuildable accelerator, and paying an fsync for it
// on the hot path would double the cost of durability for no gain in safety.
func (s *segment) writeIndex() error {
	buf := make([]byte, 0, indexHeader+len(s.entries)*indexEntryLen+4)
	buf = binary.BigEndian.AppendUint32(buf, indexMagic)
	buf = binary.BigEndian.AppendUint32(buf, indexVersion)
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.baseOffset))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.nextOffset))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.size))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.maxTS))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(s.entries)))
	for _, e := range s.entries {
		buf = binary.BigEndian.AppendUint64(buf, uint64(e.offset))
		buf = binary.BigEndian.AppendUint64(buf, uint64(e.pos))
	}
	buf = binary.BigEndian.AppendUint32(buf, crc32.Checksum(buf, castagnoli))
	if err := writeFileAtomic(s.indexPath(), buf); err != nil {
		return err
	}
	s.indexed = s.size
	return nil
}

// reopenRead swaps in a fresh read handle, used after compaction replaces the
// file underneath us.
func (s *segment) reopenRead() error {
	rf, err := os.Open(s.path())
	if err != nil {
		return err
	}
	if s.rf != nil {
		s.rf.Close()
	}
	s.rf = rf
	return nil
}

// seal finishes the segment: flush, index, and drop the write handle.
func (s *segment) seal() error {
	if err := s.sync(); err != nil {
		return err
	}
	if err := s.writeIndex(); err != nil {
		return err
	}
	if s.wf != nil {
		if err := s.wf.Close(); err != nil {
			return err
		}
		s.wf = nil
	}
	return nil
}

func (s *segment) close() error {
	var first error
	if s.wf != nil {
		if err := s.sync(); err != nil {
			first = err
		}
		if err := s.writeIndex(); err != nil && first == nil {
			first = err
		}
		if err := s.wf.Close(); err != nil && first == nil {
			first = err
		}
		s.wf = nil
	}
	if s.rf != nil {
		if err := s.rf.Close(); err != nil && first == nil {
			first = err
		}
		s.rf = nil
	}
	return first
}

// remove deletes the segment and its index from disk.
func (s *segment) remove() error {
	logPath, idxPath := s.path(), s.indexPath()
	err := s.close()
	if rerr := os.Remove(logPath); rerr != nil && err == nil {
		err = rerr
	}
	if rerr := os.Remove(idxPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// writeFileAtomic replaces path with data via a temp file and rename, fsyncing
// both the file and the directory so the replacement survives a power cut.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so that creations, renames and unlinks inside it
// are durable. Skipping this is the classic way to lose a file that was itself
// fsync'd.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("eventlog: fsync dir %s: %w", dir, err)
	}
	return nil
}
