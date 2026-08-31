# USSLP engineering roadmap

Sequenced by dependency, not by wish.

Every item below is derived from something the code actually lacks — a gap the
root `README.md` reports, a package comment that describes an adapter which does
not exist, a test that records a defect rather than fixing it, or a claim in a
document that the tree does not support. Nothing here is a feature idea.

The three horizons are not schedules. They are **preconditions**: what has to be
true before the platform can honestly be run in one real store, then in a chain,
then globally. An item appears at the earliest horizon that requires it.

---

## Horizon 0 — the ledger of what is missing today

Collected in one place, with a source for each. The horizons below are drawn from
this list.

### Reported by the root README as known gaps

| # | Gap | Status |
|---|---|---|
| 1 | **The Label Service does not consume `promotion-events`.** The interface contract lists it as a consumer and the Promotion Service's documentation says the Label Service turns promotions into shelf updates; `label.Service.Start` subscribes only to `device-events`, `price-updates` and `label-delivery`. A promotion activates and no shelf changes. `usslpd` bridges it in the composition root (`stack/promobridge.go`) | Open, worked around |
| 2 | **`sim.Engine.Stop` leaves scheduled events with stale heap indices**, so a timer cancelled after the engine has stopped panics inside `heap.Remove`. Reachable from `sec.Controller.Stop` if a store's clock is stopped before its controllers | Open, shutdown ordered around it |
| 3 | **A distinct POS delivery carrying the price already displayed still refreshes the panel** — with a **full** waveform, because `partialSafe` refuses a partial when the price has not changed. A POS republishing its price book nightly would spend the fleet's battery budget on it | Open, recorded by `TestDistinctWebhookWithAnUnchangedPriceStillRefreshes` |
| 4 | **`edge/sim`'s clock only moves when an event fires**, so on a quiet store a price is timestamped in the past and its latency under-reported — measured drift reached 2.5 s. `stack.startClockTick` bounds it to under 120 ms and publishes `clock_drift_ms` | Mitigated, not fixed |
| 5 | **A lost acknowledgement leaves the controller and the glass disagreeing.** The shelf is right and `DisplayedSequence` is wrong until the next update. Faithful hardware behaviour, but it means `DisplayedSequence` is not a safe test for "the shelf is showing a price" | By design, needs reconciliation |
| 6 | **`TestHTTPGetLabelAndHistory` is flaky**, ~1 run in 30, from a message-ordering race in that package's own test harness | Open, deliberately |
| 7 | **The controller-to-label radio budget was exceeded at p99.** `INTERFACE-CONTRACTS` §4 allowed 300 ms; measured **331 ms** over 1,000 changes and **314 ms** under sustained load. Cause: end-to-end attestation adds 199 bytes to every frame | **Closed** — the line item was the wrong side and §4 now reads 400 ms, the 100 ms taken from two cloud hops measuring 8–18 ms against a 300 ms allowance |

### Found by reading the tree for this documentation

| # | Gap | Source |
|---|---|---|
| 8 | **The Kafka adapter does not exist.** There is no `pkg/eventbus/kafka` directory and no `//go:build kafka` constraint anywhere in the tree. The package comment claimed otherwise and has been corrected; the adapter itself is still the work | `platform/pkg/eventbus/eventbus.go` vs. the filesystem |
| 9 | ~~**`analytics-service` exists and compiles, and three places say it does not.**~~ **Closed.** It is enabled in every values file, present in both image matrices and in the dev compose profile, and the gateway's analytics breaker is a real signal again rather than a permanently open one | `platform/cmd/analytics-service/main.go` |
| 10 | **No Go file connects to PostgreSQL, ClickHouse or Redis.** No `database/sql` import, no driver. All persistence is `pkg/kvstore`. The prod-like profile and the Terraform provision all three | tree-wide |
| 11 | ~~**The ack status codes and attestation verdict have no counterpart in `edge/`.**~~ **Closed.** `edge/labelsim` defines statuses 3 and 4 and the three-bit verdict, `edge/sec` routes the two refusals to their opposite runbooks, and the old inference survives only as a fallback for firmware that predates the codes — marked `label (inferred)` with an empty verdict | `edge/labelsim/wire.go`, `edge/labelsim/verdict.go`, `edge/sec/controller.go` |
| 12 | **The Zephyr half of the firmware has never been compiled.** No SDK in the environment. The portable half passes 25,961 host assertions under two compilers, ASan and UBSan | `firmware/README.md` |
| 13 | ~~**`stack/streams.go` says the catalogue is 5,568 partitions.**~~ **Closed.** It is 5,472, and `stack.TestCatalogueTotalMatchesTheCommentAndTheClusterSizing` now asserts the total | verified against `canon.AllStreams()` |
| 14 | ~~**The root README says 24,539 firmware checks; `firmware/README.md` says 25,961.**~~ **Closed.** All four host-test configurations report 25,961; the root README carried the stale figure and now agrees | stale figure in the root README |
| 15 | **The estate figures do not multiply.** 100,000 stores × 40,000 labels is 4 billion, eighty times the 50 million fleet. And "one controller per ~8 m of shelving" is inconsistent with "~25 controllers and up to 40,000 labels" in the same store. Open — but no longer propagated silently: the root README, `INTERFACE-CONTRACTS` §1, `canon/topics.go` and `registry/app/telemetry.go` now point at `scalability.md` §1 where they quote it. `label/adapters/messaging.go` still repeats it unannotated | `registry/app/telemetry.go`, `label/adapters/messaging.go` |
| 16 | ~~**The MSK module's default storage does not hold the retention model it cites.**~~ **Closed.** `broker_volume_gb` is 12,000 (72 TB across six brokers), the arithmetic is written above the variable, and the two assumptions it depends on — zstd at 3:1 or better, and a 7-day broker window for `audit-log` — are stated in the description rather than assumed | `deploy/terraform/modules/msk/main.tf`, arithmetic in `scalability.md` §2.4 |
| 17 | **`audit-log` at 365 days is petabyte-scale and is not a Kafka retention.** The tiered design it implies is now written down — a 7-day broker replay window, a Kafka Connect S3 sink into the Object Lock bucket under the region-local `events` key, and a read path over the archive — and only the first part is provisioned. USSLP produces a complete audit record and does not yet retain one for a year | `canon.StreamAudit`, `deploy/terraform/modules/msk/main.tf` |
| 18 | **No OTLP exporter.** Spans still reach the backend over the collector's `filelog` bridge rather than over OTLP. The part that made it a production outage rather than a stopgap is **closed**: the span log has its own level and its own rate (`USSLP_SPAN_LOG_LEVEL`, `USSLP_SPAN_LOG_ONE_IN`), so the application `info` default no longer silences every span | `obs.NewRuntime`, `deploy/observability/README.md` |
| 19 | **CRL distribution to the edge fleet is manual.** `pki.RevocationList` exists and `pki.Verify` enforces it; nothing pushes a fresh list on a schedule | `pkg/pki/revocation.go` |
| 20 | **No LoRa.** The architecture describes a LoRa backhaul for rural stores. There is no LoRa code anywhere in the tree | tree-wide grep |
| 21 | **Every ML model is trained on synthetic data its own tests generated.** No model has seen a retailer's transactions | `pricing/ml` package comment, root README |
| 22 | **No hardware exists.** No timing, no power and no waveform has been observed on silicon. Every figure is a datasheet or blueprint number | `firmware/README.md`, *Not verified* |

---

## Horizon 1 — what has to be true to run this in one real store

The single-process shape (`usslpd`) is already a legitimate deployment for one
store: one shared event log makes the cross-service stream real, and
`pkg/eventlog`, `pkg/mqtt` and `pkg/kvstore` all run on an appliance. What is
missing is **hardware and the things only hardware forces you to finish.**

Ordered by dependency. Nothing later can start until the item it sits under is
done.

### 1.1 Compile the firmware — everything else in this horizon depends on it

Gap 12. The Zephyr half — `main.c`, the E-Ink driver, the waveform tables, the
framebuffer packer, the Zigbee and BLE bindings, the PSA backend, the device
certificate, the PMIC and gauge code, NVS, the OTA controller and slot handling,
NFC, tamper, telemetry and provisioning — has never been fed to a compiler. It is
written against real Zephyr 3.x APIs and there will be compile errors.

Needs an nRF Connect SDK west workspace (the ZBOSS stack ships there, not in
upstream Zephyr — vanilla Zephyr configures and fails to link the radio).

`radio/zigbee.c` is called out in the firmware's own README as the least
confidently correct code in the tree, because ZBOSS's API changes between NCS
releases and there was no SDK to check against.

**Until this is done, every hardware number in the platform is a model.**

### 1.2 Link it, and find out whether it fits

`scripts/memory_report.sh` produces the real flash and RAM figures once there is
something linked. The estimate is ~343 KB against a **424 KiB OTA slot** — 81%,
and the slot, not the part, is the binding constraint: a build that fits the part
and not the slot cannot be updated over the air, which on a shelf label means it
cannot be updated at all. Only the 21,392-byte portable-core row of that estimate
is measured.

### 1.3 Measure what has only been modelled

Gap 22. In order of consequence:

- **The waveform tables on glass.** They are transcribed from datasheets with one
  deliberate change (a fourth clear pass). A wrong waveform bakes a permanent
  shadow that is invisible for weeks. This needs a temperature-chamber run and a
  thousand-cycle soak, and it is the one item here that cannot be rushed.
- **Power.** Every current in the 6.584 µA budget is a datasheet number. The
  arithmetic over them is verified to within 1 nA per component against
  `labelsim`; the inputs are not.
- **Ed25519 verification timing.** The 13 ms figure drives the whole
  frame-type-4 trade-off.
- **Airtime and the CSMA model.** `edge/mesh` is the basis for every latency the
  platform publishes about its edge tier.
- **Beacon window and duty cycle**, which decide whether 8.67 years is real.

### 1.4 Retire the ack-status inference in `edge/` — *mostly done*

Gap 11 is closed. `edge/labelsim` defines ack statuses 3 and 4 and the three-bit
verdict field; `edge/sec/controller.go` routes the two refusals to their opposite
runbooks — status 3 to a compliance alert carrying the verdict and a `tampering`
flag, status 4 to an operational alert naming the remedy.

What remains is deleting the fallback. A label whose firmware predates the codes
still reports both refusals as `bad-frame`, which is indistinguishable from a
corrupted frame, so the controller still infers for those devices. The inference
is lossy in both directions and is marked `label (inferred)` with an empty
verdict so it cannot be mistaken for a read signal. It goes when the fleet audit
shows no firmware old enough to need it.

### 1.5 Fix the unchanged-price refresh

Gap 3. In one store this is a battery bug; at fleet scale it is a fleet
replacement schedule. A POS that republishes its price book nightly asks every
label for a **full** waveform — roughly a hundred times the energy of anything
else a label does — to redraw a number that did not move.

The fix belongs in the aggregate: a no-op when the resolved price equals the
displayed price. It is not a wider dedupe window; the webhook is genuinely
distinct.

### 1.6 Make the Label Service consume `promotion-events`

Gap 1. `stack/promobridge.go` is wiring standing in for business logic and should
be deleted when the service grows its own consumer. Until then a promotion
activates and, outside `usslpd`, no shelf changes.

### 1.7 Fix `sim.Engine.Stop`, and stop working around it

Gap 2. A panic in `heap.Remove` reachable from a normal shutdown path is not
something to order the composition root around indefinitely.

### 1.8 Re-check `INTERFACE-CONTRACTS` §4 against silicon — *decided, not yet validated*

Gap 7 is closed on paper. The §4 line item was the wrong side and now reads
400 ms, re-balanced by taking 50 ms each from `Label Svc → broker` and
`broker → SGU → SEC`, which measure 8–18 ms together against a 300 ms allowance.
The total is still 3,000 ms and `TestLatencyBudgetMatchesTheContract` keeps
`stack.Budget` in step with the document.

The alternative — stop carrying the full signed tuple on every update — was
rejected and §4 records why: the 199 bytes *are* the label's ability to verify
without trusting the controller, and trimming them moves the trust boundary back
inside a box in a back room. §4 says so explicitly so the next reader does not
reach for the frame.

What is not settled is whether 400 ms survives real airtime. Every number behind
it comes from `edge/mesh`, which is a model; 1.3 is the item that replaces it
with measurements, and this line item should be re-derived from those.

### 1.9 Fix the stale figures — *done*

Gaps 13, 14, and the `deploy/README.md` and CI claims in gap 9, are closed.
Gap 15 is not fixable here — it needs a blueprint decision — but it is no longer
propagated silently: every place that quotes an estate figure points at
`scalability.md` §1, except `label/adapters/messaging.go`.

Individually trivial; collectively they were the difference between documentation
a reviewer trusts and documentation a reviewer checks.

---

## Horizon 2 — what has to be true to run a chain

A chain is tens to hundreds of stores under one tenant with a central
merchandising function. The single-process shape stops being enough, and **the
cloud tier's ports need their second implementations.**

### 2.1 Write the Kafka adapter — everything else in this horizon depends on it

Gap 8. This is the hard architectural ceiling.
`pkg/eventlog`'s consumer-group coordinator is in-process, so two OS processes
must not share one log directory, so **the multi-process topology cannot carry
the cross-service event stream at all**: in the compose profile the UIG's
`price-updates` records do not reach the Label Service.

The port (`eventbus.Bus`) is real and every service is written against it. The
adapter the package comment claims exists does not. Until it is written:

- there is no multi-process cloud tier;
- no claim about MSK behaviour at 1,024 partitions is supported by anything in
  this repository;
- the port's abstraction has never been tested by a second implementation, which
  means the seam may not be in quite the right place.

Fixing the package comment is part of this item, not separate from it.

### 2.2 Give `analytics-service` something to read — *shipped, and idle*

Gap 9 is closed: the binary is enabled everywhere, in both image matrices and in
the dev compose profile, and its IRSA role was always there.

Shipping it does not make it useful, and that is 2.1's problem rather than this
one. It consumes four streams, and `pkg/eventlog` keeps consumer-group
coordination in memory, so in any multi-process deployment it reads only the log
in its own data directory — which nothing else writes to. The reports are
correct and empty until the Kafka adapter lands. `usslpd` is the one shape where
it works today, because one `*eventlog.Log` is handed to every constructor.

The remaining work here is smaller: nothing charts `usslp_analytics_*`. See
`observability.md` §9.

### 2.3 Ship the OTLP exporter

Gap 18. At one store, structured logs correlated by `trace_id` are enough. Across
nine services and a chain's traffic they are not, and a three-second budget split
across nine hops is exactly what distributed tracing is for.

The acute half of this is already fixed: spans used to be written at debug on the
service's own logger, so production's `info` default meant **no span line was
ever emitted** and the collector's `filelog` bridge received nothing. The span
log now has its own level and its own per-trace rate
(`USSLP_SPAN_LOG_LEVEL`, `USSLP_SPAN_LOG_ONE_IN`), so traces do reach a backend.
What is left is that they get there as log lines: a line per exported span, and a
rate knob separate from the tracer's own sampling because `StartAlwaysSampled`
bypasses head sampling on a 52,000/second path. An OTLP exporter deletes the
bridge and both knobs together.

Everything else is already in place: W3C propagation through HTTP, the event bus
and MQTT payload metadata; always-on sampling for the price path (head sampling
at 1% is right for volume and wrong for the one update a regulator asks about);
and a collector configured with **tail** sampling, which is the correct choice
because head sampling cannot keep a trace *because* it turned out to be slow.

When the exporter lands, the collector's `filelog` receiver and its pipeline
entry are deleted and nothing else changes.

### 2.4 Automate CRL distribution

Gap 19. At one store, a technician syncs a list. At a chain, a gateway that has
not synced in three weeks enforces a three-week-old view of the world and looks
entirely healthy doing it.

This is bounded work — the list, its `NextUpdate`, and a scheduled fetch in the
gateway's existing maintenance window — and it is the last piece of the
revocation story ([ADR 0006](../adr/0006-crl-over-ocsp.md)).

### 2.5 Reconcile the controller's view with the glass

Gap 5. `DisplayedSequence` is wrong whenever an ack is lost, which is faithful
hardware behaviour and still leaves the fleet-health model reading a number that
is sometimes false. At one store you can ask the panel (`stack.unpricedLabels`).
Across a chain that needs to be a periodic reconciliation the controller does on
its own, reported upstream.

### 2.6 Train a model on real data, or stop shipping models

Gap 21. Every model in `pricing/ml` is trained and validated on synthetic data
generated by its own tests. The algorithms are demonstrably correct — they
recover parameters a test chose — and **no model here has seen a retailer's
transactions**, so none of its demand curves should be quoted as a finding about
retail.

For a chain this becomes a decision rather than a caveat: either train on the
tenant's own history through the feature store and the model registry (which
already refuses to promote a quantised model whose measured delta exceeds the
tenant's tolerance), or ship Tier 1 alone and be explicit that Tiers 2 and 3 are
not yet fit to drive a price.

### 2.7 Fix the flaky test

Gap 6. One run in thirty is tolerable when one person runs the suite. It is not
tolerable when it gates a chain's release train, and a test-harness race papered
over is a test-harness race that comes back.

---

## Horizon 3 — what has to be true to run globally

100,000 stores, 50 million labels, three regions, multiple tenants including
competitors. The capacity model in
[`scalability.md`](scalability.md) becomes load-bearing rather than aspirational.

### 3.1 Establish a real estate model

Gap 15. Every capacity number traces to figures that do not multiply: 100,000
stores × 40,000 labels is eighty times the 50 million fleet. And the peak-to-mean
ratio of **9** that produces the 52,000/second figure is not written down
anywhere — a chain whose nightly load compresses into thirty minutes instead of
sixty doubles the peak without changing a single other number.

Nothing below can be sized until this is settled, which is why it is first.

### 3.2 Enforce the storage tier's assumptions

Gap 16 is closed as a sizing question: `broker_volume_gb` is 12,000, 72 TB across
six brokers, with the arithmetic written above the variable.

What is open is that nothing *enforces* the two assumptions the figure rests on.
A producer that ships uncompressed batches overruns the volume, and the first
symptom is a broker refusing writes — which stops the UIG acknowledging a POS. A
`compression.type` check in the topic-provisioning Job, or a Prometheus rule on
bytes per record, would catch it before the disk does. Shortening telemetry
retention is the other lever: 72 hours of raw per-label telemetry is generous for
a stream whose consumers are an anomaly detector and a columnar store that has
already ingested it.

### 3.3 Tier the audit log to object storage

Gap 17. 365 days of every price event at the model's mean rate is roughly 7.6 PB
at RF 3 — not a Kafka retention. The design is now written down in
`deploy/terraform/modules/msk/main.tf` and `scalability.md` §2.4, in three parts:
a 7-day broker replay window (provisioned), a Kafka Connect S3 sink into the
Object Lock bucket under the region-local `events` key (not wired), and a read
path answering "what price was this label showing on this date" from the archive
(not written). The platform already has hot/warm/cold tiering where moving a
segment is a `rename`
([ADR 0014](../adr/0014-columnar-analytics-store.md)); the audit stream does not
use it.

This is also the point at which the weights-and-measures retention obligation
stops being satisfied by a Kafka setting.

### 3.4 Write the warehouse adapters, or commit to not needing them

Gap 10. `analytics/columnar` is a genuine column store and is the right answer
for a tenant with no warehouse. A tenant with analysts who want arbitrary SQL
needs the ClickHouse adapter behind the documented port — which, like the Kafka
one, does not exist. The prod-like profile and the Terraform provision the
schemas it would be written against.

The Postgres and Redis ports are a separate question and may not need answering
at all: `pkg/kvstore` is the store everywhere, it works, and adding a relational
tier because the deployment provisions one is the wrong direction.

### 3.5 Prove the cloud tier at load

No measurement in this repository ran the cloud services near their capacity
model. The load harness's own package comment says the ceiling it finds is the
edge tier's, and that aggregate throughput is bounded by how fast one Go process
can simulate several hundred radios — a fact about the simulator. The cloud
services were never the constraint in any run here, so **their saturation
behaviour has never been observed.**

Depends on 2.1 (there is no multi-process cloud tier without the Kafka adapter)
and 3.1 (there is nothing to size against without an estate model).

### 3.6 Shard the MQTT fan-out

100,000 gateway sessions over 5 fixed broker replicas is 20,000 per broker. The
platform is already regionally deployed, so the population divides naturally; the
HPA stays off unless EMQX's rebalance API is configured, because an autoscaler
that adds and removes nodes during a price fan-out moves the very sessions the
fan-out is publishing to.

### 3.7 Close the security gaps that only matter at scale

From [`security.md`](security.md) §11, in severity order:

- **No dual control or transparency log on firmware signing.** A stolen key plus
  an OTA principal is arbitrary code on 50 million devices. Staged cohorts bound
  the blast radius only if the malicious image fails a health gate, and a
  competent attacker's passes all four. The controls that exist are integrity
  controls, not compromise-recovery controls. Threshold signing, dual control on
  artifact upload and a transparency log are all appropriate at this scale and
  none exists.
- **No at-rest encryption on a store gateway.** 100,000 physically accessible
  boxes each holding a store's full pricing and planogram.
- **Stream isolation is logical, not physical.** One Kafka cluster serves every
  tenant in a pooled deployment; a consumer bug that ignores the tenant field is
  a cross-tenant leak with nothing structural to stop it. Competitors on one
  platform makes this a commercial risk, not just a technical one.
- **No automated tamper detection on the audit log.** The append-only structure
  makes a deletion detectable as a gap; nothing detects one. No WORM storage, no
  external log shipping.

### 3.8 Decide about LoRa

Gap 20. The architecture describes a LoRa backhaul for rural stores and there is
no LoRa code. The honest reading is that
[ADR 0003](../adr/0003-edge-first-architecture.md) already solves the problem
better: a rural store with an intermittent link runs autonomously, which is a
stronger answer than a low-rate backhaul whose downlink latency is measured in
seconds to minutes and could not carry a price inside any budget worth writing
down.

Either build it for a use case that genuinely needs it — telemetry-only backhaul
from a store with no broadband at all is the plausible one — or **stop claiming
a radio the platform does not have.**

---

## What this roadmap does not contain

No new product features. Not because there are none worth building, but because
every item above is a gap between what the platform claims and what it does, and
adding capability on top of an unclosed claim is how a system stops being
trustworthy.

The two most consequential items are the two that unlock their entire horizon:
**compile the firmware** (1.1) and **write the Kafka adapter** (2.1). Almost
everything else waits on one of them.
