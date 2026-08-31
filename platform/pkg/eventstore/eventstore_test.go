package eventstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func openStore(t *testing.T) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// priceEvent builds a realistic price-change envelope so the tests exercise the
// same validation path production traffic does.
func priceEvent(t *testing.T, sku string, minor int64, idem string) canon.Envelope {
	t.Helper()
	// Aggregate coordinates are deliberately left blank: the store derives them
	// from the stream, and these tests check that it does.
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "", "", "tenant-acme",
		canon.PriceChangeRequested{
			SKU:          canon.SKU(sku),
			StoreID:      "store-0042",
			Price:        canon.NewMoney(minor, "GBP"),
			EffectiveAt:  time.Now().UTC(),
			InitiatedBy:  "till-07",
			SourceSystem: "ncr",
		})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.Source = "eventstore-test"
	env.StoreID = "store-0042"
	env.IdempotencyKey = idem
	return env
}

func TestAppendAndReadBack(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stream := eventstore.Stream("label", "lbl-0001")

	if v, err := s.Version(ctx, stream); err != nil || v != 0 {
		t.Fatalf("version of new stream = %d, %v", v, err)
	}
	if err := s.Append(ctx, stream, eventstore.ExpectedNoStream,
		priceEvent(t, "SKU-1", 199, ""),
		priceEvent(t, "SKU-1", 149, "")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if v, err := s.Version(ctx, stream); err != nil || v != 2 {
		t.Fatalf("version = %d, %v", v, err)
	}

	got, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d events, want 2", len(got))
	}
	for i, r := range got {
		if r.Version != int64(i+1) {
			t.Fatalf("event %d has version %d", i, r.Version)
		}
		if r.Position != int64(i+1) {
			t.Fatalf("event %d has position %d", i, r.Position)
		}
		if r.Stream != stream {
			t.Fatalf("event %d has stream %q", i, r.Stream)
		}
		// The stored envelope is stamped with its stream coordinates.
		if r.Event.Version != r.Version {
			t.Fatalf("envelope version %d != record version %d", r.Event.Version, r.Version)
		}
		if r.Event.AggregateType != "label" || r.Event.AggregateID != "lbl-0001" {
			t.Fatalf("aggregate not derived from stream: %+v", r.Event)
		}
		if r.Event.RecordedAt.IsZero() {
			t.Fatal("recorded_at not stamped")
		}
	}
	var payload canon.PriceChangeRequested
	if err := got[1].Event.Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Price.Amount != 149 {
		t.Fatalf("price = %d", payload.Price.Amount)
	}

	// fromVersion is inclusive, and limit bounds the result.
	tail, err := s.ReadStream(ctx, stream, 2, 0)
	if err != nil || len(tail) != 1 || tail[0].Version != 2 {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	one, err := s.ReadStream(ctx, stream, 1, 1)
	if err != nil || len(one) != 1 || one[0].Version != 1 {
		t.Fatalf("limited read = %+v, %v", one, err)
	}

	// Unknown stream reads empty rather than erroring: a label with no history
	// is a normal state, not a fault.
	empty, err := s.ReadStream(ctx, eventstore.Stream("label", "nope"), 1, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown stream = %+v, %v", empty, err)
	}

	all, err := s.ReadAll(ctx, 1, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("read all = %d events, %v", len(all), err)
	}
	if s.LastPosition() != 2 {
		t.Fatalf("last position = %d", s.LastPosition())
	}

	// An empty append is a no-op, and an invalid envelope never enters the log.
	if err := s.Append(ctx, stream, eventstore.ExpectedAny); err != nil {
		t.Fatalf("empty append: %v", err)
	}
	if err := s.Append(ctx, stream, eventstore.ExpectedAny, canon.Envelope{}); err == nil {
		t.Fatal("invalid envelope accepted")
	}
	if s.LastPosition() != 2 {
		t.Fatalf("failed append moved the position to %d", s.LastPosition())
	}
}

func TestOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stream := eventstore.Stream("pricing", "store-0042/SKU-1")

	if err := s.Append(ctx, stream, eventstore.ExpectedNoStream, priceEvent(t, "SKU-1", 199, "")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The stream now exists, so ExpectedNoStream must be refused.
	err := s.Append(ctx, stream, eventstore.ExpectedNoStream, priceEvent(t, "SKU-1", 150, ""))
	if !errors.Is(err, eventstore.ErrConcurrency) {
		t.Fatalf("ExpectedNoStream on existing stream = %v", err)
	}
	// A stale expected version is refused and writes nothing.
	err = s.Append(ctx, stream, 0, priceEvent(t, "SKU-1", 150, ""))
	if !errors.Is(err, eventstore.ErrConcurrency) {
		t.Fatalf("stale version = %v", err)
	}
	if v, _ := s.Version(ctx, stream); v != 1 {
		t.Fatalf("rejected append mutated the stream: version %d", v)
	}
	// The correct version succeeds.
	if err := s.Append(ctx, stream, 1, priceEvent(t, "SKU-1", 150, "")); err != nil {
		t.Fatalf("append at correct version: %v", err)
	}

	// Two tills repricing the same SKU from the same version: exactly one wins.
	const racers = 32
	var wins, conflicts int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	base, _ := s.Version(ctx, stream)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := s.Append(ctx, stream, base, priceEvent(t, "SKU-1", int64(100+i), ""))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, eventstore.ErrConcurrency):
				conflicts++
			default:
				t.Errorf("unexpected append error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and %d", wins, conflicts, racers-1)
	}
	if v, _ := s.Version(ctx, stream); v != base+1 {
		t.Fatalf("version = %d, want %d", v, base+1)
	}
	if st := s.Stats(); st.Conflicts < uint64(racers-1) {
		t.Fatalf("conflict counter = %d", st.Conflicts)
	}
}

func TestExpectedAny(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stream := eventstore.Stream("device", "lbl-telemetry")

	// ExpectedAny appends onto a stream that does not exist yet...
	if err := s.Append(ctx, stream, eventstore.ExpectedAny, priceEvent(t, "SKU-9", 1, "")); err != nil {
		t.Fatalf("ExpectedAny on new stream: %v", err)
	}
	// ...and keeps appending regardless of how far the stream has moved.
	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Append(ctx, stream, eventstore.ExpectedAny, priceEvent(t, "SKU-9", int64(i), "")); err != nil {
				t.Errorf("ExpectedAny append: %v", err)
			}
		}(i)
	}
	wg.Wait()

	v, err := s.Version(ctx, stream)
	if err != nil || v != n+1 {
		t.Fatalf("version = %d, %v; want %d", v, err, n+1)
	}
	recs, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n+1 {
		t.Fatalf("read %d, want %d", len(recs), n+1)
	}
	// Versions are dense and strictly increasing even under contention.
	for i, r := range recs {
		if r.Version != int64(i+1) {
			t.Fatalf("version gap at %d: %d", i, r.Version)
		}
	}
	if st := s.Stats(); st.Conflicts != 0 {
		t.Fatalf("ExpectedAny produced %d conflicts", st.Conflicts)
	}
}

func TestGlobalPositionMonotonicUnderConcurrentStreams(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	const writers, each = 10, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			stream := eventstore.Stream("label", fmt.Sprintf("lbl-%03d", w))
			for i := 0; i < each; i++ {
				if err := s.Append(ctx, stream, int64(i), priceEvent(t, fmt.Sprintf("SKU-%d", w), int64(i), "")); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	all, err := s.ReadAll(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != writers*each {
		t.Fatalf("read %d events, want %d", len(all), writers*each)
	}
	perStream := map[eventstore.StreamID]int64{}
	for i, r := range all {
		if r.Position != int64(i+1) {
			t.Fatalf("position %d at index %d: global order has a gap", r.Position, i)
		}
		// Per-stream versions must also be dense and in position order.
		if r.Version != perStream[r.Stream]+1 {
			t.Fatalf("stream %s jumped from version %d to %d", r.Stream, perStream[r.Stream], r.Version)
		}
		perStream[r.Stream] = r.Version
	}
	if len(perStream) != writers {
		t.Fatalf("saw %d streams, want %d", len(perStream), writers)
	}
	if s.LastPosition() != int64(writers*each) {
		t.Fatalf("last position = %d", s.LastPosition())
	}

	// Paged reads reproduce exactly the same order.
	var paged []eventstore.Recorded
	for from := int64(1); ; {
		page, err := s.ReadAll(ctx, from, 37)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		from = page[len(page)-1].Position + 1
	}
	if len(paged) != len(all) {
		t.Fatalf("paged read %d, want %d", len(paged), len(all))
	}
	for i := range paged {
		if paged[i].Position != all[i].Position || paged[i].Event.EventID != all[i].Event.EventID {
			t.Fatalf("paged read diverged at %d", i)
		}
	}
}

func TestCatchUpSubscriptionIsGapless(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t)

	// Pre-load history so the subscription has a real catch-up phase.
	const history = 300
	histStream := eventstore.Stream("label", "history")
	for i := 0; i < history; i++ {
		if err := s.Append(ctx, histStream, int64(i), priceEvent(t, "SKU-H", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}

	// Start a writer that keeps appending across the history/live switchover,
	// which is exactly the window a naive implementation drops events in.
	const live = 400
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		liveStream := eventstore.Stream("label", "live")
		for i := 0; i < live; i++ {
			if err := s.Append(ctx, liveStream, int64(i), priceEvent(t, "SKU-L", int64(i), "")); err != nil {
				t.Errorf("live append: %v", err)
				return
			}
			if i%50 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	total := history + live
	var mu sync.Mutex
	var seen []int64
	done := make(chan error, 1)
	complete := make(chan struct{})
	go func() {
		done <- s.SubscribeAll(ctx, 1, func(_ context.Context, r eventstore.Recorded) error {
			mu.Lock()
			seen = append(seen, r.Position)
			n := len(seen)
			mu.Unlock()
			if n == total {
				close(complete)
			}
			return nil
		})
	}()

	<-writerDone
	select {
	case <-complete:
	case err := <-done:
		t.Fatalf("subscription ended early: %v", err)
	case <-time.After(20 * time.Second):
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		t.Fatalf("subscription delivered %d of %d events", n, total)
	}

	mu.Lock()
	got := append([]int64(nil), seen...)
	mu.Unlock()
	if len(got) != total {
		t.Fatalf("delivered %d, want %d", len(got), total)
	}
	// Exactly the right events: strictly increasing, dense, starting at 1. Any
	// gap is a lost event and any repeat is a double-apply.
	for i, p := range got {
		if p != int64(i+1) {
			t.Fatalf("position %d at index %d: gap or duplicate", p, i)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancelled subscription returned %v, want nil", err)
	}
}

func TestSubscriptionFromMidStreamAndHandlerError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t)
	stream := eventstore.Stream("label", "mid")
	for i := 0; i < 10; i++ {
		if err := s.Append(ctx, stream, int64(i), priceEvent(t, "SKU-M", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}

	var got []int64
	err := s.SubscribeAll(ctx, 6, func(_ context.Context, r eventstore.Recorded) error {
		got = append(got, r.Position)
		if r.Position == 9 {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("handler error = %v, want boom", err)
	}
	want := []int64{6, 7, 8, 9}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if s.Stats().Subscribers != 0 {
		t.Fatal("subscriber not deregistered after handler error")
	}
}

func TestConcurrentSubscribersAllSeeEverything(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t)

	const subs, events = 4, 250
	counts := make([]int, subs)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < subs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.SubscribeAll(ctx, 1, func(_ context.Context, r eventstore.Recorded) error {
				mu.Lock()
				counts[i]++
				if r.Position != int64(counts[i]) {
					t.Errorf("subscriber %d saw position %d as event %d", i, r.Position, counts[i])
				}
				mu.Unlock()
				return nil
			})
		}(i)
	}
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < events; i++ {
		st := eventstore.Stream("label", fmt.Sprintf("s-%d", i%7))
		v, err := s.Version(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, st, v, priceEvent(t, "SKU-C", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := true
		for _, c := range counts {
			if c != events {
				done = false
			}
		}
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	final := append([]int(nil), counts...)
	mu.Unlock()
	for i, c := range final {
		if c != events {
			t.Fatalf("subscriber %d saw %d of %d events", i, c, events)
		}
	}
	cancel()
	wg.Wait()
}

func TestIdempotentReAppendReturnsOriginal(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stream := eventstore.Stream("pricing", "store-0042/SKU-7")

	first, err := s.AppendWithResult(ctx, stream, eventstore.ExpectedNoStream,
		priceEvent(t, "SKU-7", 499, "shopify:evt_9f2a"))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if first.Duplicate || len(first.Events) != 1 {
		t.Fatalf("first delivery = %+v", first)
	}
	original := first.Events[0]

	// The webhook is redelivered. It carries the same idempotency key but a
	// fresh event id, exactly as a real retry would.
	retry := priceEvent(t, "SKU-7", 499, "shopify:evt_9f2a")
	if retry.EventID == original.Event.EventID {
		t.Fatal("test bug: retry reused the original event id")
	}
	second, err := s.AppendWithResult(ctx, stream, eventstore.ExpectedNoStream, retry)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("redelivery was not detected as a duplicate")
	}
	if len(second.Events) != 1 {
		t.Fatalf("redelivery returned %d events", len(second.Events))
	}
	if second.Events[0].Event.EventID != original.Event.EventID {
		t.Fatalf("redelivery returned %s, want the original %s",
			second.Events[0].Event.EventID, original.Event.EventID)
	}
	if second.Events[0].Position != original.Position || second.Events[0].Version != original.Version {
		t.Fatalf("redelivery coordinates = %d/%d, want %d/%d",
			second.Events[0].Position, second.Events[0].Version, original.Position, original.Version)
	}
	// Nothing was written: the stream and the global position stand still. Note
	// the redelivery passed a now-stale ExpectedNoStream and was *not* reported
	// as a conflict, because the work had already been done.
	if v, _ := s.Version(ctx, stream); v != 1 {
		t.Fatalf("stream advanced to version %d on redelivery", v)
	}
	if s.LastPosition() != original.Position {
		t.Fatalf("global position advanced to %d", s.LastPosition())
	}
	all, _ := s.ReadAll(ctx, 1, 0)
	if len(all) != 1 {
		t.Fatalf("log holds %d events after redelivery, want 1", len(all))
	}
	if st := s.Stats(); st.Duplicates != 1 {
		t.Fatalf("duplicate counter = %d", st.Duplicates)
	}

	// Idempotency keys are scoped per stream, so the same POS event id used for
	// a different label is a genuinely new event.
	other := eventstore.Stream("pricing", "store-0043/SKU-7")
	if err := s.Append(ctx, other, eventstore.ExpectedNoStream, priceEvent(t, "SKU-7", 499, "shopify:evt_9f2a")); err != nil {
		t.Fatalf("same key on another stream: %v", err)
	}

	// A batch that mixes an already-seen key with a new one is refused rather
	// than half-applied.
	err = s.Append(ctx, stream, eventstore.ExpectedAny,
		priceEvent(t, "SKU-7", 499, "shopify:evt_9f2a"),
		priceEvent(t, "SKU-7", 450, "shopify:evt_new"))
	if !errors.Is(err, eventstore.ErrPartialDuplicate) {
		t.Fatalf("mixed batch = %v", err)
	}

	// Concurrent redeliveries of the same webhook produce one event.
	var wg sync.WaitGroup
	var wrote int64
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.AppendWithResult(ctx, stream, eventstore.ExpectedAny,
				priceEvent(t, "SKU-7", 425, "ncr:txn_5511"))
			if err != nil {
				t.Errorf("concurrent redelivery: %v", err)
				return
			}
			if !res.Duplicate {
				mu.Lock()
				wrote++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wrote != 1 {
		t.Fatalf("%d concurrent redeliveries wrote an event, want 1", wrote)
	}
}

func TestSnapshotShortCircuitsLongStream(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stream := eventstore.Stream("label", "lbl-busy")

	if _, err := s.LoadSnapshot(stream); !errors.Is(err, eventstore.ErrNoSnapshot) {
		t.Fatalf("LoadSnapshot on empty stream = %v", err)
	}

	const n = 2000
	for i := 0; i < n; i++ {
		if err := s.Append(ctx, stream, int64(i), priceEvent(t, "SKU-B", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}

	// Fold the stream into an aggregate snapshot at its current version.
	full, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != n {
		t.Fatalf("full replay read %d", len(full))
	}
	state := []byte(fmt.Sprintf(`{"price":%d}`, n-1))
	if err := s.SaveSnapshot(stream, full[len(full)-1].Version, state); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// A few more events arrive after the snapshot.
	for i := 0; i < 3; i++ {
		if err := s.Append(ctx, stream, int64(n+i), priceEvent(t, "SKU-B", int64(9000+i), "")); err != nil {
			t.Fatal(err)
		}
	}

	before := s.Stats().EventsRead
	snap, err := s.LoadSnapshot(stream)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snap.Version != int64(n) || string(snap.State) != string(state) {
		t.Fatalf("snapshot = %+v", snap)
	}
	tail, err := s.ReadStream(ctx, stream, snap.Version+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 3 {
		t.Fatalf("tail after snapshot = %d events, want 3", len(tail))
	}
	if read := s.Stats().EventsRead - before; read != 3 {
		t.Fatalf("loading via the snapshot read %d events, want 3", read)
	}
	if tail[0].Version != snap.Version+1 {
		t.Fatalf("tail starts at version %d", tail[0].Version)
	}

	// Deleting the snapshot forces a full replay again, proving snapshots are
	// an optimisation and never the source of truth.
	if err := s.DeleteSnapshot(stream); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSnapshot(stream); !errors.Is(err, eventstore.ErrNoSnapshot) {
		t.Fatalf("snapshot survived delete: %v", err)
	}
	replay, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != n+3 {
		t.Fatalf("full replay = %d events, want %d", len(replay), n+3)
	}
}

func TestSnapshotSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stream := eventstore.Stream("label", "lbl-restart")
	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, stream, int64(i), priceEvent(t, "SKU-R", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveSnapshot(stream, 5, []byte(`{"price":4}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := eventstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap, err := s2.LoadSnapshot(stream)
	if err != nil {
		t.Fatalf("snapshot lost across restart: %v", err)
	}
	if snap.Version != 5 || string(snap.State) != `{"price":4}` {
		t.Fatalf("snapshot = %+v", snap)
	}
	// The global position counter resumes where it left off, so positions are
	// never reused after a restart.
	if s2.LastPosition() != 5 {
		t.Fatalf("last position after restart = %d", s2.LastPosition())
	}
	if err := s2.Append(ctx, stream, 5, priceEvent(t, "SKU-R", 99, "")); err != nil {
		t.Fatal(err)
	}
	all, _ := s2.ReadAll(ctx, 1, 0)
	if len(all) != 6 || all[5].Position != 6 || all[5].Version != 6 {
		t.Fatalf("post-restart append = %+v", all[len(all)-1])
	}
}

func TestProjectionCheckpointSurvivesRestartAndRebuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// The read model lives in the same kvstore as the events, so a projection's
	// rows and its checkpoint move together.
	newRun := func() (*eventstore.Store, *eventstore.Projection, func()) {
		s, err := eventstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.NewProjection("price-by-sku", func(_ context.Context, r eventstore.Recorded, b *kvstore.Batch) error {
			var pc canon.PriceChangeRequested
			if err := r.Event.Decode(&pc); err != nil {
				return err
			}
			b.Put([]byte("rm/"+string(pc.SKU)), []byte(fmt.Sprint(pc.Price.Amount)))
			b.Put([]byte(fmt.Sprintf("rm-count/%06d", r.Position)), []byte("1"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return s, p, func() { s.Close() }
	}

	s, p, closeFn := newRun()
	stream := eventstore.Stream("label", "lbl-proj")
	for i := 0; i < 20; i++ {
		if err := s.Append(ctx, stream, int64(i), priceEvent(t, "SKU-P", int64(100+i), "")); err != nil {
			t.Fatal(err)
		}
	}
	pos, err := p.CatchUp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 20 {
		t.Fatalf("caught up to %d, want 20", pos)
	}
	if got, _ := p.Position(); got != 20 {
		t.Fatalf("checkpoint = %d", got)
	}
	v, err := s.KV().Get([]byte("rm/SKU-P"))
	if err != nil || string(v) != "119" {
		t.Fatalf("read model = %q, %v", v, err)
	}
	closeFn()

	// Restart: the checkpoint is durable, so only the new events are applied.
	s2, p2, close2 := newRun()
	if got, _ := p2.Position(); got != 20 {
		t.Fatalf("checkpoint after restart = %d, want 20", got)
	}
	for i := 20; i < 25; i++ {
		if err := s2.Append(ctx, stream, int64(i), priceEvent(t, "SKU-P", int64(100+i), "")); err != nil {
			t.Fatal(err)
		}
	}
	applied := 0
	p3, err := s2.NewProjection("price-by-sku", func(_ context.Context, r eventstore.Recorded, b *kvstore.Batch) error {
		applied++
		b.Put([]byte("rm/seen"), []byte(fmt.Sprint(r.Position)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p3.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if applied != 5 {
		t.Fatalf("resumed run applied %d events, want 5 (checkpoint not honoured)", applied)
	}

	// Full rebuild from zero replays everything.
	rebuilt := 0
	p4, err := s2.NewProjection("price-by-sku", func(_ context.Context, r eventstore.Recorded, b *kvstore.Batch) error {
		rebuilt++
		b.Put([]byte(fmt.Sprintf("rm2/%06d", r.Position)), []byte("1"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cleared := false
	end, err := p4.Rebuild(ctx, func(context.Context) error {
		cleared = true
		it := s2.KV().Scan([]byte("rm2/"))
		defer it.Close()
		for it.Next() {
			if err := s2.KV().Delete(it.Key()); err != nil {
				return err
			}
		}
		return it.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("clear callback not invoked")
	}
	if rebuilt != 25 || end != 25 {
		t.Fatalf("rebuild applied %d events to position %d, want 25/25", rebuilt, end)
	}
	if got, _ := p4.Position(); got != 25 {
		t.Fatalf("checkpoint after rebuild = %d", got)
	}
	close2()
}

func TestProjectionRunFollowsLiveTail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t)

	var mu sync.Mutex
	var seen []int64
	reached := make(chan struct{})
	p, err := s.NewProjection("live-tail", func(_ context.Context, r eventstore.Recorded, b *kvstore.Batch) error {
		b.Put([]byte(fmt.Sprintf("lt/%06d", r.Position)), []byte("1"))
		mu.Lock()
		seen = append(seen, r.Position)
		n := len(seen)
		mu.Unlock()
		if n == 40 {
			close(reached)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	stream := eventstore.Stream("label", "lbl-live")
	for i := 0; i < 40; i++ {
		if err := s.Append(ctx, stream, int64(i), priceEvent(t, "SKU-T", int64(i), "")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("projection stopped early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("projection did not follow the live tail")
	}

	// A second concurrent Run of the same projection is refused rather than
	// silently racing on one checkpoint.
	if err := p.Run(ctx); err == nil {
		t.Fatal("concurrent Run accepted")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancelled Run returned %v", err)
	}
	if got, _ := p.Position(); got != 40 {
		t.Fatalf("checkpoint after live run = %d, want 40", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, pos := range seen {
		if pos != int64(i+1) {
			t.Fatalf("live tail delivered %v", seen)
		}
	}
}

func TestClosedStoreAndBadInput(t *testing.T) {
	ctx := context.Background()
	s, err := eventstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendWithResult(ctx, "", eventstore.ExpectedAny, priceEvent(t, "SKU-X", 1, "")); !errors.Is(err, eventstore.ErrInvalidStream) {
		t.Fatalf("empty stream = %v", err)
	}
	if _, err := s.AppendWithResult(ctx, "bad\x00stream", eventstore.ExpectedAny, priceEvent(t, "SKU-X", 1, "")); !errors.Is(err, eventstore.ErrInvalidStream) {
		t.Fatalf("NUL in stream = %v", err)
	}
	if _, err := s.NewProjection("", nil); err == nil {
		t.Fatal("nameless projection accepted")
	}
	if err := s.SubscribeAll(ctx, 1, nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "label/x", eventstore.ExpectedAny, priceEvent(t, "SKU-X", 1, "")); !errors.Is(err, eventstore.ErrClosed) {
		t.Fatalf("append after close = %v", err)
	}
	if _, err := s.ReadAll(ctx, 1, 0); !errors.Is(err, eventstore.ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestSubscriptionStopsWhenStoreCloses(t *testing.T) {
	s, err := eventstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- s.SubscribeAll(ctx, 1, func(context.Context, eventstore.Recorded) error { return nil })
	}()
	time.Sleep(30 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, eventstore.ErrClosed) {
			t.Fatalf("subscription returned %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not stop when the store closed")
	}
}
