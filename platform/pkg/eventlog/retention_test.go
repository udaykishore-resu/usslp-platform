package eventlog

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
)

// fixedRecordBytes is the framed size of the records these tests publish. It is
// computed rather than hard-coded so the tests keep working if the frame gains
// a field, and it lets a segment size be chosen that holds an exact number of
// records — which is what makes retention and compaction assertions exact
// instead of approximate.
func fixedRecordBytes(t *testing.T, keyLen, valueLen int) int64 {
	t.Helper()
	rec := record{
		timestamp: time.Now().UnixNano(),
		key:       make([]byte, keyLen),
		value:     make([]byte, valueLen),
	}
	return int64(len(encodeRecord(nil, rec)))
}

func padded(prefix string, size int) []byte {
	v := make([]byte, size)
	copy(v, prefix)
	for i := len(prefix); i < size; i++ {
		v[i] = '.'
	}
	return v
}

func TestRetentionDeletesExpiredSegments(t *testing.T) {
	t.Parallel()
	const valueLen = 200
	recBytes := fixedRecordBytes(t, 3, valueLen)
	perSegment := int64(4)

	s := canon.Stream{Name: "expiring", Partitions: 1, RetentionHours: 1, Description: "short retention"}
	l := openTestLog(t, "", []canon.Stream{s}, WithSegmentBytes(perSegment*recBytes))
	p := l.topicByName(s.Name).parts[0]

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	const n = 40
	for i := 0; i < n; i++ {
		publishN(t, l, s.Name, eventbus.Message{
			Topic: s.Name, Key: "k-1", Value: padded(strconv.Itoa(i), valueLen), Timestamp: old,
		})
	}
	if got := p.segmentCount(); got != n/int(perSegment) {
		t.Fatalf("wrote %d segments, want %d", got, n/int(perSegment))
	}

	l.enforceRetention(now)

	if got := p.segmentCount(); got != 1 {
		t.Fatalf("after retention there are %d segments, want 1 (the fresh active one)", got)
	}
	if got := p.base.Load(); got != n {
		t.Fatalf("earliest retained offset = %d, want %d", got, n)
	}
	if got := p.next.Load(); got != n {
		t.Fatalf("next offset = %d, want %d: retention must not renumber the log", got, n)
	}
	recs, next, err := p.read(0, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// A consumer that fell behind the retained window is skipped forward to the
	// earliest surviving offset rather than being stuck re-requesting data that
	// no longer exists.
	if len(recs) != 0 || next != n {
		t.Fatalf("read after full expiry returned %d records (next %d), want 0 records at %d", len(recs), next, n)
	}

	// Fresh traffic survives the next sweep.
	publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "k-1", Value: padded("fresh", valueLen)})
	l.enforceRetention(time.Now())
	if got := p.segmentCount(); got != 1 {
		t.Fatalf("live segment count = %d, want 1", got)
	}
	if got := p.next.Load(); got != n+1 {
		t.Fatalf("next offset = %d, want %d", got, n+1)
	}
}

func TestRetentionKeepsUnexpiredAndUnretainedData(t *testing.T) {
	t.Parallel()
	const valueLen = 200
	recBytes := fixedRecordBytes(t, 3, valueLen)

	t.Run("mixed ages", func(t *testing.T) {
		t.Parallel()
		s := canon.Stream{Name: "mixed-ages", Partitions: 1, RetentionHours: 1}
		l := openTestLog(t, "", []canon.Stream{s}, WithSegmentBytes(4*recBytes))
		p := l.topicByName(s.Name).parts[0]

		now := time.Now()
		for i := 0; i < 16; i++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name, Key: "k-1", Value: padded("old", valueLen), Timestamp: now.Add(-3 * time.Hour),
			})
		}
		for i := 0; i < 16; i++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name, Key: "k-1", Value: padded("new", valueLen), Timestamp: now,
			})
		}
		l.enforceRetention(now)

		if got := p.base.Load(); got != 16 {
			t.Fatalf("earliest retained offset = %d, want 16", got)
		}
		recs, _, err := p.read(16, 100)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(recs) != 16 {
			t.Fatalf("kept %d live records, want 16", len(recs))
		}
		for _, r := range recs {
			if string(r.value[:3]) != "new" {
				t.Fatalf("retained an expired record: %q", r.value[:3])
			}
		}
	})

	t.Run("retention hours of zero never expires", func(t *testing.T) {
		t.Parallel()
		s := canon.Stream{Name: "forever", Partitions: 1, RetentionHours: 0}
		l := openTestLog(t, "", []canon.Stream{s}, WithSegmentBytes(4*recBytes))
		p := l.topicByName(s.Name).parts[0]
		ancient := time.Now().Add(-5000 * time.Hour)
		for i := 0; i < 20; i++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name, Key: "k-1", Value: padded("keep", valueLen), Timestamp: ancient,
			})
		}
		before := p.segmentCount()
		l.enforceRetention(time.Now())
		if got := p.segmentCount(); got != before {
			t.Fatalf("segments went from %d to %d on a stream with no retention", before, got)
		}
		if got := p.base.Load(); got != 0 {
			t.Fatalf("earliest offset moved to %d on a stream with no retention", got)
		}
	})
}

func TestCompactionKeepsLatestValuePerKey(t *testing.T) {
	t.Parallel()
	const (
		valueLen = 200
		keys     = 5
		rounds   = 10
	)
	recBytes := fixedRecordBytes(t, 2, valueLen)

	s := canon.Stream{Name: "label-state-test", Partitions: 1, RetentionHours: 0, Compacted: true,
		Description: "compacted latest state per label"}
	// One round of keys per segment, so the segment holding the newest value of
	// every key is the active one and every closed segment is fully superseded.
	l := openTestLog(t, "", []canon.Stream{s}, WithSegmentBytes(keys*recBytes))
	p := l.topicByName(s.Name).parts[0]

	for r := 0; r < rounds; r++ {
		for k := 0; k < keys; k++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name,
				Key:   fmt.Sprintf("k%d", k),
				Value: padded(fmt.Sprintf("r%d-k%d", r, k), valueLen),
			})
		}
	}
	if got := p.segmentCount(); got != rounds {
		t.Fatalf("wrote %d segments, want %d", got, rounds)
	}
	sizeBefore := segmentBytesOnDisk(t, p)

	l.enforceRetention(time.Now())

	// Offsets are preserved: the survivors keep the offsets they were written
	// with, so a consumer's committed offsets stay meaningful across compaction.
	recs, next, err := p.read(0, 1000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != keys {
		t.Fatalf("compaction left %d records, want %d (one per key)", len(recs), keys)
	}
	if next != rounds*keys {
		t.Fatalf("next offset after compaction = %d, want %d", next, rounds*keys)
	}
	for i, r := range recs {
		wantKey := fmt.Sprintf("k%d", i)
		wantVal := fmt.Sprintf("r%d-k%d", rounds-1, i)
		if string(r.key) != wantKey {
			t.Fatalf("record %d key = %q, want %q", i, r.key, wantKey)
		}
		if got := string(r.value[:len(wantVal)]); got != wantVal {
			t.Fatalf("key %s survived with %q, want the latest %q", wantKey, got, wantVal)
		}
		if wantOffset := int64((rounds-1)*keys + i); r.offset != wantOffset {
			t.Fatalf("key %s survived at offset %d, want %d", wantKey, r.offset, wantOffset)
		}
	}
	if after := segmentBytesOnDisk(t, p); after >= sizeBefore {
		t.Fatalf("compaction reclaimed nothing: %d bytes before, %d after", sizeBefore, after)
	}

	// A second sweep must be a no-op; otherwise the background ticker would
	// rewrite the whole log every minute forever.
	if n := p.compact(); n != 0 {
		t.Fatalf("second compaction rewrote %d segments, want 0", n)
	}

	// End to end: a consumer replaying the stream sees exactly the surviving
	// records and is not stalled by the gaps left behind.
	c := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "state-rebuild", Topics: []string{s.Name}, FromBeginning: true,
	})
	sk := newSink(keys)
	runConsumer(t, c, sk.handle)
	got := sk.wait(t, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	if n := sk.count(); n != keys {
		t.Fatalf("consumer saw %d records after settling, want %d", n, keys)
	}
	for _, m := range got {
		want := fmt.Sprintf("r%d-%s", rounds-1, m.Key)
		if string(m.Value[:len(want)]) != want {
			t.Fatalf("consumer got %q for key %s, want %q", m.Value[:len(want)], m.Key, want)
		}
	}
}

// TestCompactionSurvivesRestart checks that a compacted partition reopens with
// the gap records accounted for, so offsets after a restart still line up.
func TestCompactionSurvivesRestart(t *testing.T) {
	t.Parallel()
	const valueLen = 200
	recBytes := fixedRecordBytes(t, 2, valueLen)
	dir := t.TempDir()
	s := canon.Stream{Name: "compact-restart", Partitions: 1, Compacted: true}

	l := openTestLog(t, dir, []canon.Stream{s}, WithSegmentBytes(3*recBytes))
	for r := 0; r < 6; r++ {
		for k := 0; k < 3; k++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name, Key: fmt.Sprintf("k%d", k), Value: padded(fmt.Sprintf("r%d", r), valueLen),
			})
		}
	}
	l.enforceRetention(time.Now())
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestLog(t, dir, []canon.Stream{s}, WithSegmentBytes(3*recBytes))
	p := reopened.topicByName(s.Name).parts[0]
	if got := p.next.Load(); got != 18 {
		t.Fatalf("next offset after restart = %d, want 18", got)
	}
	recs, _, err := p.read(0, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("read %d records after restart, want 3", len(recs))
	}
	for i, r := range recs {
		if wantOffset := int64(15 + i); r.offset != wantOffset {
			t.Fatalf("record %d has offset %d, want %d", i, r.offset, wantOffset)
		}
		if string(r.value[:2]) != "r5" {
			t.Fatalf("record %d survived with %q, want the latest round", i, r.value[:2])
		}
	}
	publishN(t, reopened, s.Name, eventbus.Message{Topic: s.Name, Key: "k0", Value: padded("r6", valueLen)})
	if got := p.next.Load(); got != 19 {
		t.Fatalf("next offset after appending = %d, want 19", got)
	}
}

func segmentBytesOnDisk(t *testing.T, p *partition) int64 {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var total int64
	for _, seg := range p.segs {
		total += seg.size
	}
	return total
}
