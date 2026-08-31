package kvstore_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func benchStore(b *testing.B) *kvstore.Store {
	b.Helper()
	s, err := kvstore.OpenWith(kvstore.Options{
		Sync:            kvstore.SyncNever,
		CheckpointEvery: time.Hour,
		ExpireEvery:     time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func fill(b *testing.B, s *kvstore.Store, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		if err := s.Put([]byte(fmt.Sprintf("sku/%06d", i)), []byte("1099")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGet is the point read path: one key, short version chain.
func BenchmarkGet(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 10000)
	k := []byte("sku/005000")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get(k); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetParallel is the point read path under reader concurrency.
func BenchmarkGetParallel(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		k := []byte("sku/005000")
		for pb.Next() {
			if _, err := s.Get(k); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSnapshotGetDeepChain reads one key through a snapshot pinned far
// behind the write frontier, so the version chain the read must walk is long.
// This is the shape a compliance export has: an old pinned sequence and hot
// keys that have moved on. It is the case that would suffer most if the read
// path held a lock across the chain walk.
func BenchmarkSnapshotGetDeepChain(b *testing.B) {
	s := benchStore(b)
	k := []byte("agg/00")
	if err := s.Put(k, []byte("start")); err != nil {
		b.Fatal(err)
	}
	snap := s.Snapshot()
	defer snap.Close()
	for i := 0; i < 5000; i++ {
		if err := s.Put(k, []byte(fmt.Sprint(i))); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := snap.Get(k); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutHot rewrites one key over and over: the case where version
// trimming actually has work to do on every write.
func BenchmarkPutHot(b *testing.B) {
	s := benchStore(b)
	k := []byte("sku/000001")
	v := []byte("1099")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Put(k, v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutFresh writes a new key every time: index insertion, no trimming.
func BenchmarkPutFresh(b *testing.B) {
	s := benchStore(b)
	v := []byte("1099")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Put([]byte(fmt.Sprintf("sku/%08d", i)), v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutDuringScan measures price-write throughput while a full-catalogue
// scan and a pinned snapshot are in flight. It is the property the version
// chain design exists to provide: the scan must not slow the writes down.
func BenchmarkPutDuringScan(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 20000)

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			snap := s.Snapshot()
			it := snap.Scan([]byte("sku/"))
			for it.Next() {
				_ = it.Value()
			}
			it.Close()
			snap.Close()
		}
	}()

	k := []byte("sku/000001")
	v := []byte("1099")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Put(k, v); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	stop.Store(true)
	<-done
}
