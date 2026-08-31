# 0007 — Per-key partition ordering on `store:sku`, never global ordering

**Status:** Accepted

---

## Context

Two price changes for the same product in the same store must apply in the order
they were issued. If head office marks an item down at 09:00:01 and corrects the
markdown at 09:00:02, the shelf must end up showing the correction. Applying them
out of order leaves a customer-facing price that nobody authorised, which is a
weights-and-measures exposure rather than a data-quality one.

Two price changes for *different* products, or for the same product in different
stores, have no relationship at all. Nothing anywhere in the platform depends on
their relative order.

The capacity model the stream catalogue is sized against is 52,000 price updates
per second at peak (`platform/pkg/canon/topics.go`). A total order over that is a
single-writer bottleneck by construction.

## Decision

**Ordering is guaranteed per partition key and nowhere else. `price-updates` is
keyed `store:sku`.**

`canon.AllStreams()` fixes the key and the partition count for every stream, and
the keys are chosen at the granularity where order actually matters:

| Stream | Key | Partitions | Why that key |
|---|---|---|---|
| `price-updates` | `store:sku` | 1024 | Two prices for the same shelf label must not apply out of sequence; two prices for different labels are genuinely independent |
| `label-telemetry` | `label_id` | 2048 | A consumer computing a per-label anomaly baseline needs its partition ordered by label, not by whichever controller relayed it |
| `label-delivery` | `label_id` | 512 | Delivery confirmations for one label are a sequence |
| `device-events` | `device_id` | 512 | A device's lifecycle is a state machine |
| `ota-commands` | `device_id` | 128 | Command ordering per device |
| `promotion-events` | `tenant:promo` | 128 | A promotion's lifecycle |
| `inventory-sync` | `store:sku` | 256 | Same granularity as pricing |
| `pos-integration` | `tenant:store` | 256 | Raw ingress, ordered per source |
| `audit-log` | `tenant` | 64 | Compliance reads per tenant |
| `label-state` | `label_id` | 512, **compacted** | Latest state per label, for read-model rebuilds |
| `dead-letter` | original key | 32 | Preserves the failing record's key for triage |

`pkg/eventlog` assigns a partition as `FNV-1a(key) % partitions`, so all records
for one key land in one partition and are read in append order by exactly one
member of a consumer group. Records with an empty key are round-robined and
therefore have no ordering relationship even with each other.

`INTERFACE-CONTRACTS` §2 states the rule normatively: **nothing in the platform
may assume global ordering.**

The Label Service's `ConsumerConcurrency` stays at 1 for the price path
(`platform/internal/label/service.go`), because in-flight concurrency within a
partition would discard the very guarantee the partitioning bought.

### Why 1,024 partitions on `price-updates`

Partitions bound consumer parallelism: a consumer group can have at most one
member per partition. 1,024 is sized so that 52,000 updates per second spread
across a consumer group of two hundred nodes with room to grow, and so that the
group can be rebalanced without repartitioning — which `pkg/eventlog` refuses
outright (`ErrPartitionsChanged`) and which Kafka permits but which silently
breaks key affinity for existing keys.

Partition count is not a tuning knob. It is transcribed into the Helm chart, the
compose topic job and the MSK Terraform module, and `make verify-topics` fails CI
if any of the four transcriptions has drifted by a single partition.

## Consequences

**52,000 updates a second is a parallel workload with a hard ordering guarantee
where it matters, and no coordinator.** No lock, no sequencer, no leader.

**A hot key is a hot partition and there is no escape from it.** A single SKU
being repriced thousands of times a second in one store serialises onto one
partition and one consumer. This is the correct behaviour — the guarantee
requires it — and it means throughput per key is bounded by one consumer's
service time. The design point is that no real store reprices one SKU at that
rate; a workload that did would need the key widened, which would mean giving up
the ordering guarantee for that stream.

**A single-process deployment cannot afford the real partition counts.** The full
catalogue is 5,472 partitions, and `pkg/eventlog` materialises each as a
directory with a segment file and a sparse index: 5,472 directories, at least
10,944 files, and 5,472 offset entries to plan, commit and poll every read cycle.
`stack.devStreams` clamps every count to 4 for `usslpd`. That is safe precisely
*because* the guarantee is per key rather than per partition count: `store:sku`
lands two changes to the same product on the same partition whether there are
four partitions or a thousand. `TestDevStreamsPreserveTheCatalogue` asserts the
names, retentions and compaction flags survive the clamp, and
`TestCatalogueTotalMatchesTheCommentAndTheClusterSizing` pins the 5,472 so that
the figure quoted here and in the MSK sizing cannot drift away from
`canon.AllStreams()` again.

**Cross-stream ordering does not exist and consumers must not assume it.** A
`promotion-events` record and a `price-updates` record that are causally related
carry `CausationID` and `CorrelationID` on the envelope; that is the mechanism
for reasoning about their relationship, not their arrival order.

**Global ordering questions have to be answered elsewhere.** The event *store*
does have a global monotonic position (`pkg/eventstore`), so a projection can
rebuild deterministically. That order is per aggregate store, not across the
event bus, and the two must not be confused.

## Alternatives considered

**A single partition, or a global sequencer.** Gives total order. Rejected on
throughput: one writer at 52,000 records per second with `acks=all` and an
fsync-before-acknowledge durability policy is not a design, and the ordering it
buys is ordering nothing needs.

**Keying by `sku` alone.** Would order a SKU's changes across every store in the
estate. Rejected because it is a strictly stronger guarantee than anything
requires, and it collapses the parallelism of a 100,000-store chain onto the
cardinality of its product catalogue — which for a grocer is smaller than its
store count.

**Keying by `store` alone.** Would order everything within a store, which is
appealingly simple. Rejected because a store-wide overnight reprice — 40,000
labels — would then be strictly serial on one consumer, and the fan-out is the
workload the design point is built around.

**Keying by `label_id` on `price-updates`.** Rejected because a price change
arrives as a change to a SKU and is only resolved to labels *after* the durable
append; the key has to exist at ingress, before the placement directory has been
consulted. `label-delivery` and `label-state`, which are produced after
resolution, are keyed by label for exactly that reason.
