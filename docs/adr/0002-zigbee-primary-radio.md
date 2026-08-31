# 0002 — Zigbee 3.0 as the primary label radio, BLE secondary, WiFi backbone, LoRa rural

**Status:** Accepted, partially implemented. Zigbee, BLE and the WiFi/Ethernet
backbone are implemented or modelled. **LoRa is not implemented at all** — there
is no LoRa code anywhere in this tree. See *Consequences*.

---

## Context

A Tier 1 label has to satisfy four constraints simultaneously:

- **Seven to ten years on one 500 mAh primary cell.** Every milliamp-hour spent
  on the radio is not available to the panel.
- **Reach.** A 25-metre aisle has to be covered from one controller, through
  metal shelving, chiller cabinets and shoppers.
- **Density.** A hypermarket carries up to 40,000 labels on roughly 25
  controllers (`platform/internal/label/adapters/messaging.go`).
- **Latency.** `INTERFACE-CONTRACTS` §4 gives the controller-to-label hop 400 ms
  of a 3,000 ms budget. It gave 300 ms when this decision was taken; end-to-end
  attestation ([0004](0004-end-to-end-price-attestation.md)) lengthened the frame
  and the line item moved. The comparison below is against the 300 ms that was
  current at the time, and none of the radios' relative standing changes.

No single radio is best on all four, and the honest reading of the constraint set
is that the *listening* cost dominates everything else. `firmware/README.md`
works the arithmetic: a 6.5 mA receive window of 8 ms every 250 ms is a 3.2% duty
cycle, which is 208 µA, which is 99 days on the cell — not eight years. Half the
achievable budget is spent listening for beacons even after adaptive duty
cycling brings the total to 6.584 µA and 8.67 years.

## Decision

Four radios, each doing the one job it is best at.

| Radio | Role | Why this one | Where it is in the tree |
|---|---|---|---|
| **802.15.4 / Zigbee 3.0, 2.4 GHz** | Tier 2 to Tier 1: every price update, every acknowledgement, every telemetry report, every firmware chunk | Mesh routing gives aisle-length reach from one coordinator without mains power at every hop; a sleepy end device costs micro­amps at rest; 250 kbit/s is ample for a 199-byte attested price frame | `edge/mesh` (airtime, CSMA, routing, healing), `firmware/src/radio/zigbee.c`, `firmware/Kconfig` `USSLP_ZIGBEE` |
| **BLE 5.0** | Commissioning, field diagnostics, optional shopper beacon. **Never a price** | A technician has a phone; a label that has never joined a mesh still has to be associated with a planogram slot | `firmware/src/radio/ble.c`, `firmware/Kconfig` `USSLP_BLE` |
| **WiFi / Ethernet** | Tier 3 to Tier 2 inside the building, and Tier 3 to the cloud | The Store Gateway Unit is a mains-powered Linux box in a back office; there is no reason to put its traffic on a constrained radio | `edge/sgu` bridge and broker; `deploy/edge/install.sh` |
| **LoRa** | Intended for rural stores whose WAN is intermittent | — | **Nothing. No LoRa code exists.** |

### Comparison, on the four constraints that decided it

| | Zigbee 3.0 (802.15.4) | BLE 5.0 | WiFi | LoRa / LoRaWAN |
|---|---|---|---|---|
| Rate on air | 250 kbit/s shared per channel per zone (`mesh.DataRateBps`) | 1–2 Mbit/s | 50+ Mbit/s | 0.3–50 kbit/s |
| Frame limit | 127 B PHY PDU (`mesh.MaxFrameBytes`), so a 199-byte attested price fragments | 251 B | 1500 B | ~250 B, duty-cycle capped |
| Topology | Mesh, ≤3 hops in the platform's budget, self-healing | Star / connectable, no native mesh in this build | Star to an AP | Star to a gateway |
| Sleepy-device cost | ~0.8 µA in System OFF with a co-processor holding the receive window at 6.5 mA | Comparable at rest; advertising costs airtime the primary radio then does not have | Tens of mA associated — disqualifying | Very low, but downlink is scheduled and slow |
| Reach through steel shelving | Adequate with mains-powered relays (`USSLP_ZIGBEE_ROUTER`) | Short | Adequate, wrong power class | Kilometres, wrong latency class |
| Fit against the hop budget (300 ms at the time, 400 ms now) | Measured 161 ms p50 / 331 ms p99 over 1,000 changes | Not on the price path | Not on the price path | Downlink latency is seconds to minutes |

The co-processor split is part of this decision. The nRF52840 has a perfectly
good 802.15.4 radio and a single-chip design would be cheaper in bill of
materials. The TI CC2652P is there because it can hold the receive window at
6.5 mA with the application MCU in System OFF at 0.8 µA, and beacon listening is
more than half the device's entire energy budget. `firmware/README.md` states
plainly that a single-chip build would not reach the battery target.

BLE's exclusion from the price path is also part of the decision and is stated in
`firmware/src/radio/ble.c`: a price has to arrive over a path the mesh accounting
can see and the controller can acknowledge, so a phone-delivered price would be a
price with no attestation chain and no delivery record. The commissioning
characteristic can set a label's identity; it cannot set what the label displays.

## Consequences

**The 127-byte PHY limit is now load-bearing.** End-to-end attestation
([0004](0004-end-to-end-price-attestation.md)) added 199 bytes to every price
frame, which does not fit in one 802.15.4 frame. Fragmentation is a first-class
part of `edge/mesh`'s airtime model, and the measured cost is real: the
controller-to-label hop is **331 ms at p99 against what was then a 300 ms
budget** over 1,000 changes on a 2-core container, and **314 ms at p99** under
the 40/s sustained load run. Channel utilisation per zone rose from about 1.55%
to about 2.08–2.20% — the same traffic in larger frames.

**That measurement moved the contract, not the platform.** The three-second total
holds with room, so the §4 line item was the wrong side of the comparison; it now
reads **400 ms**, with the 100 ms taken from two cloud hops that measure 8–18 ms
against a 300 ms allowance. Zigbee still fits the hop it is chosen for, with less
margin than this ADR originally recorded.

**A resting label is not reachable inside the hop budget at all.** The hop budget
describes a zone in its active window. At the default 13.7-second
sustainable resting interval a label is on average half an interval from being
reachable, so a price load is planned as a window: the controller broadcasts a
wake, the zone comes up over one resting interval, and prices then flow inside
budget (`firmware/src/power`, `labelsim.OpenActiveWindow`).

**BLE costs airtime on the shared antenna.** An advertisement that collides with
a beacon window delays a price update by up to one resting interval. The
advertising interval is long, the commissioning window is bounded, and
`CONFIG_USSLP_BLE=n` frees roughly 90 KB of flash and the airtime for a store
that commissions over NFC only.

**LoRa is a claim with no implementation behind it.** A grep of the tree for
`LoRa` returns nothing outside prose. A rural store today gets the same WiFi or
cellular backhaul as any other store and relies on
[0003](0003-edge-first-architecture.md) — store autonomy — rather than on a
low-rate backhaul. That is arguably the better answer, since LoRa's downlink
latency is measured in seconds to minutes and would not carry a price inside any
budget worth writing down; but the platform should stop claiming a radio it does
not have.

**Everything Tier 1 and Tier 2 is simulated.** `edge/mesh` and `edge/labelsim`
model airtime, CSMA-CA backoff, per-hop store-and-forward delay, MAC retries,
tree formation and repair, and the neighbour-table caps that real firmware has
(24 entries for an end device, 64 for a relay). The model is honest and it is not
silicon. No timing in this record has been observed on hardware:
`firmware/README.md` says so explicitly under *Not verified*.

## Alternatives considered

**BLE mesh as the primary.** Attractive because every phone speaks it, which
makes commissioning and field service trivial. Rejected: BLE mesh is a flooding
protocol, and flooding 40,000 nodes for every price change is a fundamentally
different congestion story from routed unicast. The relay nodes must also stay
awake, which puts them back into the mains-powered class without the routing
benefit.

**WiFi at the label.** Rejected on power alone. An associated WiFi station draws
tens of milliamps; the whole label budget is 6.584 µA.

**Proprietary sub-GHz.** Better link budget through steel and better range.
Rejected on ecosystem: no certified stack, no second source for the transceiver,
and a fleet of 50 million devices is exactly the wrong place to be the only user
of a radio.

**802.15.4 without Zigbee** — a bare MAC with a USSLP network layer. Would have
saved roughly 120 KB of flash (the ZBOSS stack is the largest single item in the
firmware's flash budget). Rejected: network-layer security, key establishment,
and tree formation are the parts that are hard to get right, and writing them
would have moved the fleet's security onto code with no field history. The
security cost is in the airtime baseline rather than in a footnote —
`mesh.MACOverheadBytes` is 25 bytes with Zigbee network-layer security enabled,
because an unencrypted mesh would let anyone with a software-defined radio watch
a store's pricing.
