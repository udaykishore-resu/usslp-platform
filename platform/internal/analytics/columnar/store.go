package columnar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row is one record being ingested, keyed by column name.
//
// It is a map rather than a positional slice because the four ingest paths each
// build rows from a different event payload and a positional API would put the
// column ordering in four places. The map is converted to columns once per
// flush, so the cost is paid per block rather than per row.
type Row map[string]any

// Store is a column-oriented, block-structured time-series store.
//
// # Layout on disk
//
//	<dir>/<table>/<tier>/seg-<nanos>-<seq>.usc
//
// One file per flushed batch of blocks, named by the earliest timestamp it
// holds. Files are immutable once written: there is no compaction, no merge and
// no rewrite. That is the deliberate simplification a store fed by append-only
// event streams can make, and it is what lets retention be implemented as
// "delete the files whose newest row is older than the policy", which is an
// unlink rather than a rewrite of a terabyte.
//
// # Tiers
//
// Hot, warm and cold are directories, and moving a segment between them is a
// rename. The production intent is that warm sits on cheaper block storage and
// cold in object storage; the store's contract is only that a query reads every
// tier the policy says still holds data, so a deployment that maps all three to
// one disk is correct and merely not cheap.
type Store struct {
	dir    string
	schema Schema

	mu sync.RWMutex
	// buffer accumulates rows until BlockRows, at which point they become a
	// block. It is column-major so that the flush is a copy rather than a
	// transpose.
	buffer   []ColumnValues
	buffered int
	// pending holds encoded blocks not yet written to a segment file.
	pending    [][]byte
	pendingMin int64
	pendingMax int64
	seq        uint64

	blockRows      int
	blocksPerSeg   int
	rawBytes       int64
	compressedByte int64
	rowsWritten    int64
}

// Tier names a storage tier.
type Tier string

// The retention tiers.
const (
	// TierHot holds recent data on the fastest storage.
	TierHot Tier = "hot"
	// TierWarm holds data past the hot window.
	TierWarm Tier = "warm"
	// TierCold holds data kept only for compliance and long-range trends.
	TierCold Tier = "cold"
)

// AllTiers is the read order: hot first, because a query for the last hour
// finds everything it needs there and never touches the others.
func AllTiers() []Tier { return []Tier{TierHot, TierWarm, TierCold} }

// Options configure a store.
type Options struct {
	// Dir is the root directory.
	Dir string
	// Schema is the table definition.
	Schema Schema
	// BlockRows overrides DefaultBlockRows.
	BlockRows int
	// BlocksPerSegment bounds a segment file. Larger files mean fewer file
	// handles and a longer minimum retention granularity, since a segment is
	// deleted whole.
	BlocksPerSegment int
}

// DefaultBlocksPerSegment is how many blocks a segment file holds.
//
// Sixteen blocks of 8,192 rows is about 130,000 rows, which for a store's
// telemetry is roughly ten minutes. That makes ten minutes the granularity of
// retention and of block skipping at the file level, which is fine for a
// retention policy measured in days and fine for a query measured in hours.
const DefaultBlocksPerSegment = 16

// Open creates or opens a store.
func Open(opts Options) (*Store, error) {
	if err := opts.Schema.Validate(); err != nil {
		return nil, err
	}
	if opts.Dir == "" {
		return nil, errors.New("columnar: a directory is required")
	}
	if opts.BlockRows <= 0 {
		opts.BlockRows = DefaultBlockRows
	}
	if opts.BlocksPerSegment <= 0 {
		opts.BlocksPerSegment = DefaultBlocksPerSegment
	}
	s := &Store{
		dir: opts.Dir, schema: opts.Schema,
		blockRows: opts.BlockRows, blocksPerSeg: opts.BlocksPerSegment,
	}
	for _, tier := range AllTiers() {
		if err := os.MkdirAll(s.tierDir(tier), 0o750); err != nil {
			return nil, fmt.Errorf("columnar: creating %s: %w", s.tierDir(tier), err)
		}
	}
	s.resetBuffer()
	return s, nil
}

func (s *Store) tierDir(t Tier) string {
	return filepath.Join(s.dir, s.schema.Table, string(t))
}

// Schema returns the table definition.
func (s *Store) Schema() Schema { return s.schema }

func (s *Store) resetBuffer() {
	s.buffer = make([]ColumnValues, len(s.schema.Columns))
	for i, c := range s.schema.Columns {
		v := ColumnValues{Type: c.Type}
		switch c.Type {
		case TypeTimestamp, TypeInt64:
			v.Ints = make([]int64, 0, s.blockRows)
		case TypeFloat64:
			v.Floats = make([]float64, 0, s.blockRows)
		case TypeString:
			v.Strings = make([]string, 0, s.blockRows)
		case TypeBool:
			v.Bools = make([]bool, 0, s.blockRows)
		}
		s.buffer[i] = v
	}
	s.buffered = 0
}

// ErrBadRow marks a row that does not fit the schema.
var ErrBadRow = errors.New("columnar: row does not match the schema")

// Append adds rows, flushing blocks as they fill.
//
// Rows may arrive out of order — four streams feed this store and their clocks
// and partitions interleave — and the format does not require order. Ordered
// arrival simply compresses better and prunes better, because a block's
// timestamp range is then tight.
func (s *Store) Append(rows ...Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		if err := s.appendLocked(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendLocked(row Row) error {
	for i, col := range s.schema.Columns {
		raw, ok := row[col.Name]
		if !ok {
			return fmt.Errorf("%w: missing column %s", ErrBadRow, col.Name)
		}
		switch col.Type {
		case TypeTimestamp:
			var v int64
			switch t := raw.(type) {
			case time.Time:
				v = t.UnixNano()
			case int64:
				v = t
			default:
				return fmt.Errorf("%w: %s is a %T, want a time or nanoseconds", ErrBadRow, col.Name, raw)
			}
			s.buffer[i].Ints = append(s.buffer[i].Ints, v)
		case TypeInt64:
			switch t := raw.(type) {
			case int64:
				s.buffer[i].Ints = append(s.buffer[i].Ints, t)
			case int:
				s.buffer[i].Ints = append(s.buffer[i].Ints, int64(t))
			default:
				return fmt.Errorf("%w: %s is a %T, want an integer", ErrBadRow, col.Name, raw)
			}
		case TypeFloat64:
			switch t := raw.(type) {
			case float64:
				s.buffer[i].Floats = append(s.buffer[i].Floats, t)
			case int64:
				s.buffer[i].Floats = append(s.buffer[i].Floats, float64(t))
			case int:
				s.buffer[i].Floats = append(s.buffer[i].Floats, float64(t))
			default:
				return fmt.Errorf("%w: %s is a %T, want a number", ErrBadRow, col.Name, raw)
			}
		case TypeString:
			t, ok := raw.(string)
			if !ok {
				return fmt.Errorf("%w: %s is a %T, want a string", ErrBadRow, col.Name, raw)
			}
			s.buffer[i].Strings = append(s.buffer[i].Strings, t)
		case TypeBool:
			t, ok := raw.(bool)
			if !ok {
				return fmt.Errorf("%w: %s is a %T, want a bool", ErrBadRow, col.Name, raw)
			}
			s.buffer[i].Bools = append(s.buffer[i].Bools, t)
		}
	}
	s.buffered++
	if s.buffered >= s.blockRows {
		return s.sealBlockLocked()
	}
	return nil
}

// sealBlockLocked turns the buffer into an encoded block.
func (s *Store) sealBlockLocked() error {
	if s.buffered == 0 {
		return nil
	}
	block, err := BuildBlock(s.schema, s.buffer)
	if err != nil {
		return err
	}
	encoded, err := block.Encode(s.schema)
	if err != nil {
		return err
	}
	ti := s.schema.Index(s.schema.TimeColumn)
	if len(s.pending) == 0 {
		s.pendingMin, s.pendingMax = block.Stats[ti].MinInt, block.Stats[ti].MaxInt
	} else {
		if block.Stats[ti].MinInt < s.pendingMin {
			s.pendingMin = block.Stats[ti].MinInt
		}
		if block.Stats[ti].MaxInt > s.pendingMax {
			s.pendingMax = block.Stats[ti].MaxInt
		}
	}
	s.rawBytes += int64(block.RawBytes(s.schema))
	s.compressedByte += int64(len(encoded))
	s.rowsWritten += int64(block.Rows)
	s.pending = append(s.pending, encoded)
	s.resetBuffer()
	if len(s.pending) >= s.blocksPerSeg {
		return s.writeSegmentLocked()
	}
	return nil
}

// segMagic identifies a segment file.
var segMagic = [4]byte{'U', 'S', 'C', 'S'}

// writeSegmentLocked writes the pending blocks to a new segment file.
//
// The write is to a temporary name and then renamed, so a crash mid-write
// leaves a stray temporary rather than a half-written segment that the next
// query would try to parse. Rename on the same filesystem is atomic, which is
// the whole reason for the dance.
func (s *Store) writeSegmentLocked() error {
	if len(s.pending) == 0 {
		return nil
	}
	body := make([]byte, 0, 1<<20)
	body = append(body, segMagic[:]...)
	body = binary.LittleEndian.AppendUint16(body, blockFormatVersion)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(s.pending)))
	body = binary.LittleEndian.AppendUint64(body, uint64(s.pendingMin))
	body = binary.LittleEndian.AppendUint64(body, uint64(s.pendingMax))
	for _, blk := range s.pending {
		body = binary.LittleEndian.AppendUint32(body, uint32(len(blk)))
		body = append(body, blk...)
	}

	s.seq++
	name := fmt.Sprintf("seg-%020d-%06d.usc", s.pendingMin, s.seq)
	final := filepath.Join(s.tierDir(TierHot), name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return fmt.Errorf("columnar: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("columnar: publishing %s: %w", final, err)
	}
	s.pending = nil
	return nil
}

// Flush seals the partial block and writes any pending segment.
//
// A query reads only what has been flushed. That is a deliberate contract: the
// alternative is a query path that also walks the in-memory buffer under the
// write lock, which would put every read behind every write on a store taking
// 167,000 rows a second. The ingest loop flushes on a timer, so the visibility
// lag is bounded and small.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sealBlockLocked(); err != nil {
		return err
	}
	return s.writeSegmentLocked()
}

// Stats summarises what the store holds.
type Stats struct {
	// RowsWritten is how many rows have been sealed into blocks.
	RowsWritten int64 `json:"rows_written"`
	// RawBytes is what the same rows would occupy uncompressed.
	RawBytes int64 `json:"raw_bytes"`
	// CompressedBytes is what the blocks actually occupy.
	CompressedBytes int64 `json:"compressed_bytes"`
	// CompressionRatio is RawBytes / CompressedBytes.
	CompressionRatio float64 `json:"compression_ratio"`
	// Segments and Blocks are the file and block counts per tier.
	Segments map[string]int `json:"segments"`
	// BytesOnDisk is the total file size per tier.
	BytesOnDisk map[string]int64 `json:"bytes_on_disk"`
}

// Stats reports the store's size and its measured compression ratio.
func (s *Store) Stats() (Stats, error) {
	s.mu.RLock()
	st := Stats{
		RowsWritten: s.rowsWritten, RawBytes: s.rawBytes, CompressedBytes: s.compressedByte,
		Segments: map[string]int{}, BytesOnDisk: map[string]int64{},
	}
	s.mu.RUnlock()
	if st.CompressedBytes > 0 {
		st.CompressionRatio = float64(st.RawBytes) / float64(st.CompressedBytes)
	}
	for _, tier := range AllTiers() {
		entries, err := os.ReadDir(s.tierDir(tier))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Stats{}, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".usc") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			st.Segments[string(tier)]++
			st.BytesOnDisk[string(tier)] += info.Size()
		}
	}
	return st, nil
}

// segment is a segment file's header plus its raw blocks.
type segment struct {
	path     string
	tier     Tier
	minNanos int64
	maxNanos int64
	blocks   [][]byte
}

// loadSegment reads and validates a segment file.
func loadSegment(path string, tier Tier) (*segment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4+2+4+16 {
		return nil, fmt.Errorf("%w: %s is shorter than a segment header", ErrCorrupt, path)
	}
	if raw[0] != segMagic[0] || raw[1] != segMagic[1] || raw[2] != segMagic[2] || raw[3] != segMagic[3] {
		return nil, fmt.Errorf("%w: %s has bad magic", ErrCorrupt, path)
	}
	pos := 4
	if v := binary.LittleEndian.Uint16(raw[pos:]); v != blockFormatVersion {
		return nil, fmt.Errorf("%w: %s is version %d", ErrCorrupt, path, v)
	}
	pos += 2
	n := int(binary.LittleEndian.Uint32(raw[pos:]))
	pos += 4
	seg := &segment{
		path: path, tier: tier,
		minNanos: int64(binary.LittleEndian.Uint64(raw[pos:])),
		maxNanos: int64(binary.LittleEndian.Uint64(raw[pos+8:])),
	}
	pos += 16
	if n < 0 || n > len(raw) {
		return nil, fmt.Errorf("%w: %s claims %d blocks", ErrCorrupt, path, n)
	}
	seg.blocks = make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if pos+4 > len(raw) {
			return nil, fmt.Errorf("%w: %s truncated at block %d", ErrCorrupt, path, i)
		}
		l := int(binary.LittleEndian.Uint32(raw[pos:]))
		pos += 4
		if pos+l > len(raw) {
			return nil, fmt.Errorf("%w: %s block %d claims %d bytes", ErrCorrupt, path, i, l)
		}
		seg.blocks = append(seg.blocks, raw[pos:pos+l])
		pos += l
	}
	return seg, nil
}

// segments lists the store's segment files in time order, optionally restricted
// to a time range.
//
// The file-level range check is the first level of pruning: a query for the
// last hour opens one or two files out of a year's worth without reading a byte
// of any other. The block-level check inside a file is the second.
func (s *Store) segments(fromNanos, toNanos int64, tiers []Tier) ([]*segment, int, error) {
	if len(tiers) == 0 {
		tiers = AllTiers()
	}
	var out []*segment
	skippedFiles := 0
	for _, tier := range tiers {
		entries, err := os.ReadDir(s.tierDir(tier))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, 0, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".usc") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			// The file name carries the minimum timestamp, so a file wholly
			// after the query range is skipped without opening it.
			if min, ok := minNanosFromName(name); ok && toNanos != 0 && min > toNanos {
				skippedFiles++
				continue
			}
			seg, err := loadSegment(filepath.Join(s.tierDir(tier), name), tier)
			if err != nil {
				return nil, 0, err
			}
			if fromNanos != 0 && seg.maxNanos < fromNanos {
				skippedFiles++
				continue
			}
			if toNanos != 0 && seg.minNanos > toNanos {
				skippedFiles++
				continue
			}
			out = append(out, seg)
		}
	}
	return out, skippedFiles, nil
}

// minNanosFromName parses the timestamp out of a segment file name.
func minNanosFromName(name string) (int64, bool) {
	if !strings.HasPrefix(name, "seg-") {
		return 0, false
	}
	rest := strings.TrimPrefix(name, "seg-")
	dash := strings.IndexByte(rest, '-')
	if dash < 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(rest[:dash], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// MoveTier relocates every segment whose newest row is older than before,
// implementing the hot-to-warm and warm-to-cold transitions.
//
// It is a rename, not a rewrite. Moving a day of telemetry between tiers is
// therefore a few hundred directory operations rather than a re-encode of tens
// of gigabytes, which is what makes a nightly tiering job finish inside its
// window.
func (s *Store) MoveTier(from, to Tier, before time.Time) (moved int, err error) {
	entries, err := os.ReadDir(s.tierDir(from))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cut := before.UnixNano()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".usc") {
			continue
		}
		path := filepath.Join(s.tierDir(from), e.Name())
		seg, err := loadSegment(path, from)
		if err != nil {
			return moved, err
		}
		if seg.maxNanos >= cut {
			continue
		}
		if err := os.Rename(path, filepath.Join(s.tierDir(to), e.Name())); err != nil {
			return moved, fmt.Errorf("columnar: moving %s to %s: %w", e.Name(), to, err)
		}
		moved++
	}
	return moved, nil
}

// DropBefore deletes every segment in a tier whose newest row is older than the
// cut.
//
// The comparison is against the segment's *newest* row, so a segment straddling
// the cut is kept whole. Deleting it would silently drop rows the policy says
// to retain, and the alternative — rewriting the segment without them — would
// turn retention into a compaction job. Keeping a segment up to ten minutes
// past its retention is the cheaper mistake.
func (s *Store) DropBefore(tier Tier, before time.Time) (dropped int, freed int64, err error) {
	entries, err := os.ReadDir(s.tierDir(tier))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	cut := before.UnixNano()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".usc") {
			continue
		}
		path := filepath.Join(s.tierDir(tier), e.Name())
		seg, err := loadSegment(path, tier)
		if err != nil {
			return dropped, freed, err
		}
		if seg.maxNanos >= cut {
			continue
		}
		info, statErr := os.Stat(path)
		if err := os.Remove(path); err != nil {
			return dropped, freed, err
		}
		dropped++
		if statErr == nil {
			freed += info.Size()
		}
	}
	return dropped, freed, nil
}

// Close flushes anything buffered.
func (s *Store) Close() error { return s.Flush() }
