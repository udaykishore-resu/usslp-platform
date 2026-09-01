# USSLP — the Universal Smart Shelf Label Platform

A retail operating system for electronic shelf labels. It exists to make two
things true at once:

- **a price change in a retailer's point of sale reaches the E-Ink label on the
  shelf in under three seconds**, and the label can prove the price it is
  showing was authorised;
- **a store keeps trading through a wide-area network outage**, with no shelf
  going blank and no price going stale, and reconciles cleanly when the link
  returns.

Both are measured, not asserted. `make demo` runs them in front of you; `make
test-e2e` fails the build if they stop being true.

```bash
make demo        # two minutes: the whole platform, narrated, with real numbers
make run         # the whole platform in one process on documented ports
make test-e2e    # the claims above, as assertions
```

Go 1.24, standard library only. No Docker, no Kafka, no cluster required to run
any of the above.

---

## What makes it "universal"

Every other ESL platform ships bespoke middleware per POS vendor, which is why a
retailer running a custom or legacy ERP cannot buy electronic shelf labels at
all. USSLP replaces that with one pipeline behind a protocol-adapter seam. A
Shopify webhook, a Square catalogue event, an NCR item-price message in XML or
JSON, a SAP PRICAT IDoc, an Oracle Retail SOAP envelope, a Lightspeed item
update, a Clover object reference that has to be fetched back, and a fixed-width
file an AS/400 wrote at two in the morning all arrive downstream as the same
`canon.PriceChangeRequested`.

Adding a retailer is a configuration change. Adding a POS vendor is one adapter
implementing four methods.

---

## Architecture

Four tiers. A tier only ever talks to the tier directly above and below it:
nothing in the cloud addresses a label's radio, and nothing on a label knows the
cloud exists.

```
 ┌──────────────────────────────────────────────────────────────────────────┐
 │ TIER 4 · CLOUD                                     multi-region, event-  │
 │                                                    sourced, per-tenant   │
 │   POS/ERP ──▶ UIG ──▶┌──────────────────────────────────────┐            │
 │   (9 adapters)       │ event streams (Kafka / pkg/eventlog) │            │
 │                      │  price-updates · device-events ·     │            │
 │                      │  label-delivery · promotion-events · │            │
 │                      │  ota-commands · audit-log · …        │            │
 │                      └───┬───────┬────────┬────────┬────────┘            │
 │                          │       │        │        │                     │
 │                    Label Svc  Registry  Pricing  Promotion  Analytics    │
 │                          │       │        │        │            │        │
 │                          └───────┴────┬───┴────────┴────────────┘        │
 │                              API Gateway  ◀── operators, integrations    │
 └──────────────────────────────────┬───────────────────────────────────────┘
                                    │  MQTT  usslp/{tenant}/{region}/{store}/…
                                    │  QoS 1, retained prices
 ┌──────────────────────────────────▼───────────────────────────────────────┐
 │ TIER 3 · STORE GATEWAY UNIT          one per store, in the back office   │
 │   the store's own MQTT broker · a bridge to the cloud that buffers to    │
 │   disk · a local promotion calendar on the store's own clock · CRDT      │
 │   reconciliation · guard rails · an optional delegated signing key       │
 └──────────────────────────────────┬───────────────────────────────────────┘
                                    │  LAN, inside the building
 ┌──────────────────────────────────▼───────────────────────────────────────┐
 │ TIER 2 · SHELF EDGE CONTROLLER       one per ~8 m of shelving †          │
 │   verifies every attestation before a pixel moves · renders the          │
 │   framebuffer · diffs it to choose a partial or full waveform ·          │
 │   forwards the signed tuple onward · Zigbee coordinator · mesh healing   │
 └──────────────────────────────────┬───────────────────────────────────────┘
                                    │  802.15.4 mesh, ≤3 hops, signature rides along
 ┌──────────────────────────────────▼───────────────────────────────────────┐
 │ TIER 1 · SMART LABEL      E-Ink · coin cell · 7–10 year target           │
 │   duty-cycled radio · monotonic sequence · holds its own key ring and    │
 │   verifies the signature itself · displays nothing it cannot verify      │
 └──────────────────────────────────────────────────────────────────────────┘
```

The normative agreement between the tiers — stream catalogue, MQTT namespace,
the hop-by-hop latency budget, the attestation rules, the idempotency
boundaries — is [`docs/architecture/INTERFACE-CONTRACTS.md`](docs/architecture/INTERFACE-CONTRACTS.md).
It is enforced by code in `platform/pkg/canon` and checked against the runtime
by `TestLatencyBudgetMatchesTheContract`.

Every diagram in the repository — 84 of them, covering the context, the
components, the boards, the sequences with their latency budgets, the state
machines and the runbook decision trees — is indexed by kind in
[`docs/architecture/DIAGRAMS.md`](docs/architecture/DIAGRAMS.md).

† **The blueprint estate figures do not multiply, and this README repeats them
rather than quietly picking one.** "One controller per ~8 m of shelving" cannot
hold at the same time as "~25 controllers and up to 40,000 labels" in one store
(`canon/topics.go`): 40,000 labels needs roughly a kilometre of shelving, which
at 8 m is about 125 controllers. And 100,000 stores × 40,000 labels is 4 billion
labels against a stated fleet of 50 million. Both are catalogued in
[`docs/architecture/scalability.md` §1](docs/architecture/scalability.md), which
states the reading everything downstream actually derives from: **50 million
labels across 100,000 stores, 500 per store on average.** Nothing measured below
depends on either figure.

### The one property everything else hangs off

**A label never displays a price it cannot verify.** The Label Service signs a
canonical digest of *(tenant, store, label, SKU, price, effective time,
sequence, promotion)* with the tenant's Ed25519 price-authority key. That
signature is then checked **twice**, and the second check is the one that
matters most.

The Shelf Edge Controller recomputes the digest from the update it is holding —
never from the digest on the wire — and verifies it against the key ring it last
synced. It then forwards the whole signed tuple over the radio, and the label
rebuilds the canonical string for itself and verifies the signature against its
*own* key ring before it drives a single pixel. A failure at either point means
the update is dropped, the previous price stays on the glass, and a compliance
alert is raised.

The second check exists because the first one's trust boundary has a controller
inside it. A shelf label is a device the public can stand in front of; a
controller is a box in a back room that can be rooted or physically swapped.
Verifying only at the controller means the last hop is protected by the thing an
attacker would replace. Verifying at the glass means a price is provable at the
point a shopper reads it, which is the point a trading standards officer asks
about.

A compromised controller, a corrupted mesh frame, or an attacker with write
access to the store's broker therefore cannot change a displayed price. They can
only prevent one from changing, which is visible within three missed heartbeats.
`test/e2e/attestation_test.go` attacks exactly that surface and asserts the
shelf does not move; `test/e2e/fleet_attestation_test.go` audits the fleet
itself, and fails if any label is in a mode where it would take a controller's
word for it.

---

## Running it

### One process (development, a lab, a single store)

```bash
make run
bin/usslpctl status
bin/usslpctl price set --sku SKU-… --price 1.99
bin/usslpctl watch
```

`usslpd` constructs every service in-process against one shared event log, one
cloud MQTT broker, one store gateway, its controllers, and a simulated label
fleet. This is a legitimate deployment shape, not only a test fixture: it is
what a single-store or disconnected pilot wants, and it is the only shape in
which the cross-service event stream currently works (see *Honest status*
below).

Useful flags: `--ephemeral`, `--stores`, `--controllers`, `--labels`, `--tenants`,
`--seed`, `--base-port`, `--status-file`. See the package comment on
[`platform/cmd/usslpd/stack`](platform/cmd/usslpd/stack/doc.go) for when this
shape is right and when the Kubernetes topology is.

### Nine processes (the documented production topology)

```bash
make dev          # pure-Go compose profile
make prod-like    # against Kafka, EMQX, Postgres, ClickHouse, Redis, Prometheus
```

See [`deploy/README.md`](deploy/README.md), which also documents the Helm chart,
the Terraform for three regions, the Istio policy, and the runbooks.

### The demo

[`docs/DEMO.md`](docs/DEMO.md) is a five-minute walkthrough a reviewer can
follow. `make demo` runs it scripted.

---

## Honest status: what is real, what is a production target

This section is the one to read before drawing conclusions from anything else.

### Real, exercised end to end, load-bearing

- **The whole price path.** POS webhook → HMAC verification → deduplication →
  normalisation → durable append → label resolution → Ed25519 attestation →
  MQTT QoS 1 retained → store bridge → signature verification → render →
  waveform decision → radio → panel → acknowledgement → SLO read model. Every
  step is production code. `test/e2e` measures it.
- **The attestation.** Real keys, real signatures, real verification at the
  controller *and* at the label, which holds its own key ring and checks the
  signed tuple before driving the glass. Tampering is detected and refused at
  both.
- **Promotion fan-out.** A first-class Label Service consumer
  (`label-service.promotions` on `promotion-events`), not a bridge in a
  composition root. A merchandiser activates a promotion in the Promotion
  Service; the Label Service resolves the affected shelves with the promotion
  domain's own compiled matcher and drives them through the same batch path a
  store-wide repricing takes — per-tenant rate limiting, attestation,
  sequencing, guard rails and per-label failure reporting. The authored display
  block travels with it, so a rule that briefs a red LED gets a red LED.
  `usslpd` is not on that path at all; `test/e2e/promotion_test.go` activates
  through the Promotion Service and asserts on the glass, which is what makes it
  evidence of the wiring rather than of the test.
- **Zero-touch provisioning.** Real certificate hierarchy (`pkg/pki`: six
  authorities, path-length constraints, revocation), real chain verification,
  real manufacturing-manifest comparison, real anti-cloning check.
- **The MQTT tier.** `platform/pkg/mqtt` is a complete MQTT 3.1.1
  implementation — protocol level 4, retained messages, QoS 0/1/2, sessions that
  survive a reconnect, last wills. The services speak to it exactly as they
  speak to EMQX.
- **Store autonomy.** WAN detection by acknowledged probe, durable upstream
  buffering, a local promotion calendar on the store's own clock, a hybrid
  logical clock, and CRDT reconciliation with a stated conflict policy.
  A store can also be given a **delegated store-scoped signing key**
  (`sgu.Config.LocalAuthority`), so that a price originated by a local point of
  sale during an outage is attested and the controllers accept it.

  It is **nil by default**, and that is not a degraded mode. With no local
  authority `Gateway.LocalPriceChange` returns `ErrNoLocalAuthority` and the
  price is **refused outright** rather than published unsigned; the gateway logs
  the consequence at boot — a local price change is recorded and reported and
  does not reach a shelf. A label refuses any price it cannot verify, so "the
  shelf did not change" is the correct outcome rather than a defect. That is why
  `edge/sgu/schedule.go` says the gateway holds no signing key by default: the
  two statements are only true together. Scheduled promotions survive an outage
  because the cloud pushed *already attested* updates ahead of time, not because
  the store can sign.
- **OTA safety.** Signed artifacts (the signature covers version, hardware tier
  and digest, so a correct image cannot be flashed onto the wrong panel), staged
  cohorts, four health gates, automatic rollback.
- **The metrics surface.** `obs.Registry` writes the Prometheus text format
  directly. Every metric name in every alert rule and dashboard is checked
  against the Go source in CI (`make verify-metrics`).
- **The stream catalogue.** Partition counts and retentions come from
  `canon.AllStreams()`; the Helm chart, the compose topic job and the MSK module
  each transcribe it and `make verify-topics` fails if any has drifted.

### Simulated, faithfully and deliberately

- **The labels and their radio.** `edge/labelsim` and `edge/mesh` over
  `edge/sim`, a deterministic discrete-event engine, paced 1:1 against the wall
  clock. Airtime, CSMA backoff, mesh hops, duty cycling, E-Ink waveform
  durations and battery draw are modelled from the hardware budget. The model is
  honest and it is not silicon: a latency measured here is a real measurement of
  everything above the radio and a faithful model of the radio itself.

### Provisioned, not yet wired

- **The event stream across processes.** `pkg/eventlog` keeps consumer-group
  coordination in memory, so two OS processes must not share one log directory.
  In the multi-process compose profile each service therefore has its own log
  and the UIG's `price-updates` records do **not** reach the Label Service.
  `usslpd` exists partly to fix this: one `*eventlog.Log` handed to every
  constructor makes the cross-service stream real. The Kafka adapter behind
  `pkg/eventbus.Bus` is the documented production port.
- **PostgreSQL, ClickHouse and Redis.** Nothing in the Go tree connects to any
  of them. The event store and every read model are on `pkg/kvstore`, an
  embedded LSM store. The prod-like profile and the Terraform provision all
  three with the schemas the documented ports expect.
- **OTLP tracing.** `obs.NewRuntime` wires the tracer with `obs.LogExporter`:
  spans go to the structured log and the OTel collector's `filelog` bridge
  reconstructs them, which works and is still a stopgap. The span log has its
  own level and its own rate — `USSLP_SPAN_LOG_LEVEL` (default `info`) and
  `USSLP_SPAN_LOG_ONE_IN` (100 in production) — because it used to write at
  debug on the *service* logger, which production's `info` default silenced, so
  no trace backend received anything at all.

### Written but not compiled or trained

- **The Tier-1 firmware.** `firmware/` is a Zephyr application for an nRF52840
  and has **never been compiled** — no Zephyr SDK in the environment it was
  written in. The portable half (attestation, sequencing, framebuffer, wire
  codec) builds and passes **25,961 checks** on the host under `-Werror`, ASan
  and two compilers — the same figure in all four runs (`gcc`; `gcc` under
  ASan and UBSan; `clang`; trapping UBSan at `-O0`), and the same figure
  `firmware/README.md` reports. The Zephyr half is a reference implementation, not a build
  artefact. `firmware/README.md` says so at the top.
- **The pricing ML.** `platform/internal/pricing/ml` implements elasticity
  regression, gradient-boosted trees, an isolation forest and an LSTM from
  scratch. Every model is **trained and validated on synthetic data generated by
  its own tests**. The algorithms are tested against known-truth generators; no
  model here has seen a real retailer's transactions, and none of the demand
  curves should be quoted as a finding about retail.

---

## Measured numbers

From this repository, on a 2-core container, with the edge tier simulated at 1:1
wall clock. Reproduce with `make test-e2e` and `make load`.

Every price below was verified twice on its way to the glass: once by the Shelf
Edge Controller and once by the label itself, which rebuilds the canonical
string and checks the Ed25519 signature against its own key ring before driving
a pixel. That second check is not free — the signed tuple makes the air frame
199 bytes larger and the verification costs the label about 13 ms — and the
figures here are what it costs.

**One price change, POS webhook to pixels settled**

| | |
|---|---|
| end to end | **387–548 ms** (platform), 414–568 ms (wall clock outside the process) |
| of which the radio | 75–244 ms against a 400 ms budget |
| of which the panel | 300 ms (a partial waveform) against a 2,000 ms budget |
| of which cloud + bridge | 8–18 ms against a 400 ms budget |

Ranges rather than a single figure because one sample is not a measurement. Over
forty serial changes into a settled 36-label store the distribution is p50
461 ms, p90 551 ms, max 583 ms, with the cloud share between 8 and 18 ms
throughout.

The two clocks agree: over twenty changes the platform's own median was 463 ms
and a stopwatch outside the process said 481 ms, a difference of 18 ms. That
check is a test of its own (`TestEndToEndLatencyAgreesWithWallClock`), because a
latency measured against a simulated clock is worth nothing until something has
compared it with a real one.

**1,000 price changes at ten in flight into a 100-label store**

Three consecutive runs, because a p99 quoted from one run is an anecdote:

| run | p50 | p95 | p99 | max | delivered | budget |
|---|---|---|---|---|---|---|
| 1 | 526 ms | 1,823 ms | **2,420 ms** | 3,096 ms | 996/1000 | 3,000 ms |
| 2 | 529 ms | 1,744 ms | **1,890 ms** | 2,355 ms | 998/1000 | 3,000 ms |
| 3 | 541 ms | 1,834 ms | **2,441 ms** | 2,919 ms | 998/1000 | 3,000 ms |

The p99 holds, and so now does the per-hop budget for the last radio leg: the
controller-to-label time is 331–343 ms at p99, against a line item that §4 sets
at 400 ms. It used to say 300 ms, which is what this measurement showed to be
wrong — see *Known gaps* below. The two-to-four changes per thousand
that never arrive were abandoned by the radio after three attempts and reported
upstream as `label.update.failed` — visible, not lost.

The concurrency is chosen below the store's ceiling on purpose: offering more
measures a queue rather than a price change. At sixteen in flight the same
benchmark reports a p99 of about 3.3 s, and every millisecond of the excess is a
transmission slot being waited for — which is the experiment `test/load` runs
deliberately.

**A store-wide promotion**: 24 shelf positions resolved and on the controllers
26–52 ms after the Promotion Service accepted the activation; every panel
settled 3.6–3.9 s later, one waveform each, all 24 carrying the rule's authored
red LED.

**Sustained load, two stores, 480 labels, 40/s offered for 45 s**

Two runs:

| | run 1 | run 2 |
|---|---|---|
| delivered | 37.6/s, 1,796 of 1,800 | 37.7/s, 1,796 of 1,800 |
| end to end | p50 1,378 ms, p99 2,697 ms, max 3,751 ms | p50 1,405 ms, p99 2,706 ms, max 3,843 ms |
| inside the SLO | 99.61% | 99.61% |
| bottleneck | dispatch queueing at the edge, not the cloud tier | the same |

The ceiling is the edge: a controller transmits to at most eight labels at once
because a label's radio is off while its panel runs a waveform. At this rate the
first five contract hops — everything from the POS to the controller — are 922 ms
at p50 against a 400 ms budget, and that excess is queue, not service time: the
same hops are 33–49 ms at p50 when the store is not saturated. Channel
utilisation is about 2.08% per zone, up from 1.55% before end-to-end attestation:
the same traffic in larger frames.

**What the three-second claim does not cover.** Three cases, all measured and
all reported rather than averaged away.

*A store that cannot be reached.* A price change the cloud accepts while a
store's uplink is severed is not delivered in three seconds; it is held durably
and applied in order when the link returns. `make demo` measures one
deliberately and reports it at 8–10 s — most of which is the outage. The
guarantee during an outage is a different one, and it is the one the store keeps:
the shelves go on trading, nothing goes blank, and no price changes without a
signature the label itself checked.

*A store held at saturation.* A store-wide fan-out asks every panel to run a
waveform at once and a controller transmits to at most eight labels at a time,
so 36 positions repriced together give p50 1,847 ms and p99 3,551 ms, with two
thirds inside the budget. The claim is about a price change, not about a store
at its ceiling; `test/load` measures where that ceiling is.

*A delivery the radio has to repeat.* INTERFACE-CONTRACTS §4 budgets hops, not
retransmissions. The coordinator backs off 500 ms and then a second, so a third
attempt on top of a 1,500 ms full waveform lands at 3.2–4.3 s — over the budget.
This matters most immediately after a mesh reroute, where links are freshly
re-formed and drop frames: about one delivery in six there needs a second or
third attempt, and end-to-end attestation made that likelier by adding 199 bytes
to every frame. `TestMeshReroutesAroundADeadRelay` asserts the budget on
first-attempt deliveries — 1,779–2,105 ms, comfortably inside — and prints the
retried ones with their attempt counts instead of hiding them in an average.

---

## Repository map

```
platform/
  cmd/
    usslpd/            the whole platform in one process — start here
      stack/           the composition root: services, PKI, stores, control API
    api-gateway/       the front door: auth, RBAC, rate limits, WebSocket feed
    uig/               the Universal Integration Gateway
    label-service/     price fan-out and attestation
    device-registry/   identity, placement, health, planograms
    ota-service/       staged firmware rollouts
    pricing-service/   elasticity, optimisation, anomaly detection
    promotion-service/ the promotion DSL, lifecycle and conflict policy
    analytics-service/ column store, reports, SLO attainment
  internal/            the services' domain, application and adapter layers
  pkg/
    canon/             the contract: events, IDs, money, topics, attestation
    eventlog/          embedded partitioned log (stands in for Kafka)
    eventstore/        event sourcing over kvstore
    kvstore/           embedded LSM store with a write-ahead log
    mqtt/              complete MQTT 3.1.1 broker and client
    pki/               certificate hierarchy, price authority, key rings, JWT
    obs/               logging, metrics, tracing, health, admin server
    idem/ retry/ config/ eventbus/ msgbus/
edge/
  sgu/                 Store Gateway Unit: bridge, buffer, autonomy, CRDT merge
  sec/                 Shelf Edge Controller: verify, render, waveform, deliver
  labelsim/            simulated labels: E-Ink, power model, wire protocol
  mesh/                802.15.4 model: airtime, CSMA, routing, healing
  sim/                 deterministic discrete-event engine
firmware/              Tier-1 Zephyr application (uncompiled; see above)
tools/
  usslpctl/            the operator CLI
  demo.sh              the scripted walkthrough
test/
  e2e/                 the claims, as assertions
  load/                the load harness
deploy/                Docker, compose, Helm, Istio, Argo, Terraform, runbooks
docs/
  DEMO.md              the five-minute walkthrough
  architecture/INTERFACE-CONTRACTS.md   the normative contract
```

**378 Go files, 124 of them tests, 139,468 lines** — measured on 2026-08-31,
call it about 139,000. Counted with:

```bash
find . -name '*.go' -type f | wc -l                     # 378
find . -name '*_test.go' -type f | wc -l                # 124
find . -name '*.go' -type f -exec cat {} + | wc -l      # 139,468
```

The count is dated because it rots on the next commit. It is also worth naming
what "tests" means here: 124 is a count of *files*, and the line this replaces
said "116 of them tests", which more than one reader took for a count of tests.
The tree holds **964 `Test…` functions**
(`grep -rho '^func Test[A-Za-z0-9_]*' --include='*_test.go' . | wc -l`), which is
closer to what `go test ./...` actually runs — and still an undercount, because
table-driven subtests are not separate functions.

Firmware is separate and is C: the portable core is 25,961 host assertions
(`cd firmware/tests && make`), reported in `firmware/README.md`.

---

## Testing

```bash
make test-short   # unit tests only; anything that boots a platform is skipped
make test         # everything, including the end-to-end claims (a few minutes)
make test-race    # the same under the race detector
make test-e2e     # just the claims, verbosely, with the measured numbers printed
make lint         # gofmt -l and go vet
make load         # the load harness
make verify       # what CI runs: lint, tests, and the deployment-layer checks
```

The end-to-end suite boots a real runtime per test group with an ephemeral data
directory and asserts on behaviour rather than internals. It is slow because it
waits for simulated E-Ink panels in real time, which is the point: a suite that
skipped the waiting would not be measuring what it claims to measure.

---

## Known gaps

Reported rather than hidden. These are defects or divergences found while
building the runtime and the test suite; none is worked around silently.

1. **Overlapping promotions resolve last-activation-wins at the shelf.**
   `promotion-events` carries the promotion *rule*, by the producer's design —
   a national activation must not become two thousand stores' worth of
   simultaneous lookups against one service — so the Label Service's consumer
   sees one rule at a time and has no basis on which to arbitrate between two.
   Priority, stacking and exclusive groups are settled by `promodomain.Resolve`
   against the whole active set, and only the Promotion Service holds that set.
   Where two promotions overlap, the shelf therefore shows the one activated
   most recently, because it takes the higher per-label sequence, even when the
   other has the higher priority. What the shelf does *not* do is compound them:
   each activation prices from the label's stored everyday price, so the result
   is always a price one of the two promotions authorises.
   `TestOverlappingPromotionsResolveLastActivationWins` pins both halves down.
   Closing the gap needs `Resolve`'s output on the event, not more logic in the
   consumer.

2. **`sim.Engine.Stop` leaves scheduled events with stale heap indices**, so a
   timer cancelled after the engine has stopped panics inside `heap.Remove`.
   Reachable from `sec.Controller.Stop` if a store's clock is stopped before its
   controllers. `usslpd` orders shutdown around it.

3. **A distinct POS delivery carrying the price already displayed still refreshes
   the panel.** All four documented idempotency boundaries hold, and none covers
   this case; the aggregate applies the change and the label runs a full
   waveform to redraw the same number. A full waveform is roughly a hundred times
   the energy of anything else a label does, so a POS that republishes its price
   book nightly would spend the fleet's battery budget on it.
   `TestDistinctWebhookWithAnUnchangedPriceStillRefreshes` records the behaviour
   so that fixing it is visible.

4. **The edge simulation's clock only moves when an event fires.** `edge/sim` is
   a discrete-event engine, so on a quiet store — no price changes, telemetry
   every thirty seconds — `Engine.Now()` can sit seconds behind the wall clock.
   Every latency the platform reports is the difference between a wall-clock
   timestamp the cloud stamped and a simulated-clock timestamp the controller
   stamped, so a price arriving during a quiet spell is timestamped in the past
   and its latency is under-reported — far enough, on a two-core container, to
   come out negative. This is the one case where measured drift reached 2.5 s.
   `usslpd` bounds it by scheduling a 20 ms heartbeat on each store's engine
   (`stack.startClockTick`), which brings the observed drift to under 120 ms,
   and publishes what is left as `clock_drift_ms` in every SLO report so the
   error bar travels with the number.

5. **A lost acknowledgement leaves the controller and the glass disagreeing.**
   The radio model drops acknowledgements as readily as it drops updates. When
   an ack is lost the label has applied the price and the controller has not
   heard so; its retransmission of the same sequence is answered
   `stale-sequence`, which the coordinator records as a failed delivery. The
   shelf is right and the controller's `DisplayedSequence` is wrong until the
   next update. This is faithful hardware behaviour rather than a platform
   defect — reconciling it is what the controller's delivery-failure reporting
   and the registry's health derivation exist for — but it means
   `DisplayedSequence` is not a safe test for "the shelf is showing a price".
   `usslpd` asks the panel instead; see `stack.unpricedLabels`.

6. **`TestHTTPGetLabelAndHistory` in `platform/internal/label` is flaky**, at
   roughly one run in thirty. The cause is a message-ordering race in that
   package's own test harness (`waitForMessages`), not in the service. Left
   alone deliberately: it is outside the paths this work is allowed to touch,
   and a test harness race papered over is a test harness race that comes back.

7. **The controller-to-label radio budget was wrong, and has been corrected
   rather than met.** INTERFACE-CONTRACTS §4 used to allow that hop 300 ms; it
   measures 331–343 ms at p99 over a thousand changes, repeatably. The cause is
   end-to-end attestation: carrying the signed tuple to the label makes the air
   frame 199 bytes larger, and airtime is linear in frame length. Since the
   three-second total holds with room, the line item was the wrong side and §4
   now reads **400 ms**, with the 100 ms taken from the two cloud hops that
   measure 8–18 ms against a 300 ms allowance. §4 also records *why* the frame
   grew, because the tempting fix — shrink the frame — is the one that gives up
   the label's own verification and moves the trust boundary back inside a box
   in a back room.

---

## Licence and provenance

See the individual package comments for design rationale; most of the
interesting decisions in this tree are explained where they were made rather
than in a wiki.
