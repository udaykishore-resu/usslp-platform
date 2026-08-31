package kvstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// Write-ahead log
//
// The WAL is the durability contract. A write is acknowledged only once its
// record has reached the log, so a Store Gateway Unit that loses power mid-way
// through a promotion rollout comes back with every price it acknowledged.
//
// Framing is deliberately boring: a 4-byte little-endian payload length, a
// 4-byte CRC-32C of the payload, then the payload. CRC-32C (Castagnoli) rather
// than the IEEE polynomial because it has hardware support on both the x86 and
// ARM SoCs the edge tier ships on, and checksumming every record on recovery of
// a multi-hundred-megabyte log is otherwise a measurable chunk of boot time.
//
// A batch is one record. That is what makes Batch atomic for free: a partially
// written batch is a partially written record, which fails its length or CRC
// check and is discarded wholesale on recovery. There is no separate commit
// marker to get wrong.
// ---------------------------------------------------------------------------

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// maxRecordBytes bounds what recovery will believe a length header. Without it,
// four bytes of garbage at the tail of a torn log could ask for a 4 GiB
// allocation on a gateway with 512 MiB of RAM.
const maxRecordBytes = 1 << 26 // 64 MiB

// Record operations.
const (
	opPut    byte = 1
	opDelete byte = 2
)

// entry is one key mutation inside a WAL record.
type entry struct {
	op        byte
	key       string
	val       []byte
	expiresAt int64
}

// record is one atomically applied group of entries stamped with the sequence
// number the whole group takes.
type record struct {
	seq     uint64
	entries []entry
}

// encodeRecord appends the framed, checksummed record to dst.
func encodeRecord(dst []byte, rec record) []byte {
	var payload []byte
	payload = binary.LittleEndian.AppendUint64(payload, rec.seq)
	payload = binary.AppendUvarint(payload, uint64(len(rec.entries)))
	for _, e := range rec.entries {
		payload = append(payload, e.op)
		payload = binary.AppendUvarint(payload, uint64(len(e.key)))
		payload = append(payload, e.key...)
		if e.op == opPut {
			payload = binary.LittleEndian.AppendUint64(payload, uint64(e.expiresAt))
			payload = binary.AppendUvarint(payload, uint64(len(e.val)))
			payload = append(payload, e.val...)
		}
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(payload)))
	dst = binary.LittleEndian.AppendUint32(dst, crc32.Checksum(payload, castagnoli))
	return append(dst, payload...)
}

// errTorn marks a record that could not be read in full or failed its checksum.
// Recovery treats it as the end of the log rather than as corruption, because
// the overwhelmingly likely cause is a power cut between two write() calls.
var errTorn = errors.New("kvstore: torn wal record")

// decodeRecord reads one record from buf, returning it and the number of bytes
// consumed. It returns errTorn for anything it cannot fully and validly read.
func decodeRecord(buf []byte) (record, int, error) {
	if len(buf) < 8 {
		return record{}, 0, errTorn
	}
	n := int(binary.LittleEndian.Uint32(buf[0:4]))
	sum := binary.LittleEndian.Uint32(buf[4:8])
	if n < 0 || n > maxRecordBytes || len(buf) < 8+n {
		return record{}, 0, errTorn
	}
	payload := buf[8 : 8+n]
	if crc32.Checksum(payload, castagnoli) != sum {
		return record{}, 0, errTorn
	}
	rec, err := decodePayload(payload)
	if err != nil {
		return record{}, 0, err
	}
	return rec, 8 + n, nil
}

func decodePayload(p []byte) (record, error) {
	if len(p) < 8 {
		return record{}, errTorn
	}
	var rec record
	rec.seq = binary.LittleEndian.Uint64(p[:8])
	p = p[8:]
	count, w := binary.Uvarint(p)
	if w <= 0 {
		return record{}, errTorn
	}
	p = p[w:]
	if count > uint64(len(p))+1 {
		return record{}, errTorn
	}
	rec.entries = make([]entry, 0, count)
	for i := uint64(0); i < count; i++ {
		if len(p) < 1 {
			return record{}, errTorn
		}
		var e entry
		e.op = p[0]
		p = p[1:]
		kl, w := binary.Uvarint(p)
		if w <= 0 || uint64(len(p[w:])) < kl {
			return record{}, errTorn
		}
		p = p[w:]
		e.key = string(p[:kl])
		p = p[kl:]
		switch e.op {
		case opPut:
			if len(p) < 8 {
				return record{}, errTorn
			}
			e.expiresAt = int64(binary.LittleEndian.Uint64(p[:8]))
			p = p[8:]
			vl, w := binary.Uvarint(p)
			if w <= 0 || uint64(len(p[w:])) < vl {
				return record{}, errTorn
			}
			p = p[w:]
			e.val = append([]byte(nil), p[:vl]...)
			p = p[vl:]
		case opDelete:
			// no body
		default:
			return record{}, errTorn
		}
		rec.entries = append(rec.entries, e)
	}
	return rec, nil
}

// walWriter appends records to one log file. Every Append issues a single
// write(2) — no user-space buffering — so a crash of the *process* can never
// lose an acknowledged write regardless of sync policy; only a crash of the
// *machine* is governed by SyncPolicy.
type walWriter struct {
	f     *os.File
	base  uint64
	bytes int64
	buf   []byte
}

func openWALWriter(path string, base uint64) (*walWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("kvstore: open wal %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("kvstore: stat wal %s: %w", path, err)
	}
	return &walWriter{f: f, base: base, bytes: st.Size()}, nil
}

func (w *walWriter) append(rec record) error {
	w.buf = encodeRecord(w.buf[:0], rec)
	n, err := w.f.Write(w.buf)
	w.bytes += int64(n)
	if err != nil {
		return fmt.Errorf("kvstore: append wal: %w", err)
	}
	return nil
}

func (w *walWriter) sync() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("kvstore: sync wal: %w", err)
	}
	return nil
}

func (w *walWriter) close() error { return w.f.Close() }

// replayWAL reads every intact record from path in order, invoking fn. It
// returns the byte offset just past the last intact record, so the caller can
// truncate away a torn tail and resume appending at a known-good boundary.
func replayWAL(path string, fn func(record) error) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("kvstore: read wal %s: %w", path, err)
	}
	var off int
	for off < len(data) {
		rec, n, err := decodeRecord(data[off:])
		if err != nil {
			// Torn or truncated tail: everything before this point is intact and
			// everything after it was never acknowledged to a caller.
			break
		}
		if err := fn(rec); err != nil {
			return int64(off), err
		}
		off += n
	}
	return int64(off), nil
}

// truncateTo cuts a log file back to a known-good boundary and fsyncs it, so a
// second crash during recovery cannot resurrect the torn bytes.
func truncateTo(path string, off int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("kvstore: open wal for truncate %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("kvstore: stat wal %s: %w", path, err)
	}
	if st.Size() == off {
		return nil
	}
	if err := f.Truncate(off); err != nil {
		return fmt.Errorf("kvstore: truncate wal %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("kvstore: sync truncated wal %s: %w", path, err)
	}
	return nil
}

// syncDir fsyncs a directory so that a rename or create is itself durable.
// Without it, a snapshot file can be fully written and fsynced and still not
// exist after a power cut, because its directory entry was never flushed.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("kvstore: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("kvstore: sync dir %s: %w", dir, err)
	}
	return nil
}
