package kvstore

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"time"
)

// ---------------------------------------------------------------------------
// Read snapshots
// ---------------------------------------------------------------------------

// Snapshot is a consistent read view of the store at one instant.
//
// Everything read through it reflects the store exactly as it was when the
// snapshot was taken, no matter how many price changes land afterwards. That
// is what makes an audit export defensible: the regulator is shown a set of
// prices that all existed simultaneously, not a smear across the ten minutes
// the export took to write.
//
// TTL is the deliberate exception — a key whose promotional window closes
// during the snapshot's lifetime stops being served, because continuing to
// serve an expired promotional price is the worse failure.
//
// A Snapshot pins version history and must be closed.
type Snapshot struct {
	store  *Store
	seq    uint64
	closed bool
}

// Snapshot returns a consistent read view at the current sequence.
func (s *Store) Snapshot() *Snapshot {
	return &Snapshot{store: s, seq: s.pin()}
}

// Sequence returns the write sequence this snapshot is pinned to. It is useful
// as a durable watermark: a projection can record "I have consumed everything
// up to sequence N".
func (sn *Snapshot) Sequence() uint64 { return sn.seq }

// Get returns the value visible in the snapshot.
func (sn *Snapshot) Get(key []byte) ([]byte, error) {
	if sn.closed {
		return nil, ErrClosed
	}
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	v, ok := sn.store.lookup(string(key), sn.seq)
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Has reports whether the key holds a live value in the snapshot.
func (sn *Snapshot) Has(key []byte) (bool, error) {
	if sn.closed {
		return false, ErrClosed
	}
	if len(key) == 0 {
		return false, ErrEmptyKey
	}
	_, ok := sn.store.lookup(string(key), sn.seq)
	return ok, nil
}

// Scan iterates every key with the given prefix as of the snapshot.
func (sn *Snapshot) Scan(prefix []byte) *Iterator {
	return sn.Range(prefix, prefixEnd(prefix))
}

// Range iterates [start, end) as of the snapshot. The iterator borrows the
// snapshot's pin, so it must not outlive the snapshot.
func (sn *Snapshot) Range(start, end []byte) *Iterator {
	if sn.closed {
		return &Iterator{err: ErrClosed}
	}
	return &Iterator{
		store:    sn.store,
		seq:      sn.seq,
		nodes:    sn.store.collect(start, end),
		now:      time.Now().UnixNano(),
		pos:      -1,
		borrowed: true,
	}
}

// Close releases the snapshot's hold on version history. It is safe to call
// more than once.
func (sn *Snapshot) Close() error {
	if sn.closed {
		return nil
	}
	sn.closed = true
	sn.store.unpin(sn.seq)
	return nil
}

// ---------------------------------------------------------------------------
// On-disk checkpoints
//
// File layout:
//
//	magic   8 bytes  "USSLPKV1"
//	seq     8 bytes  little-endian sequence the checkpoint captures
//	entries N x { uvarint keyLen, key, int64 expiresAt, uvarint valLen, val }
//	count   8 bytes  little-endian entry count
//	crc     4 bytes  CRC-32C over every preceding byte of the file
//
// The count and checksum live in a trailer rather than a header so the file can
// be written in one forward streaming pass without knowing the total up front,
// and so a truncated write is detected: a half-written checkpoint has no valid
// trailer and is simply skipped in favour of the previous one.
// ---------------------------------------------------------------------------

var snapMagic = [8]byte{'U', 'S', 'S', 'L', 'P', 'K', 'V', '1'}

// Checkpoint folds the live key set into a new snapshot file and starts a fresh
// write-ahead log, then removes the files the new checkpoint supersedes.
//
// This is the compaction that bounds recovery time. Without it a gateway that
// has been up for six months replays six months of price changes on boot; with
// it, boot cost is one checkpoint file plus at most CheckpointBytes of log.
//
// Ordering is chosen so that a crash at any point leaves a recoverable store:
// the new WAL is created and switched to *before* the checkpoint file is
// written, and the old files are deleted only *after* it lands. A crash in
// between leaves the old checkpoint plus both logs, which recovers to exactly
// the same state.
func (s *Store) Checkpoint() error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.writeMu.Lock()

	s.mu.RLock()
	seq := s.seq
	s.mu.RUnlock()

	if seq == s.snapSeq.Load() && s.wal.bytes == 0 {
		// Nothing has been written since the last checkpoint.
		s.writeMu.Unlock()
		return nil
	}

	oldWALBase := s.wal.base
	newWAL, err := openWALWriter(s.walPath(seq), seq)
	if err != nil {
		s.writeMu.Unlock()
		return err
	}
	if err := syncDir(s.dir); err != nil {
		newWAL.close()
		s.writeMu.Unlock()
		return err
	}
	var oldWAL *walWriter
	if seq != oldWALBase {
		oldWAL, s.wal = s.wal, newWAL
		s.counters.walBytes.Store(0)
	} else {
		// The active log already starts at this sequence; keep it.
		newWAL.close()
	}

	// Materialise the live set under the read lock. Writers are already blocked
	// by writeMu, so this is a stable view; readers are not blocked.
	s.mu.RLock()
	now := time.Now().UnixNano()
	type kv struct {
		key       string
		val       []byte
		expiresAt int64
	}
	live := make([]kv, 0, s.idx.length)
	for n := s.idx.first(); n != nil; n = n.tower[0] {
		v := n.visible(seq)
		if v == nil || v.tomb {
			continue
		}
		if v.expiresAt != 0 && now >= v.expiresAt {
			continue
		}
		live = append(live, kv{n.key, v.val, v.expiresAt})
	}
	s.mu.RUnlock()
	s.writeMu.Unlock()

	if oldWAL != nil {
		_ = oldWAL.sync()
		_ = oldWAL.close()
	}

	tmp := s.snapPath(seq) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("kvstore: create checkpoint: %w", err)
	}
	h := crc32.New(castagnoli)
	buf := make([]byte, 0, 1<<16)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		h.Write(buf)
		if _, err := f.Write(buf); err != nil {
			return err
		}
		buf = buf[:0]
		return nil
	}
	buf = append(buf, snapMagic[:]...)
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	for _, e := range live {
		buf = binary.AppendUvarint(buf, uint64(len(e.key)))
		buf = append(buf, e.key...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.expiresAt))
		buf = binary.AppendUvarint(buf, uint64(len(e.val)))
		buf = append(buf, e.val...)
		if len(buf) >= 1<<16 {
			if err := flush(); err != nil {
				f.Close()
				os.Remove(tmp)
				return fmt.Errorf("kvstore: write checkpoint: %w", err)
			}
		}
	}
	buf = binary.LittleEndian.AppendUint64(buf, uint64(len(live)))
	if err := flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("kvstore: write checkpoint: %w", err)
	}
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], h.Sum32())
	if _, err := f.Write(sum[:]); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("kvstore: write checkpoint trailer: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("kvstore: sync checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kvstore: close checkpoint: %w", err)
	}
	if err := os.Rename(tmp, s.snapPath(seq)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kvstore: publish checkpoint: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}

	s.snapSeq.Store(seq)
	s.snapAt.Store(time.Now().UnixNano())
	s.counters.ckpts.Add(1)
	if s.metrics != nil {
		s.metrics.checkpoints.Inc()
	}
	s.removeSuperseded(seq)
	return nil
}

// removeSuperseded deletes checkpoints and logs that the checkpoint at seq has
// made redundant. Failures are not fatal: a leftover file costs disk, whereas
// refusing to continue costs the store.
func (s *Store) removeSuperseded(seq uint64) {
	if snaps, err := s.listSeqs(snapPrefix, snapSuffix); err == nil {
		for _, v := range snaps {
			if v < seq {
				_ = os.Remove(s.snapPath(v))
			}
		}
	}
	if wals, err := s.listSeqs(walPrefix, walSuffix); err == nil {
		for _, v := range wals {
			if v < seq {
				_ = os.Remove(s.walPath(v))
			}
		}
	}
}

// loadCheckpoint replays a checkpoint file into the index. Every key is stamped
// with the checkpoint's own sequence, collapsing the history the checkpoint
// folded away.
func (s *Store) loadCheckpoint(path string, want uint64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("kvstore: read checkpoint %s: %w", path, err)
	}
	const headLen = 16
	const trailerLen = 12
	if len(data) < headLen+trailerLen {
		return fmt.Errorf("kvstore: checkpoint %s too short", path)
	}
	if string(data[:8]) != string(snapMagic[:]) {
		return fmt.Errorf("kvstore: checkpoint %s bad magic", path)
	}
	body := data[:len(data)-4]
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return fmt.Errorf("kvstore: checkpoint %s failed checksum", path)
	}
	seq := binary.LittleEndian.Uint64(data[8:16])
	if seq != want {
		return fmt.Errorf("kvstore: checkpoint %s claims sequence %d, want %d", path, seq, want)
	}
	count := binary.LittleEndian.Uint64(body[len(body)-8:])
	p := body[headLen : len(body)-8]

	entries := make([]entry, 0, count)
	for i := uint64(0); i < count; i++ {
		kl, w := binary.Uvarint(p)
		if w <= 0 || uint64(len(p[w:])) < kl {
			return fmt.Errorf("kvstore: checkpoint %s truncated key", path)
		}
		p = p[w:]
		key := string(p[:kl])
		p = p[kl:]
		if len(p) < 8 {
			return fmt.Errorf("kvstore: checkpoint %s truncated expiry", path)
		}
		exp := int64(binary.LittleEndian.Uint64(p[:8]))
		p = p[8:]
		vl, w := binary.Uvarint(p)
		if w <= 0 || uint64(len(p[w:])) < vl {
			return fmt.Errorf("kvstore: checkpoint %s truncated value", path)
		}
		p = p[w:]
		val := append([]byte(nil), p[:vl]...)
		p = p[vl:]
		entries = append(entries, entry{op: opPut, key: key, val: val, expiresAt: exp})
	}
	s.applyLocked(record{seq: seq, entries: entries})
	return nil
}
