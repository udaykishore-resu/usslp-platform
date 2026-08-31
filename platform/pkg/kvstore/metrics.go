package kvstore

import "github.com/usslp/usslp/platform/pkg/obs"

// storeMetrics is the optional Prometheus surface of a store.
//
// These four numbers are what an operator actually needs when a store gateway
// starts misbehaving: how much is in the store, how far behind the last
// checkpoint the log has drifted (recovery time), how stale the checkpoint is,
// and whether some forgotten iterator is pinning version history and quietly
// growing the heap.
type storeMetrics struct {
	keys        *obs.Gauge
	walBytes    *obs.Gauge
	snapshotAge *obs.Gauge
	activeSnaps *obs.Gauge
	checkpoints *obs.Counter
}

func newStoreMetrics(r *obs.Registry, ns string) *storeMetrics {
	return &storeMetrics{
		keys: r.Gauge(ns+"_keys",
			"Number of live keys in the embedded key/value store.").With(),
		walBytes: r.Gauge(ns+"_wal_bytes",
			"Size in bytes of the active write-ahead log; predicts recovery time.").With(),
		snapshotAge: r.Gauge(ns+"_snapshot_age_seconds",
			"Age in seconds of the most recent on-disk checkpoint.").With(),
		activeSnaps: r.Gauge(ns+"_active_snapshots",
			"Open snapshots and iterators pinning MVCC version history.").With(),
		checkpoints: r.Counter(ns+"_compactions_total",
			"Completed checkpoint (compaction) runs.").With(),
	}
}
