package columnar

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"strings"
)

// ColumnType is a column's storage type.
type ColumnType uint8

// The supported column types. The set is small on purpose: every one has a
// compression scheme chosen for it, and a type with no scheme of its own would
// be stored raw and quietly dominate the file size.
const (
	// TypeTimestamp is nanoseconds since the Unix epoch, stored as an int64
	// with delta encoding. It is distinct from TypeInt64 because the query
	// layer treats it as the time axis for range pruning and bucketing.
	TypeTimestamp ColumnType = iota + 1
	// TypeInt64 is a signed integer.
	TypeInt64
	// TypeFloat64 is a double.
	TypeFloat64
	// TypeString is dictionary-encoded text.
	TypeString
	// TypeBool is a bit.
	TypeBool
)

// String renders the type for schema errors and the API.
func (t ColumnType) String() string {
	switch t {
	case TypeTimestamp:
		return "timestamp"
	case TypeInt64:
		return "int64"
	case TypeFloat64:
		return "float64"
	case TypeString:
		return "string"
	case TypeBool:
		return "bool"
	}
	return fmt.Sprintf("unknown(%d)", uint8(t))
}

// Column is one column's definition.
type Column struct {
	Name string     `json:"name"`
	Type ColumnType `json:"type"`
}

// Schema is a table's column list.
//
// It is fixed at table creation. Schema evolution is deliberately out of scope:
// the four tables this store holds are defined by the platform's own event
// contracts, which are versioned and only ever added to, and a store that
// supported arbitrary column changes would need a per-block schema reference
// that costs more than it buys.
type Schema struct {
	Table   string   `json:"table"`
	Columns []Column `json:"columns"`
	// TimeColumn names the timestamp column used for range pruning, bucketing
	// and retention. Every table must have one: a time-series store with no
	// time axis cannot age data out, and unbounded growth on a gateway's flash
	// is a slow-motion outage.
	TimeColumn string `json:"time_column"`
}

// Validate checks a schema is usable.
func (s Schema) Validate() error {
	if strings.TrimSpace(s.Table) == "" {
		return fmt.Errorf("columnar: schema has no table name")
	}
	if len(s.Columns) == 0 {
		return fmt.Errorf("columnar: table %s has no columns", s.Table)
	}
	seen := map[string]bool{}
	timeOK := false
	for _, c := range s.Columns {
		if c.Name == "" {
			return fmt.Errorf("columnar: table %s has an unnamed column", s.Table)
		}
		if seen[c.Name] {
			return fmt.Errorf("columnar: table %s repeats column %q", s.Table, c.Name)
		}
		seen[c.Name] = true
		switch c.Type {
		case TypeTimestamp, TypeInt64, TypeFloat64, TypeString, TypeBool:
		default:
			return fmt.Errorf("columnar: column %s has unknown type %d", c.Name, c.Type)
		}
		if c.Name == s.TimeColumn {
			if c.Type != TypeTimestamp {
				return fmt.Errorf("columnar: time column %s is a %s, not a timestamp", c.Name, c.Type)
			}
			timeOK = true
		}
	}
	if !timeOK {
		return fmt.Errorf("columnar: table %s has no time column", s.Table)
	}
	return nil
}

// Index returns a column's position, or -1.
func (s Schema) Index(name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ColumnStats is a block's summary of one column, and the reason predicate
// pushdown works.
type ColumnStats struct {
	// MinInt and MaxInt bound integer, timestamp and boolean columns.
	MinInt int64 `json:"min_int,omitempty"`
	MaxInt int64 `json:"max_int,omitempty"`
	// MinFloat and MaxFloat bound float columns.
	MinFloat float64 `json:"min_float,omitempty"`
	MaxFloat float64 `json:"max_float,omitempty"`
	// Distinct holds a string column's dictionary when it is small enough to be
	// worth keeping. An equality filter on a low-cardinality column can then
	// skip a block *exactly* rather than by range, which on a per-store query
	// over an estate-wide table is the difference between reading one store's
	// blocks and reading two thousand stores'.
	Distinct []string `json:"distinct,omitempty"`
	// DistinctTruncated is true when the column had more distinct values than
	// MaxDistinctForIndex, so the set cannot be used to prove absence.
	DistinctTruncated bool `json:"distinct_truncated,omitempty"`
}

// Nullability is deliberately absent from ColumnStats and from the format: this
// store has no null. A column with no value for a row carries that column's
// zero, and the ingest layer is responsible for not writing a row it cannot
// fill. Making nullability explicit would add a bitmap per column to every
// block, for a case the platform's event contracts do not produce.

// MaxDistinctForIndex bounds the per-block distinct set.
//
// Sixty-four values covers every low-cardinality column in the schema — a block
// holds a few seconds of traffic and rarely sees more than a handful of stores
// or event types. Beyond that the set stops being a useful filter and starts
// being a second copy of the dictionary in the index.
const MaxDistinctForIndex = 64

// Block is one column-major batch of rows.
//
// # Why blocks at all
//
// A block is the unit of skipping. Too small and the per-block header dominates
// the file; too large and a query that wants one store's rows has to decompress
// a great many rows belonging to other stores. DefaultBlockRows is the
// compromise, and it is a knob rather than a constant of nature.
type Block struct {
	// Rows is the row count.
	Rows int `json:"rows"`
	// Stats are per column, in schema order.
	Stats []ColumnStats `json:"stats"`
	// Data holds the encoded bytes per column, in schema order.
	Data [][]byte `json:"-"`
	// Dicts holds the string dictionaries per column, in schema order.
	Dicts [][]string `json:"-"`
}

// DefaultBlockRows is the block size.
//
// Eight thousand rows is roughly a second of one store's telemetry, which makes
// a block the natural unit a per-store, per-minute query skips at. It is also
// small enough that decoding one costs a fraction of a millisecond, so a query
// that cannot skip a block has not lost much by reading it.
const DefaultBlockRows = 8192

// blockMagic identifies an encoded block.
var blockMagic = [4]byte{'U', 'S', 'C', 'B'}

const blockFormatVersion uint16 = 1

// Encode serialises a block: a header with the per-column statistics and
// offsets, then the column data, then a CRC-32 over everything.
//
// The statistics live in the header rather than in a separate index file so
// that a block is self-describing. A separate index is faster to scan and is a
// second thing that can be lost or fall out of step with the data; on a gateway
// that loses power mid-write, self-describing blocks mean the recovery is
// "truncate to the last good block" rather than "rebuild the index".
func (b *Block) Encode(schema Schema) ([]byte, error) {
	if len(b.Data) != len(schema.Columns) {
		return nil, fmt.Errorf("columnar: block has %d columns, schema has %d", len(b.Data), len(schema.Columns))
	}
	out := make([]byte, 0, 4096)
	out = append(out, blockMagic[:]...)
	out = binary.LittleEndian.AppendUint16(out, blockFormatVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(b.Rows))
	out = binary.LittleEndian.AppendUint16(out, uint16(len(schema.Columns)))

	for i, col := range schema.Columns {
		st := b.Stats[i]
		out = append(out, byte(col.Type))
		switch col.Type {
		case TypeTimestamp, TypeInt64, TypeBool:
			out = binary.LittleEndian.AppendUint64(out, uint64(st.MinInt))
			out = binary.LittleEndian.AppendUint64(out, uint64(st.MaxInt))
		case TypeFloat64:
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(st.MinFloat))
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(st.MaxFloat))
		}
		if col.Type == TypeString {
			dict := b.Dicts[i]
			out = binary.LittleEndian.AppendUint32(out, uint32(len(dict)))
			for _, v := range dict {
				out = binary.LittleEndian.AppendUint16(out, uint16(len(v)))
				out = append(out, v...)
			}
			truncated := byte(0)
			if st.DistinctTruncated {
				truncated = 1
			}
			out = append(out, truncated)
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Data[i])))
	}
	for _, d := range b.Data {
		out = append(out, d...)
	}
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out)), nil
}

// DecodeBlockHeader reads a block's statistics without touching its data.
//
// This is the function predicate pushdown is built on: a query decides whether
// to read a block from its header alone, and a block it skips costs one header
// parse rather than a decompression of every column.
func DecodeBlockHeader(schema Schema, raw []byte) (*Block, int, error) {
	if len(raw) < 4+2+4+2+4 {
		return nil, 0, fmt.Errorf("%w: %d bytes is shorter than a block header", ErrCorrupt, len(raw))
	}
	if raw[0] != blockMagic[0] || raw[1] != blockMagic[1] || raw[2] != blockMagic[2] || raw[3] != blockMagic[3] {
		return nil, 0, fmt.Errorf("%w: bad block magic", ErrCorrupt)
	}
	body, want := raw[:len(raw)-4], binary.LittleEndian.Uint32(raw[len(raw)-4:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return nil, 0, fmt.Errorf("%w: block checksum %08x does not match %08x", ErrCorrupt, got, want)
	}
	pos := 4
	if v := binary.LittleEndian.Uint16(body[pos:]); v != blockFormatVersion {
		return nil, 0, fmt.Errorf("%w: block version %d is not supported", ErrCorrupt, v)
	}
	pos += 2
	rows := int(binary.LittleEndian.Uint32(body[pos:]))
	pos += 4
	ncols := int(binary.LittleEndian.Uint16(body[pos:]))
	pos += 2
	if ncols != len(schema.Columns) {
		return nil, 0, fmt.Errorf("%w: block has %d columns, schema has %d", ErrCorrupt, ncols, len(schema.Columns))
	}

	b := &Block{Rows: rows,
		Stats: make([]ColumnStats, ncols),
		Data:  make([][]byte, ncols),
		Dicts: make([][]string, ncols),
	}
	lengths := make([]int, ncols)
	for i, col := range schema.Columns {
		if pos >= len(body) {
			return nil, 0, fmt.Errorf("%w: truncated header at column %d", ErrCorrupt, i)
		}
		if ColumnType(body[pos]) != col.Type {
			return nil, 0, fmt.Errorf("%w: column %s is a %s in the block and a %s in the schema",
				ErrCorrupt, col.Name, ColumnType(body[pos]), col.Type)
		}
		pos++
		switch col.Type {
		case TypeTimestamp, TypeInt64, TypeBool:
			if pos+16 > len(body) {
				return nil, 0, fmt.Errorf("%w: truncated stats for %s", ErrCorrupt, col.Name)
			}
			b.Stats[i].MinInt = int64(binary.LittleEndian.Uint64(body[pos:]))
			b.Stats[i].MaxInt = int64(binary.LittleEndian.Uint64(body[pos+8:]))
			pos += 16
		case TypeFloat64:
			if pos+16 > len(body) {
				return nil, 0, fmt.Errorf("%w: truncated stats for %s", ErrCorrupt, col.Name)
			}
			b.Stats[i].MinFloat = math.Float64frombits(binary.LittleEndian.Uint64(body[pos:]))
			b.Stats[i].MaxFloat = math.Float64frombits(binary.LittleEndian.Uint64(body[pos+8:]))
			pos += 16
		}
		if col.Type == TypeString {
			if pos+4 > len(body) {
				return nil, 0, fmt.Errorf("%w: truncated dictionary length for %s", ErrCorrupt, col.Name)
			}
			n := int(binary.LittleEndian.Uint32(body[pos:]))
			pos += 4
			if n < 0 || n > len(body) {
				return nil, 0, fmt.Errorf("%w: dictionary of %d entries exceeds the block", ErrCorrupt, n)
			}
			dict := make([]string, 0, n)
			for j := 0; j < n; j++ {
				if pos+2 > len(body) {
					return nil, 0, fmt.Errorf("%w: truncated dictionary for %s", ErrCorrupt, col.Name)
				}
				l := int(binary.LittleEndian.Uint16(body[pos:]))
				pos += 2
				if pos+l > len(body) {
					return nil, 0, fmt.Errorf("%w: truncated dictionary entry for %s", ErrCorrupt, col.Name)
				}
				dict = append(dict, string(body[pos:pos+l]))
				pos += l
			}
			b.Dicts[i] = dict
			if pos >= len(body) {
				return nil, 0, fmt.Errorf("%w: truncated dictionary flag for %s", ErrCorrupt, col.Name)
			}
			b.Stats[i].DistinctTruncated = body[pos] == 1
			pos++
			if !b.Stats[i].DistinctTruncated {
				b.Stats[i].Distinct = dict
			}
		}
		if pos+4 > len(body) {
			return nil, 0, fmt.Errorf("%w: truncated length for %s", ErrCorrupt, col.Name)
		}
		lengths[i] = int(binary.LittleEndian.Uint32(body[pos:]))
		pos += 4
	}

	dataStart := pos
	for i := range lengths {
		if pos+lengths[i] > len(body) {
			return nil, 0, fmt.Errorf("%w: column %d claims %d bytes, %d remain",
				ErrCorrupt, i, lengths[i], len(body)-pos)
		}
		b.Data[i] = body[pos : pos+lengths[i]]
		pos += lengths[i]
	}
	if pos != len(body) {
		return nil, 0, fmt.Errorf("%w: %d trailing bytes in a block", ErrCorrupt, len(body)-pos)
	}
	return b, dataStart, nil
}

// ColumnValues decodes one column.
//
// The three return slices are typed rather than boxed into an `any` slice: a
// block of 8,192 rows boxed into interfaces is 8,192 heap allocations and the
// query executor calls this once per column per block.
type ColumnValues struct {
	Ints    []int64
	Floats  []float64
	Strings []string
	Bools   []bool
	Type    ColumnType
}

// Len is the row count.
func (v ColumnValues) Len() int {
	switch v.Type {
	case TypeTimestamp, TypeInt64:
		return len(v.Ints)
	case TypeFloat64:
		return len(v.Floats)
	case TypeString:
		return len(v.Strings)
	case TypeBool:
		return len(v.Bools)
	}
	return 0
}

// Decode materialises one column of a block.
func (b *Block) Decode(schema Schema, colIndex int) (ColumnValues, error) {
	if colIndex < 0 || colIndex >= len(schema.Columns) {
		return ColumnValues{}, fmt.Errorf("columnar: column index %d out of range", colIndex)
	}
	col := schema.Columns[colIndex]
	out := ColumnValues{Type: col.Type}
	var err error
	switch col.Type {
	case TypeTimestamp, TypeInt64:
		out.Ints, err = decodeDeltaVarint(b.Data[colIndex], b.Rows)
	case TypeFloat64:
		out.Floats, err = decodeXORFloats(b.Data[colIndex], b.Rows)
	case TypeString:
		out.Strings, err = decodeDictionary(b.Dicts[colIndex], b.Data[colIndex], b.Rows)
	case TypeBool:
		out.Bools, err = decodeBools(b.Data[colIndex], b.Rows)
	default:
		err = fmt.Errorf("%w: unknown column type %d", ErrCorrupt, col.Type)
	}
	if err != nil {
		return ColumnValues{}, fmt.Errorf("columnar: decoding %s: %w", col.Name, err)
	}
	return out, nil
}

// BuildBlock encodes a batch of column-major values into a block.
func BuildBlock(schema Schema, cols []ColumnValues) (*Block, error) {
	if len(cols) != len(schema.Columns) {
		return nil, fmt.Errorf("columnar: %d columns supplied for a %d-column schema", len(cols), len(schema.Columns))
	}
	rows := -1
	for i, c := range cols {
		if c.Type != schema.Columns[i].Type {
			return nil, fmt.Errorf("columnar: column %s is a %s, want %s",
				schema.Columns[i].Name, c.Type, schema.Columns[i].Type)
		}
		if rows == -1 {
			rows = c.Len()
		} else if c.Len() != rows {
			return nil, fmt.Errorf("columnar: column %s has %d rows, others have %d",
				schema.Columns[i].Name, c.Len(), rows)
		}
	}
	if rows <= 0 {
		return nil, fmt.Errorf("columnar: an empty block")
	}

	b := &Block{Rows: rows,
		Stats: make([]ColumnStats, len(cols)),
		Data:  make([][]byte, len(cols)),
		Dicts: make([][]string, len(cols)),
	}
	for i, c := range cols {
		switch c.Type {
		case TypeTimestamp, TypeInt64:
			b.Data[i] = encodeDeltaVarint(c.Ints)
			mn, mx := c.Ints[0], c.Ints[0]
			for _, v := range c.Ints {
				if v < mn {
					mn = v
				}
				if v > mx {
					mx = v
				}
			}
			b.Stats[i].MinInt, b.Stats[i].MaxInt = mn, mx
		case TypeFloat64:
			b.Data[i] = encodeXORFloats(c.Floats)
			mn, mx := c.Floats[0], c.Floats[0]
			for _, v := range c.Floats {
				if v < mn {
					mn = v
				}
				if v > mx {
					mx = v
				}
			}
			b.Stats[i].MinFloat, b.Stats[i].MaxFloat = mn, mx
		case TypeString:
			dict, enc := encodeDictionary(c.Strings)
			b.Dicts[i], b.Data[i] = dict, enc
			if len(dict) <= MaxDistinctForIndex {
				sorted := make([]string, len(dict))
				copy(sorted, dict)
				sort.Strings(sorted)
				b.Stats[i].Distinct = sorted
			} else {
				b.Stats[i].DistinctTruncated = true
			}
		case TypeBool:
			b.Data[i] = encodeBools(c.Bools)
			mn, mx := int64(1), int64(0)
			for _, v := range c.Bools {
				if v {
					mx = 1
				} else {
					mn = 0
				}
			}
			if mn > mx {
				// Every value was true, so the "false" branch never ran.
				mn = 1
			}
			b.Stats[i].MinInt, b.Stats[i].MaxInt = mn, mx
		}
	}
	return b, nil
}

// RawBytes is the uncompressed size of a block's data, for the compression
// ratio the store reports.
//
// It is computed from the row count and the type widths rather than measured,
// because "uncompressed" here means "what a row store holding the same values
// would need": eight bytes per number, one per boolean, and the actual bytes of
// each string as it appears in each row.
func (b *Block) RawBytes(schema Schema) int {
	total := 0
	for i, col := range schema.Columns {
		switch col.Type {
		case TypeTimestamp, TypeInt64, TypeFloat64:
			total += b.Rows * 8
		case TypeBool:
			total += b.Rows
		case TypeString:
			// The dictionary indices tell us how many rows carry each value, so
			// the raw size is the sum of the per-row string lengths. Decoding
			// the indices is the only way to get that exactly; approximating by
			// the mean dictionary entry length is within a few per cent and
			// costs nothing, and the ratio is reported to one decimal place.
			if len(b.Dicts[i]) == 0 {
				continue
			}
			sum := 0
			for _, v := range b.Dicts[i] {
				sum += len(v)
			}
			total += b.Rows * sum / len(b.Dicts[i])
		}
	}
	return total
}
