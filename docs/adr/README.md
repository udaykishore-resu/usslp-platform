# Architecture decision records

Every record here documents a decision the USSLP codebase actually made. Each
one cites the code that implements it, and where the implementation falls short
of what the decision intended, the record says so in *Consequences* rather than
in a footnote.

The format is Status / Context / Decision / Consequences / Alternatives
considered. Records are immutable once accepted: a decision that is later
reversed gets a new record that supersedes the old one, and the old one is
marked superseded rather than edited.

Measurements quoted in these records were taken on a **2-core container with
the Tier 1 and Tier 2 radio simulated at 1:1 wall clock** (`edge/sim`,
`edge/mesh`, `edge/labelsim`). Everything above the radio is production code
doing production work. Where a figure is a model rather than a measurement, the
record says which.

---

## Index

| # | Decision | Status |
|---|---|---|
| [0001](0001-event-sourced-cqrs-for-the-price-path.md) | Event-driven CQRS with event sourcing for the price path, and not for telemetry | Accepted |
| [0002](0002-zigbee-primary-radio.md) | Zigbee 3.0 as the primary label radio, BLE secondary, WiFi backbone, LoRa rural | Accepted, partially implemented |
| [0003](0003-edge-first-architecture.md) | Edge-first: the cloud is an optimisation, not a dependency | Accepted |
| [0004](0004-end-to-end-price-attestation.md) | Cryptographic price attestation, verified end to end at the label | Accepted, supersedes the controller-terminated design |
| [0005](0005-ed25519-for-price-attestation.md) | Ed25519 rather than ECDSA P-256 for price attestation | Accepted |
| [0006](0006-crl-over-ocsp.md) | Signed CRLs rather than OCSP for a fleet that is mostly offline | Accepted |
| [0007](0007-per-key-partition-ordering.md) | Per-key partition ordering on `store:sku`, never global ordering | Accepted |
| [0008](0008-at-least-once-with-monotonic-sequence.md) | At-least-once delivery plus a per-label monotonic sequence, not exactly-once | Accepted |
| [0009](0009-hybrid-logical-clocks-for-reconciliation.md) | Hybrid logical clocks and a typed conflict policy, not wall-clock last-writer-wins | Accepted |
| [0010](0010-go-and-hexagonal-architecture.md) | Go and a ports-and-adapters structure for the backend | Accepted |
| [0011](0011-in-tree-log-and-broker-behind-ports.md) | An in-tree event log and MQTT broker behind ports, with Kafka and EMQX as the production adapters | Accepted, adapters not written |
| [0012](0012-multi-tenancy-and-the-tenant-boundary.md) | Multi-tenancy: three isolation models, and a tenant boundary that is constructed rather than checked | Accepted |
| [0013](0013-hand-written-ml-in-go.md) | Hand-written ML in Go rather than a Python inference service | Accepted, models trained only on synthetic data |
| [0014](0014-columnar-analytics-store.md) | A columnar analytics store rather than a row store | Accepted |
| [0015](0015-retained-messages-for-cold-start.md) | MQTT retained messages as the store's cold-start recovery mechanism | Accepted |
| [0016](0016-staged-ota-with-signed-manifest.md) | Staged OTA rollout with automatic rollback, signing the manifest rather than the image | Accepted |
| [0017](0017-eink-partial-refresh-policy.md) | The E-Ink partial-versus-full refresh policy and the ghosting constraint | Accepted |

---

## Status vocabulary

| Status | Meaning |
|---|---|
| Accepted | The decision is made and the code implements it. |
| Accepted, partially implemented | The decision is made; part of what it describes has no code. The record says which part. |
| Accepted, adapters not written | The seam exists and is exercised by one implementation; the second implementation the decision names does not exist yet. |
| Superseded by NNNN | A later record reverses or narrows this one. |

No record here is in *Proposed*. Everything listed is a decision the tree has
already committed to, which is the point of writing them after the fact rather
than before.

---

## What is not here

Decisions that were never actually made in this codebase are not recorded, even
where a blueprint or a README implies them. In particular:

- **A LoRa backhaul.** There is no LoRa code anywhere in the tree — see
  [0002](0002-zigbee-primary-radio.md), which records the radio decision that
  *was* made and states plainly which quarter of it is unimplemented.
- **A relational or warehouse persistence tier.** No Go file in the tree opens a
  PostgreSQL, ClickHouse or Redis connection. Persistence is
  `platform/pkg/kvstore` everywhere. That is recorded as a consequence in
  [0011](0011-in-tree-log-and-broker-behind-ports.md) rather than as a decision
  to use those systems.
- **An OTLP tracing pipeline.** `obs.NewRuntime` exports spans to the structured
  log. See `docs/architecture/observability.md`.
