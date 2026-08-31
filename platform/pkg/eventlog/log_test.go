package eventlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// dlqStream mirrors the canonical dead-letter name, because
// SubscribeOptions.WithDefaults points every consumer at it.
var dlqStream = canon.Stream{Name: canon.StreamDLQ.Name, Partitions: 2, RetentionHours: 24, Description: "test DLQ"}

func stream(name string, partitions int) canon.Stream {
	return canon.Stream{Name: name, Partitions: partitions, RetentionHours: 24, Description: "test stream"}
}

// openTestLog opens a log and guarantees it is closed when the test ends. An
// empty dir exercises the temp-directory mode; a non-empty one lets a test
// close and reopen the same data.
func openTestLog(t *testing.T, dir string, streams []canon.Stream, opts ...Option) *Log {
	t.Helper()
	base := []Option{
		WithLogger(obs.NopLogger()),
		WithSync(SyncNever),
		// Long enough that no test races the background sweep; tests that want
		// retention drive enforceRetention directly.
		WithRetentionInterval(time.Hour),
	}
	l, err := Open(dir, append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if len(streams) > 0 {
		if err := l.EnsureStreams(context.Background(), append(streams, dlqStream)...); err != nil {
			t.Fatalf("EnsureStreams: %v", err)
		}
	}
	return l
}

func publishN(t *testing.T, l *Log, topic string, msgs ...eventbus.Message) {
	t.Helper()
	if err := l.Publish(context.Background(), msgs...); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// sink accumulates delivered messages and signals when want of them arrive.
type sink struct {
	mu     sync.Mutex
	msgs   []eventbus.Message
	want   int
	done   chan struct{}
	closed bool
	// fail, when set, decides whether a message's handler returns an error.
	fail func(eventbus.Message) error
	// attempts counts handler invocations per key, including retries.
	attempts map[string]int
}

func newSink(want int) *sink {
	return &sink{want: want, done: make(chan struct{}), attempts: map[string]int{}}
}

func (s *sink) handle(_ context.Context, m eventbus.Message) error {
	s.mu.Lock()
	s.attempts[m.Key]++
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if err := fail(m); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	if !s.closed && s.want > 0 && len(s.msgs) >= s.want {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *sink) all() []eventbus.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]eventbus.Message(nil), s.msgs...)
}

func (s *sink) attemptsFor(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[key]
}

func (s *sink) wait(t *testing.T, d time.Duration) []eventbus.Message {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %d messages, got %d", d, s.want, s.count())
	}
	return s.all()
}

// runConsumer starts a member and returns a function that stops it and waits
// for it to leave the group.
func runConsumer(t *testing.T, c eventbus.Consumer, h eventbus.Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = c.Run(ctx, h)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, eventbus.ErrClosed) {
				t.Errorf("Run returned %v", runErr)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func subscribe(t *testing.T, l *Log, opts eventbus.SubscribeOptions) eventbus.Consumer {
	t.Helper()
	c, err := l.Subscribe(opts)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// groupSize reports how many members a group currently has, so tests can wait
// for a rebalance instead of guessing at it.
func (l *Log) groupSize(id string) int {
	l.mu.RLock()
	g := l.groups[id]
	l.mu.RUnlock()
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.members)
}

func waitForGroupSize(t *testing.T, l *Log, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.groupSize(id) == want {
			// The assignment is applied before join returns, but a member that
			// is losing partitions still has to notice; give its Run loop room
			// to stop those workers before the test produces anything.
			time.Sleep(150 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("group %q never reached %d members (has %d)", id, want, l.groupSize(id))
}

// ---------------------------------------------------------------------------
// Publish / consume
// ---------------------------------------------------------------------------

func TestPublishConsumeRoundTrip(t *testing.T) {
	t.Parallel()
	s := stream("round-trip", 4)
	l := openTestLog(t, "", []canon.Stream{s})

	const n = 200
	msgs := make([]eventbus.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, eventbus.Message{
			Topic: s.Name,
			Key:   fmt.Sprintf("store-1:sku-%d", i),
			Value: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			Headers: map[string]string{
				eventbus.HeaderEventType: canon.EvtPriceUpdated,
				eventbus.HeaderTenantID:  "acme",
			},
		})
	}
	publishN(t, l, s.Name, msgs...)

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "rt", Topics: []string{s.Name}, FromBeginning: true})
	sk := newSink(n)
	runConsumer(t, c, sk.handle)
	got := sk.wait(t, 10*time.Second)

	seen := map[string][]byte{}
	for _, m := range got {
		if m.Topic != s.Name {
			t.Fatalf("topic = %q, want %q", m.Topic, s.Name)
		}
		if m.Headers[eventbus.HeaderEventType] != canon.EvtPriceUpdated {
			t.Fatalf("event-type header lost: %v", m.Headers)
		}
		if m.Headers[eventbus.HeaderTenantID] != "acme" {
			t.Fatalf("tenant header lost: %v", m.Headers)
		}
		if m.Timestamp.IsZero() {
			t.Fatalf("broker did not stamp a timestamp on %q", m.Key)
		}
		if m.Partition < 0 || m.Partition >= s.Partitions {
			t.Fatalf("partition %d out of range", m.Partition)
		}
		seen[m.Key] = m.Value
	}
	if len(seen) != n {
		t.Fatalf("received %d distinct keys, want %d", len(seen), n)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("store-1:sku-%d", i)
		if want := fmt.Sprintf(`{"seq":%d}`, i); string(seen[key]) != want {
			t.Fatalf("value for %q = %q, want %q", key, seen[key], want)
		}
	}
}

func TestPublishHonoursSuppliedTimestamp(t *testing.T) {
	t.Parallel()
	s := stream("timestamps", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	when := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)
	publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "k", Value: []byte("v"), Timestamp: when})

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "ts", Topics: []string{s.Name}, FromBeginning: true})
	sk := newSink(1)
	runConsumer(t, c, sk.handle)
	got := sk.wait(t, 5*time.Second)
	if !got[0].Timestamp.Equal(when) {
		t.Fatalf("timestamp = %s, want %s", got[0].Timestamp, when)
	}
}

func TestPublishUnknownTopicChangesNothing(t *testing.T) {
	t.Parallel()
	s := stream("known", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	err := l.Publish(context.Background(),
		eventbus.Message{Topic: s.Name, Key: "a", Value: []byte("1")},
		eventbus.Message{Topic: "typo", Key: "b", Value: []byte("2")},
	)
	if !errors.Is(err, eventbus.ErrNoTopic) {
		t.Fatalf("err = %v, want ErrNoTopic", err)
	}
	if got := l.topicByName(s.Name).parts[0].next.Load(); got != 0 {
		t.Fatalf("partition advanced to %d despite a rejected batch", got)
	}
}

func TestClosedLogRejectsWork(t *testing.T) {
	t.Parallel()
	s := stream("closing", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Publish(context.Background(), eventbus.Message{Topic: s.Name, Value: []byte("x")}); !errors.Is(err, eventbus.ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
	if _, err := l.Subscribe(eventbus.SubscribeOptions{Group: "g", Topics: []string{s.Name}}); !errors.Is(err, eventbus.ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Partitioning
// ---------------------------------------------------------------------------

func TestPartitioning(t *testing.T) {
	t.Parallel()
	s := stream("partitioning", 8)
	l := openTestLog(t, "", []canon.Stream{s})
	top := l.topicByName(s.Name)

	t.Run("same key always lands in the same partition", func(t *testing.T) {
		for _, key := range []string{"store-1:sku-1", "store-99:sku-abc", "x", ""} {
			if key == "" {
				continue
			}
			first := top.partitionOf([]byte(key))
			for i := 0; i < 100; i++ {
				if got := top.partitionOf([]byte(key)); got != first {
					t.Fatalf("key %q moved from partition %d to %d", key, first, got)
				}
			}
		}
	})

	t.Run("hash is FNV-1a and stable across encodings", func(t *testing.T) {
		for _, key := range []string{"a", "store-1:sku-1", "\x00\xff"} {
			if fnv1a(key) != fnv1aBytes([]byte(key)) {
				t.Fatalf("string and byte hashes disagree for %q", key)
			}
		}
		// A pinned vector: the mapping is an on-disk contract, so a change here
		// must be a deliberate, breaking decision rather than a refactor.
		if got := fnv1a("store-1:sku-1"); got != 0x5fd41529 {
			t.Fatalf("fnv1a(\"store-1:sku-1\") = %#x; the key-to-partition mapping changed", got)
		}
	})

	t.Run("unkeyed records round-robin", func(t *testing.T) {
		counts := make([]int, s.Partitions)
		for i := 0; i < s.Partitions*50; i++ {
			counts[top.partitionOf(nil)]++
		}
		for p, n := range counts {
			if n != 50 {
				t.Fatalf("partition %d got %d unkeyed records, want an even 50", p, n)
			}
		}
	})

	t.Run("PartitionFor agrees with the writer", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			key := fmt.Sprintf("store-%d:sku-%d", i, i*7)
			want := top.partitionOf([]byte(key))
			got, err := l.PartitionFor(s.Name, key)
			if err != nil {
				t.Fatalf("PartitionFor: %v", err)
			}
			if got != want {
				t.Fatalf("PartitionFor(%q) = %d, want %d", key, got, want)
			}
		}
		if _, err := l.PartitionFor("nope", "k"); !errors.Is(err, eventbus.ErrNoTopic) {
			t.Fatalf("PartitionFor on unknown topic = %v, want ErrNoTopic", err)
		}
	})
}

func TestPerKeyOrderingUnderConcurrentProducers(t *testing.T) {
	t.Parallel()
	s := stream("ordering", 8)
	l := openTestLog(t, "", []canon.Stream{s})

	const producers, perProducer = 8, 150
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			key := fmt.Sprintf("store-1:sku-%d", p)
			for i := 0; i < perProducer; i++ {
				if err := l.Publish(context.Background(), eventbus.Message{
					Topic: s.Name, Key: key, Value: []byte(strconv.Itoa(i)),
				}); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "order", Topics: []string{s.Name}, FromBeginning: true})
	sk := newSink(producers * perProducer)
	runConsumer(t, c, sk.handle)
	got := sk.wait(t, 20*time.Second)

	last := map[string]int{}
	partitionOf := map[string]int{}
	for _, m := range got {
		seq, err := strconv.Atoi(string(m.Value))
		if err != nil {
			t.Fatalf("value %q: %v", m.Value, err)
		}
		if prev, ok := last[m.Key]; ok && seq != prev+1 {
			t.Fatalf("key %q delivered %d after %d: per-key order broken", m.Key, seq, prev)
		} else if !ok && seq != 0 {
			t.Fatalf("key %q started at %d, want 0", m.Key, seq)
		}
		last[m.Key] = seq
		if p, ok := partitionOf[m.Key]; ok && p != m.Partition {
			t.Fatalf("key %q appeared in partitions %d and %d", m.Key, p, m.Partition)
		}
		partitionOf[m.Key] = m.Partition
	}
	if len(last) != producers {
		t.Fatalf("saw %d keys, want %d", len(last), producers)
	}
	for key, seq := range last {
		if seq != perProducer-1 {
			t.Fatalf("key %q ended at %d, want %d", key, seq, perProducer-1)
		}
	}
}

// ---------------------------------------------------------------------------
// Durability and recovery
// ---------------------------------------------------------------------------

func TestRestartRecoversRecordsAndOffsets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := stream("restart", 2)

	const first = 120
	func() {
		l := openTestLog(t, dir, []canon.Stream{s}, WithSync(SyncAlways), WithSegmentBytes(2048))
		for i := 0; i < first; i++ {
			publishN(t, l, s.Name, eventbus.Message{
				Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte(strconv.Itoa(i)),
			})
		}
		c := subscribe(t, l, eventbus.SubscribeOptions{Group: "restarting", Topics: []string{s.Name}, FromBeginning: true})
		sk := newSink(first)
		stop := runConsumer(t, c, sk.handle)
		sk.wait(t, 10*time.Second)
		stop()
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	l := openTestLog(t, dir, []canon.Stream{s}, WithSync(SyncAlways), WithSegmentBytes(2048))

	// Every record is still there for a fresh group.
	replay := subscribe(t, l, eventbus.SubscribeOptions{Group: "replay", Topics: []string{s.Name}, FromBeginning: true})
	replaySink := newSink(first)
	stopReplay := runConsumer(t, replay, replaySink.handle)
	replayed := replaySink.wait(t, 10*time.Second)
	stopReplay()
	if len(replayed) != first {
		t.Fatalf("replayed %d records, want %d", len(replayed), first)
	}

	// Appends continue at the right offset rather than overwriting history.
	total := int64(0)
	for _, p := range l.topicByName(s.Name).parts {
		total += p.next.Load()
	}
	if total != first {
		t.Fatalf("next offsets sum to %d, want %d", total, first)
	}

	// The old group resumes from its committed offsets: only new records.
	const more = 30
	for i := first; i < first+more; i++ {
		publishN(t, l, s.Name, eventbus.Message{
			Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte(strconv.Itoa(i)),
		})
	}
	resumed := subscribe(t, l, eventbus.SubscribeOptions{Group: "restarting", Topics: []string{s.Name}, FromBeginning: true})
	resumedSink := newSink(more)
	runConsumer(t, resumed, resumedSink.handle)
	got := resumedSink.wait(t, 10*time.Second)
	if len(got) != more {
		t.Fatalf("resumed group saw %d records, want exactly the %d new ones", len(got), more)
	}
	for _, m := range got {
		seq, _ := strconv.Atoi(string(m.Value))
		if seq < first {
			t.Fatalf("resumed group re-read record %d, which it had already committed", seq)
		}
	}
	// Give the resumed group a moment to prove it does not deliver more.
	time.Sleep(200 * time.Millisecond)
	if n := resumedSink.count(); n != more {
		t.Fatalf("resumed group saw %d records after settling, want %d", n, more)
	}
}

func TestCorruptSegmentTailIsTruncatedOnOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"flipped byte in the last record", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if _, err := f.ReadAt(b[:], st.Size()-1); err != nil {
				t.Fatal(err)
			}
			b[0] ^= 0xff
			if _, err := f.WriteAt(b[:], st.Size()-1); err != nil {
				t.Fatal(err)
			}
		}},
		{"torn write at the tail", func(t *testing.T, path string) {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, st.Size()-3); err != nil {
				t.Fatal(err)
			}
		}},
		{"garbage appended after the last record", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write([]byte{0x00, 0x00, 0x10, 0x00, 0xde, 0xad}); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := stream("corrupt", 1)
			const n = 10

			l := openTestLog(t, dir, []canon.Stream{s}, WithSync(SyncAlways))
			for i := 0; i < n; i++ {
				publishN(t, l, s.Name, eventbus.Message{
					Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte(fmt.Sprintf("value-%d", i)),
				})
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			seg := segmentPath(partitionDir(filepath.Join(dir, s.Name), 0), 0)
			tc.damage(t, seg)

			reopened := openTestLog(t, dir, []canon.Stream{s}, WithSync(SyncAlways))
			p := reopened.topicByName(s.Name).parts[0]

			wantSurvivors := n - 1
			if tc.name == "garbage appended after the last record" {
				wantSurvivors = n // the garbage is past the last good record
			}
			if got := p.next.Load(); got != int64(wantSurvivors) {
				t.Fatalf("next offset = %d, want %d", got, wantSurvivors)
			}

			c := subscribe(t, reopened, eventbus.SubscribeOptions{Group: "after-corruption", Topics: []string{s.Name}, FromBeginning: true})
			sk := newSink(wantSurvivors)
			runConsumer(t, c, sk.handle)
			got := sk.wait(t, 10*time.Second)
			for i, m := range got {
				if want := fmt.Sprintf("value-%d", i); string(m.Value) != want {
					t.Fatalf("record %d = %q, want %q", i, m.Value, want)
				}
			}

			// The log is writable again and continues at the recovered offset.
			publishN(t, reopened, s.Name, eventbus.Message{Topic: s.Name, Key: "after", Value: []byte("after")})
			if got := p.next.Load(); got != int64(wantSurvivors)+1 {
				t.Fatalf("next offset after append = %d, want %d", got, wantSurvivors+1)
			}
		})
	}
}

func TestIndexIsRebuiltWhenMissingOrStale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		damage  func(t *testing.T, path string)
		segment int64
	}{
		{"missing", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"garbage", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not an index"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"checksum broken", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-1] ^= 0xff
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := stream("index", 1)
			const n = 400

			l := openTestLog(t, dir, []canon.Stream{s}, WithSegmentBytes(1<<20), WithIndexInterval(128))
			for i := 0; i < n; i++ {
				publishN(t, l, s.Name, eventbus.Message{
					Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte(strconv.Itoa(i)),
				})
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			pdir := partitionDir(filepath.Join(dir, s.Name), 0)
			tc.damage(t, indexPath(pdir, tc.segment))

			reopened := openTestLog(t, dir, []canon.Stream{s}, WithSegmentBytes(1<<20), WithIndexInterval(128))
			p := reopened.topicByName(s.Name).parts[0]
			if got := p.next.Load(); got != n {
				t.Fatalf("next offset = %d, want %d after rebuilding the index", got, n)
			}
			if len(p.segs[0].entries) < 2 {
				t.Fatalf("index was not rebuilt: %d entries", len(p.segs[0].entries))
			}
			// A seek into the middle of the segment must land on the right
			// record, which is the only thing the index is for.
			recs, next, err := p.read(300, 5)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(recs) != 5 || recs[0].offset != 300 || next != 305 {
				t.Fatalf("read from 300 gave %d records starting at %d (next %d)", len(recs), recs[0].offset, next)
			}
			if string(recs[0].value) != "300" {
				t.Fatalf("record at offset 300 = %q, want \"300\"", recs[0].value)
			}
		})
	}
}

func TestSyncPolicies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy SyncPolicy
		want   string
	}{
		{"always", SyncAlways, "always"},
		{"never", SyncNever, "never"},
		{"interval", SyncInterval(20 * time.Millisecond), "interval(20ms)"},
		{"non-positive interval degrades to always", SyncInterval(0), "always"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.policy.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
			dir := t.TempDir()
			s := stream("sync", 1)
			l := openTestLog(t, dir, []canon.Stream{s}, WithSync(tc.policy))
			for i := 0; i < 20; i++ {
				publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "k", Value: []byte(strconv.Itoa(i))})
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			// Whatever the policy, an orderly Close is durable.
			reopened := openTestLog(t, dir, []canon.Stream{s}, WithSync(tc.policy))
			if got := reopened.topicByName(s.Name).parts[0].next.Load(); got != 20 {
				t.Fatalf("recovered %d records, want 20", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stream management
// ---------------------------------------------------------------------------

func TestEnsureStreams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := openTestLog(t, dir, nil)
	ctx := context.Background()

	s := stream("catalogue", 4)
	if err := l.EnsureStreams(ctx, s, dlqStream); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	if err := l.EnsureStreams(ctx, s, dlqStream); err != nil {
		t.Fatalf("EnsureStreams is not idempotent: %v", err)
	}

	changed := s
	changed.RetentionHours = 999
	if err := l.EnsureStreams(ctx, changed); err != nil {
		t.Fatalf("changing retention: %v", err)
	}
	if got := l.topicByName(s.Name).stream.RetentionHours; got != 999 {
		t.Fatalf("retention = %d, want 999", got)
	}

	repartitioned := s
	repartitioned.Partitions = 8
	if err := l.EnsureStreams(ctx, repartitioned); !errors.Is(err, ErrPartitionsChanged) {
		t.Fatalf("repartitioning = %v, want ErrPartitionsChanged", err)
	}

	for _, bad := range []string{"", ".", "..", offsetsDirName, "a/b", `a\b`} {
		if err := l.EnsureStreams(ctx, canon.Stream{Name: bad, Partitions: 1}); !errors.Is(err, ErrInvalidTopic) {
			t.Fatalf("EnsureStreams(%q) = %v, want ErrInvalidTopic", bad, err)
		}
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openTestLog(t, dir, nil)
	var names []string
	for _, got := range reopened.Streams() {
		names = append(names, got.Name)
	}
	sort.Strings(names)
	want := []string{canon.StreamDLQ.Name, "catalogue"}
	sort.Strings(want)
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("recovered streams %v, want %v", names, want)
	}
	if got := reopened.topicByName("catalogue"); got == nil || got.stream.RetentionHours != 999 || got.stream.Partitions != 4 {
		t.Fatalf("stream definition not recovered: %+v", got)
	}
}

// TestEnsureCanonicalStreams checks the real catalogue provisions cleanly,
// including the 2,048-partition telemetry stream, and that doing so costs no
// file handles until something is written.
func TestEnsureCanonicalStreams(t *testing.T) {
	t.Parallel()
	l := openTestLog(t, "", nil)
	if err := l.EnsureStreams(context.Background(), canon.AllStreams()...); err != nil {
		t.Fatalf("EnsureStreams(AllStreams): %v", err)
	}
	for _, s := range canon.AllStreams() {
		got := l.topicByName(s.Name)
		if got == nil {
			t.Fatalf("stream %q missing", s.Name)
		}
		if len(got.parts) != s.Partitions {
			t.Fatalf("stream %q has %d partitions, want %d", s.Name, len(got.parts), s.Partitions)
		}
		for _, p := range got.parts {
			if p.segmentCount() != 0 {
				t.Fatalf("stream %q partition %d allocated a segment before any write", s.Name, p.id)
			}
		}
	}
	publishN(t, l, canon.StreamPriceUpdates.Name, eventbus.Message{
		Topic: canon.StreamPriceUpdates.Name, Key: "store-1:sku-1", Value: []byte("{}"),
	})
	p, err := l.PartitionFor(canon.StreamPriceUpdates.Name, "store-1:sku-1")
	if err != nil {
		t.Fatalf("PartitionFor: %v", err)
	}
	if got := l.topicByName(canon.StreamPriceUpdates.Name).parts[p].segmentCount(); got != 1 {
		t.Fatalf("written partition has %d segments, want 1", got)
	}
}

// TestCloseStopsRunningConsumers checks the shutdown path a service actually
// takes: Close is called while consumers are mid-poll, and every Run must
// return rather than block the process from exiting.
func TestCloseStopsRunningConsumers(t *testing.T) {
	t.Parallel()
	s := stream("closing-consumers", 4)
	l := openTestLog(t, "", []canon.Stream{s})
	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "shutdown", Topics: []string{s.Name}, FromBeginning: true})

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			errs <- c.Run(context.Background(), func(context.Context, eventbus.Message) error { return nil })
		}()
	}
	waitForGroupSize(t, l, "shutdown", 2)
	for i := 0; i < 20; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte("v")})
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, eventbus.ErrClosed) {
				t.Fatalf("Run returned %v, want ErrClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after the log was closed")
		}
	}
}
