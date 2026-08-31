# USSLP in five minutes

This is a walkthrough for someone who has never seen the platform and wants to
know whether its two headline claims are true:

1. a price change in a retailer's POS reaches the E-Ink label on the shelf in
   **under three seconds**, verified cryptographically;
2. a store **keeps trading through a WAN outage** with zero label downtime.

You will run the whole platform, watch both happen, and read the numbers it
measured rather than the numbers it asserts.

Everything below needs Go 1.24 and nothing else. No Docker, no Kafka, no
cluster. The repository has no external Go dependencies.

---

## The fast path

```bash
make demo
```

That builds two binaries, boots the entire platform in one process, and narrates
eight steps through the operator CLI. It takes about two minutes and cleans up
after itself. If you only do one thing, do that.

The rest of this document is the same walkthrough done by hand, so you can stop
and poke at each step.

---

## 1. Start the platform

```bash
make run
```

`usslpd` is the whole platform in one operating-system process: eight cloud
services, a cloud MQTT broker, a certificate hierarchy, a store gateway, its
shelf edge controllers, and a simulated label fleet — all against one shared
event log. It boots in a few seconds and prints every URL you need:

```
────────────────────────────────────────────────────────────────────────────
  USSLP — single-process runtime dev
  booted in 8013 ms · 1 tenant(s) · 1 store(s) · 4 controllers · 100 labels
────────────────────────────────────────────────────────────────────────────

  OPERATOR
    console              http://127.0.0.1:8080/console
    API (OpenAPI)        http://127.0.0.1:8080/openapi.json
    live event feed      ws://127.0.0.1:8080/v1/stream
    usslpd control       http://127.0.0.1:8079

  SERVICES
    uig                  http://127.0.0.1:8081   admin http://127.0.0.1:9081
    label-service        http://127.0.0.1:8082   admin http://127.0.0.1:9082
    device-registry      http://127.0.0.1:8083   admin http://127.0.0.1:9083
    ...

  STORES
    demo-retail-store-01 mqtt 127.0.0.1:1883 · diagnostics http://127.0.0.1:8090 · connected

  CREDENTIALS
    demo-retail (owner)  usslp_live_...
    shopify HMAC key     adf0bde04c4a5049ef92b7be85f2f4d2
    shopify ingest       http://127.0.0.1:8081/v1/ingest/{tenant}/pos
```

**"Booted" means the store is open for trade.** Every device has been enrolled
through the real zero-touch provisioning path — a genuine certificate from the
platform's own sub-CA, checked against a manufacturing manifest, with the
anti-cloning check run — a planogram has been applied, and an opening price book
has been delivered to every label and confirmed by every panel. The banner does
not print until the last waveform has finished.

Leave it running and open a second terminal.

---

## 2. Look at the store

```bash
bin/usslpctl status
bin/usslpctl stores
bin/usslpctl labels --limit 5
```

```
LABEL                                  SKU                ON THE GLASS  SEQ  ATTESTED  REFRESHES  BATT
demo-retail-store-01-sec-01-lbl-00000  SKU-0D5D8A-01-001  $20.74        1    yes       1          100%
demo-retail-store-01-sec-01-lbl-00001  SKU-0D5D8A-01-002  $6.35         1    yes       1          100%
```

`ATTESTED` is not decoration. Each of those prices carries an Ed25519 signature
over a canonical digest of *(tenant, store, label, SKU, price, effective time,
sequence, promotion)*, made by the tenant's price authority. The controller
recomputed that digest from the update it was holding and verified the signature
against its key ring — and then sent the whole signed tuple onward, so the label
itself rebuilt the canonical string and checked the signature against its own
key ring before it drove a single pixel. A price that fails to verify is not
displayed, at either point.

That second check is why the label carries a key ring at all. A controller is a
box in a back room and a label is a device a shopper can put a hand on;
verifying only at the controller would leave the final hop protected by the one
component an attacker would replace.

Add `--json` to any command to get the raw response for `jq`.

---

## 3. Change a price

Pick a SKU from the table above and move it by a modest amount:

```bash
bin/usslpctl slo --reset
bin/usslpctl price set --sku SKU-0D5D8A-01-001 --price 16.59 --was 20.74
```

> **Why "modest"?** The Label Service refuses a change of more than five times
> the current price as a corrupt feed rather than a decision — the failure it
> exists to catch is a decimal point lost between an ERP and a CSV. Ask for
> £3.49 when the shelf says £20.74 and the platform will decline, correctly.

Wait a second, then look again:

```bash
bin/usslpctl labels --limit 3
bin/usslpctl slo
```

```
1 deliveries measured, 0 failed; 100.000% inside the 3000 ms budget
p50 1678 ms   p95 1678 ms   p99 1678 ms   max 1678 ms

MEASURED
  HOP                                   BUDGET   P50      P99      OK   HOW
  POS -> ... -> SEC (cloud and bridge)  400 ms   3 ms     3 ms     yes  residual
  SEC -> label                          400 ms   166 ms   166 ms   yes  measured
  label refresh                         2000 ms  1500 ms  1500 ms  yes  measured
```

One sample is not a distribution, and the scripted `make demo` does not present
it as one: it runs twelve changes one at a time and reports the spread. A
representative run is p50 1,772 ms, max 1,828 ms, all twelve inside the budget,
with the cloud and bridge share between 3 and 13 ms. Over forty changes with
prices that move only the last digits — so the panel can use a 300 ms partial
waveform instead of a 1,500 ms full one — the same store gives p50 461 ms and
max 583 ms. The panel, not the platform, is what the number is mostly made of.

That is the first claim, measured. The number is taken from the envelope's
`RecordedAt` — the moment USSLP took durable responsibility for the change — to
the moment the panel finished its waveform, which is the definition in
[INTERFACE-CONTRACTS §4](architecture/INTERFACE-CONTRACTS.md).

The `--reset` before the change matters: without it the population also contains
the opening price book, which is a store-wide fan-out and a different
experiment. See step 4.

### What actually happened

`usslpctl price set` posted to the API Gateway, which authenticated the API key,
derived the tenant from it, and forwarded to the Label Service. The demo script
and the end-to-end suite instead push a **signed Shopify webhook** at the
Universal Integration Gateway, which is the full path:

```
POST /v1/ingest/{tenant}/pos   HMAC-SHA256 verified against the binding's key
  -> deduplicated on the Shopify webhook id (24-hour window)
  -> normalised: "16.59" parsed to minor units without touching a float
  -> durably appended to price-updates, keyed store:sku
  -> Label Service resolves the labels showing that SKU in that store
  -> signs each with the tenant's Ed25519 price-authority key
  -> publishes QoS 1, retained, to the owning controller's MQTT topic
  -> Store Gateway Unit bridges it into the store's own broker
  -> Shelf Edge Controller recomputes the digest, verifies, renders,
     diffs the framebuffer, chooses partial or full waveform, and transmits
     the price *and its signature* as an attested frame
  -> label checks its monotonic sequence, rebuilds the canonical digest,
     verifies the signature against its own key ring, refreshes, acknowledges
  -> back up the bridge -> label-delivery -> the SLO read model
```

You can push one yourself; the HMAC key is in the banner.

---

## 4. Reprice the whole store

```bash
bin/usslpctl labels --limit 0 --json \
  | jq '[.[] | {sku, price: ((.displayed_price|ltrimstr("$")|tonumber) * 0.8 | tostring)}]' \
  > /tmp/prices.json
bin/usslpctl slo --reset
bin/usslpctl price batch --file /tmp/prices.json
```

```
36 items -> 36 labels: 36 applied, 0 scheduled, 0 rejected, 0 failed, in 20ms
```

Twenty milliseconds is how long it took the *platform* to accept, attest and
publish a store's worth of price changes. Getting them onto that many pieces of
glass takes several seconds more, and the SLO report will show a tail well past three
seconds:

```
36 deliveries measured; 66.7% inside the 3000 ms budget
p50 1847 ms   p95 3473 ms   p99 3551 ms
```

**That is not a defect and it is worth understanding.** A controller transmits
to at most eight labels at a time — a label's radio is off while its panel is
running a waveform — so a store-wide fan-out queues at the radio by
construction. The three-second budget is a statement about *a price change*, not
about a store held permanently at saturation. `make load` measures exactly where
that line is, and prints which component moved it.

---

## 5. Cut the WAN

```bash
bin/usslpctl chaos wan-outage
bin/usslpctl stores
```

```
STORE                 TENANT       MODE        LABELS  SECS  QUEUE  WAN
demo-retail-store-01  demo-retail  autonomous  100     4/4   4      CUT
```

The link is severed for real: the store's uplink is a TCP proxy, and cutting it
closes every established connection and refuses new ones. Nothing told the
gateway it was down — its own probe, a QoS 1 publish that must be acknowledged,
stopped being answered, and it declared itself autonomous.

Now look at the shelves:

```bash
bin/usslpctl labels --limit 5
```

Every price is exactly what it was. Nothing is blank. The store's MQTT broker,
its controllers and its labels are all inside the building and none of them
needed the cloud.

Change a price while it is down:

```bash
bin/usslpctl price set --sku SKU-0D5D8A-01-001 --price 14.93
bin/usslpctl stores        # the upstream queue is filling
bin/usslpctl labels --limit 3   # the shelf still shows the last verified price
```

The change is accepted, durable, and undeliverable. That is the correct
behaviour: the store cannot be reached, and nothing pretends otherwise.

---

## 6. Bring it back

```bash
bin/usslpctl chaos wan-outage --restore
sleep 6
bin/usslpctl stores
bin/usslpctl labels --limit 3
```

The gateway reconnects, collects the cloud's retained view of the store,
reconciles it against what the store decided on its own, flushes the buffered
messages upstream in order, and the price you set during the outage appears on
the glass.

The reconciliation report is on the gateway's own diagnostics surface (the URL
is in `usslpctl stores`), including the clock skew it measured and any conflicts
it resolved.

**That price change will not be inside three seconds, and it should not be.**
The SLO clock starts when the cloud takes durable responsibility for a change,
and the store could not be reached for most of what followed, so the outage is
inside the number — `make demo` measures one deliberately and reports it at
8–10 s. The three-second claim is about a store the platform can reach. The
claim about a store it cannot reach is the one you have just watched: the
shelves go on trading, nothing goes blank, and the change is held durably and
applied in order the moment the link returns.

---

## 7. Break something else

```bash
bin/usslpctl chaos kill-sec --sec demo-retail-store-01-sec-03
bin/usslpctl stores
```

The controller's link is severed without a clean disconnect, so the gateway
learns about it from the retained last will rather than by polling. It takes
about fifteen seconds: the broker publishes a will only once the session's
keep-alive grace has expired, and the controllers keep alive every ten. A
politely closed connection would produce no will at all — MQTT 3.1.1 discards
one on DISCONNECT — which is exactly why this fault cuts the network under the
client rather than closing the client.

```bash
bin/usslpctl chaos degrade-link --delay 250ms --loss 5
bin/usslpctl chaos kill-relay --sec demo-retail-store-01-sec-01
```

`kill-relay` removes a mains-powered relay from a zone's mesh, orphaning
every label parented to it. The controller notices the dead link, routes
around it, and delivery continues — `test/e2e/mesh_test.go` asserts it.

---

## 8. Watch it live

```bash
bin/usslpctl watch
```

```
00:44:39  label.update.delivered  demo-retail-store-01  ...-lbl-00004  1438ms end to end  1500ms waveform  1 hop(s)  seq 2
```

That is the API Gateway's WebSocket feed, filtered to your tenant, straight off
the platform's event streams.

---

## The tests are the claims

Everything above is asserted, not narrated, in `test/e2e`. Each test boots its
own runtime and fails if the platform stops behaving:

```bash
make test-e2e
```

| Test | The claim |
|---|---|
| `TestPriceReachesTheGlassWithinBudget` | one price change, right label, verified attestation, inside three seconds |
| `TestPriceLatencyPercentiles` | 1,000 changes, p50/p95/p99 with a per-hop breakdown against the contract |
| `TestEndToEndLatencyAgreesWithWallClock` | the platform's own clock agrees with a stopwatch outside the process |
| `TestStoreWidePromotionFansOut` | activated in the Promotion Service, fanned out by the Label Service's own `promotion-events` consumer: every affected label moved, none of the others did, one waveform each, and the rule's authored LED colour survived to the wire |
| `TestOverlappingPromotionsResolveLastActivationWins` | where two promotions overlap the shelf shows the one activated last, and prices from the everyday price rather than compounding |
| `TestStoreSurvivesWANOutage` | autonomy, a local price change, a scheduled promotion on the local clock, ordered flush, reconciliation |
| `TestZeroTouchProvisioning` | a new label joins and is trading with no human step |
| `TestTamperedPriceIsRefused` | a forged price on the store's broker does not move the shelf |
| `TestUnattestedPriceIsRefused` | neither does an unsigned one |
| `TestEveryLabelVerifiesForItself` | no label in the fleet is in a mode where it would take the controller's word for a price |
| `TestFleetBootsWithNoAttestationRefusal` | a fleet finishes boot having verified every price and refused none |
| `TestOTARollbackOnCohortFailure` | a failing cohort halts the rollout and nothing else gets the bad image |
| `TestMeshReroutesAroundADeadRelay` | the mesh heals and delivery completes inside budget |
| `TestRedeliveredWebhookIsAppliedOnce` | ten deliveries, one price change, one waveform |
| `TestNoCrossTenantLeakage` | two tenants, one process, no leakage on API, stream or MQTT |
| `TestLatencyBudgetMatchesTheContract` | the budget the runtime reports against is the one in the document |

`go test -short ./...` skips everything that boots a platform, for an inner
loop.

---

## Load

```bash
make load
```

An open-loop harness: a fixed offered rate rather than fixed concurrency, so
saturation is visible instead of being hidden behind a throughput number. It
prints latency percentiles, the per-hop split, every controller's queue depth,
and a plain-language account of what ran out of room first.

Tune it:

```bash
make load LOAD_STORES=2 LOAD_LABELS=60 LOAD_RATE=60 LOAD_DURATION=60s
```

---

## What you are looking at, honestly

The labels are simulated. `edge/labelsim` and `edge/mesh` model an 802.15.4
network and an E-Ink panel over a discrete-event clock paced 1:1 against the
wall clock: real airtime, real CSMA backoff, real duty cycling, real waveform
durations from the hardware budget. A 2.9-inch panel really does take about
1.5 s for a full refresh and about 300 ms for a partial one, and that is what
dominates every latency you will see.

Everything above the radio is the code that would ship, doing the work it would
do. Nothing is stubbed for the demo.

The event log and the MQTT broker are the platform's own implementations
standing in for Kafka and EMQX behind `pkg/eventbus` and `pkg/msgbus`. See the
root `README.md` for the full list of what is real and what is a documented
production target.
