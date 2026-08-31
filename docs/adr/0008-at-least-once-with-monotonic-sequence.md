# 0008 — At-least-once delivery plus a per-label monotonic sequence, not exactly-once

**Status:** Accepted

---

## Context

A price change crosses at least six delivery boundaries between a POS webhook and
a pixel: HTTP into the UIG, an append to the event stream, a consumer fetch, an
MQTT publish, a bridge into the store broker, and an 802.15.4 transmission across
a mesh. Each one can duplicate, and the last one can duplicate a great deal — the
coordinator retries after 500 ms and again after a second.

"Exactly once" is available in pieces on some of those hops. MQTT has QoS 2.
Kafka has idempotent producers and transactions. Neither composes across the set,
and neither is available at all on the radio hop, which is the one that
duplicates most.

The consequence of getting this wrong is asymmetric. A duplicate price update
costs a panel refresh — roughly a hundred times the energy of anything else the
label does, but recoverable. A *lost* price update is a shelf showing a price the
till does not agree with, which is a compliance incident.

## Decision

**Delivery is at-least-once everywhere. Every consumer is idempotent. The final
correctness guarantee is a per-label monotonic sequence enforced at the glass.**

`INTERFACE-CONTRACTS` §6 names four boundaries and the mechanism at each:

| Boundary | Mechanism | Owner |
|---|---|---|
| POS → UIG | `idem.Guard`, 24-hour window, key derived from adapter-supplied parts | UIG |
| UIG → event store | `Envelope.IdempotencyKey`, no-op re-append that returns the original event | `pkg/eventstore` |
| Command → aggregate | `expectedVersion` optimistic concurrency | Label Service |
| Cloud → label | per-label monotonic `Sequence` | label firmware / `edge/labelsim` |

The last one is the one that makes the rest safe. **A label discards any update
whose sequence is not strictly greater than the one it is displaying.** A
duplicated frame is a no-op; a reordered frame cannot roll a price backwards. It
is enforced in `firmware/src/app/usslp_seq.c` and mirrored in
`edge/labelsim`, and it is checked *before* the Ed25519 verification because a
duplicate is the common case and a verification is 13 ms of a coin cell's life.

The log's delivery semantics match: `pkg/eventlog` commits an offset only after
the handler returns nil or the record has been dead-lettered, and commits are
flushed asynchronously, so a crash re-delivers a bounded suffix. That is
deliberately the same shape as Kafka, so no service code changes when the
deployment grows a real broker
([0011](0011-in-tree-log-and-broker-behind-ports.md)).

QoS is chosen per topic rather than uniformly (`pkg/msgbus`):

| QoS | Used for | Reasoning |
|---|---|---|
| 0 | Telemetry, heartbeats, mesh status | High volume, individually worthless, reconstructable |
| 1 | **Every price update** | A duplicate is harmless because of the sequence rule; a loss is a compliance incident |
| 2 | OTA triggers only | A duplicate costs a battery-powered device an entire redundant firmware download |

## Consequences

**Measured.** `TestRedeliveredWebhookIsAppliedOnce`: ten deliveries of one
Shopify webhook produce sequence 1 → 2, **one** panel refresh, and zero stale
frames discarded at the label — the ingress guard catches all nine duplicates
before the price path spends anything on them.

**The idempotency boundaries are complete only for identical deliveries.** A
*distinct* POS delivery — a different webhook id — carrying the price already on
the glass passes every one of the four boundaries and refreshes the panel.
`TestDistinctWebhookWithAnUnchangedPriceStillRefreshes` records it: sequence
2 → 3, one panel refresh, for no change a shopper can see. A POS that republishes
its whole price book nightly would spend the fleet's battery budget on redraws of
numbers that did not move. This is a known gap, recorded rather than worked
around, and the fix belongs in the aggregate — a no-op when the resolved price
equals the displayed price — not in a wider dedupe window.

**A lost acknowledgement leaves the controller and the glass disagreeing.** The
radio model drops acks as readily as it drops updates. When an ack is lost, the
label has applied the price and the controller has not heard so; its
retransmission of the same sequence is answered `stale-sequence`, which the
coordinator records as a failed delivery. **The shelf is right and the
controller's `DisplayedSequence` is wrong** until the next update. That is
faithful hardware behaviour rather than a platform defect — reconciling it is
what the controller's delivery-failure reporting and the registry's health
derivation exist for — but it means `DisplayedSequence` is not a safe test for
"the shelf is showing a price". `stack.unpricedLabels` asks the panel instead.

**Some changes never arrive, and they are visible rather than lost.** Over 1,000
serial changes, 999 were delivered and 1 was abandoned by the radio after three
attempts and reported upstream as `label.update.failed`. Under 40/s sustained
load for 45 s, 1,796 of 1,799 accepted changes reached the glass and 3 did not,
for the same reason. That is a 99.6–99.9% delivery rate at the radio, reported as
a delivery failure with a store, a label and an attempt count — not averaged
away.

**Every handler in the tree carries the idempotency tax.** That is the permanent
cost of this decision and there is no way to pay it once.

## Alternatives considered

**MQTT QoS 2 for prices.** A four-way handshake per price update. Rejected: it
buys exactly-once between the broker and the controller and nothing at all across
the radio hop beyond it, which is where duplication actually happens. Meanwhile
it triples the packet count on the hop where the store's uplink is narrowest. It
is used for OTA triggers, where a duplicate genuinely costs something a
retransmission cannot recover.

**Kafka transactions across the ingest and the fan-out.** Would give
exactly-once-ish semantics inside the cloud tier. Rejected: the guarantee stops
at the MQTT boundary, so the label still needs the sequence rule, and having both
means paying for the transaction coordinator to solve a problem the sequence rule
has already solved.

**Deduplicating at the controller by content hash.** Would catch the
distinct-webhook-unchanged-price case. Rejected as the primary mechanism because
it moves a correctness property into a component that
[0004](0004-end-to-end-price-attestation.md) explicitly does not trust, and
because a content hash cannot distinguish "the same price re-sent" from "the
price was changed and changed back", which are different facts for an audit.

**A wider ingress dedupe window.** The `idem.Guard` window is 24 hours. Widening
it would not help: the distinct-webhook case has a genuinely distinct key, so no
window catches it.
