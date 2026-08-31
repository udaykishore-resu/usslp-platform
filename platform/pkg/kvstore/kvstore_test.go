package kvstore_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

func openTemp(t *testing.T) *kvstore.Store {
	t.Helper()
	s, err := kvstore.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustPut(t *testing.T, s *kvstore.Store, k, v string) {
	t.Helper()
	if err := s.Put([]byte(k), []byte(v)); err != nil {
		t.Fatalf("put %s: %v", k, err)
	}
}

func mustGet(t *testing.T, s *kvstore.Store, k string) string {
	t.Helper()
	v, err := s.Get([]byte(k))
	if err != nil {
		t.Fatalf("get %s: %v", k, err)
	}
	return string(v)
}

func TestPutGetHasDelete(t *testing.T) {
	s := openTemp(t)

	if _, err := s.Get([]byte("sku/0001")); !errors.Is(err, kvstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	mustPut(t, s, "sku/0001", "GBP 1.99")
	if got := mustGet(t, s, "sku/0001"); got != "GBP 1.99" {
		t.Fatalf("got %q", got)
	}
	ok, err := s.Has([]byte("sku/0001"))
	if err != nil || !ok {
		t.Fatalf("Has = %v, %v", ok, err)
	}

	mustPut(t, s, "sku/0001", "GBP 1.49")
	if got := mustGet(t, s, "sku/0001"); got != "GBP 1.49" {
		t.Fatalf("overwrite not visible: %q", got)
	}

	if err := s.Delete([]byte("sku/0001")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get([]byte("sku/0001")); !errors.Is(err, kvstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if ok, _ := s.Has([]byte("sku/0001")); ok {
		t.Fatal("Has true after delete")
	}
	// Deleting an absent key is a no-op, not an error.
	if err := s.Delete([]byte("sku/nope")); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	// Value slices handed back are copies.
	mustPut(t, s, "sku/0002", "GBP 2.00")
	v, _ := s.Get([]byte("sku/0002"))
	v[0] = 'X'
	if got := mustGet(t, s, "sku/0002"); got != "GBP 2.00" {
		t.Fatalf("caller mutated store state: %q", got)
	}
	if err := s.Put(nil, []byte("x")); !errors.Is(err, kvstore.ErrEmptyKey) {
		t.Fatalf("empty key accepted: %v", err)
	}
}

func TestScanAndRangeOrdering(t *testing.T) {
	s := openTemp(t)
	keys := []string{
		"label/003", "label/001", "label/002",
		"price/aa", "price/ab", "price/b",
		"zz/last", "aa/first",
	}
	for _, k := range keys {
		mustPut(t, s, k, "v-"+k)
	}

	collect := func(it *kvstore.Iterator) []string {
		defer it.Close()
		var out []string
		for it.Next() {
			out = append(out, string(it.Key())+"="+string(it.Value()))
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iterator: %v", err)
		}
		return out
	}

	got := collect(s.Scan([]byte("label/")))
	want := []string{"label/001=v-label/001", "label/002=v-label/002", "label/003=v-label/003"}
	if !equalStrings(got, want) {
		t.Fatalf("prefix scan = %v, want %v", got, want)
	}

	// A prefix scan must not leak into the neighbouring prefix even when one
	// key is a strict prefix of another.
	got = collect(s.Scan([]byte("price/a")))
	want = []string{"price/aa=v-price/aa", "price/ab=v-price/ab"}
	if !equalStrings(got, want) {
		t.Fatalf("prefix scan = %v, want %v", got, want)
	}

	// Full scan is total order over all keys.
	got = collect(s.Scan(nil))
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if len(got) != len(sorted) {
		t.Fatalf("full scan len %d want %d", len(got), len(sorted))
	}
	for i := range sorted {
		if !strings.HasPrefix(got[i], sorted[i]+"=") {
			t.Fatalf("full scan[%d] = %q want key %q", i, got[i], sorted[i])
		}
	}

	// Range is half-open: start inclusive, end exclusive.
	got = collect(s.Range([]byte("label/002"), []byte("price/ab")))
	want = []string{"label/002=v-label/002", "label/003=v-label/003", "price/aa=v-price/aa"}
	if !equalStrings(got, want) {
		t.Fatalf("range = %v, want %v", got, want)
	}

	// Deleted keys disappear from scans.
	if err := s.Delete([]byte("label/002")); err != nil {
		t.Fatal(err)
	}
	got = collect(s.Scan([]byte("label/")))
	want = []string{"label/001=v-label/001", "label/003=v-label/003"}
	if !equalStrings(got, want) {
		t.Fatalf("scan after delete = %v, want %v", got, want)
	}
}

func TestBatchAtomicVisibility(t *testing.T) {
	s := openTemp(t)
	mustPut(t, s, "k/keep", "1")

	b := s.NewBatch()
	for i := 0; i < 50; i++ {
		b.Put([]byte(fmt.Sprintf("b/%03d", i)), []byte("v"))
	}
	b.Delete([]byte("k/keep"))
	// Last write wins within a batch.
	b.Put([]byte("b/000"), []byte("final"))
	if b.Len() != 51 {
		t.Fatalf("batch len = %d, want 51", b.Len())
	}

	// Before the write, none of it is visible.
	if ok, _ := s.Has([]byte("b/000")); ok {
		t.Fatal("staged batch visible before write")
	}
	if err := s.Write(b); err != nil {
		t.Fatalf("write batch: %v", err)
	}
	if got := mustGet(t, s, "b/000"); got != "final" {
		t.Fatalf("b/000 = %q, want final", got)
	}
	if ok, _ := s.Has([]byte("k/keep")); ok {
		t.Fatal("batched delete not applied")
	}
	n := 0
	it := s.Scan([]byte("b/"))
	for it.Next() {
		n++
	}
	it.Close()
	if n != 50 {
		t.Fatalf("scanned %d batched keys, want 50", n)
	}

	// A batch belongs to the store that made it.
	other := openTemp(t)
	if err := other.Write(b); !errors.Is(err, kvstore.ErrForeignBatch) {
		t.Fatalf("foreign batch accepted: %v", err)
	}

	b.Reset()
	if b.Len() != 0 {
		t.Fatalf("reset left %d entries", b.Len())
	}
}

func TestBatchIsAtomicAcrossCrash(t *testing.T) {
	dir := t.TempDir()
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "before/1", "one")
	mustPut(t, s, "before/2", "two")

	b := s.NewBatch()
	for i := 0; i < 40; i++ {
		b.Put([]byte(fmt.Sprintf("batch/%02d", i)), bytes.Repeat([]byte("x"), 64))
	}
	if err := s.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate losing power part-way through the batch's single WAL record.
	wal := activeWAL(t, dir)
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(wal, info.Size()-100); err != nil {
		t.Fatal(err)
	}

	s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
	if err != nil {
		t.Fatalf("reopen after torn batch: %v", err)
	}
	defer s2.Close()

	if got := mustGet(t, s2, "before/1"); got != "one" {
		t.Fatalf("pre-batch write lost: %q", got)
	}
	if got := mustGet(t, s2, "before/2"); got != "two" {
		t.Fatalf("pre-batch write lost: %q", got)
	}
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("batch/%02d", i)
		if ok, _ := s2.Has([]byte(k)); ok {
			t.Fatalf("torn batch partially applied: %s survived", k)
		}
	}
	// The store must be writable again, appending after the truncated tail.
	mustPut(t, s2, "after/1", "ok")
	if got := mustGet(t, s2, "after/1"); got != "ok" {
		t.Fatalf("post-recovery write = %q", got)
	}
}

func TestCrashRecoveryTornFinalRecord(t *testing.T) {
	dir := t.TempDir()
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		mustPut(t, s, fmt.Sprintf("sku/%04d", i), fmt.Sprintf("price-%d", i))
	}
	if err := s.Delete([]byte("sku/0005")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	wal := activeWAL(t, dir)
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	// Cut 3 bytes: enough to leave a record whose length header promises more
	// payload than the file holds.
	if err := os.Truncate(wal, info.Size()-3); err != nil {
		t.Fatal(err)
	}

	s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncAlways})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// Everything before the torn record survived.
	for i := 0; i < 200; i++ {
		if i == 5 {
			continue
		}
		if got := mustGet(t, s2, fmt.Sprintf("sku/%04d", i)); got != fmt.Sprintf("price-%d", i) {
			t.Fatalf("sku/%04d = %q", i, got)
		}
	}
	// The truncation cut into the final record — the delete — so that record is
	// torn and must be discarded wholesale. The key it removed is therefore
	// still present, which is the correct outcome: the delete was never
	// durably acknowledged.
	if ok, _ := s2.Has([]byte("sku/0005")); !ok {
		t.Fatal("torn final record was partially applied")
	}
	if got := s2.Stats().Keys; got != 200 {
		t.Fatalf("keys after recovery = %d, want 200", got)
	}

	// Recovery must have truncated the log back to a clean boundary, so a
	// second crash-and-recover cycle is stable.
	if err := s2.Put([]byte("sku/9999"), []byte("late")); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := kvstore.OpenWith(kvstore.Options{Dir: dir})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer s3.Close()
	if got := mustGet(t, s3, "sku/9999"); got != "late" {
		t.Fatalf("write after recovery lost: %q", got)
	}
	if got := mustGet(t, s3, "sku/0100"); got != "price-100" {
		t.Fatalf("older key lost: %q", got)
	}
}

func TestRecoveryAfterCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s, err := kvstore.OpenWith(kvstore.Options{
		Dir:             dir,
		Sync:            kvstore.SyncAlways,
		CheckpointEvery: time.Hour, // only explicit checkpoints in this test
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		mustPut(t, s, fmt.Sprintf("k/%04d", i), fmt.Sprintf("v%d", i))
	}
	for i := 0; i < 100; i++ {
		if err := s.Delete([]byte(fmt.Sprintf("k/%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	st := s.Stats()
	if st.Checkpoints != 1 {
		t.Fatalf("checkpoints = %d", st.Checkpoints)
	}
	if st.Keys != 400 {
		t.Fatalf("keys = %d, want 400", st.Keys)
	}
	if st.WALBytes != 0 {
		t.Fatalf("wal not truncated after checkpoint: %d bytes", st.WALBytes)
	}
	// Post-checkpoint writes go to the fresh log.
	mustPut(t, s, "k/9999", "post")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Only one checkpoint and one log should remain on disk.
	snaps, _ := filepath.Glob(filepath.Join(dir, "snap-*.dat"))
	if len(snaps) != 1 {
		t.Fatalf("expected 1 checkpoint file, found %v", snaps)
	}
	wals, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if len(wals) != 1 {
		t.Fatalf("expected 1 wal file, found %v", wals)
	}

	s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if got := s2.Stats().Keys; got != 401 {
		t.Fatalf("recovered keys = %d, want 401", got)
	}
	for i := 0; i < 100; i++ {
		if ok, _ := s2.Has([]byte(fmt.Sprintf("k/%04d", i))); ok {
			t.Fatalf("deleted key k/%04d resurrected by checkpoint recovery", i)
		}
	}
	for i := 100; i < 500; i++ {
		if got := mustGet(t, s2, fmt.Sprintf("k/%04d", i)); got != fmt.Sprintf("v%d", i) {
			t.Fatalf("k/%04d = %q", i, got)
		}
	}
	if got := mustGet(t, s2, "k/9999"); got != "post" {
		t.Fatalf("post-checkpoint write lost: %q", got)
	}
}

func TestTTLLazyAndBackgroundExpiry(t *testing.T) {
	s, err := kvstore.OpenWith(kvstore.Options{ExpireEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutTTL([]byte("promo/1"), []byte("2 for 3"), 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "promo/perm", "always")
	if got := mustGet(t, s, "promo/1"); got != "2 for 3" {
		t.Fatalf("promo/1 = %q", got)
	}

	// Lazy expiry: the key stops being served the moment its deadline passes,
	// with no dependence on the sweeper having run.
	time.Sleep(80 * time.Millisecond)
	if _, err := s.Get([]byte("promo/1")); !errors.Is(err, kvstore.ErrNotFound) {
		t.Fatalf("expired key still served: %v", err)
	}
	if ok, _ := s.Has([]byte("promo/1")); ok {
		t.Fatal("Has true for expired key")
	}
	n := 0
	it := s.Scan([]byte("promo/"))
	for it.Next() {
		n++
	}
	it.Close()
	if n != 1 {
		t.Fatalf("expired key visible to scan: %d live keys", n)
	}

	// Background expiry: the sweeper reclaims it so the key count falls.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Stats().Keys == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := s.Stats()
	if st.Keys != 1 {
		t.Fatalf("background sweep did not reclaim: keys = %d", st.Keys)
	}
	if st.Expired == 0 {
		t.Fatal("expired counter not advanced")
	}
	if got := mustGet(t, s, "promo/perm"); got != "always" {
		t.Fatalf("sweeper removed a non-expiring key: %q", got)
	}
}

func TestTTLSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, ExpireEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutTTL([]byte("promo/short"), []byte("x"), 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTTL([]byte("promo/long"), []byte("y"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)

	s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir, ExpireEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Get([]byte("promo/short")); !errors.Is(err, kvstore.ErrNotFound) {
		t.Fatalf("expiry not persisted across restart: %v", err)
	}
	if got := mustGet(t, s2, "promo/long"); got != "y" {
		t.Fatalf("promo/long = %q", got)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	s := openTemp(t)
	mustPut(t, s, "sku/a", "1.00")
	mustPut(t, s, "sku/b", "2.00")

	snap := s.Snapshot()
	defer snap.Close()

	// Mutate everything the snapshot covers.
	mustPut(t, s, "sku/a", "1.50")
	if err := s.Delete([]byte("sku/b")); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "sku/c", "3.00")

	v, err := snap.Get([]byte("sku/a"))
	if err != nil || string(v) != "1.00" {
		t.Fatalf("snapshot Get sku/a = %q, %v; want 1.00", v, err)
	}
	if ok, _ := snap.Has([]byte("sku/b")); !ok {
		t.Fatal("snapshot lost a key deleted after it was taken")
	}
	if ok, _ := snap.Has([]byte("sku/c")); ok {
		t.Fatal("snapshot saw a key written after it was taken")
	}

	var got []string
	it := snap.Scan([]byte("sku/"))
	for it.Next() {
		got = append(got, string(it.Key())+"="+string(it.Value()))
	}
	it.Close()
	want := []string{"sku/a=1.00", "sku/b=2.00"}
	if !equalStrings(got, want) {
		t.Fatalf("snapshot scan = %v, want %v", got, want)
	}

	// The live store shows the new reality.
	if got := mustGet(t, s, "sku/a"); got != "1.50" {
		t.Fatalf("live sku/a = %q", got)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Get([]byte("sku/a")); !errors.Is(err, kvstore.ErrClosed) {
		t.Fatalf("closed snapshot still readable: %v", err)
	}
}

func TestPutIfAbsentIsExclusive(t *testing.T) {
	s := openTemp(t)

	ok, err := s.PutIfAbsent([]byte("claim"), []byte("first"), 0)
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	ok, err = s.PutIfAbsent([]byte("claim"), []byte("second"), 0)
	if err != nil || ok {
		t.Fatalf("second claim = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "claim"); got != "first" {
		t.Fatalf("claim = %q, want first", got)
	}

	// An expired claim is claimable again.
	if _, err := s.PutIfAbsent([]byte("short"), []byte("a"), 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	ok, err = s.PutIfAbsent([]byte("short"), []byte("b"), 0)
	if err != nil || !ok {
		t.Fatalf("reclaim after ttl = %v, %v", ok, err)
	}

	// Exactly one winner under contention.
	const racers = 64
	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := s.PutIfAbsent([]byte("race"), []byte(fmt.Sprint(i)), 0)
			if err != nil {
				t.Errorf("PutIfAbsent: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("PutIfAbsent winners = %d, want 1", wins)
	}
}

func TestConcurrentWritersAndScanners(t *testing.T) {
	s, err := kvstore.OpenWith(kvstore.Options{
		Sync:            kvstore.SyncNever,
		ExpireEvery:     10 * time.Millisecond,
		CheckpointEvery: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const writers, scanners, rounds = 8, 4, 150
	var wg, writeWG sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func(w int) {
			defer writeWG.Done()
			for r := 0; r < rounds; r++ {
				k := fmt.Sprintf("sku/%02d/%04d", w, r)
				if err := s.Put([]byte(k), []byte(fmt.Sprintf("p-%d-%d", w, r))); err != nil {
					t.Errorf("put: %v", err)
					return
				}
				if r%7 == 0 {
					b := s.NewBatch()
					b.Put([]byte(fmt.Sprintf("agg/%02d", w)), []byte(fmt.Sprint(r)))
					b.PutTTL([]byte(fmt.Sprintf("tmp/%02d/%04d", w, r)), []byte("t"), 15*time.Millisecond)
					if err := b.Write(); err != nil {
						t.Errorf("batch: %v", err)
						return
					}
				}
				if r%23 == 0 {
					if err := s.Delete([]byte(fmt.Sprintf("sku/%02d/%04d", w, r/2))); err != nil {
						t.Errorf("delete: %v", err)
						return
					}
				}
			}
		}(w)
	}

	for c := 0; c < scanners; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				it := s.Scan([]byte("sku/"))
				var last string
				for it.Next() {
					k := string(it.Key())
					if last != "" && k <= last {
						t.Errorf("scan out of order: %q after %q", k, last)
						break
					}
					last = k
					_ = it.Value()
				}
				it.Close()

				snap := s.Snapshot()
				a, _ := snap.Get([]byte("agg/00"))
				b, _ := snap.Get([]byte("agg/00"))
				if !bytes.Equal(a, b) {
					t.Errorf("snapshot not stable: %q vs %q", a, b)
				}
				snap.Close()
				_ = s.Stats()
			}
		}()
	}

	// Let the writers finish, then stop the scanners.
	writeWG.Wait()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Sanity: the last write of each writer is readable.
	for w := 0; w < writers; w++ {
		k := fmt.Sprintf("sku/%02d/%04d", w, rounds-1)
		if got := mustGet(t, s, k); got != fmt.Sprintf("p-%d-%d", w, rounds-1) {
			t.Fatalf("%s = %q", k, got)
		}
	}
}

func TestCheckpointUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncNever, CheckpointEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.Put([]byte(fmt.Sprintf("live/%06d", i)), []byte("v")); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 5; i++ {
		if err := s.Checkpoint(); err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	before := s.Stats().Keys
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if after := s2.Stats().Keys; after != before {
		t.Fatalf("keys after recovery = %d, want %d", after, before)
	}
}

func TestStatsAndMetrics(t *testing.T) {
	reg := obs.NewRegistry("service", "kvstore-test")
	s, err := kvstore.OpenWith(kvstore.Options{Registry: reg, MetricNamespace: "edgekv"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 10; i++ {
		mustPut(t, s, fmt.Sprintf("k%02d", i), "v")
	}
	if err := s.Delete([]byte("k00")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get([]byte("missing")); !errors.Is(err, kvstore.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	st := s.Stats()
	if st.Keys != 9 {
		t.Fatalf("keys = %d, want 9", st.Keys)
	}
	if st.Puts != 10 || st.Deletes != 1 {
		t.Fatalf("puts/deletes = %d/%d", st.Puts, st.Deletes)
	}
	if st.Misses != 1 {
		t.Fatalf("misses = %d", st.Misses)
	}
	if st.Checkpoints != 1 || st.SnapshotSequence == 0 {
		t.Fatalf("checkpoint stats = %+v", st)
	}
	if st.Sync != kvstore.SyncAlways || st.Sync.String() != "always" {
		t.Fatalf("sync = %v", st.Sync)
	}
	if st.Dir == "" {
		t.Fatal("empty dir in stats")
	}

	var buf bytes.Buffer
	reg.WriteText(&buf)
	out := buf.String()
	for _, want := range []string{
		"edgekv_keys{service=\"kvstore-test\"} 9",
		"edgekv_compactions_total{service=\"kvstore-test\"} 1",
		"edgekv_snapshot_age_seconds",
		"edgekv_wal_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, out)
		}
	}
}

func TestTempDirRemovedOnClose(t *testing.T) {
	s, err := kvstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	dir := s.Dir()
	if dir == "" {
		t.Fatal("no dir")
	}
	mustPut(t, s, "k", "v")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir missing while open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir survived Close: %v", err)
	}
	// Everything is closed-safe rather than panicking.
	if err := s.Put([]byte("k"), []byte("v")); !errors.Is(err, kvstore.ErrClosed) {
		t.Fatalf("put after close: %v", err)
	}
	if _, err := s.Get([]byte("k")); !errors.Is(err, kvstore.ErrClosed) {
		t.Fatalf("get after close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestSyncPolicies(t *testing.T) {
	for _, p := range []kvstore.SyncPolicy{kvstore.SyncAlways, kvstore.SyncEvery, kvstore.SyncNever} {
		dir := t.TempDir()
		s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: p, SyncEvery: 5 * time.Millisecond})
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
		for i := 0; i < 20; i++ {
			mustPut(t, s, fmt.Sprintf("k%02d", i), "v")
		}
		if err := s.Close(); err != nil {
			t.Fatalf("%v close: %v", p, err)
		}
		// Close flushes regardless of policy, so a clean shutdown always
		// recovers everything.
		s2, err := kvstore.OpenWith(kvstore.Options{Dir: dir})
		if err != nil {
			t.Fatalf("%v reopen: %v", p, err)
		}
		if got := s2.Stats().Keys; got != 20 {
			t.Fatalf("%v: keys after clean restart = %d, want 20", p, got)
		}
		s2.Close()
	}
}

func activeWAL(t *testing.T, dir string) string {
	t.Helper()
	wals, err := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if err != nil || len(wals) == 0 {
		t.Fatalf("no wal in %s: %v", dir, err)
	}
	sort.Strings(wals)
	return wals[len(wals)-1]
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
