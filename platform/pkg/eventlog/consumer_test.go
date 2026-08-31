package eventlog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// keysForPartitions finds one key per partition, so a test can put a known
// number of records in each partition instead of hoping the hash spreads them.
func keysForPartitions(t *testing.T, l *Log, topicName string, n int) []string {
	t.Helper()
	keys := make([]string, n)
	found := 0
	for i := 0; found < n; i++ {
		if i > 1_000_000 {
			t.Fatalf("could not find keys covering %d partitions", n)
		}
		k := fmt.Sprintf("store-1:sku-%d", i)
		p, err := l.PartitionFor(topicName, k)
		if err != nil {
			t.Fatalf("PartitionFor: %v", err)
		}
		if keys[p] == "" {
			keys[p] = k
			found++
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

func TestRebalancePlan(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"a": 6, "b": 3}

	newMember := func(topics ...string) *member {
		return &member{topics: topics, owned: map[tp]bool{}, notify: make(chan struct{}, 1)}
	}

	cases := []struct {
		name    string
		members []*member
		want    []map[tp]bool
	}{
		{
			name:    "single member owns everything",
			members: []*member{newMember("a", "b")},
			want: []map[tp]bool{{
				{"a", 0}: true, {"a", 1}: true, {"a", 2}: true, {"a", 3}: true, {"a", 4}: true, {"a", 5}: true,
				{"b", 0}: true, {"b", 1}: true, {"b", 2}: true,
			}},
		},
		{
			name:    "two members split each topic",
			members: []*member{newMember("a"), newMember("a")},
			want: []map[tp]bool{
				{{"a", 0}: true, {"a", 2}: true, {"a", 4}: true},
				{{"a", 1}: true, {"a", 3}: true, {"a", 5}: true},
			},
		},
		{
			name:    "members only get topics they subscribe to",
			members: []*member{newMember("a"), newMember("b")},
			want: []map[tp]bool{
				{{"a", 0}: true, {"a", 1}: true, {"a", 2}: true, {"a", 3}: true, {"a", 4}: true, {"a", 5}: true},
				{{"b", 0}: true, {"b", 1}: true, {"b", 2}: true},
			},
		},
		{
			name:    "three members, uneven partitions",
			members: []*member{newMember("b"), newMember("b"), newMember("b")},
			want: []map[tp]bool{
				{{"b", 0}: true}, {{"b", 1}: true}, {{"b", 2}: true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &group{id: "g"}
			for _, m := range tc.members {
				g.joinLocked(m, counts)
			}
			for i, m := range tc.members {
				got := m.assignment()
				if len(got) != len(tc.want[i]) {
					t.Fatalf("member %d owns %v, want %v", i, got, tc.want[i])
				}
				for k := range tc.want[i] {
					if !got[k] {
						t.Fatalf("member %d is missing %s", i, k)
					}
				}
			}
			// Stability: re-planning with the same membership must not move a
			// single partition, because every move costs a re-read.
			before := make([]map[tp]bool, len(tc.members))
			for i, m := range tc.members {
				before[i] = m.assignment()
			}
			for i := 0; i < 5; i++ {
				g.rebalance(counts)
			}
			for i, m := range tc.members {
				after := m.assignment()
				if len(after) != len(before[i]) {
					t.Fatalf("member %d assignment changed size on replan", i)
				}
				for k := range before[i] {
					if !after[k] {
						t.Fatalf("member %d lost %s on an identical replan", i, k)
					}
				}
			}
		})
	}
}

func TestConsumerGroupSplitsAndRebalancesOnExit(t *testing.T) {
	t.Parallel()
	s := stream("group-split", 4)
	l := openTestLog(t, "", []canon.Stream{s})
	keys := keysForPartitions(t, l, s.Name, s.Partitions)

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "split", Topics: []string{s.Name}, FromBeginning: true})
	sinkA, sinkB := newSink(0), newSink(0)
	runConsumer(t, c, sinkA.handle)
	waitForGroupSize(t, l, "split", 1)
	stopB := runConsumer(t, c, sinkB.handle)
	waitForGroupSize(t, l, "split", 2)

	const perPartition = 10
	publish := func(round int) {
		for p, key := range keys {
			for i := 0; i < perPartition; i++ {
				publishN(t, l, s.Name, eventbus.Message{
					Topic: s.Name, Key: key, Value: []byte(fmt.Sprintf("%d-%d-%d", round, p, i)),
				})
			}
		}
	}
	publish(0)

	total := s.Partitions * perPartition
	waitFor(t, 10*time.Second, "the group to consume the first round", func() bool {
		return sinkA.count()+sinkB.count() >= total
	})

	// Four partitions over two members: each member owns two, so each sees
	// exactly half the traffic.
	if a, b := sinkA.count(), sinkB.count(); a != total/2 || b != total/2 {
		t.Fatalf("split was %d/%d, want %d/%d", a, b, total/2, total/2)
	}
	assertNoDuplicates(t, append(sinkA.all(), sinkB.all()...))

	// A member leaving must hand its partitions to the survivor.
	stopB()
	waitForGroupSize(t, l, "split", 1)
	bAtHandover := sinkB.count()

	publish(1)
	waitFor(t, 10*time.Second, "the surviving member to pick up the revoked partitions", func() bool {
		return sinkA.count() >= total/2+total
	})
	if got := sinkB.count(); got != bAtHandover {
		t.Fatalf("departed member received %d more records after leaving", got-bAtHandover)
	}
	assertNoDuplicates(t, append(sinkA.all(), sinkB.all()...))
}

func assertNoDuplicates(t *testing.T, msgs []eventbus.Message) {
	t.Helper()
	type coord struct {
		topic     string
		partition int
		offset    int64
	}
	seen := make(map[coord]int, len(msgs))
	for _, m := range msgs {
		c := coord{m.Topic, m.Partition, m.Offset}
		seen[c]++
		if seen[c] > 1 {
			t.Fatalf("record %s/%d@%d delivered %d times within one group", c.topic, c.partition, c.offset, seen[c])
		}
	}
}

func TestSeparateGroupsEachSeeEveryRecord(t *testing.T) {
	t.Parallel()
	s := stream("fanout", 2)
	l := openTestLog(t, "", []canon.Stream{s})
	const n = 50
	for i := 0; i < n; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: fmt.Sprintf("k-%d", i), Value: []byte(strconv.Itoa(i))})
	}
	for _, group := range []string{"projector", "auditor"} {
		c := subscribe(t, l, eventbus.SubscribeOptions{Group: group, Topics: []string{s.Name}, FromBeginning: true})
		sk := newSink(n)
		runConsumer(t, c, sk.handle)
		if got := len(sk.wait(t, 10*time.Second)); got != n {
			t.Fatalf("group %q saw %d records, want %d", group, got, n)
		}
	}
}

func TestNewGroupStartsAtTailUnlessFromBeginning(t *testing.T) {
	t.Parallel()
	s := stream("tail", 2)
	l := openTestLog(t, "", []canon.Stream{s})
	for i := 0; i < 20; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: fmt.Sprintf("old-%d", i), Value: []byte("old")})
	}

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "live", Topics: []string{s.Name}})
	sk := newSink(5)
	runConsumer(t, c, sk.handle)
	// Let the member pin the tail before anything new is produced.
	waitForGroupSize(t, l, "live", 1)

	for i := 0; i < 5; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: fmt.Sprintf("new-%d", i), Value: []byte("new")})
	}
	got := sk.wait(t, 10*time.Second)
	for _, m := range got {
		if string(m.Value) != "new" {
			t.Fatalf("live group replayed history: %q", m.Key)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := sk.count(); n != 5 {
		t.Fatalf("live group saw %d records, want the 5 produced after it joined", n)
	}
}

// ---------------------------------------------------------------------------
// Retry and dead-lettering
// ---------------------------------------------------------------------------

func TestRetryThenDeadLetter(t *testing.T) {
	t.Parallel()
	s := stream("poison", 1)
	l := openTestLog(t, "", []canon.Stream{s})

	publishN(t, l,
		s.Name,
		eventbus.Message{Topic: s.Name, Key: "poison", Value: []byte("bad"),
			Headers: map[string]string{eventbus.HeaderTenantID: "acme"}},
		eventbus.Message{Topic: s.Name, Key: "healthy", Value: []byte("good")},
	)

	sk := newSink(1) // only "healthy" ever reaches the sink
	var retryCounts []string
	var mu sync.Mutex
	sk.fail = func(m eventbus.Message) error {
		if m.Key != "poison" {
			return nil
		}
		mu.Lock()
		retryCounts = append(retryCounts, m.Headers[eventbus.HeaderRetryCount])
		mu.Unlock()
		return errors.New("render pipeline unavailable")
	}

	c := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "poisoned", Topics: []string{s.Name}, FromBeginning: true,
		MaxRetries: 2, RetryBackoff: time.Millisecond,
	})
	runConsumer(t, c, sk.handle)
	got := sk.wait(t, 10*time.Second)

	if got[0].Key != "healthy" {
		t.Fatalf("delivered %q, want the record after the poison one", got[0].Key)
	}
	if n := sk.attemptsFor("poison"); n != 3 {
		t.Fatalf("poison record was attempted %d times, want 1 try + 2 retries", n)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, rc := range retryCounts {
		if rc != strconv.Itoa(i) {
			t.Fatalf("attempt %d carried retry-count header %q, want %q", i, rc, strconv.Itoa(i))
		}
	}
}

func TestDeadLetteredRecordCarriesProvenance(t *testing.T) {
	t.Parallel()
	s := stream("dlq-origin", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	publishN(t, l, s.Name, eventbus.Message{
		Topic: s.Name, Key: "store-9:sku-9", Value: []byte(`{"cents":100}`),
		Headers: map[string]string{eventbus.HeaderEventType: canon.EvtPriceUpdated},
	})

	c := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "failing", Topics: []string{s.Name}, FromBeginning: true,
		MaxRetries: 1, RetryBackoff: time.Millisecond,
	})
	failing := newSink(0)
	failing.fail = func(eventbus.Message) error { return errors.New("schema registry down") }
	runConsumer(t, c, failing.handle)

	dlq := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "dlq-reader", Topics: []string{dlqStream.Name}, FromBeginning: true,
	})
	dlqSink := newSink(1)
	runConsumer(t, dlq, dlqSink.handle)
	got := dlqSink.wait(t, 10*time.Second)[0]

	if got.Key != "store-9:sku-9" {
		t.Fatalf("dead-lettered key = %q", got.Key)
	}
	if string(got.Value) != `{"cents":100}` {
		t.Fatalf("dead-lettered value = %q", got.Value)
	}
	if got.Headers[eventbus.HeaderDLQOrigin] != s.Name {
		t.Fatalf("origin header = %q, want %q", got.Headers[eventbus.HeaderDLQOrigin], s.Name)
	}
	if got.Headers[eventbus.HeaderDLQReason] != "schema registry down" {
		t.Fatalf("reason header = %q", got.Headers[eventbus.HeaderDLQReason])
	}
	if got.Headers[eventbus.HeaderEventType] != canon.EvtPriceUpdated {
		t.Fatalf("original headers were not preserved: %v", got.Headers)
	}
	// HeaderRetryCount describes the delivery in progress, not the record's
	// history, so the DLQ reader sees its own attempt number.
	if got.Headers[eventbus.HeaderRetryCount] != "0" {
		t.Fatalf("retry-count header = %q, want the DLQ reader's own attempt count", got.Headers[eventbus.HeaderRetryCount])
	}
}

// TestPermanentErrorSkipsRetries proves retry.Stop short-circuits the backoff:
// a record that will fail identically forever should reach the dead-letter
// stream immediately rather than after five sleeps.
func TestPermanentErrorSkipsRetries(t *testing.T) {
	t.Parallel()
	s := stream("permanent", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "bad", Value: []byte("x")})

	sk := newSink(0)
	// The handler marks the failure permanent, so only one attempt should
	// happen even though the policy allows five retries with a long backoff.
	sk.fail = func(eventbus.Message) error { return retry.Stop(canon.ErrEnvelopeInvalid) }
	c := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "perm", Topics: []string{s.Name}, FromBeginning: true,
		MaxRetries: 5, RetryBackoff: 250 * time.Millisecond,
	})
	runConsumer(t, c, sk.handle)

	dlq := subscribe(t, l, eventbus.SubscribeOptions{Group: "perm-dlq", Topics: []string{dlqStream.Name}, FromBeginning: true})
	dlqSink := newSink(1)
	runConsumer(t, dlq, dlqSink.handle)
	dlqSink.wait(t, 5*time.Second)
	if n := sk.attemptsFor("bad"); n != 1 {
		t.Fatalf("permanent failure was attempted %d times, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Concurrency, lag and races
// ---------------------------------------------------------------------------

func TestConcurrencyRunsHandlersInParallel(t *testing.T) {
	t.Parallel()
	s := stream("parallel", 1)
	l := openTestLog(t, "", []canon.Stream{s})
	const n = 40
	for i := 0; i < n; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "same-key", Value: []byte(strconv.Itoa(i))})
	}

	var inFlight, peak atomic.Int64
	sk := newSink(n)
	handler := func(ctx context.Context, m eventbus.Message) error {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return sk.handle(ctx, m)
	}

	c := subscribe(t, l, eventbus.SubscribeOptions{
		Group: "parallel", Topics: []string{s.Name}, FromBeginning: true, Concurrency: 4,
	})
	runConsumer(t, c, handler)
	got := sk.wait(t, 15*time.Second)
	if len(got) != n {
		t.Fatalf("delivered %d records, want %d", len(got), n)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak in-flight handlers was %d; Concurrency=4 did not parallelise", peak.Load())
	}
	if peak.Load() > 4 {
		t.Fatalf("peak in-flight handlers was %d, above the configured Concurrency of 4", peak.Load())
	}
	assertNoDuplicates(t, got)
}

func TestLagAccounting(t *testing.T) {
	t.Parallel()
	s := stream("lag", 2)
	l := openTestLog(t, "", []canon.Stream{s})
	keys := keysForPartitions(t, l, s.Name, s.Partitions)

	ctx := context.Background()
	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "lagging", Topics: []string{s.Name}, FromBeginning: true})

	lag, err := c.Lag(ctx)
	if err != nil {
		t.Fatalf("Lag: %v", err)
	}
	if total := sumLag(lag); total != 0 {
		t.Fatalf("lag on an empty log = %d, want 0", total)
	}

	const perPartition = 12
	for p, key := range keys {
		for i := 0; i < perPartition; i++ {
			publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: key, Value: []byte(fmt.Sprintf("%d-%d", p, i))})
		}
	}

	lag, err = c.Lag(ctx)
	if err != nil {
		t.Fatalf("Lag: %v", err)
	}
	for p := 0; p < s.Partitions; p++ {
		if lag[p] != perPartition {
			t.Fatalf("partition %d lag = %d, want %d", p, lag[p], perPartition)
		}
	}

	sk := newSink(perPartition * s.Partitions)
	runConsumer(t, c, sk.handle)
	sk.wait(t, 10*time.Second)
	waitFor(t, 5*time.Second, "lag to drain", func() bool {
		got, err := c.Lag(ctx)
		return err == nil && sumLag(got) == 0
	})

	// A group parked at the tail has no lag even though the log is not empty.
	tail := subscribe(t, l, eventbus.SubscribeOptions{Group: "tail-group", Topics: []string{s.Name}})
	tailLag, err := tail.Lag(ctx)
	if err != nil {
		t.Fatalf("Lag: %v", err)
	}
	if total := sumLag(tailLag); total != 0 {
		t.Fatalf("tail group lag = %d, want 0", total)
	}
}

func sumLag(m map[int]int64) int64 {
	var total int64
	for _, v := range m {
		total += v
	}
	return total
}

// TestConcurrentPublishAndConsume is the -race workhorse: producers, consumers,
// segment rolls, rebalances and metric updates all happening at once.
func TestConcurrentPublishAndConsume(t *testing.T) {
	t.Parallel()
	s := stream("racy", 8)
	reg := obs.NewRegistry("service", "test")
	l := openTestLog(t, "", []canon.Stream{s}, WithSegmentBytes(4096), WithMetrics(reg))

	const producers, perProducer = 6, 120
	total := producers * perProducer

	sk := newSink(total)
	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "racy", Topics: []string{s.Name}, FromBeginning: true})
	runConsumer(t, c, sk.handle)
	runConsumer(t, c, sk.handle)
	runConsumer(t, c, sk.handle)

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				if err := l.Publish(context.Background(), eventbus.Message{
					Topic: s.Name,
					Key:   fmt.Sprintf("store-%d:sku-%d", p, i),
					Value: []byte(fmt.Sprintf("%d/%d", p, i)),
				}); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
			}
		}(p)
	}
	// Read lag concurrently with everything else; it must never block or race.
	stopLag := make(chan struct{})
	var lagWG sync.WaitGroup
	lagWG.Add(1)
	go func() {
		defer lagWG.Done()
		for {
			select {
			case <-stopLag:
				return
			default:
				if _, err := c.Lag(context.Background()); err != nil && !errors.Is(err, eventbus.ErrClosed) {
					t.Errorf("Lag: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
	got := sk.wait(t, 30*time.Second)
	close(stopLag)
	lagWG.Wait()

	seen := make(map[string]bool, total)
	for _, m := range got {
		seen[string(m.Value)] = true
	}
	if len(seen) != total {
		t.Fatalf("received %d distinct records, want %d", len(seen), total)
	}
	assertNoDuplicates(t, got)
}

func TestSubscribeValidation(t *testing.T) {
	t.Parallel()
	s := stream("subscribe", 1)
	l := openTestLog(t, "", []canon.Stream{s})

	if _, err := l.Subscribe(eventbus.SubscribeOptions{Topics: []string{s.Name}}); err == nil {
		t.Fatal("Subscribe without a group succeeded")
	}
	if _, err := l.Subscribe(eventbus.SubscribeOptions{Group: "g"}); err == nil {
		t.Fatal("Subscribe without topics succeeded")
	}
	if _, err := l.Subscribe(eventbus.SubscribeOptions{Group: "g", Topics: []string{"missing"}}); !errors.Is(err, eventbus.ErrNoTopic) {
		t.Fatalf("Subscribe to an unknown topic = %v, want ErrNoTopic", err)
	}
	if _, err := l.Subscribe(eventbus.SubscribeOptions{
		Group: "g", Topics: []string{s.Name}, DLQTopic: "no-such-dlq",
	}); !errors.Is(err, eventbus.ErrNoTopic) {
		t.Fatalf("Subscribe with a missing DLQ = %v, want ErrNoTopic", err)
	}

	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "g", Topics: []string{s.Name}})
	if err := c.Run(context.Background(), nil); err == nil {
		t.Fatal("Run with a nil handler succeeded")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Run(context.Background(), func(context.Context, eventbus.Message) error { return nil }); !errors.Is(err, eventbus.ErrClosed) {
		t.Fatalf("Run after Close = %v, want ErrClosed", err)
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	t.Parallel()
	s := stream("metrics", 1)
	reg := obs.NewRegistry("service", "eventlog-test")
	l := openTestLog(t, "", []canon.Stream{s}, WithMetrics(reg))

	const n = 10
	for i := 0; i < n; i++ {
		publishN(t, l, s.Name, eventbus.Message{Topic: s.Name, Key: "k", Value: []byte(strconv.Itoa(i))})
	}
	c := subscribe(t, l, eventbus.SubscribeOptions{Group: "metrics", Topics: []string{s.Name}, FromBeginning: true})
	sk := newSink(n)
	runConsumer(t, c, sk.handle)
	sk.wait(t, 10*time.Second)

	if got := l.m.appended.With(s.Name).Value(); got != n {
		t.Fatalf("records_appended = %d, want %d", got, n)
	}
	if got := l.m.bytes.With(s.Name).Value(); got == 0 {
		t.Fatal("bytes_appended stayed at zero")
	}
	if got := l.m.handler.With(s.Name, "metrics").Count(); got < n {
		t.Fatalf("handler observations = %d, want at least %d", got, n)
	}
	if _, err := c.Lag(context.Background()); err != nil {
		t.Fatalf("Lag: %v", err)
	}
	if got := l.m.lag.With("metrics", s.Name, "0").Value(); got != 0 {
		t.Fatalf("lag gauge = %v, want 0", got)
	}
}
