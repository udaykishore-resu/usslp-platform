// Package eventlog is an embedded, file-backed, partitioned, append-only log
// that implements the platform's eventbus.Bus port.
//
// It exists so that the whole of USSLP — Label Service, UIG, OTA, analytics —
// can be developed, tested and (for a single-store deployment) operated without
// a Kafka cluster, while still exercising the exact semantics production
// depends on: partitioning by key, consumer groups with rebalancing, committed
// offsets, replay from an arbitrary offset, retention, compaction, and
// dead-lettering. `make dev` runs this; the test suite runs against this; a
// store that lost its WAN link runs against this.
//
// # Durability model
//
// Records are framed with a CRC32C and appended to segment files. A batch is
// written with a single write(2) and, under the default SyncAlways policy,
// fsync'd before Publish returns — a lost price change is a compliance
// incident, so the default trades throughput for durability. See SyncPolicy for
// the weaker policies and exactly what each one gives up.
//
// A process that dies mid-append leaves a partial record at the tail of a
// segment. That is not corruption of the log, it is the expected shape of a
// crash: on the next Open the tail record fails its CRC or its length check,
// is logged, and the segment is truncated back to the last intact record. The
// log is always readable.
//
// # Ordering
//
// Ordering is guaranteed per key and nowhere else. A record's partition is
// FNV-1a(key) % partitions, so all records for one key land in one partition
// and are read in append order by exactly one member of a consumer group.
// Records with different keys may sit in different partitions, which are
// consumed concurrently by different members, so no ordering exists between
// them. Records with an empty key are round-robined across partitions and
// therefore have no ordering relationship even with each other. USSLP keys
// price traffic by "store:sku" precisely because that is the granularity at
// which order actually matters: two prices for the same shelf label must not
// be applied out of sequence, while two prices for different labels are
// genuinely independent.
//
// # Delivery
//
// Delivery is at-least-once, as on Kafka. An offset is committed only after
// the handler returns nil (or after the record has been dead-lettered), and
// commits are flushed to disk asynchronously, so a crash re-delivers a bounded
// suffix. Handlers must be idempotent; canon.Envelope carries the idempotency
// key that makes that practical.
//
// # Scope
//
// The consumer-group coordinator is in-process: members are Run calls on the
// same *Log, not network peers. That is the whole point — this is an embedded
// log — but the semantics visible to a service (partitions split between
// members, rebalance on join and leave, offsets survive restart) are the ones
// it will meet on MSK, so no service code changes when the deployment grows a
// real broker.
package eventlog
