# 07 — Application and user flows

**Derived from:** `platform/internal/apigw/routes.go`,
`platform/internal/apigw/{console.go,stream.go,rbac.go,doc.go}`,
`tools/usslpctl/{main.go,commands.go,ops.go}`,
`platform/internal/registry/{app,domain}`,
`platform/internal/promotion/domain/{conflict.go,dsl.go,lifecycle.go}`,
`platform/internal/pricing/{service.go,app,domain}`,
`platform/internal/label/domain/{render.go,policy.go}`,
`edge/sec/render.go`, `edge/labelsim/eink.go`, `edge/sgu/wan.go`,
`platform/internal/uig/{adapter,gateway,deliveries}`,
`deploy/observability/prometheus/rules/*.yaml`, `deploy/runbooks/*.md`,
`deploy/edge/{README.md,RUNBOOK.md}`, `docs/DEMO.md`.

See also: [05 — Sequence diagrams](05-sequence-diagrams.md) ·
[03 — Components](03-components.md)

---

# Part A — human flows

## A1. The store manager's day

Everything here goes through the API Gateway, which derives the tenant from the
credential and scopes every store-path route against the principal's store list.
A manager of store 7 asking for store 9 gets **404, not 403** — confirming that
an identifier exists somewhere in the platform is itself a cross-tenant leak.

### A1.1 Morning health check

```mermaid
flowchart TB
    start(["Start of trade"]) --> health
    health["GET /v1/stores/{id}/overview"] --> tri{"Anything red?"}
    tri -->|"no"| ok["Open the aisle"]
    tri -->|"labels not showing a price"| lbls["GET /v1/stores/{id}/labels<br/>state, pending_sequence,<br/>last_failure_reason"]
    tri -->|"devices offline"| devs["GET /v1/stores/{id}/devices<br/>and /mesh for the topology"]
    tri -->|"latency drifting"| slo["GET /v1/stores/{id}/slo<br/>hop table against the budget"]
    devs --> runway["GET /v1/stores/{id}/runway<br/>battery hours remaining per label"]
    runway --> ticket["Raise a technician visit before<br/>the shelf edge goes blank"]
    lbls --> judge{"Is the label offline?"}
    judge -->|"yes"| notrade["Still showing an authorised price.<br/>A technician problem, not a trading one."]
    judge -->|"no, delivery failed"| deliv["Radio or panel.<br/>See the incident flow, A5."]
```

`LabelState.Healthy()` is `state == active` **and** `pending_sequence == 0`
**and** `last_failure_reason == ""`. Nothing else counts as green.

### A1.2 Running a promotion

```mermaid
flowchart TB
    look["GET /v1/promotions"] --> pick{"Scheduled for today?"}
    pick -->|"yes, let it fire"| wait["The lifecycle sweep activates it on<br/>store local time, once a minute"]
    pick -->|"needs to go now"| act["POST /v1/promotions/{id}/activate"]
    wait --> fan["Fan-out — see 05 section 3"]
    act --> fan
    fan --> verify["Walk the aisle: promotional labels<br/>carry a green LED and a SALE badge"]
    verify --> miss{"A label did not change?"}
    miss -->|"no"| done(["Done"])
    miss -->|"yes"| why["The Resolution records every<br/>suppressed promotion with a reason<br/>and the promotion that beat it"]
```

### A1.3 Acknowledging an alert

```mermaid
flowchart TB
    alerts["An alert fires in Alertmanager<br/>carrying a runbook_url"] --> kind{"Which alert?"}
    kind -->|"USSLPControllerComplianceRefusal"| comp["A price was refused at a controller<br/>or at the glass. Read the verdict."]
    kind -->|"USSLPLabelDeliveryFailureRate"| deliv["Radio or panel. Check the mesh map<br/>and the zone's in-flight slots."]
    kind -->|"USSLPStoreAutonomous"| aut["The store is trading on its own.<br/>Nothing to do at the shelf."]
    kind -->|"USSLPPriceGuardrailRejectionSpike"| guard["A feed is sending prices the platform<br/>refuses. Check the POS deliveries."]
    comp --> rb1["deploy/runbooks/attestation-failure.md"]
    deliv --> rb2["deploy/runbooks/fleet-health.md"]
    aut --> rb3["deploy/runbooks/sgu-recovery.md"]
    guard --> rb4["deploy/runbooks/pos-ingest.md"]
```

**On "acknowledgement".** There is no in-platform alert-acknowledgement
endpoint. The route table has no such route, and neither the controller's
`ComplianceAlerts`/`OperationalAlerts` lists nor the registry expose one — those
lists are bounded rings an operator reads, not a workflow. Acknowledgement
happens in Alertmanager, and every alert rule carries a `runbook_url` pointing
into `deploy/runbooks/`. That is the truth of the code; a reader expecting an
"acknowledge" button will not find one.

---

## A2. The field technician's day

### A2.1 Battery replacement

```mermaid
flowchart TB
    list["Work list from<br/>GET /v1/stores/{id}/runway"] --> why["BatteryRunway is a least-squares fit<br/>over reported charge, not a two-point<br/>extrapolation — the number dispatches<br/>a van, so it has to survive noise"]
    why --> find["Find the label:<br/>locator LED, pulsed for milliseconds<br/>and only when a human is expected to act"]
    find --> swapcell["Swap the cell"]
    swapcell --> rejoin["The label rejoins the mesh<br/>and reports telemetry"]
    rejoin --> derive["HealthPolicy.DeriveState moves it<br/>degraded or offline back to active"]
    derive --> caveat["Health is derived. Quarantine,<br/>retirement and assignment are decisions<br/>and are never undone by a heartbeat."]
```

### A2.2 Device swap and commissioning

```mermaid
flowchart TB
    subgraph retire_s["Retiring the dead unit"]
        r1["POST /v1/devices/{id}/retire"]
        r2["The registry clears the retained config<br/>and price on the old zone topic"]
        r3["Retired is terminal. A refurbished unit<br/>returns with a new certificate<br/>and a new manifest entry."]
        r1 --> r2 --> r3
    end

    subgraph commission_s["Commissioning the replacement"]
        p1["Clip the new label onto the rail"]
        p2["First power-on announcement,<br/>relayed by the controller"]
        p3["Zero-touch provisioning — see 05 section 2"]
        p4{"Manifest matched?<br/>Identity unique?"}
        pq["Quarantined. Escalate:<br/>two things claim one identity,<br/>and both are out of service<br/>until someone walks the aisle."]
        p5["Retained DeviceConfig pushed:<br/>topics, cadences, key ring"]
        p6["The stored planogram already declares<br/>this position, so the label is bound<br/>immediately"]
        p7["First price delivered and confirmed<br/>before the technician reaches<br/>the end of the aisle"]
        pb["Commissioning also computes the battery<br/>projection and raises<br/>device.battery.projection.short<br/>while the technician is still present"]
        p1 --> p2 --> p3 --> p4
        p4 -->|"no"| pq
        p4 -->|"yes"| p5 --> p6 --> p7
        p5 --> pb
    end

    r3 --> p1
```

**Quarantine is a one-way door back to the beginning.**
`POST /v1/devices/{id}/release` returns a quarantined device to `provisioned`,
not to whatever it was doing when it was seized: releasing it restores its
operational life from the start rather than restoring a state nobody has
re-verified.

---

## A3. The pricing analyst's flow

```mermaid
flowchart TB
    subgraph get_s["Getting a recommendation"]
        g1["Point-in-time feature store"]
        g2["Tier 2: per-store demand model plus<br/>expected-margin optimiser, 8 to 15 ms"]
        g3["Candidates are drawn from Tier 1's<br/>feasible set, so nothing has to be<br/>clamped afterwards"]
        g4{"Close substitutes<br/>in the category?"}
        g5["Tier 3: coordinate descent across<br/>substitutes with cross-elasticity terms"]
        g6["Reports the independent Tier-2 sum<br/>alongside the coordinated answer,<br/>so the cannibalisation adjustment<br/>is visible rather than buried"]
        g7["Recommendation with a rationale<br/>and a confidence flag"]
        g1 --> g2 --> g3 --> g4
        g4 -->|"no"| g7
        g4 -->|"yes"| g5 --> g6 --> g7
    end

    subgraph commit_s["Committing it"]
        c1["POST /v1/pricing/simulate"]
        c2{"Inside the tenant's<br/>Tier-1 guard rails?"}
        c3["Adjust, or change the rule:<br/>POST /v1/pricing/rules"]
        c4["Commit the price change"]
        c5["The ordinary price path — 05 section 1"]
        c1 --> c2
        c2 -->|"no"| c3 --> c1
        c2 -->|"yes"| c4 --> c5
    end

    subgraph measure_s["Measuring it"]
        m1["Analytics price_updates table:<br/>units_sold, waste_units,<br/>price_delay_seconds"]
        m2["Promotion lift measurement"]
        m1 --> m2
    end

    start(["A category is under-performing"]) --> g1
    g7 --> c1
    c5 --> m1
    m2 --> start
```

**The caveat an analyst must be told.** Every model in `pricing/ml` — the
elasticity regression, the gradient-boosted trees, the isolation forest and the
LSTM — is trained and validated on synthetic data generated by its own tests,
against known-truth generators. The algorithms are tested; no model here has
seen a real retailer's transactions, and none of the demand curves should be
quoted as a finding about retail.

---

## A4. Integration engineer onboarding a new POS

```mermaid
flowchart TB
    subgraph build_s["Writing an adapter — the seam is four methods"]
        b1["Name — the stable identifier that appears<br/>in metrics, Envelope.Source and binding<br/>configuration. Never renamed."]
        b2["Verify — authentication, first,<br/>on the raw bytes exactly as received"]
        b3["IdempotencyParts — vendor message identity,<br/>also on the raw bytes, so a redelivery is<br/>recognised without paying to parse it"]
        b4["Ingest — parse and schema-map to<br/>canon.PriceChangeRequested"]
        b5["Register at start-up. A duplicate name is<br/>refused outright, not resolved<br/>last-write-wins."]
        b1 --> b2 --> b3 --> b4 --> b5
    end

    subgraph cfg_s["Configuring the binding"]
        f1["Secrets: HMAC key or mTLS subject.<br/>Never a header the caller controls."]
        f2["Currency default and allowed set"]
        f3["Store map: source store code to canonical<br/>StoreID, in one place, so a renumbered<br/>estate changes one binding"]
        f4["Rate limit keyed tenant, adapter, binding —<br/>a nightly file drop must not starve<br/>the webhooks"]
        f5["RetainRaw, when the payloads must be<br/>kept for triage"]
        f1 --> f2 --> f3 --> f4 --> f5
    end

    subgraph test_s["Proving it"]
        t1["Send a real delivery"]
        t2{"Result status"}
        t3["Accepted or partial:<br/>GET /v1/pos/deliveries for the<br/>emitted count and row failures"]
        t4["Quarantined — a 4xx with the body<br/>retained. Fix the mapping."]
        t5["Operator replay: re-ingest past the<br/>dedupe guard, marked ReplayOf<br/>and ReplayCount"]
        t6["Rejected — it will never parse.<br/>The source is wrong, and the platform<br/>must not answer 500 to it."]
        t1 --> t2
        t2 -->|"accepted"| t3
        t2 -->|"quarantined"| t4 --> t5 --> t1
        t2 -->|"rejected"| t6
    end

    s(["A retailer's POS or ERP<br/>needs to reach USSLP"]) --> q{"Is there an adapter<br/>for this vendor?"}
    q -->|"no"| b1
    q -->|"yes, one of nine"| f1
    b5 --> f1
    f5 --> t1
    t3 --> live(["Live"])
```

**Why the four-method split is the whole design.** An adapter is a parser and a
signature check. It does not get to decide how deduplication works, whether a
store code is resolved, what a 4xx means, or when a caller is acknowledged. Nine
adapters with nine slightly different opinions about idempotency would be nine
different production incidents.

---

## A5. The operator's incident flow

```mermaid
flowchart TB
    page(["Page fires"]) --> which{"Which alert?"}

    which -->|"USSLPPricePathErrorBudgetBurnFast"| p1
    which -->|"USSLPStoresAutonomousMany"| p2
    which -->|"USSLPAttestationFailure or<br/>USSLPControllerComplianceRefusal"| p3
    which -->|"USSLPOTARollbackTriggered"| p4
    which -->|"USSLPGatewayUpstreamBreakerOpen"| p5
    which -->|"USSLPConsumerLagCritical or<br/>USSLPDeadLetterQueueGrowing"| p6

    p1["Price path latency"] --> p1a["The SLO report gives the hop table<br/>plus clock_drift_ms, so the error bar<br/>travels with the number"]
    p1a --> p1b{"Cloud share<br/>or edge share?"}
    p1b -->|"cloud"| p1c["runbooks/price-path-latency.md:<br/>consumer lag, device publish duration,<br/>signing throughput"]
    p1b -->|"edge"| p1d["A saturated store queues at the radio.<br/>Eight in flight per controller is the<br/>ceiling. This may be correct behaviour."]

    p2["Many stores autonomous"] --> p2a["A WAN or broker event, not a shelf event.<br/>The shelves are still trading."]
    p2a --> p2b["runbooks/mqtt-broker.md,<br/>runbooks/sgu-recovery.md"]
    p2b --> p2c["Watch USSLPSGUUpstreamQueueNearFull:<br/>it means critical evidence is about<br/>to be dropped and the cloud's record<br/>of the outage will have a hole"]

    p3["Attestation refusal"] --> p3a{"Verdict on the ack"}
    p3a -->|"unknown-key-id or<br/>key-outside-validity"| p3b["A stale key ring. Redistribute it.<br/>Not a compliance incident."]
    p3a -->|"digest-mismatch"| p3c["The price on the wire is not the price<br/>that was signed. Compliance incident.<br/>runbooks/attestation-failure.md"]
    p3a -->|"status 4, unattested frame"| p3d["Fleet configuration: the controller needs<br/>updating. Operational queue, not the<br/>compliance one."]

    p4["OTA rolled back"] --> p4a{"Which gate fired?"}
    p4a -->|"error rate"| p4b["The image fails outright"]
    p4a -->|"boot failure"| p4c["Devices must be physically retrieved"]
    p4a -->|"silence"| p4d["The worst case: devices took it<br/>and stopped speaking"]
    p4a -->|"battery anomaly"| p4e["A bug in the sleep path"]
    p4b --> p4f["runbooks/ota-rollout.md and<br/>runbooks/rollback.md. No halted-to-running<br/>edge exists: a new job with a new<br/>artifact is the way forward."]
    p4c --> p4f
    p4d --> p4f
    p4e --> p4f

    p5["Upstream breaker open"] --> p5a["The gateway is shedding to one service;<br/>the others are unaffected.<br/>runbooks/cloud-api-availability.md"]

    p6["Stream health"] --> p6a["Lag, retry storm or poison records.<br/>A dead-letter record will never parse —<br/>triage it, do not replay it blindly."]

    p1c --> post
    p1d --> post
    p2c --> post
    p3b --> post
    p3c --> post
    p3d --> post
    p4f --> post
    p5a --> post
    p6a --> post
    post(["Post-incident: was the budget<br/>the wrong shape, or the platform?"])
```

**Check this before believing a latency number.** Every latency the platform
reports is the difference between a wall-clock timestamp the cloud stamped and a
simulated-clock timestamp the controller stamped. `edge/sim` is a discrete-event
engine whose clock only moves when an event fires, so on a quiet store a price
arriving during a lull is timestamped in the past and its latency is
under-reported — far enough, on a two-core container, to come out negative.
`usslpd` bounds it with a 20 ms heartbeat per store engine, which brings observed
drift under 120 ms, and publishes `clock_drift_ms` in every SLO report.

---

# Part B — the platform's own decision flows

## B1. Partial versus full E-Ink refresh

Three components decide, in order, and the **label has the last word**. A full
waveform is ~1,500 ms on the 2.9″ tier and roughly a hundred times the energy of
anything else a label does; a partial is 300 ms. Inside a three-second budget
whose largest single line item is the refresh, that 1.2 s is the difference
between meeting the SLO and missing it — and on a shelf of forty labels updating
together, the flash is the difference between "the prices changed" and
"something is wrong with the shelf".

### B1.1 Cloud — `label/domain.DecideRender` decides whether to *offer* a partial

```mermaid
flowchart TB
    c0["A price change is accepted"] --> c1{"Has the label ever<br/>displayed a price?"}
    c1 -->|"no"| f1["FULL. Nothing cached on the controller<br/>and nothing on the glass."]
    c1 -->|"yes"| c2{"PartialsSinceFull has reached<br/>Policy.FullRefreshEvery, default 8?"}
    c2 -->|"yes"| f2["FULL, to clear accumulated ghosting"]
    c2 -->|"no"| c3{"Template, badge, LED colour<br/>or show-was changed?"}
    c3 -->|"yes"| f3["FULL. The layout moved, so the cached<br/>framebuffer for those regions<br/>is no longer valid."]
    c3 -->|"no"| c4{"Currency changed?"}
    c4 -->|"yes"| f4["FULL. The symbol moves the whole field."]
    c4 -->|"no"| c5{"Same amount as displayed?"}
    c5 -->|"yes"| f5["FULL. Redraw from a known state rather<br/>than partially rewriting nothing."]
    c5 -->|"no"| c6{"Rendered width unchanged?<br/>9.99 to 10.99 is not"}
    c6 -->|"no"| f6["FULL. The digit field is re-laid out and<br/>the partial window would not cover it."]
    c6 -->|"yes"| offer["OFFER a partial:<br/>RenderSpec.PartialRefresh = true"]
```

At the default of 8, a label repriced twice a day is fully cleared every four
days, and a promotion-heavy label repriced hourly every eight hours — inside the
ghosting threshold measured on the platform's panels with margin.

### B1.2 Controller — `sec.DecidePartial` decides from a real pixel diff

```mermaid
flowchart TB
    s1{"Does the panel support<br/>a partial waveform?"} -->|"no, the 5.85 in colour tier"| g1["FULL"]
    s1 -->|"yes"| s2{"Was a partial offered<br/>by the render spec?"}
    s2 -->|"no"| g2["FULL"]
    s2 -->|"yes"| s3{"Is there a previous image<br/>on file for this label?"}
    s3 -->|"no"| g3["FULL"]
    s3 -->|"yes"| s4{"Panel geometry changed?"}
    s4 -->|"yes"| g4["FULL"]
    s4 -->|"no"| s5{"Did any pixel change?"}
    s5 -->|"no"| g5["FULL"]
    s5 -->|"yes"| s6{"Does the change touch<br/>the colour plane?"}
    s6 -->|"yes"| g6["FULL. Red particles need the full<br/>waveform however few pixels move,<br/>or last week's SALE badge leaves<br/>a pink smear."]
    s6 -->|"no"| s7{"More than 25 percent<br/>of pixels changed?"}
    s7 -->|"yes"| g7["FULL"]
    s7 -->|"no"| s8{"Does the changed region span<br/>more than 50 percent of the panel?"}
    s8 -->|"yes"| g8["FULL. A partial over most of the panel<br/>costs nearly as much energy and<br/>leaves the panel worse."]
    s8 -->|"no"| part["PARTIAL. Transmit only<br/>the changed window."]
```

### B1.3 Label — `planRefresh` has the last word

```mermaid
flowchart TB
    l1{"Partial requested and<br/>the panel supports one?"} -->|"no"| lf1["FULL waveform, 1500 ms on 2.9 in"]
    l1 -->|"yes"| l2{"partialsSinceFull has reached<br/>the panel's MaxPartials, 8?"}
    l2 -->|"yes"| lf2["FULL, and ForcedFull is reported upward<br/>so the controller's energy model learns<br/>it did not get what it asked for"]
    l2 -->|"no"| l3{"Below about minus 10 C?<br/>a driver-level rule the policy cannot know"}
    l3 -->|"yes"| lf3["FULL. A single-phase drive does not<br/>complete at that temperature and<br/>produces a smeared digit."]
    l3 -->|"no"| lp["PARTIAL waveform, 300 ms"]
```

**Why the label has the last word.** Only the label knows how many partials
actually reached the glass; the controller's count is an estimate that a lost
frame, a reboot or a manual refresh invalidates. A disagreement in the
controller's favour means a shopper can read the previous price ghosted behind
the current one — a weights-and-measures problem, not a cosmetic one. The
counter is persisted alongside the sequence because an E-Ink panel is bistable,
so the residue survives a reboot even though RAM does not.

---

## B2. The three-tier pricing decision

```mermaid
flowchart TB
    subgraph tier1["Tier 1 — rules engine: pure, deterministic, sub-millisecond"]
        a1["Constraints: floor, ceiling, margin,<br/>rounding, statutory limits"]
        a2["Feasible set"]
        a3["Decision record: the price and why"]
        a1 --> a2 --> a3
    end

    subgraph tier2["Tier 2 — edge ML, 8 to 15 ms"]
        b1["Per-store demand model:<br/>gradient-boosted trees plus<br/>own-price elasticity"]
        b2["Expected-margin optimiser over candidates"]
        b3["Candidates are drawn from Tier 1's<br/>feasible set, so the answer never has<br/>to be clamped afterwards"]
        b1 --> b2 --> b3
    end

    subgraph tier3["Tier 3 — cloud optimisation, every 15 minutes"]
        d1["Sequence-model forecast replaces the<br/>trailing velocity as the baseline"]
        d2["Coordinate descent across substitutes,<br/>with cross-elasticity terms"]
        d3["Reports baseline, the naive Tier-2 sum,<br/>and the coordinated profit — the<br/>difference is what justifies the pass"]
        d1 --> d2 --> d3
    end

    ask(["What should this price be?"]) --> when{"How much time<br/>is available?"}
    when -->|"sub-millisecond,<br/>every price, always"| a1
    when -->|"8 to 15 ms, per store"| b1
    when -->|"asynchronous"| d1
    b3 --> a1
    d3 --> b1
    a3 --> mode{"Is the store<br/>autonomous?"}
    mode -->|"no"| cloudp["The cloud path prices it"]
    mode -->|"yes"| sgup["The same Tier-1 code runs inside the<br/>Store Gateway Unit from a compact<br/>policy pack, so the store reaches the<br/>identical decision the cloud would have"]
```

**Why Tier 3 is a separate pass and not Tier 2 in a loop.** Given a category of
close substitutes, Tier 2 will happily recommend discounting all of them: each
recommendation looks like it wins volume, and collectively they win nothing but
a lower average selling price. The cross-elasticity term is the only thing that
makes the category-level answer differ from the sum of the line-level answers.

---

## B3. Promotion conflict resolution

`promotion/domain.Resolve`. A documented contract rather than an implementation
detail, because a retailer's finance team reconciles against it.

```mermaid
flowchart TB
    inp(["N promotions match one product"]) --> base["Price every candidate against the BASE<br/>price first, so the comparison ranks them<br/>on equal terms. Ranking on already-stacked<br/>prices would make the order depend on itself."]
    base --> r1{"1. Highest priority?"}
    r1 -->|"one wins"| winner
    r1 -->|"tie"| r2{"2. Best for the customer —<br/>the lower shelf price?"}
    r2 -->|"one wins"| winner
    r2 -->|"tie"| r3{"3. Most specific — a SKU list beats<br/>a brand, a brand beats a category,<br/>a category beats everything?"}
    r3 -->|"one wins"| winner
    r3 -->|"tie"| r4["4. Promotion id ascending. Arbitrary but<br/>STABLE, so two nodes evaluating the same<br/>inputs reach the same answer, which map<br/>iteration order would deny them."]
    r4 --> winner

    winner["Winner: owns the badge, the LED<br/>colour and the display"] --> stack{"Is the current winner<br/>stackable?"}
    stack -->|"no"| stop["The chain ends. Every other match is<br/>recorded as Suppressed, with a reason<br/>and the promotion that beat it."]
    stack -->|"yes"| nextc{"Next candidate in the order:<br/>same exclusive group?"}
    nextc -->|"yes"| skip["Never both apply. Suppressed."]
    skip --> stack
    nextc -->|"no"| applyp["Apply on top of the price<br/>the previous one produced"]
    applyp --> stack

    stop --> out["Resolution: applied list, suppressed list,<br/>final price, total discount,<br/>winner, RenderSpec"]
    out --> shelf{"Does the winning mechanic produce<br/>a definite shelf price?"}
    shelf -->|"yes"| show["Show the discounted price"]
    shelf -->|"no — THRESHOLD depends<br/>on the whole basket"| badge["Drive a badge and an LED and keep the<br/>undiscounted price. It is the only thing<br/>that will match the till, and displaying<br/>see till for price fails a price-marking<br/>inspection."]
```

**Why "best for the customer" is rule 2 and not rule 4.** A customer who sees
two offers and gets the worse one has a complaint that is usually also a
regulatory one, whereas giving the better one is at worst a margin decision.

**Why suppression is recorded rather than discarded.** An operator's first
question when a promotion does not show on a shelf is "why", and `Suppressed`
with its `Reason` and `BeatenBy` is the answer.

**Where this flow does *not* run today: at the shelf.** `Resolve` needs the whole
active set, and only the Promotion Service holds it. `promotion-events` carries
the rule but not `Resolve`'s output, so the Label Service's promotion consumer
prices each activation against the label's base price on its own. Where two
promotions overlap, the shelf therefore shows **the most recently activated
one** — it takes the higher per-label sequence — rather than the winner this
flow would pick. The fix is to put the resolved outcome on the event; see
[05 §3](05-sequence-diagrams.md#3-store-wide-promotion-fan-out). Two further
constraints the shelf tier applies before it prices anything: a rule scoped by
`store_groups`, `min_inventory` or `max_days_to_expiry` is **refused by name**,
because the shelf has never been told those attributes and both ways of guessing
are wrong; and `THRESHOLD` or customer-segmented rules are skipped as not
shelf-priceable.

---

## B4. Autonomous-mode entry and exit hysteresis

`edge/sgu/wan.go`. Deliberately asymmetric.

```mermaid
flowchart TB
    subgraph connected_s["Mode: connected"]
        k1["Every 5 s: read link state,<br/>then a QoS 1 probe that must<br/>be acknowledged"]
        k2{"Probe result"}
        k3["consecFail = 0, consecOK++"]
        k4["consecOK = 0, consecFail++,<br/>record firstFail"]
        k5{"consecFail at least 3 AND at least<br/>12 s since firstFail?"}
        k1 --> k2
        k2 -->|"acknowledged"| k3 --> k1
        k2 -->|"failed"| k4 --> k5
        k5 -->|"no"| k1
    end

    subgraph enter_s["Entering autonomy"]
        e1["Record divergedAt on the<br/>hybrid logical clock"]
        e2["Arm cloud-state collection for the<br/>whole outage, not just at recovery"]
        e3["Stop bridging upstream.<br/>The local broker never stops."]
        e4["Buffer upstream in order, with the<br/>three sacrifice classes"]
        e5["Announce store.mode.autonomous"]
        e1 --> e2 --> e3 --> e4 --> e5
    end

    subgraph autonomous_s["Mode: autonomous"]
        n1["Every 5 s: probe"]
        n2{"Probe result"}
        n3["consecOK = 0"]
        n4["consecFail = 0, consecOK++,<br/>record firstOK"]
        n5{"consecOK at least 4 AND at least<br/>15 s since firstOK?"}
        n1 --> n2
        n2 -->|"failed"| n3 --> n1
        n2 -->|"acknowledged"| n4 --> n5
        n5 -->|"no"| n1
    end

    subgraph leave_s["Leaving autonomy"]
        v1["Re-subscribe. MQTT 3.1.1 section 3.8.4<br/>makes the cloud redeliver its<br/>retained state."]
        v2["Hold cloud prices in a 3 s settle buffer"]
        v3["Merge per key against divergedAt"]
        v4["Flush the buffer in order,<br/>skipping the durable sent-index"]
        v5["Announce store.mode.reconciled with<br/>counts, conflicts and outage seconds"]
        v1 --> v2 --> v3 --> v4 --> v5
    end

    k5 -->|"yes"| e1
    e5 --> n1
    n5 -->|"yes"| v1
    v5 --> k1
    force["Operator override: Detector.ForceMode,<br/>for planned WAN maintenance"] -.-> e1
    force -.-> v1
```

**Why the thresholds differ.** Entering autonomy is cheap and reversible and
should happen quickly once the evidence is unambiguous — three probes at five
seconds is fifteen seconds of unambiguous evidence, long enough that a
two-second blip cannot trigger it and short enough that a store notices a real
outage before the first scheduled promotion of the morning. Leaving it means
replaying a buffer and running a merge, so it waits longer: a flapping link
declared healthy causes a reconciliation, and a reconciliation interrupted
halfway is the one genuinely messy state in this design.

**Why `FailFor` and `RecoverFor` exist alongside the counts.** They are minimum
wall-clock spans, so shortening `Interval` for a test does not accidentally
shorten the hysteresis.

**Why the probe is a round trip.** The failure it has to catch is the one a
connection state cannot see: a TCP session to a load balancer that is still open
while everything behind it is gone.

**Why there is an operator override.** An engineer who knows the WAN is about to
be cut for maintenance can put a store into autonomy deliberately rather than
let it discover the outage. Relatedly, `deploy/edge/update.sh` refuses to update
a store that is currently autonomous, because restarting the gateway then takes
down the only thing pricing that store's shelves.
