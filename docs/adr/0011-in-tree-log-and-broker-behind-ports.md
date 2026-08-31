# 0011 — An in-tree event log and MQTT broker behind ports, with Kafka and EMQX as the production adapters

**Status:** Accepted for the ports and the in-tree implementations.
**The production adapters do not exist.** See *Consequences*, which is the part
of this record that matters.

---

## Context

[0003](0003-edge-first-architecture.md) requires a store to keep pricing
correctly with nothing upstream running. That means the store needs its own
durable log, its own broker and its own key/value store, on an appliance with no
container runtime.

[0010](0010-go-and-hexagonal-architecture.md) requires the platform to run in one
process with no infrastructure, so that the end-to-end suite is a set of claims
rather than a set of mocks.

The cloud tier, meanwhile, is sized for 52,000 price updates per second across
1,024 partitions and a consumer group of two hundred nodes. That is Kafka's
workload, not an embedded log's.

## Decision

**Two narrow ports, two in-tree implementations that are complete rather than
stubbed, and named production adapters behind the same ports.**

| Port | In-tree implementation | Documented production adapter |
|---|---|---|
| `platform/pkg/eventbus.Bus` | `platform/pkg/eventlog` — file-backed partitioned log with consumer groups, rebalancing, committed offsets, replay, retention, compaction, dead-lettering | Apache Kafka (MSK / Confluent) |
| `platform/pkg/msgbus.Client` | `platform/pkg/mqtt` — complete MQTT 3.1.1 broker and client (protocol level 4), retained messages, QoS 0/1/2, sessions surviving reconnect, last wills | EMQX |
| (no port; direct dependency) | `platform/pkg/kvstore` — WAL + skip-list MVCC index + checkpoints | PostgreSQL / ClickHouse / Redis |

The ports are deliberately narrower than the protocols behind them. `msgbus`
exposes publish, subscribe, QoS 0/1/2, retained and last-will — and nothing else
— so that `pkg/mqtt` and EMQX are genuinely interchangeable rather than nominally
so.

The in-tree implementations are held to production semantics on purpose:

- `eventlog` frames records with CRC-32C, writes a batch with a single `write(2)`
  and, under the default `SyncAlways`, fsyncs before `Publish` returns. A lost
  price change is a compliance incident, so the default trades throughput for
  durability. A process that dies mid-append leaves a partial record; the next
  `Open` fails its CRC or length check, logs it and truncates the segment back to
  the last intact record.
- `eventlog`'s consumer-group semantics — partitions split between members,
  rebalance on join and leave, offsets surviving restart — are the ones a service
  will meet on MSK, so no service code changes when the deployment grows a real
  broker.
- `kvstore`'s durability is stated precisely because "durable" is a compliance
  word: `SyncAlways` for the pricing queue on a gateway, `SyncEvery` for
  telemetry and read models, `SyncNever` only for caches and tests.

## Consequences

This is the section to read before drawing any conclusion about how this platform
would deploy.

**The Kafka adapter does not exist.** The port is real; the second
implementation behind it is not written. There is no
`platform/pkg/eventbus/kafka` directory and no file anywhere in the tree carries
a `//go:build kafka` constraint.

The package comment used to assert the opposite — "the adapter lives in
`pkg/eventbus/kafka` and is built with the `kafka` build tag" — which made a
documentation defect out of what is otherwise an honest gap. It now describes
what is there: one implementation (`pkg/eventlog`), a port, and a Kafka adapter
named as the production work rather than as existing code.

**No Go file connects to PostgreSQL, ClickHouse or Redis.** There is no
`database/sql` import, no driver, no client. The event store and every read model
are on `pkg/kvstore`. The prod-like compose profile and the Terraform provision
all three with the schemas, roles and parameters the documented ports expect, so
the adapters have something to be written against — but the adapters are the next
piece of work, not this one.

**`eventlog`'s consumer-group coordinator is in-process, and that has a visible
consequence today.** Members are `Run` calls on the same `*Log`, not network
peers, so two OS processes must not share one log directory. In the multi-process
compose profile each service therefore has its own log, and **the UIG's
`price-updates` records do not reach the Label Service's consumer.** The services
are wired over MQTT and HTTP, which is real; the cross-service event stream is
not.

`usslpd` exists partly to fix that: one `*eventlog.Log` handed to every
constructor makes the cross-service stream genuinely real, which is why the
single-process shape is described as the only one in which it currently works.

**One consequence of that gap is a shim in the composition root.**
`stack/promobridge.go` bridges `promotion-events` to the Label Service because
`label.Service.Start` subscribes only to `device-events`, `price-updates` and
`label-delivery` — the interface contract lists it as a consumer of
`promotion-events` and it is not one. That is wiring rather than business logic
and should be deleted when the service grows its own consumer.

**The MQTT tier is the one that fully honours the decision.** `pkg/mqtt` is a
complete MQTT 3.1.1 implementation and the services speak to it exactly as they
speak to EMQX. The prod-like profile exercises the same code against a real EMQX.
This is the port that has actually been proved by two implementations.

**The stream catalogue survives the substitution.** `canon.AllStreams()` is the
single source of truth, transcribed into the Helm chart, the compose topic job
and the MSK Terraform module, with `make verify-topics` failing CI if any of the
four has drifted by a single partition. Whatever ends up behind the port gets the
same topology.

**A single-store deployment does not need any of the production adapters**, which
is the half of the decision that is fully delivered. A store gateway running
`pkg/eventlog`, `pkg/mqtt` and `pkg/kvstore` on one appliance is a supported
production shape, not a development convenience.

**What is owned now.** A log, a broker, an LSM store, a metrics registry, a JWT
implementation and a WebSocket implementation are all code this platform
maintains, with none of the field history of the systems they stand in for. That
is the standing cost of [0010](0010-go-and-hexagonal-architecture.md), and this
record is where it comes due.

## Alternatives considered

**Kafka and EMQX from the start, with no in-tree implementations.** Would have
avoided writing a log and a broker. Rejected on the appliance: neither runs on a
2 GB fanless box in a stock room in a store that has lost its WAN, and
[0003](0003-edge-first-architecture.md) requires that box to keep pricing
correctly on its own.

**An in-tree implementation for the edge only, with the cloud on Kafka from day
one.** The right long-term shape, and it is what the ports are for. Not what was
built: the cloud tier runs on `eventlog` today, and the Kafka adapter is
unwritten. The distinction between "the port is designed for it" and "the adapter
exists" is exactly what this record is here to keep honest.

**Embedded NATS or an embedded Kafka-compatible broker (Redpanda).** Would have
given a real broker on the appliance without writing one. Rejected on the
dependency rule in [0010](0010-go-and-hexagonal-architecture.md), and — for
Redpanda — on the memory and CPU footprint of a fanless store gateway.

**SQLite instead of `pkg/kvstore`.** Would have been a smaller thing to own and
has a field history measured in decades. Rejected on cgo: `CGO_ENABLED=0` is what
makes the arm64 cross-compile trivial for a fleet of gateways. A pure-Go SQLite
was not considered mature enough for a store where a corrupted write-ahead log
means a technician in a van.
