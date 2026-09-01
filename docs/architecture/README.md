# USSLP core architecture

The architecture of the Universal Smart Shelf Label Platform, described from the
code that implements it.

This set exists alongside — and never contradicts —
[`INTERFACE-CONTRACTS.md`](INTERFACE-CONTRACTS.md), which is normative. Where a
document here needs a stream name, a QoS, a topic shape, a latency line item or
an attestation rule, it references the contract rather than restating it. Where
the contract and the code disagree, these documents describe the code and say so
in a *Divergence* note.

## Ground rules used throughout

- **Measured numbers are labelled as such.** Every latency, percentile and
  battery figure quoted here comes from `test/e2e`, `test/load`, the root
  `README.md` "Measured numbers" section, or `firmware/README.md`. They were
  measured **on a 2-core container with the Tier 1 and Tier 2 hardware
  simulated at 1:1 wall-clock pacing** (`edge/sim`, `edge/mesh`,
  `edge/labelsim`). A latency here is a real measurement of everything above
  the radio and a faithful model of the radio and the panel. Nothing in these
  documents quotes an aspirational figure as if it had been observed.
- **Budgets are labelled as budgets.** The hop budget is
  INTERFACE-CONTRACTS §4, transcribed in code as `stack.Budget`
  (`platform/cmd/usslpd/stack/slo.go`) and checked against the Markdown by
  `TestLatencyBudgetMatchesTheContract`.
- **Every diagram is Mermaid**, kept small enough to read. Where one picture
  would sprawl, it is split. All 84 of them are indexed by kind in
  [`DIAGRAMS.md`](DIAGRAMS.md), and all 84 parse.

## The documents

| | | |
|---|---|---|
| [`01-context.md`](01-context.md) | C4 level 1 | The system in its world: retailers, POS/ERP, staff, shoppers, technicians, regulators, the supply chain. |
| [`02-containers.md`](02-containers.md) | C4 level 2 | Every deployable unit, its technology, scaling unit and failure mode. |
| [`03-components.md`](03-components.md) | C4 level 3 | The hexagonal interior of the seven significant services, by Go package. |
| [`04-block-diagrams.md`](04-block-diagrams.md) | Hardware | Label PCB, Shelf Edge Controller, Store Gateway Unit, the four tiers, the Zephyr layer stack. |
| [`05-sequence-diagrams.md`](05-sequence-diagrams.md) | Behaviour | Ten end-to-end walkthroughs with budgets and measured latencies. **Start here if you are on call.** |
| [`06-data-architecture.md`](06-data-architecture.md) | Data | Streams, CQRS split, aggregates, read models, the columnar store, edge stores, four state machines. |
| [`07-flows.md`](07-flows.md) | Flows | Five human workflows and four of the platform's own decision procedures. |
| [`DIAGRAMS.md`](DIAGRAMS.md) | Index | Every diagram in the repository, grouped by kind: context, component, block, sequence, state, ER, flowchart. |

Companion documents in this directory and alongside it, produced separately and
not maintained here: [`INTERFACE-CONTRACTS.md`](INTERFACE-CONTRACTS.md)
(normative), [`security.md`](security.md),
[`observability.md`](observability.md), [`scalability.md`](scalability.md), and
the decision records under [`../adr/`](../adr/).

## Reading order

- **On call, first time:** [05](05-sequence-diagrams.md) §1 (the price path),
  then [02](02-containers.md) for failure modes, then
  [07](07-flows.md) §"Operator incident flow".
- **New to the codebase:** [01](01-context.md) → [02](02-containers.md) →
  [03](03-components.md).
- **Changing an interface:** [`INTERFACE-CONTRACTS.md`](INTERFACE-CONTRACTS.md)
  first, then [06](06-data-architecture.md).
- **Working on hardware or firmware:** [04](04-block-diagrams.md), then
  `firmware/README.md`.

## The one property everything hangs off

A label never displays a price it cannot verify. The Label Service signs a
canonical digest of *(tenant, store, label, SKU, price, effective time,
sequence, promotion)* with the tenant's Ed25519 price-authority key
(`canon.AttestationInput.CanonicalString`, `pki.PriceAuthority`). That signature
is checked twice: once by the Shelf Edge Controller, which recomputes the digest
from the update it is holding, and once by the label itself, which rebuilds the
canonical string and verifies against its own key ring before driving a pixel.
Both checks are in the code; both are exercised by `test/e2e/attestation_test.go`.

Everything in these documents that looks like an unusual decision — why the
Label Service keeps its own placement directory, why the frame on the air is
199 bytes larger than it needs to be, why the store gateway holds a delegated
signing key, why the sequence is persisted before the panel is driven — is
downstream of that one property.

## Divergences recorded in this set

Collected here so they are findable; each is argued where it appears.

| Where | Divergence |
|---|---|
| [04](04-block-diagrams.md) | The firmware tree has modules the blueprint figures do not show (`ota/usslp_patch.c`, `ota/usslp_inflate.c`, `radio/usslp_route.c`, `power/usslp_budget.c`) and splits every load-bearing algorithm into a portable half that is compiled and tested and a Zephyr half that has never been compiled. |
| [01](01-context.md), [`INTERFACE-CONTRACTS.md`](INTERFACE-CONTRACTS.md) §1 | The blueprint estate figures do not reconcile: 100,000 stores × 40,000 labels is 4 billion against a 50 million fleet, and "one controller per ~8 m of shelving" does not fit "~25 controllers and up to 40,000 labels". Catalogued in [`scalability.md` §1](scalability.md); repeated with a pointer wherever it appears rather than silently resolved. |
| [05](05-sequence-diagrams.md) §7 | `edge/mesh.KillNode` orphans a relay's children but does not schedule their rejoin; the e2e test drives re-association itself. Everything above the radio in that path is real. |
| [05](05-sequence-diagrams.md) §3, [03](03-components.md) §1 | Overlapping promotions resolve **last-activation-wins at the shelf**. `promodomain.Resolve` arbitrates against the whole active set and only the Promotion Service holds it; `promotion-events` carries the rule but not `Resolve`'s output. The fix is to put the resolved outcome on the event, not to add a second arbiter to the consumer. |

### Divergences that were here and have been closed

| Where | What it said | What is true now |
|---|---|---|
| [02](02-containers.md), [06](06-data-architecture.md) | The Label Service does not consume `promotion-events`; `usslpd` bridges it in `stack/promobridge.go` | Closed 2026-08-31. `label.Service.Start` subscribes as consumer group `label-service.promotions` at concurrency 1, resolves labels with the promotion domain's own matcher and drives them through the existing batch fan-out. The bridge is gone |
| [02](02-containers.md) | `analytics-service` is an empty directory, `enabled: false` in the chart | Closed 2026-08-31. The binary exists, is constructed by `usslpd`, is enabled in the chart, both image matrices and the dev compose profile |
| [05](05-sequence-diagrams.md) §1 | The `SEC to label` budget of 300 ms is exceeded at p99 | §4's line item was the wrong side and now reads 400 ms, with the 100 ms taken from the two cloud hops; the cause (199 bytes of attestation per frame) is recorded there |
| [04](04-block-diagrams.md) | The root `README.md` says 24,539 firmware checks against `firmware/README.md`'s 25,961 | 25,961, confirmed by running all four host-test configurations; the root README carried the stale figure |
| [02](02-containers.md) | The root `README.md`'s file and line counts are stale | Recounted, dated, and stated with the commands that produced them |
| [`scalability.md`](scalability.md) §1 | `stack/streams.go` says 5,568 partitions | 5,472, now asserted by `stack.TestCatalogueTotalMatchesTheCommentAndTheClusterSizing` |
| [`security.md`](security.md), [`observability.md`](observability.md) | The firmware's ack status codes and verdict have no counterpart in `edge/` | `edge/` implements statuses 3 and 4 and the three-bit verdict; the inference survives only as a fallback for old firmware |
