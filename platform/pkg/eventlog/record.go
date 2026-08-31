package eventlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
)

// castagnoli is CRC32C, the polynomial with hardware support on every CPU this
// platform targets. Checksumming every record on every read is only affordable
// because of that instruction; without it the verification would be the
// bottleneck rather than the disk.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt reports a record that failed its length or CRC check. Callers in
// this package treat it as end-of-segment rather than as a fatal error: a
// half-written record at the tail is the normal shape of a crash, and refusing
// to open the log because of one would turn a recoverable event into an outage.
var ErrCorrupt = errors.New("eventlog: corrupt record")

const (
	// maxRecordBytes bounds a single framed record. It exists so that a
	// corrupted length prefix cannot make the reader attempt a multi-gigabyte
	// allocation before the CRC ever gets a chance to reject it.
	maxRecordBytes = 64 << 20

	lengthPrefixBytes = 4
	crcPrefixBytes    = 4
)

// headerGap marks a compaction gap record: a stub left behind when compaction
// discards a superseded value. The stub keeps the segment's record count — and
// therefore every later offset — unchanged, which is what lets compaction run
// without renumbering offsets that consumers have already committed. Readers
// count gap records for offset accounting and never deliver them.
const headerGap = "usslp-eventlog-gap"

// record is one framed entry. Offsets are positional (baseOffset of the segment
// plus the record's index within it) and so are not stored in the frame.
type record struct {
	offset    int64
	timestamp int64 // unix nanoseconds
	key       []byte
	headers   map[string]string
	value     []byte
	gap       bool
}

// encodeRecord appends the wire form of r to dst and returns the extended
// slice. The frame is:
//
//	uint32 length      bytes that follow this field (crc + body)
//	uint32 crc         CRC32C of body
//	body:
//	  int64  timestamp   unix nanoseconds
//	  uint32 keyLen, key bytes
//	  uint32 headerCount, then repeated uint32 kLen, k, uint32 vLen, v
//	  uint32 valueLen, value bytes
//
// The CRC covers the body rather than only the value so that a flipped bit in a
// key or a header — which would silently mis-route a record to the wrong
// partition or the wrong tenant — is caught by the same check.
//
// Headers are written in sorted key order so that encoding is deterministic:
// two runs over the same record produce byte-identical segments, which is what
// makes compaction rewrites and test fixtures reproducible.
func encodeRecord(dst []byte, r record) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0) // length placeholder
	dst = append(dst, 0, 0, 0, 0) // crc placeholder
	bodyStart := len(dst)

	dst = binary.BigEndian.AppendUint64(dst, uint64(r.timestamp))
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(r.key)))
	dst = append(dst, r.key...)

	names := make([]string, 0, len(r.headers)+1)
	for k := range r.headers {
		if k == headerGap {
			continue
		}
		names = append(names, k)
	}
	if r.gap {
		names = append(names, headerGap)
	}
	sort.Strings(names)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(names)))
	for _, k := range names {
		v := r.headers[k]
		if k == headerGap {
			v = "1"
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(k)))
		dst = append(dst, k...)
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(v)))
		dst = append(dst, v...)
	}

	dst = binary.BigEndian.AppendUint32(dst, uint32(len(r.value)))
	dst = append(dst, r.value...)

	body := dst[bodyStart:]
	binary.BigEndian.PutUint32(dst[start:], uint32(crcPrefixBytes+len(body)))
	binary.BigEndian.PutUint32(dst[start+lengthPrefixBytes:], crc32.Checksum(body, castagnoli))
	return dst
}

// readRecord decodes one record from r.
//
// It distinguishes three outcomes that callers must handle differently: a clean
// io.EOF at a record boundary (the segment ended), an ErrCorrupt (the tail is
// damaged or truncated and the segment ends here), and success. n is the number
// of bytes consumed, which the caller uses to track the file position of the
// next record.
func readRecord(r io.Reader) (rec record, n int, err error) {
	var prefix [lengthPrefixBytes]byte
	read, err := io.ReadFull(r, prefix[:])
	switch {
	case errors.Is(err, io.EOF) && read == 0:
		return record{}, 0, io.EOF
	case err != nil:
		return record{}, 0, fmt.Errorf("%w: truncated length prefix (%d bytes)", ErrCorrupt, read)
	}
	total := binary.BigEndian.Uint32(prefix[:])
	if total < crcPrefixBytes || int64(total) > maxRecordBytes {
		return record{}, 0, fmt.Errorf("%w: implausible record length %d", ErrCorrupt, total)
	}
	buf := make([]byte, total)
	if _, err := io.ReadFull(r, buf); err != nil {
		return record{}, 0, fmt.Errorf("%w: truncated body: %v", ErrCorrupt, err)
	}
	want := binary.BigEndian.Uint32(buf[:crcPrefixBytes])
	body := buf[crcPrefixBytes:]
	if got := crc32.Checksum(body, castagnoli); got != want {
		return record{}, 0, fmt.Errorf("%w: crc mismatch (want %08x, got %08x)", ErrCorrupt, want, got)
	}
	rec, err = decodeBody(body)
	if err != nil {
		return record{}, 0, err
	}
	return rec, lengthPrefixBytes + int(total), nil
}

// decodeBody parses a CRC-verified body. Every length is bounds-checked anyway:
// a CRC proves the bytes are the bytes that were written, not that the writer
// wrote something sane, and a reader that trusts a length field is one bug in a
// future writer away from a panic in production.
func decodeBody(b []byte) (record, error) {
	c := &cursor{b: b}
	rec := record{timestamp: int64(c.uint64())}
	rec.key = c.blob()
	count := c.uint32()
	if c.err == nil && int64(count) > int64(len(b)) {
		return record{}, fmt.Errorf("%w: header count %d exceeds body size", ErrCorrupt, count)
	}
	for i := uint32(0); i < count && c.err == nil; i++ {
		k := c.blob()
		v := c.blob()
		if c.err != nil {
			break
		}
		name := string(k)
		if name == headerGap {
			rec.gap = true
			continue
		}
		if rec.headers == nil {
			rec.headers = make(map[string]string, count)
		}
		rec.headers[name] = string(v)
	}
	rec.value = c.blob()
	if c.err != nil {
		return record{}, c.err
	}
	if len(c.b) != 0 {
		return record{}, fmt.Errorf("%w: %d trailing bytes in record body", ErrCorrupt, len(c.b))
	}
	if len(rec.key) == 0 {
		rec.key = nil
	}
	return rec, nil
}

// cursor is a bounds-checked reader over a record body. It latches the first
// error so the decode path stays free of per-field error handling.
type cursor struct {
	b   []byte
	err error
}

func (c *cursor) take(n int) []byte {
	if c.err != nil {
		return nil
	}
	if n < 0 || n > len(c.b) {
		c.err = fmt.Errorf("%w: field of %d bytes exceeds remaining %d", ErrCorrupt, n, len(c.b))
		return nil
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out
}

func (c *cursor) uint32() uint32 {
	v := c.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (c *cursor) uint64() uint64 {
	v := c.take(8)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// blob reads a uint32-prefixed byte slice.
func (c *cursor) blob() []byte {
	n := c.uint32()
	if c.err != nil {
		return nil
	}
	return c.take(int(n))
}

// gapRecord builds the stub that compaction writes in place of a superseded
// record. The timestamp is preserved so that time-based retention still sees a
// correct maximum age for the segment and eventually deletes it.
func gapRecord(ts int64) record {
	return record{timestamp: ts, gap: true}
}
