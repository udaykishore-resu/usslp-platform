package kvstore

// Tests for the MVCC pin/trim protocol — the contract that makes a Snapshot a
// real point-in-time view rather than an approximate one.
//
// These are in-package because the property under test is precisely the one the
// public API cannot show you directly: that the version chain a reader is
// walking, with no lock held, is not being rewritten underneath it. Driving
// that through Get and Snapshot alone only reproduces it on an unlucky
// scheduling interleaving, which is how it survived into production in the
// first place.

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	// Checkpointing and expiry are pushed out of the way so that what these
	// tests observe is the write path's own trimming and nothing else.
	s, err := OpenWith(Options{Sync: SyncNever, CheckpointEvery: time.Hour, ExpireEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// chainOf snapshots a published version chain as a plain slice, so a later
// comparison can tell whether anything in it was mutated after publication.
type chainLink struct {
	v    *version
	next *version
	seq  uint64
	val  string
}

func chainOf(head *version) []chainLink {
	var out []chainLink
	for v := head; v != nil; v = v.next.Load() {
		out = append(out, chainLink{v: v, next: v.next.Load(), seq: v.seq, val: string(v.val)})
	}
	return out
}

// TestPublishedVersionChainIsImmutable is the direct regression test for the
// pin/trim interleaving.
//
// A reader loads a key's chain head and then walks it with no lock held. It may
// be descheduled anywhere along that walk — arbitrarily long, as a preempted
// goroutine on a two-core gateway routinely is. Trimming must therefore never
// touch a version that is already reachable from a published head; it must
// publish a shorter chain and leave the old one intact.
//
// Against the previous behaviour, where trim severed the live chain in place
// with v.next.Store(nil), this fails on every run.
func TestPublishedVersionChainIsImmutable(t *testing.T) {
	s := testStore(t)
	key := []byte("sku/0001")

	// Hold a snapshot while the chain is built so that trimming leaves the
	// history in place and the reader below has a deep chain to walk.
	if err := s.Put(key, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	pinned := s.Snapshot()
	const depth = 12
	for i := 2; i <= depth; i++ {
		if err := s.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	// A reader loads the head and the sequence it is reading at, exactly as
	// lookup does, and is then descheduled before it walks.
	s.mu.RLock()
	readerSeq := s.seq
	node := s.idx.get(string(key))
	head := node.head.Load()
	s.mu.RUnlock()
	if head == nil {
		t.Fatal("key has no versions")
	}
	before := chainOf(head)
	if len(before) < depth {
		t.Fatalf("expected a chain of at least %d versions to walk, got %d", depth, len(before))
	}

	// Release the pin and keep writing. This is what makes trimming bite: with
	// no snapshot holding history back, minPin jumps to the write frontier and
	// everything the reader is standing on becomes trimmable.
	pinned.Close()
	for i := depth + 1; i <= depth+8; i++ {
		if err := s.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	// The reader now walks. Every link it was given must be exactly as it was.
	after := chainOf(head)
	if len(after) != len(before) {
		t.Fatalf("published chain was rewritten under a reader: length %d became %d", len(before), len(after))
	}
	for i := range before {
		if after[i].v != before[i].v || after[i].next != before[i].next {
			t.Fatalf("published version at depth %d (seq %d) was mutated after publication: next %p became %p",
				i, before[i].seq, before[i].next, after[i].next)
		}
		if after[i].val != before[i].val || after[i].seq != before[i].seq {
			t.Fatalf("published version at depth %d changed value: seq %d %q became seq %d %q",
				i, before[i].seq, before[i].val, after[i].seq, after[i].val)
		}
	}

	// And it must still resolve, for every sequence the chain covered, the value
	// that was current at that sequence.
	now := time.Now().UnixNano()
	for i := 1; i <= depth; i++ {
		want := fmt.Sprintf("v%d", i)
		got, ok := liveIn(head, uint64(i), now)
		if !ok {
			t.Fatalf("reader at sequence %d lost its version: want %q, got not-found", i, want)
		}
		if string(got) != want {
			t.Fatalf("reader at sequence %d: want %q, got %q", i, want, got)
		}
	}
	if _, ok := liveIn(head, readerSeq, now); !ok {
		t.Fatalf("reader at its own resolved sequence %d lost its version", readerSeq)
	}
}

// TestTrimStillBoundsChainLength guards the other side of the fix. Publishing a
// shorter chain instead of severing the live one must still actually shorten
// it: a key rewritten all day with no snapshot open has to settle at a short
// chain, or an edge gateway's memory grows without bound.
func TestTrimStillBoundsChainLength(t *testing.T) {
	s := testStore(t)
	key := []byte("sku/0001")
	for i := 0; i < 500; i++ {
		if err := s.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.RLock()
	n := s.idx.get(string(key))
	got := len(chainOf(n.head.Load()))
	s.mu.RUnlock()
	if got > 2 {
		t.Fatalf("chain length %d after 500 writes with no snapshot open; trimming is not reclaiming", got)
	}

	// With a snapshot pinned, history must be held back instead.
	sn := s.Snapshot()
	defer sn.Close()
	for i := 500; i < 540; i++ {
		if err := s.Put(key, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.RLock()
	pinnedLen := len(chainOf(n.head.Load()))
	s.mu.RUnlock()
	if pinnedLen < 40 {
		t.Fatalf("chain length %d while a snapshot is pinned; history a snapshot needs was trimmed", pinnedLen)
	}
	if v, err := sn.Get(key); err != nil || string(v) != "v499" {
		t.Fatalf("pinned snapshot: got %q, %v; want %q", v, err, "v499")
	}
}

// TestSnapshotAtSequenceZeroIsIsolated is the regression test for the reported
// failure. Sequence zero is an ordinary sequence — every store has it before
// its first write — and a snapshot taken there is a real point-in-time view of
// an empty store. It used to collide with the sentinel meaning "read the
// latest", which quietly turned such a snapshot into a live read.
func TestSnapshotAtSequenceZeroIsIsolated(t *testing.T) {
	s := testStore(t)
	sn := s.Snapshot()
	defer sn.Close()
	if sn.Sequence() != 0 {
		t.Fatalf("expected a snapshot of an unwritten store to sit at sequence 0, got %d", sn.Sequence())
	}
	for i := 0; i < 5; i++ {
		if err := s.Put([]byte("agg/00"), []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		v, err := sn.Get([]byte("agg/00"))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("write %d landed inside a snapshot taken before it: got %q, %v", i, v, err)
		}
		if has, err := sn.Has([]byte("agg/00")); err != nil || has {
			t.Fatalf("write %d visible to Has on an earlier snapshot: %v, %v", i, has, err)
		}
	}
	// The live store must of course see them.
	if v, err := s.Get([]byte("agg/00")); err != nil || string(v) != "4" {
		t.Fatalf("live Get: got %q, %v; want %q", v, err, "4")
	}
	// And an iterator taken at sequence zero must be empty for the same reason.
	it := sn.Scan([]byte("agg/"))
	defer it.Close()
	if it.Next() {
		t.Fatalf("iterator on a sequence-0 snapshot yielded %q", it.Key())
	}
}

// TestSnapshotGetIsRepeatable states the guarantee the reported failure broke:
// two reads of one key through one pinned snapshot always agree, whatever the
// writers do in between. It is checked across every sequence a snapshot can be
// taken at, sequence zero included.
func TestSnapshotGetIsRepeatable(t *testing.T) {
	s := testStore(t)
	key := []byte("agg/00")
	for pinAt := 0; pinAt < 6; pinAt++ {
		sn := s.Snapshot()
		if sn.Sequence() != uint64(pinAt) {
			t.Fatalf("expected pin at %d, got %d", pinAt, sn.Sequence())
		}
		first, firstErr := sn.Get(key)
		for i := 0; i < 20; i++ {
			if err := s.Put(key, []byte(fmt.Sprintf("p%d-%d", pinAt, i))); err != nil {
				t.Fatal(err)
			}
		}
		again, againErr := sn.Get(key)
		if string(first) != string(again) || !errors.Is(againErr, firstErr) {
			t.Fatalf("snapshot pinned at %d is not stable: %q (%v) then %q (%v)",
				pinAt, first, firstErr, again, againErr)
		}
		sn.Close()
		// Leave the store one sequence further on for the next iteration.
		if err := s.Put([]byte("filler"), []byte("x")); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.seq = uint64(pinAt) + 1
		s.recomputeMinPin()
		s.mu.Unlock()
	}
}

// TestLiveGetNeverMissesLivingKey covers the other half of the same defect. A
// plain Get is not pinned: it resolves a sequence and then walks the chain. If
// trimming can rewrite that chain in between, Get reports ErrNotFound — "no
// price on file" — for a key that has existed continuously. Against the
// previous behaviour this fires within a second, every run.
func TestLiveGetNeverMissesLivingKey(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; runs for a second")
	}
	s := testStore(t)
	key := []byte("sku/hot")
	if err := s.Put(key, []byte("v0")); err != nil {
		t.Fatal(err)
	}

	var misses, reads atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < 200; j++ {
					if _, err := s.Get(key); errors.Is(err, ErrNotFound) {
						misses.Add(1)
					}
					reads.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := s.Put(key, []byte(fmt.Sprintf("v%d-%d", w, n))); err != nil {
					return
				}
			}
		}(i)
	}
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	if m := misses.Load(); m != 0 {
		t.Fatalf("Get reported ErrNotFound %d times in %d reads for a key that never stopped existing",
			m, reads.Load())
	}
}

// TestSnapshotIsolatedUnderConcurrentTrimming pins a snapshot over a store
// being rewritten hard from several goroutines and checks, key by key, that it
// keeps returning the values that were current when it was taken.
func TestSnapshotIsolatedUnderConcurrentTrimming(t *testing.T) {
	s := testStore(t)
	const keys = 64
	want := make(map[string]string, keys)
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("sku/%04d", i)
		v := fmt.Sprintf("base-%d", i)
		if err := s.Put([]byte(k), []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}

	sn := s.Snapshot()
	defer sn.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				k := fmt.Sprintf("sku/%04d", n%keys)
				if err := s.Put([]byte(k), []byte(fmt.Sprintf("churn-%d-%d", w, n))); err != nil {
					return
				}
				if n%17 == 0 {
					_ = s.Delete([]byte(k))
				}
			}
		}(w)
	}

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		for k, v := range want {
			got, err := sn.Get([]byte(k))
			if err != nil {
				t.Fatalf("snapshot lost %s: %v", k, err)
			}
			if string(got) != v {
				t.Fatalf("snapshot %s: got %q, want %q", k, got, v)
			}
		}
	}
	close(stop)
	wg.Wait()
}
