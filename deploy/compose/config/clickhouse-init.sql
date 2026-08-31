-- USSLP — ClickHouse bootstrap for the prod-like compose profile.
--
-- IMPORTANT: nothing in the Go tree connects to ClickHouse today.
-- platform/internal/analytics and platform/cmd/analytics-service are both real
-- and both run, but the service keeps its rows in its own column-oriented store
-- (platform/internal/analytics/columnar) rather than in ClickHouse. This script
-- provisions the tables the documented production port targets so that the
-- adapter has a schema to be written against and the topology can be measured.
--
-- The column sets are transcribed from the real canonical payloads, not
-- invented: canon.Telemetry and canon.LabelDelivered in
-- platform/pkg/canon/events.go. Anything not on those structs is not here.

CREATE DATABASE IF NOT EXISTS usslp;

-- ---------------------------------------------------------------------------
-- label_telemetry — canon.Telemetry, arriving on the `label-telemetry` stream
-- (2048 partitions, 3-day Kafka retention; ClickHouse is where it lives after
-- that).
--
-- ORDER BY (store_id, label_id, reported_at) because every analytical question
-- asked of this table is scoped to a store first: battery runway for a store,
-- mesh quality for a store, a single label's history within a store. Partition
-- by month so that a retention drop is a partition drop rather than a mutation.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usslp.label_telemetry
(
    label_id        LowCardinality(String),
    store_id        LowCardinality(String),
    sec_id          LowCardinality(String),
    reported_at     DateTime64(3, 'UTC'),
    battery_mv      UInt16,
    battery_pct     UInt8,
    temperature_c   Float32,
    rssi            Int16,
    lqi             UInt8,
    mesh_hops       UInt8,
    parent_id       String,
    firmware_version LowCardinality(String),
    refresh_count   UInt64,
    nfc_tap_count   UInt64,
    uptime_seconds  UInt64,
    tamper          UInt8,
    -- Ingest bookkeeping, not part of the canonical payload.
    ingested_at     DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(reported_at)
ORDER BY (store_id, label_id, reported_at)
TTL toDateTime(reported_at) + INTERVAL 400 DAY
SETTINGS index_granularity = 8192;

-- ---------------------------------------------------------------------------
-- label_delivery — canon.LabelDelivered, arriving on the `label-delivery`
-- stream. This is the table the SLO is computed from: latency_ms is measured
-- from the envelope's RecordedAt to the moment the pixels settled, which is the
-- only number a retailer can verify by looking at a shelf
-- (INTERFACE-CONTRACTS section 4).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usslp.label_delivery
(
    label_id      LowCardinality(String),
    store_id      LowCardinality(String),
    sec_id        LowCardinality(String),
    sequence      Int64,
    delivered_at  DateTime64(3, 'UTC'),
    latency_ms    Int64,
    mesh_hops     UInt8,
    refresh_ms    UInt32,
    partial_refresh UInt8,
    ingested_at   DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(delivered_at)
ORDER BY (store_id, delivered_at, label_id)
TTL toDateTime(delivered_at) + INTERVAL 400 DAY
SETTINGS index_granularity = 8192;

-- ---------------------------------------------------------------------------
-- A per-store, per-hour rollup of the 3-second price SLO.
--
-- quantilesTDigestState keeps a mergeable sketch rather than a pre-computed
-- percentile, so the same rollup answers "p99 for this store this hour" and
-- "p99 for this chain this quarter" without a second table.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usslp.price_slo_hourly
(
    store_id     LowCardinality(String),
    hour         DateTime('UTC'),
    deliveries   AggregateFunction(count, UInt64),
    within_3s    AggregateFunction(sum, UInt64),
    latency_ms   AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), Int64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (store_id, hour);

CREATE MATERIALIZED VIEW IF NOT EXISTS usslp.price_slo_hourly_mv
TO usslp.price_slo_hourly
AS
SELECT
    store_id,
    toStartOfHour(delivered_at) AS hour,
    countState()                             AS deliveries,
    sumState(toUInt64(latency_ms <= 3000))   AS within_3s,
    quantilesTDigestState(0.5, 0.95, 0.99)(latency_ms) AS latency_ms
FROM usslp.label_delivery
GROUP BY store_id, hour;
