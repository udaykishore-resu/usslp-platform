# Diagram index

Every diagram in the repository, in one place. **84 diagrams**, all Mermaid, all
rendered by GitHub without a plugin. They are indexed here by *kind* rather than
by document, because the question people actually arrive with is "is there a
picture of the price path" and not "which numbered file is that in".

Nothing on this page is a diagram. It is a table of contents; the diagrams live
next to the prose that makes them true, which is where they should stay.

| Kind | Count | Where they mostly are |
|---|---:|---|
| [Context and container](#context-and-container) | 4 | `01-context`, `02-containers` |
| [Component](#component) | 7 | `03-components` |
| [Block — hardware, firmware, deployment](#block--hardware-firmware-deployment) | 13 | `04-block-diagrams`, `operations`, `scalability` |
| [Sequence](#sequence) | 19 | `05-sequence-diagrams` |
| [State machine](#state-machine) | 5 | `06-data-architecture`, ADR 0016 |
| [Entity relationship](#entity-relationship) | 1 | `06-data-architecture` |
| [Flowchart and decision tree](#flowchart-and-decision-tree) | 35 | `07-flows`, `security`, `observability`, runbooks |

**Every diagram here has been checked against the code**, not against the
document that surrounds it. Where the two disagreed, the code won and the
diagram was changed. Where a figure is modelled rather than measured, the
diagram says so on the node rather than in a caption underneath it.

---

## Context and container

The system from outside, then the boxes inside it. C4 levels 1 and 2.

| Diagram | Document | What it answers |
|---|---|---|
| Context | [`01-context.md`](01-context.md#context-diagram) | Who touches the platform, and where the trust anchors are — including the supply chain, which is a trust anchor and not a logistics detail |
| Cloud tier | [`02-containers.md`](02-containers.md#diagram-1--the-cloud-tier) | The eight cloud services, the streams between them, and which services *consume* streams as well as publish to them |
| Store and device tiers | [`02-containers.md`](02-containers.md#diagram-2--the-store-and-device-tiers) | The gateway, the controllers and the labels, and the LAN boundary the cloud never crosses |
| Two deployment shapes | [`02-containers.md`](02-containers.md#two-legitimate-deployment-shapes) | One process against nine, and why both are legitimate rather than one being a test fixture |

## Component

C4 level 3. Each cloud service has the same shape — ports, adapters, an app
layer and a domain — so these are worth reading as a set rather than singly.

| Diagram | Document |
|---|---|
| Label Service | [`03-components.md`](03-components.md#1-label-service) |
| Universal Integration Gateway | [`03-components.md`](03-components.md#2-universal-integration-gateway) |
| Device Registry | [`03-components.md`](03-components.md#3-device-registry) |
| OTA Service | [`03-components.md`](03-components.md#4-ota-service) |
| Pricing Service | [`03-components.md`](03-components.md#5-pricing-service) |
| Store Gateway Unit | [`03-components.md`](03-components.md#6-store-gateway-unit) |
| Shelf Edge Controller | [`03-components.md`](03-components.md#7-shelf-edge-controller) |

## Block — hardware, firmware, deployment

Physical and topological. What is on the board, what is in flash, what runs on
which machine, and what scales against what.

| Diagram | Document | What it answers |
|---|---|---|
| The four tiers | [`04-block-diagrams.md`](04-block-diagrams.md#1-the-four-tiers) | The whole estate in one frame, with the radio and MQTT parameters on the arrows |
| Smart-label PCB | [`04-block-diagrams.md`](04-block-diagrams.md#2-smart-label-pcb) | Every part, bus and GPIO line, checked against the devicetree overlay |
| Flash and OTA partition map | [`04-block-diagrams.md`](04-block-diagrams.md#flash-and-ota-partition-map) | Why two full-size slots and not swap-move; what NVS actually holds |
| Zephyr firmware layer stack | [`04-block-diagrams.md`](04-block-diagrams.md#3-zephyr-firmware-layer-stack) | Which source file sits at which layer |
| The verification boundary | [`04-block-diagrams.md`](04-block-diagrams.md#the-verification-boundary) | The fourteen modules that are genuinely Zephyr-free, and therefore testable on a host |
| The one ordering that matters | [`04-block-diagrams.md`](04-block-diagrams.md#the-one-ordering-that-matters) | The seven steps of `app/price.c`, and why persisting the sequence must precede driving the panel |
| Shelf Edge Controller appliance | [`04-block-diagrams.md`](04-block-diagrams.md#4-shelf-edge-controller) | The controller as a box on a shelf rather than a pod |
| Store Gateway Unit appliance | [`04-block-diagrams.md`](04-block-diagrams.md#5-store-gateway-unit) | Every file in `edge/sgu` and what it is for |
| Deployment topology | [`operations.md`](operations.md#1-deployment-topology) | Namespaces, workloads and what runs where |
| Regions, and what is shed first | [`operations.md`](operations.md#priority-classes-the-platforms-statement-of-what-it-will-lose) | What is global, what is regional, and the eviction order the platform commits to |
| Scaling units and fan-out | [`scalability.md`](scalability.md#25-connection-counts) | What scales on replicas, what is per-store, what is fixed, and the multipliers between them |
| Modelled against measured | [`scalability.md`](scalability.md#4-the-gap-between-section-2-and-section-3-stated-plainly) | The ratio between the capacity model and what has actually been run — about 1,380× on throughput |
| Edge fleet and firmware rollout | [`operations.md`](operations.md#firmware) | Two update mechanisms that look alike and share nothing |

## Sequence

Time on the vertical. These carry the latency budgets, and §1 is the one to read
first: it is the price path end to end, and every hop in it is a line item in
[`INTERFACE-CONTRACTS.md`](INTERFACE-CONTRACTS.md) that `TestLatencyBudgetMatchesTheContract`
enforces.

| Diagram | Document |
|---|---|
| Real-time price update — POS to pixels | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#1-real-time-price-update--pos-to-pixels) |
| Zero-touch provisioning — factory to first display | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#2-zero-touch-device-provisioning--factory-to-first-display) |
| Store-wide promotion fan-out, and its expiry | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#3-store-wide-promotion-fan-out) (two diagrams) |
| Outage — detection and entry | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#4a-detection-and-entry) |
| Outage — operating alone | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#4b-operating-alone) |
| Outage — recovery and reconciliation | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#4c-recovery-and-reconciliation) |
| Staged OTA rollout, with cohort-failure rollback | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#5-staged-ota-rollout-including-the-cohort-failure-rollback) |
| Attestation verification, including refusal | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#6-price-attestation-verification-including-refusal) |
| Mesh healing — predictive | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#7a-predictive--before-the-link-fails) |
| Mesh healing — reactive | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#7b-reactive--after-a-relay-dies) |
| POS ingest, with a redelivered webhook | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#8-pos-ingest-through-the-uig-with-a-redelivered-webhook) |
| Batch fan-out — worker pool and rate limiting | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#9-batch-price-fan-out--worker-pool-and-per-tenant-rate-limiting) |
| NFC shopper tap | [`05-sequence-diagrams.md`](05-sequence-diagrams.md#10-nfc-shopper-tap) |
| Price-authority key rotation | [`security.md`](security.md#6-key-rotation-and-the-price-authority-key-ring) |
| Trace propagation | [`observability.md`](observability.md#the-propagation-path) |
| One price change, as telemetry | [`observability.md`](observability.md#7-dashboards) |
| The chain of custody | [ADR 0004](../adr/0004-end-to-end-price-attestation.md#the-chain-of-custody) |
| Cold start from retained messages | [ADR 0015](../adr/0015-retained-messages-for-cold-start.md#decision) |

## State machine

Five lifecycles with enumerated transitions. Each is generated from the same
tables the code enforces, which is why two OTA edges are deliberately absent.

| Diagram | Document |
|---|---|
| Label lifecycle | [`06-data-architecture.md`](06-data-architecture.md#81-label-lifecycle) |
| Device lifecycle | [`06-data-architecture.md`](06-data-architecture.md#82-device-lifecycle) |
| Promotion lifecycle | [`06-data-architecture.md`](06-data-architecture.md#83-promotion-lifecycle) |
| OTA rollout | [`06-data-architecture.md`](06-data-architecture.md#84-ota-rollout) |
| OTA job states, and the two forbidden edges | [ADR 0016](../adr/0016-staged-ota-with-signed-manifest.md#the-state-machine-forbids-two-edges-deliberately) |

## Entity relationship

| Diagram | Document |
|---|---|
| Logical domain model | [`06-data-architecture.md`](06-data-architecture.md#7-logical-domain-model) |

## Flowchart and decision tree

The largest group, and the most heterogeneous: data flow, human workflow,
branching policy, and two runbook triage trees.

### Data and streams

| Diagram | Document |
|---|---|
| The event streams | [`06-data-architecture.md`](06-data-architecture.md#1-the-event-streams) |
| The CQRS write/read split | [`06-data-architecture.md`](06-data-architecture.md#2-the-cqrs-writeread-split) |
| The columnar analytics store | [`06-data-architecture.md`](06-data-architecture.md#5-the-columnar-analytics-store) |
| Edge durable stores | [`06-data-architecture.md`](06-data-architecture.md#6-edge-durable-stores) |
| The two paths — command and telemetry | [ADR 0001](../adr/0001-event-sourced-cqrs-for-the-price-path.md#decision) |

### Human flows

| Diagram | Document |
|---|---|
| Morning health check | [`07-flows.md`](07-flows.md#a11-morning-health-check) |
| Running a promotion | [`07-flows.md`](07-flows.md#a12-running-a-promotion) |
| Acknowledging an alert | [`07-flows.md`](07-flows.md#a13-acknowledging-an-alert) |
| Battery replacement | [`07-flows.md`](07-flows.md#a21-battery-replacement) |
| Device swap and commissioning | [`07-flows.md`](07-flows.md#a22-device-swap-and-commissioning) |
| The pricing analyst's flow | [`07-flows.md`](07-flows.md#a3-the-pricing-analysts-flow) |
| Onboarding a new POS | [`07-flows.md`](07-flows.md#a4-integration-engineer-onboarding-a-new-pos) |
| The operator's incident flow | [`07-flows.md`](07-flows.md#a5-the-operators-incident-flow) |

### Decision policy

The three refresh diagrams are one decision made in three places, and they are
worth reading together: the cloud *offers* a partial, the controller decides
from a real pixel diff, and the label has the last word.

| Diagram | Document |
|---|---|
| Cloud — whether to offer a partial | [`07-flows.md`](07-flows.md#b11-cloud--labeldomaindeciderender-decides-whether-to-offer-a-partial) |
| Controller — the real pixel diff | [`07-flows.md`](07-flows.md#b12-controller--secdecidepartial-decides-from-a-real-pixel-diff) |
| Label, then the waveform driver | [`07-flows.md`](07-flows.md#b13-label--planrefresh-then-the-waveform-driver-have-the-last-word) |
| The same decision, as an ADR | [ADR 0017](../adr/0017-eink-partial-refresh-policy.md#the-label-decides-labelsimplanrefresh--displayusslp_render_policyc) |
| The three-tier pricing decision | [`07-flows.md`](07-flows.md#b2-the-three-tier-pricing-decision) |
| Promotion conflict resolution | [`07-flows.md`](07-flows.md#b3-promotion-conflict-resolution) |
| Autonomous-mode hysteresis | [`07-flows.md`](07-flows.md#b4-autonomous-mode-entry-and-exit-hysteresis) |
| Edge-first tiering | [ADR 0003](../adr/0003-edge-first-architecture.md#decision) |
| Hexagonal layout | [ADR 0010](../adr/0010-go-and-hexagonal-architecture.md#the-layout) |
| Three isolation models | [ADR 0012](../adr/0012-multi-tenancy-and-the-tenant-boundary.md#part-three-three-isolation-models-chosen-per-tenant) |

### Security

| Diagram | Document |
|---|---|
| The PKI hierarchy | [`security.md`](security.md#2-the-pki-hierarchy) |
| The signing key rings | [`security.md`](security.md#2-the-pki-hierarchy) |
| Encryption and trust boundaries per hop | [`security.md`](security.md#4-mtls-everywhere-and-the-encryption-matrix-per-hop) |
| The tenant boundary — every enforcement point | [`security.md`](security.md#5-the-tenant-boundary) |

### Scale

| Diagram | Document |
|---|---|
| Where the capacity numbers come from | [`scalability.md`](scalability.md#2-the-capacity-model-derived) |
| Partition sizing and per-key ordering | [`scalability.md`](scalability.md#23-partition-sizing) |

### Operations and observability

| Diagram | Document |
|---|---|
| GitOps and the supply chain | [`operations.md`](operations.md#2-gitops-flow) |
| Progressive rollout — canary steps and gates | [`operations.md`](operations.md#cloud-price-path-services) |
| The signal pipeline — metrics, traces, logs | [`observability.md`](observability.md#usslp-observability) |
| SLO and error-budget mechanism | [`observability.md`](observability.md#6-multi-window-multi-burn-rate-alerting) |

### Runbook triage

| Diagram | Runbook |
|---|---|
| Attestation failure — which side failed | [`attestation-failure.md`](../../deploy/runbooks/attestation-failure.md#first-which-side-failed) |
| OTA rollout — pause first, diagnose second | [`ota-rollout.md`](../../deploy/runbooks/ota-rollout.md#runbook--ota-rollout) |

---

## Keeping this honest

Two properties are worth preserving as the tree moves:

**Every block parses.** All 84 are valid Mermaid against the current parser. A
diagram that fails to parse renders on GitHub as a grey box of source, which is
worse than no diagram, and it fails silently.

**Every node is traceable.** A node names a package, a file, a metric, a topic,
an alert or a constant that exists. Where a figure is a model rather than a
measurement — the energy budget, the capacity chain, the airtime numbers — the
diagram says so on the node itself. A diagram is a claim about the system, and
an untraceable node is an unfalsifiable claim.
