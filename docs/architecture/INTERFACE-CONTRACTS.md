# USSLP Interface Contracts

The normative agreement between the platform's components. Everything here is
enforced by code in `platform/pkg/canon`; this document explains the *why* and
fixes the details that are conventions rather than types.

Read this before changing any component that talks to another one.

---

## 1. Tiers

```
Tier 4  Cloud platform          multi-region Kubernetes, event-sourced
Tier 3  Store Gateway Unit      one per store, back office, MQTT broker + offline brain
Tier 2  Shelf Edge Controller   one per ~8 m shelf section, Zigbee coordinator
Tier 1  Smart label             E-Ink, Zigbee mesh node, 7–10 year battery
```

A tier only ever talks to the tier directly above and below it. Nothing in the
cloud addresses a label's radio; nothing on a label knows the cloud exists.

**The blueprint estate figures do not reconcile, and this document repeats them
rather than resolving them.** "One controller per ~8 m shelf section" above and
"~25 controllers and up to 40,000 labels" in one store (`canon/topics.go`,
`label/adapters/messaging.go`) cannot both hold: 40,000 labels needs on the order
of a kilometre of shelving, which at 8 m is roughly 125 controllers. Separately,
100,000 stores × 40,000 labels is 4 billion labels against a stated fleet of
50 million. Both are catalogued in
[`scalability.md` §1](scalability.md), which also states the reading everything
downstream actually uses: **50 million labels across 100,000 stores, 500 per
store on average.** Nothing normative in this document depends on either figure.

---

## 2. Event stream (Kafka / `pkg/eventlog`)

Every record is a JSON-encoded `canon.Envelope`. Streams, partition counts and
retention are defined once in `canon.AllStreams()`.

| Stream | Produced by | Consumed by | Key |
|---|---|---|---|
| `pos-integration` | UIG | audit, analytics | `tenant:store` |
| `price-updates` | UIG | Label Service, Pricing, Analytics, Audit | `store:sku` |
| `label-delivery` | Label Service (from edge ACKs) | Analytics, SLO, Audit | `label_id` |
| `device-events` | Device Registry | Label Service, OTA, Monitoring | `device_id` |
| `label-telemetry` | Device Registry (from edge batches) | Analytics, Anomaly detection | `label_id` |
| `promotion-events` | Promotion Service | Label Service, Analytics | `tenant:promo` |
| `inventory-sync` | UIG | Pricing, Analytics | `store:sku` |
| `ota-commands` | OTA Service | Device Registry, edge | `device_id` |
| `audit-log` | every service | Compliance, SIEM | `tenant` |
| `label-state` | Label Service | read-model rebuilds | `label_id` (compacted) |
| `dead-letter` | every consumer | human triage | original key |

**Ordering guarantee.** Per partition key only. `store:sku` keying means two
price changes for the same product in the same store are strictly ordered, while
different products proceed in parallel across 1,024 partitions. Nothing in the
platform may assume global ordering.

**Delivery guarantee.** At least once. Every consumer must be idempotent. The
mechanisms are: `IdempotencyKey` on the envelope (ingress dedupe), per-aggregate
`Version` (optimistic concurrency in the event store), and per-label `Sequence`
(monotonic, enforced at the label).

---

## 3. Device messaging (MQTT)

Namespace: `usslp/{tenant}/{region}/{store}/…`, built only through
`canon.TopicScope`. The tenant segment sits immediately below the root so that
a single ACL rule — *subscribe only to `usslp/{your-tenant}/#`* — is complete
isolation. Every device credential is issued with exactly that constraint;
cloud service credentials are issued with a cross-tenant wildcard.

### Downstream (cloud → store → label)

| Topic | QoS | Retain | Payload |
|---|---|---|---|
| `…/sec/{sec}/labels/{label}/price` | 1 | yes | `Envelope{label.price.updated → canon.PriceUpdated}` |
| `…/sec/{sec}/labels/{label}/config` | 1 | yes | `Envelope{…}` |
| `…/sec/{sec}/labels/{label}/ota` | 2 | no | `Envelope{ota.device.updated}` |
| `…/sec/{sec}/zone/price` | 1 | no | zone-wide promotion broadcast |
| `…/store/planogram/update` | 1 | yes | planogram push |
| `…/store/promotion/activate` | 1 | no | promotion activation |

Price updates are **retained** so that a controller rebooting after a power cut
recovers the current price of every label in its zone from the local broker,
without a round trip to a cloud that may be unreachable.

### Upstream (label → store → cloud)

| Topic | QoS | Retain | Payload |
|---|---|---|---|
| `…/sec/{sec}/labels/{label}/ack` | 1 | no | `Envelope{label.update.delivered → canon.LabelDelivered}` |
| `…/sec/{sec}/heartbeat` | 0 | yes | controller health |
| `…/sec/{sec}/mesh/status` | 0 | yes | `Envelope{mesh.topology.changed → canon.MeshTopology}` |
| `…/sec/{sec}/telemetry` | 0 | no | `Envelope{device.telemetry.reported → []canon.Telemetry}` (batched) |
| `…/store/mode` | 1 | yes | `Envelope{store.mode.* → canon.StoreModeChanged}` |

Telemetry is batched per controller. Forwarding it per label would be 13 million
messages per second across the fleet; batched per controller it is under one
message per second per store.

Cloud-side subscription filters are the `canon.FilterAll*` constants.

### The bridge

The Store Gateway Unit runs the store's MQTT broker *and* is a client of the
cloud broker. It bridges:

- **cloud → local** for the downstream table, so controllers only ever talk to
  a broker inside the building;
- **local → cloud** for the upstream table, buffering to disk while the WAN is
  down.

When the cloud link drops the bridge stops; the local broker does not. That is
the entire mechanism behind zero label downtime during a WAN outage.

---

## 4. The price path, hop by hop

The 3-second SLO is a budget, and each hop owns a slice of it:

```
POS → UIG            ≤  50 ms   validate, dedupe, normalise, publish
UIG → stream         ≤  30 ms   durable append (acks=all)
stream → Label Svc   ≤ 120 ms   consume, resolve labels, price, attest
Label Svc → broker   ≤ 100 ms   MQTT QoS 1 publish
broker → SGU → SEC   ≤ 100 ms   bridge + LAN
SEC → label          ≤ 400 ms   Zigbee, up to 3 hops, attested frame + queueing
label refresh        ≤ 2000 ms  E-Ink full waveform (300 ms partial)
ACK back to cloud    ≤ 200 ms   confirmation
                     ───────
                       3000 ms
```

`canon.LabelDelivered.LatencyMS` is measured from the envelope's `RecordedAt`
(the moment USSLP took durable responsibility) to the moment the pixels
settled. That is the number the SLO is written against, because it is the only
one a retailer can verify by looking at a shelf.

**Why the last radio hop is 400 ms and not 300, and where the 100 ms came
from.** The original table gave `SEC → label` 300 ms, sized for a bare price
frame at roughly 15 ms per mesh hop. End-to-end price attestation (frame type 4,
added after this table was written) carries the signed tuple all the way to the
glass, which makes the air frame **199 bytes larger** — and airtime is linear in
frame length, so every transmission and every retransmission on that hop costs
proportionally more. Measured p99 over 1,000 changes is **331–343 ms**,
repeatably, across three consecutive runs. The line item was wrong, not the
platform.

The 100 ms is taken from `Label Svc → broker` and `broker → SGU → SEC`, 50 ms
each, because those two hops have by far the most unused slack: the whole
cloud-and-bridge stretch measures 8–18 ms on an unsaturated store and 33–49 ms
at p50 across all five pre-radio hops. The end-to-end total is unchanged at
3,000 ms and the measured end-to-end p99 is 1,890–2,441 ms, so the slack is
real rather than borrowed.

**Do not "fix" this by shrinking the frame.** The 199 bytes are the label's
ability to verify the price it is displaying without trusting the controller
that sent it — the one property in §5 that everything else hangs off. A frame
trimmed back to 300 ms of airtime is a frame that has given up the second
verification, which moves the trust boundary back inside a box in a back room.
The airtime is the cost of the guarantee and it is the correct trade; if this
hop has to come down, the way down is fewer retransmissions (mesh link quality,
transmit power, scheduling) or a faster PHY, not a smaller signed tuple.

---

## 5. Price attestation

Non-negotiable: **a label never displays a price it cannot verify.**

1. Label Service computes `canon.AttestationInput` and signs the SHA-256 of its
   canonical string with the tenant's Ed25519 price-authority key
   (`pki.PriceAuthority`).
2. The signature travels inside `canon.PriceUpdated.Attestation`.
3. The Shelf Edge Controller **recomputes** the digest from the update it is
   holding — never from the transmitted digest — and verifies it against the
   `pki.KeyRing` it last synced. Failure means the update is dropped, the
   previous price stays on the glass, and a `compliance` alert is raised.
4. The attestation is retained in `audit-log` for the statutory period.

A compromised controller, a corrupted mesh frame, or an attacker with write
access to the store's broker therefore cannot change a displayed price. They can
only prevent one from changing, which is visible within three missed heartbeats.

---

## 6. Idempotency and sequencing

| Boundary | Mechanism | Owner |
|---|---|---|
| POS → UIG | `idem.Guard`, 24 h window, key from adapter-supplied parts | UIG |
| UIG → event store | `Envelope.IdempotencyKey`, no-op re-append | eventstore |
| Command → aggregate | `expectedVersion` optimistic concurrency | Label Service |
| Cloud → label | per-label monotonic `Sequence` | label firmware / simulator |

A label **discards any update whose sequence is not greater than the one it is
displaying**. This is what makes at-least-once mesh delivery safe: a duplicated
frame is a no-op, and a reordered one cannot roll a price backwards.

---

## 7. Health and readiness

Every binary exposes `/metrics`, `/healthz`, `/readyz` on its admin port
(`obs.Runtime`). Liveness is "the process is scheduling goroutines". Readiness
is "dependencies are reachable *and* start-up finished". Dependency checks are
registered on **readiness only** — a broker blip must remove a pod from the
load balancer, never restart it, or a five-second dependency wobble becomes a
cluster-wide restart storm.

---

## 8. Configuration

`config.Loader` with the `USSLP_` prefix, and every value also resolvable from
`NAME_FILE=/run/secrets/x` — because the Store Gateway Unit takes its
credentials from a mounted secret on a device that is not running Kubernetes.
