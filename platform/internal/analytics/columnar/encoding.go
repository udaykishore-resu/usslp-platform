// Package columnar is USSLP's column-oriented time-series store: the
// ClickHouse stand-in that the platform ships when a tenant is not running one.
//
// # What it is for
//
// Four streams feed it — label telemetry, delivery confirmations, price updates
// and promotion events — at a combined rate that reaches 167,000 rows a second
// across a large estate. The questions asked of it are analytical, not
// transactional: "p99 delivery latency by store for the last week", "units per
// day against price for this SKU", "which stores are furthest from the
// benchmark". Every one of those reads two or three columns out of fifteen and
// scans millions of rows, which is the shape a column store is for and the
// shape a row store is worst at.
//
// # The three things that make it fast
//
//   - Column-major layout, so a query reads only the columns it names. A
//     latency percentile touches two columns of a fifteen-column table and
//     therefore reads about a seventh of the bytes.
//   - Per-column compression chosen for what the column actually holds:
//     delta plus zigzag varint for the monotonic timestamps and small-range
//     integers, XOR-of-previous for floats, and a per-block dictionary for the
//     low-cardinality strings that dominate the schema (store ids, event types,
//     firmware versions). The measured ratios are in the package's tests.
//   - A per-block minimum and maximum for every column, so a query with a time
//     range or an equality filter skips whole blocks without decompressing
//     them. On a week-scoped query over a year of data that is a fiftyfold
//     reduction in work before a single row is decoded.
package columnar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// ErrCorrupt marks data that cannot be decoded.
var ErrCorrupt = errors.New("columnar: corrupt block")

// ---------------------------------------------------------------------------
// Varint and zigzag
// ---------------------------------------------------------------------------

// zigzag maps a signed integer onto an unsigned one so that small magnitudes of
// either sign encode short. Without it a delta of -1 encodes as ten bytes.
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

func unzigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func readUvarint(b []byte, pos int) (uint64, int, error) {
	v, n := binary.Uvarint(b[pos:])
	if n <= 0 {
		return 0, pos, fmt.Errorf("%w: malformed varint at offset %d", ErrCorrupt, pos)
	}
	return v, pos + n, nil
}

// ---------------------------------------------------------------------------
// Integer and timestamp columns: delta + zigzag varint
// ---------------------------------------------------------------------------

// encodeDeltaVarint compresses a run of int64s as a first value followed by
// zigzag-varint deltas.
//
// # Why delta and not the values themselves
//
// The two columns that dominate this store's volume are the timestamp and the
// latency. Timestamps arrive in near-order and differ by microseconds, so the
// deltas are one or two bytes where the absolute values are eight. Latencies
// cluster tightly around a mean, so their deltas are small too. Integers whose
// deltas are *not* small — a hash, a random id — encode at nine bytes instead
// of eight, and the block header records the encoding so a future column type
// can opt out; no column in the platform's schema is of that shape.
func encodeDeltaVarint(values []int64) []byte {
	if len(values) == 0 {
		return nil
	}
	out := make([]byte, 0, len(values)*2)
	out = appendUvarint(out, zigzag(values[0]))
	prev := values[0]
	for _, v := range values[1:] {
		out = appendUvarint(out, zigzag(v-prev))
		prev = v
	}
	return out
}

func decodeDeltaVarint(b []byte, count int) ([]int64, error) {
	if count == 0 {
		return nil, nil
	}
	out := make([]int64, 0, count)
	pos := 0
	raw, pos, err := readUvarint(b, pos)
	if err != nil {
		return nil, err
	}
	cur := unzigzag(raw)
	out = append(out, cur)
	for i := 1; i < count; i++ {
		raw, pos, err = readUvarint(b, pos)
		if err != nil {
			return nil, err
		}
		cur += unzigzag(raw)
		out = append(out, cur)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Float columns: XOR of the previous value
// ---------------------------------------------------------------------------

// encodeXORFloats compresses float64s by XOR-ing each against its predecessor
// and storing only the meaningful bits.
//
// # Why XOR rather than delta
//
// Floating-point deltas are not floating-point-friendly: subtracting two nearby
// float64s loses precision and the result still needs eight bytes. XOR-ing them
// is exact and, for a series that moves slowly — a temperature, a battery
// voltage, a price — leaves the exponent and most of the mantissa identical, so
// the XOR is mostly zeroes. The encoding is the one from Facebook's Gorilla
// paper: a single bit for "identical", otherwise the count of leading and
// meaningful bits followed by the meaningful bits themselves.
//
// On the telemetry columns this store actually holds, that is measured at
// between three and six times smaller than raw float64s; on genuinely random
// floats it is slightly *larger*, which is the honest trade and is why the
// block header records which encoding a column used.
func encodeXORFloats(values []float64) []byte {
	if len(values) == 0 {
		return nil
	}
	w := newBitWriter(len(values))
	prev := math.Float64bits(values[0])
	w.writeBits(prev, 64)
	prevLeading, prevTrailing := uint8(255), uint8(0)

	for _, v := range values[1:] {
		cur := math.Float64bits(v)
		x := cur ^ prev
		if x == 0 {
			// Identical to the predecessor: one bit. Constant columns — a
			// firmware version's numeric form, a flag that never changes —
			// collapse to almost nothing.
			w.writeBit(0)
			prev = cur
			continue
		}
		w.writeBit(1)
		leading := uint8(bits.LeadingZeros64(x))
		trailing := uint8(bits.TrailingZeros64(x))
		if leading >= 32 {
			// The leading-zero count is stored in five bits, so 31 is the most
			// that can be expressed. Clamping costs a few wasted bits on a
			// value that was going to be cheap anyway.
			leading = 31
		}
		if prevLeading != 255 && leading >= prevLeading && trailing >= prevTrailing {
			// The meaningful bits fit inside the previous window: reuse it and
			// spend one bit instead of thirteen.
			w.writeBit(0)
			w.writeBits(x>>prevTrailing, int(64-prevLeading-prevTrailing))
		} else {
			w.writeBit(1)
			w.writeBits(uint64(leading), 5)
			meaningful := 64 - int(leading) - int(trailing)
			// A meaningful width of 64 would need seven bits; it cannot occur,
			// because a non-zero XOR has at least one leading or trailing zero
			// unless it is exactly 0xFFFF...  which would mean the two values
			// differ in every bit. Storing width-1 in six bits covers 1..64.
			w.writeBits(uint64(meaningful-1), 6)
			w.writeBits(x>>trailing, meaningful)
			prevLeading, prevTrailing = leading, trailing
		}
		prev = cur
	}
	return w.bytes()
}

func decodeXORFloats(b []byte, count int) ([]float64, error) {
	if count == 0 {
		return nil, nil
	}
	r := newBitReader(b)
	first, err := r.readBits(64)
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, count)
	out = append(out, math.Float64frombits(first))
	prev := first
	var leading, trailing uint8

	for i := 1; i < count; i++ {
		bit, err := r.readBit()
		if err != nil {
			return nil, err
		}
		if bit == 0 {
			out = append(out, math.Float64frombits(prev))
			continue
		}
		ctrl, err := r.readBit()
		if err != nil {
			return nil, err
		}
		if ctrl == 1 {
			l, err := r.readBits(5)
			if err != nil {
				return nil, err
			}
			w, err := r.readBits(6)
			if err != nil {
				return nil, err
			}
			leading = uint8(l)
			meaningful := int(w) + 1
			if meaningful < 1 || meaningful > 64 || int(leading)+meaningful > 64 {
				return nil, fmt.Errorf("%w: xor window %d/%d is impossible", ErrCorrupt, leading, meaningful)
			}
			trailing = uint8(64 - int(leading) - meaningful)
		}
		width := 64 - int(leading) - int(trailing)
		if width <= 0 || width > 64 {
			return nil, fmt.Errorf("%w: xor width %d", ErrCorrupt, width)
		}
		v, err := r.readBits(width)
		if err != nil {
			return nil, err
		}
		prev ^= v << trailing
		out = append(out, math.Float64frombits(prev))
	}
	return out, nil
}

// bitWriter appends bits most-significant first.
type bitWriter struct {
	buf  []byte
	cur  byte
	bits uint8
}

func newBitWriter(hint int) *bitWriter {
	return &bitWriter{buf: make([]byte, 0, hint*4)}
}

func (w *bitWriter) writeBit(b uint8) {
	w.cur = (w.cur << 1) | (b & 1)
	w.bits++
	if w.bits == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur, w.bits = 0, 0
	}
}

func (w *bitWriter) writeBits(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		w.writeBit(uint8((v >> uint(i)) & 1))
	}
}

// bytes flushes the partial byte. The padding bits are zeroes and are never
// read, because the decoder is driven by the row count rather than by the byte
// length.
func (w *bitWriter) bytes() []byte {
	if w.bits > 0 {
		w.buf = append(w.buf, w.cur<<(8-w.bits))
		w.cur, w.bits = 0, 0
	}
	return w.buf
}

// bitReader reads bits most-significant first.
type bitReader struct {
	buf []byte
	pos int
	bit uint8
}

func newBitReader(b []byte) *bitReader { return &bitReader{buf: b} }

func (r *bitReader) readBit() (uint8, error) {
	if r.pos >= len(r.buf) {
		return 0, fmt.Errorf("%w: ran off the end of a bit stream", ErrCorrupt)
	}
	v := (r.buf[r.pos] >> (7 - r.bit)) & 1
	r.bit++
	if r.bit == 8 {
		r.bit = 0
		r.pos++
	}
	return v, nil
}

func (r *bitReader) readBits(n int) (uint64, error) {
	var v uint64
	for i := 0; i < n; i++ {
		b, err := r.readBit()
		if err != nil {
			return 0, err
		}
		v = (v << 1) | uint64(b)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// String columns: per-block dictionary
// ---------------------------------------------------------------------------

// encodeDictionary compresses strings as a table of distinct values plus one
// varint index per row.
//
// # Why a dictionary and why per block
//
// Every string column in this store's schema is low cardinality: a store
// identifier repeats across every row that store produced, an event type takes
// one of a dozen values, a firmware version one of five. Storing the string
// once and a one-byte index per row turns a 24-byte store id into a single
// byte, which on the delivery table is the single largest saving the format
// makes.
//
// The dictionary is per block rather than global because a global one is
// mutable shared state that every writer must coordinate on, and because block
// locality is real: a block covers a few seconds of one store's traffic and
// usually contains one or two distinct values, so its dictionary is tiny.
// A global dictionary would compress marginally better and would make every
// block un-decodable without it.
func encodeDictionary(values []string) (dict []string, encoded []byte) {
	if len(values) == 0 {
		return nil, nil
	}
	index := make(map[string]int, 8)
	dict = make([]string, 0, 8)
	encoded = make([]byte, 0, len(values))
	for _, v := range values {
		id, ok := index[v]
		if !ok {
			id = len(dict)
			index[v] = id
			dict = append(dict, v)
		}
		encoded = appendUvarint(encoded, uint64(id))
	}
	return dict, encoded
}

func decodeDictionary(dict []string, b []byte, count int) ([]string, error) {
	out := make([]string, 0, count)
	pos := 0
	for i := 0; i < count; i++ {
		id, next, err := readUvarint(b, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		if int(id) >= len(dict) {
			return nil, fmt.Errorf("%w: dictionary index %d of %d", ErrCorrupt, id, len(dict))
		}
		out = append(out, dict[id])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Boolean columns: bitmap
// ---------------------------------------------------------------------------

// encodeBools packs booleans one bit each. Nothing cleverer is worth it: a
// bitmap is already eight times smaller than a byte per row, and run-length
// encoding on top would win only on the columns that are constant, which the
// block's own min/max index already lets a query skip entirely.
func encodeBools(values []bool) []byte {
	out := make([]byte, (len(values)+7)/8)
	for i, v := range values {
		if v {
			out[i/8] |= 1 << uint(7-i%8)
		}
	}
	return out
}

func decodeBools(b []byte, count int) ([]bool, error) {
	if len(b) < (count+7)/8 {
		return nil, fmt.Errorf("%w: %d bytes cannot hold %d booleans", ErrCorrupt, len(b), count)
	}
	out := make([]bool, count)
	for i := 0; i < count; i++ {
		out[i] = b[i/8]&(1<<uint(7-i%8)) != 0
	}
	return out, nil
}
