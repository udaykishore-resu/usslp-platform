package idem_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func newGuard(t *testing.T, opts ...idem.Option) (*idem.Guard, *kvstore.Store) {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{ExpireEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("kvstore: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	be, err := idem.NewKVBackend(kv, "")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	g, err := idem.New(be, opts...)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	return g, kv
}

func TestKeyDerivationIsStableAndUnambiguous(t *testing.T) {
	// Golden values: the derivation is a wire contract between ingress replicas
	// and across releases, so changing it must break this test loudly.
	if got := idem.Key("shopify", "tenant-acme", "evt_9f2a"); got !=
		"4e4339ea5f185f410ff1720cc1abcb55a4306019a1cf85600d6d8751fa7389f1" {
		t.Fatalf("key derivation changed: %s", got)
	}
	if got := idem.Key(); got !=
		"76a1613e17b9d0f6571b0c5879c2b43413eceb7202fba3c70efa67a24c70a840" {
		t.Fatalf("empty key derivation changed: %s", got)
	}

	// Deterministic across calls.
	for i := 0; i < 100; i++ {
		if idem.Key("sap", "0042", "IDOC-88231") != idem.Key("sap", "0042", "IDOC-88231") {
			t.Fatal("key derivation is not deterministic")
		}
	}
	// Length prefixing, not separators: regrouping the same bytes must not
	// collide, and neither must a part that contains a plausible separator.
	pairs := [][2][]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"a:b", "c"}, {"a", "b:c"}},
		{{"", "abc"}, {"abc", ""}},
		{{"a", "b", "c"}, {"a", "bc"}},
	}
	for _, p := range pairs {
		if idem.Key(p[0]...) == idem.Key(p[1]...) {
			t.Fatalf("collision between %v and %v", p[0], p[1])
		}
	}
	// Hex of a SHA-256 digest.
	if n := len(idem.Key("x")); n != 64 {
		t.Fatalf("key length = %d, want 64", n)
	}
	for _, c := range idem.Key("x") {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("non-hex character %q in key", c)
		}
	}
}

func TestCheckRecordAndReplay(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t)
	key := idem.Key("shopify", "tenant-acme", "evt_1")

	first, prev, err := g.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !first || prev != nil {
		t.Fatalf("first delivery = %v, %q", first, prev)
	}

	// A second delivery while the first is still in flight: not first seen, and
	// no result yet to replay.
	first, prev, err = g.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if first || prev != nil {
		t.Fatalf("in-flight duplicate = %v, %q", first, prev)
	}
	e, err := g.Lookup(ctx, key)
	if err != nil || e.State != idem.StatePending {
		t.Fatalf("lookup = %+v, %v", e, err)
	}

	// The first delivery finishes and records its response.
	if err := g.Record(ctx, key, []byte(`{"price_change_id":"pc-77"}`), 0); err != nil {
		t.Fatal(err)
	}
	first, prev, err = g.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("duplicate reported as first seen after Record")
	}
	if string(prev) != `{"price_change_id":"pc-77"}` {
		t.Fatalf("replayed result = %q", prev)
	}
	e, err = g.Lookup(ctx, key)
	if err != nil || e.State != idem.StateDone || e.RecordedAt.IsZero() {
		t.Fatalf("lookup after record = %+v, %v", e, err)
	}

	// Empty keys are rejected on every entry point.
	if _, _, err := g.Check(ctx, ""); err == nil {
		t.Fatal("empty key accepted by Check")
	}
	if err := g.Record(ctx, "", nil, 0); err == nil {
		t.Fatal("empty key accepted by Record")
	}
	if err := g.Release(ctx, ""); err == nil {
		t.Fatal("empty key accepted by Release")
	}
	if _, err := g.Lookup(ctx, ""); err == nil {
		t.Fatal("empty key accepted by Lookup")
	}
	if _, err := g.Lookup(ctx, idem.Key("never", "seen")); !errors.Is(err, idem.ErrNotFound) {
		t.Fatalf("lookup of unknown key = %v", err)
	}
}

func TestReleaseAllowsRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t)
	key := idem.Key("sap", "IDOC-1")

	first, _, err := g.Check(ctx, key)
	if err != nil || !first {
		t.Fatalf("first check = %v, %v", first, err)
	}
	// Processing failed, so the reservation is dropped and the producer's retry
	// must be allowed to do the work.
	if err := g.Release(ctx, key); err != nil {
		t.Fatal(err)
	}
	first, prev, err := g.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !first || prev != nil {
		t.Fatalf("retry after release = %v, %q; want a fresh first-seen", first, prev)
	}
	// Releasing an unknown key is harmless.
	if err := g.Release(ctx, idem.Key("nothing")); err != nil {
		t.Fatalf("release of unknown key: %v", err)
	}
}

func TestWindowExpiry(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t, idem.WithWindow(60*time.Millisecond))
	if g.Window() != 60*time.Millisecond {
		t.Fatalf("window = %v", g.Window())
	}
	key := idem.Key("ncr", "txn-1")

	first, _, err := g.Check(ctx, key)
	if err != nil || !first {
		t.Fatalf("first check = %v, %v", first, err)
	}
	if err := g.Record(ctx, key, []byte("ok"), 0); err != nil {
		t.Fatal(err)
	}
	if first, prev, _ := g.Check(ctx, key); first || string(prev) != "ok" {
		t.Fatalf("inside window = %v, %q", first, prev)
	}

	// Past the window, the key is forgotten and a delivery is first seen again.
	time.Sleep(100 * time.Millisecond)
	first, prev, err := g.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !first || prev != nil {
		t.Fatalf("after window = %v, %q; want a fresh first-seen", first, prev)
	}

	// An explicit ttl overrides the window for a single result.
	long := idem.Key("ncr", "txn-2")
	if _, _, err := g.Check(ctx, long); err != nil {
		t.Fatal(err)
	}
	if err := g.Record(ctx, long, []byte("kept"), time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if first, prev, _ := g.Check(ctx, long); first || string(prev) != "kept" {
		t.Fatalf("explicit ttl not honoured: %v, %q", first, prev)
	}
}

func TestExactlyOneFirstSeenUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t)

	const keys, racersPerKey = 12, 24
	var mu sync.Mutex
	firsts := make(map[string]int)
	replays := make(map[string]int)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for k := 0; k < keys; k++ {
		key := idem.Key("shopify", "tenant-acme", fmt.Sprintf("evt_%d", k))
		for r := 0; r < racersPerKey; r++ {
			wg.Add(1)
			go func(key string, r int) {
				defer wg.Done()
				<-start
				first, prev, err := g.Check(ctx, key)
				if err != nil {
					t.Errorf("check: %v", err)
					return
				}
				mu.Lock()
				if first {
					firsts[key]++
				} else if prev != nil {
					replays[key]++
				}
				mu.Unlock()
				if first {
					// Only the winner does the work and records the result.
					if err := g.Record(ctx, key, []byte("result-"+key), 0); err != nil {
						t.Errorf("record: %v", err)
					}
				}
			}(key, r)
		}
	}
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(firsts) != keys {
		t.Fatalf("%d keys were claimed, want %d", len(firsts), keys)
	}
	for key, n := range firsts {
		if n != 1 {
			t.Fatalf("key %s was first-seen %d times, want exactly 1", key, n)
		}
	}
	// Every loser must have observed either the in-flight reservation or the
	// recorded result; none may have been told it was first.
	total := 0
	for _, n := range firsts {
		total += n
	}
	if total != keys {
		t.Fatalf("total first-seen = %d, want %d", total, keys)
	}
}

func TestGuardStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	open := func() (*idem.Guard, *kvstore.Store) {
		kv, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
		if err != nil {
			t.Fatal(err)
		}
		be, err := idem.NewKVBackend(kv, "uig/")
		if err != nil {
			t.Fatal(err)
		}
		g, err := idem.New(be)
		if err != nil {
			t.Fatal(err)
		}
		return g, kv
	}

	key := idem.Key("sap", "tenant-acme", "IDOC-88231")
	g, kv := open()
	if first, _, err := g.Check(ctx, key); err != nil || !first {
		t.Fatalf("first check = %v, %v", first, err)
	}
	if err := g.Record(ctx, key, []byte("applied"), 0); err != nil {
		t.Fatal(err)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}

	// A gateway restart must not turn a redelivery into a second price change.
	g2, kv2 := open()
	defer kv2.Close()
	first, prev, err := g2.Check(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("redelivery after restart was reported as first seen")
	}
	if string(prev) != "applied" {
		t.Fatalf("replayed result after restart = %q", prev)
	}
}

func TestBackendPrefixIsolation(t *testing.T) {
	ctx := context.Background()
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	shopify, err := idem.NewKVBackend(kv, "idem/shopify/")
	if err != nil {
		t.Fatal(err)
	}
	sap, err := idem.NewKVBackend(kv, "idem/sap/")
	if err != nil {
		t.Fatal(err)
	}
	gs, err := idem.New(shopify)
	if err != nil {
		t.Fatal(err)
	}
	gp, err := idem.New(sap)
	if err != nil {
		t.Fatal(err)
	}

	key := idem.Key("shared-id")
	if first, _, _ := gs.Check(ctx, key); !first {
		t.Fatal("shopify guard did not claim")
	}
	if first, _, _ := gp.Check(ctx, key); !first {
		t.Fatal("sap guard blocked by an unrelated guard's key")
	}
	if _, err := idem.NewKVBackend(nil, ""); err == nil {
		t.Fatal("nil kvstore accepted")
	}
	if _, err := idem.New(nil); err == nil {
		t.Fatal("nil backend accepted")
	}
}

func TestClockInjection(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	g, _ := newGuard(t, idem.WithClock(func() time.Time { return fixed }))
	key := idem.Key("clock")
	if _, _, err := g.Check(ctx, key); err != nil {
		t.Fatal(err)
	}
	e, err := g.Lookup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !e.RecordedAt.Equal(fixed) {
		t.Fatalf("RecordedAt = %v, want %v", e.RecordedAt, fixed)
	}
}

func TestContextCancellation(t *testing.T) {
	g, _ := newGuard(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := idem.Key("cancelled")
	if _, _, err := g.Check(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("Check on cancelled context = %v", err)
	}
	if err := g.Record(ctx, key, nil, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Record on cancelled context = %v", err)
	}
	if err := g.Release(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("Release on cancelled context = %v", err)
	}
}
