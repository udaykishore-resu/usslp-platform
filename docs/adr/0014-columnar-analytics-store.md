# 0014 — A columnar analytics store rather than a row store

**Status:** Accepted

---

## Context

Four streams feed analytics: `label-telemetry`, `label-delivery`,
`price-updates` and `promotion-events`. At the estate the platform is sized for,
the combined ingest reaches **167,000 rows per second**
(`platform/internal/analytics/columnar/encoding.go`).

The questions asked of that data are analytical, not transactional:

- p99 delivery latency by store for the last week
- units per day against price for this SKU
- which stores are furthest from the benchmark
- how much of the price path's error budget is left

Every one of those reads two or three columns out of fifteen and scans millions
of rows. That is the shape a column store is for and the shape a row store is
worst at: a row store must read all fifteen columns of every row it touches to
answer a question about two of them.

There is also a deployment constraint. A tenant running its own ClickHouse should
point the ingest at it. A tenant without one still has to get the reports, and
the platform cannot make a warehouse a prerequisite for a store to be sold a
shelf-label system.

## Decision

**`platform/internal/analytics/columnar` — a column-oriented, block-structured
time-series store, described in its own package comment as the ClickHouse
stand-in the platform ships when a tenant is not running one.**

Three properties make it fast, and they are the reasons a column store was chosen
rather than incidental optimisations:

**Column-major layout.** A query reads only the columns it names. A latency
percentile touches two columns of a fifteen-column table and therefore reads
about a seventh of the bytes.

**Per-column compression chosen for what the column holds:**

| Column shape | Encoding |
|---|---|
| Monotonic timestamps, small-range integers | Delta + zigzag varint |
| Floats | XOR against the previous value |
| Low-cardinality strings (store ids, event types, firmware versions) | Per-block dictionary |

Those three cover the schema, because a telemetry or delivery row is mostly a
timestamp, a handful of small integers, a couple of floats and a set of
identifiers drawn from a small domain.

**A per-block minimum and maximum for every column**, so a query with a time
range or an equality filter skips whole blocks without decompressing them. On a
week-scoped query over a year of data that is roughly a fiftyfold reduction in
work before a single row is decoded.

### Storage layout and tiering

```
<dir>/<table>/<tier>/seg-<nanos>-<seq>.usc
```

One file per flushed batch of blocks, named by the earliest timestamp it holds.
**Files are immutable once written: no compaction, no merge, no rewrite.** That is
the simplification a store fed by append-only event streams can make, and it is
what lets retention be "delete the files whose newest row is older than the
policy" — an `unlink` rather than a rewrite of a terabyte.

Hot, warm and cold are directories, and moving a segment between them is a
`rename`. The production intent is warm on cheaper block storage and cold in
object storage; the store's contract is only that a query reads every tier the
policy says still holds data, so a deployment that maps all three to one disk is
correct and merely not cheap.

### Metrics it exports

`usslp_analytics_rows_ingested_total`, `usslp_analytics_rows_dropped_total`,
`usslp_analytics_blocks_total`, `usslp_analytics_compression_ratio`,
`usslp_analytics_query_seconds`, `usslp_analytics_retention_total`. The
compression ratio being a live gauge rather than a documented claim is the point:
the measured ratios are in the package's own tests, and drift is visible.

## Consequences

**A tenant with no warehouse still gets the reports**, which is what makes the
platform sellable to a mid-size grocer.

**The SLO read model is queryable from the same store**, so "99% of price updates
reached the glass within 3 seconds, per store, for the last month" is one query
over two columns rather than a separate aggregation pipeline.

**Immutability removes an entire class of failure**, and costs the things
immutability always costs: no updates, no deletes below segment granularity, and
retention resolution bounded by segment size. For an append-only telemetry
workload that is free; for anything requiring a correction it is not, and nothing
in this store is meant to require one.

**The service that runs it ships.** `platform/cmd/analytics-service/main.go`
exists, compiles, is constructed by `usslpd`, is `enabled: true` in the Helm
chart in every environment, and appears in the CI and release image matrices and
in the dev compose profile. Its IRSA role has been in the Terraform all along.

For a while it did not: four places — `deploy/README.md`, both image matrices and
the chart's `enabled` flag — went on describing the command directory as empty
long after the binary landed, and because the API Gateway was pointed at it
regardless, the visible symptom was
`usslp_gateway_breaker_state{upstream="analytics",state="open"}` sitting open
permanently. Nothing was broken except the documentation, which is the failure
mode worth naming: an open breaker that everybody has learned to expect is an
alert that has stopped working.

**This is not ClickHouse and does not claim to be.** No distributed query, no
materialised views, no join engine, no SQL. The queries it answers are the ones
`analytics/columnar/query.go` implements, and a tenant whose analysts want
arbitrary SQL should point the ingest at a real warehouse — which is the
documented port. That port, like the Kafka one, has no adapter written yet
([0011](0011-in-tree-log-and-broker-behind-ports.md)).

**Retention is per-file, so a single segment holding one very old row keeps the
whole segment.** Segment sizing therefore trades ingest efficiency against
retention precision, and the store exposes both knobs rather than picking for the
operator.

## Alternatives considered

**A row store — the same `pkg/kvstore` everything else uses.** Rejected on the
read amplification above: fifteen columns read to answer a question about two,
multiplied by millions of rows, multiplied by every dashboard refresh.

**ClickHouse as a hard dependency.** The right engine, and it is what the
prod-like compose profile and the Terraform provision. Rejected as a
*requirement* because it makes a warehouse a prerequisite for selling a
shelf-label system to a retailer who does not have one, and because
[0010](0010-go-and-hexagonal-architecture.md) requires the whole platform to run
with Go and nothing else.

**Prometheus as the analytics store.** It already holds the operational metrics
and its query language is good at exactly the percentile-over-time questions the
SLO needs. Rejected: Prometheus is a metrics store with a bounded retention and a
cardinality model that a per-label, per-SKU dimension would destroy. It answers
"is the platform healthy"; it cannot answer "units per day against price for this
SKU".

**Parquet files plus an embedded query engine.** Genuinely close to what was
built, with the advantage of an interchange format other tools can read.
Rejected on the dependency rule — every Go Parquet implementation is a large
third-party surface — but it is the alternative most worth revisiting, because
the interoperability is worth something the in-tree `.usc` format is not.
