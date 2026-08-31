# Runbook — Fleet health

**Alerts:** `USSLPLabelAvailabilityBurnFast`, `...BurnSlow`,
`USSLPLabelAvailabilityLeak`, `USSLPStoreAutonomous`, `USSLPStoresAutonomousMany`,
`USSLPLabelsPendingDeliveryRising`, `USSLPMeshDeliveryFailures`,
`USSLPDevicesQuarantinedSpike`

**SLO:** 99.5% of labels online. Budget 0.5%.

---

## The first question: is this an incident or a battery?

```promql
usslp:fleet_labels:online_ratio
usslp:sli_label_online:error_ratio1h    # spiky?
usslp:sli_label_online:error_ratio3d    # or a steady floor?
```

A **spike** is an incident: a store lost its gateway, a region lost its broker.

A **steady 0.5% that never spikes** is a fleet reaching the end of its battery
life. A smart label has a 7–10 year battery; at 50 million labels, a fraction of
a percent reaching end-of-life at any moment is arithmetic, not a fault. Check
the runway before treating it as one:

```bash
curl -s https://api.<region>.usslp.example/v1/stores/<store>/runway | jq
```

`USSLPLabelAvailabilityLeak` (burn rate 1, three-day window) is the alert
deliberately shaped to catch this case, which is why its description says so.

---

## Where "online" comes from

`registry_devices` is a gauge by lifecycle state
(`platform/internal/registry/domain/device.go`): `manufactured`, `provisioned`,
`assigned`, `active`, `degraded`, `offline`, `quarantined`, `retired`.

Online is **active + degraded** — a degraded label is still displaying the right
price, it just has a battery or link problem. The denominator excludes
`manufactured` (never deployed) and `retired` (deliberately removed), because
counting a warehouse of spares as unavailable would make a healthy fleet look
like an outage.

```promql
sum by (state) (registry_devices)
```

---

## Is the cloud blind, or are the stores actually down?

This distinction decides everything that follows.

```promql
usslp:stores_autonomous:count        # stores pricing from their own rules
usslp:stores_total:count
usslp:sgu_availability:ratio5m       # gateways that answer a scrape
usslp:sgu_upstream_queue_depth:max   # how much they are buffering
```

**Stores autonomous, gateways up, queues filling** — the stores are fine. Their
labels are being priced correctly from local rules; the cloud has lost sight of
them. The alert is a warning, not a page, and the work is on the WAN or the
cloud broker. What is accumulating is a gap in the cloud's record, which the
gateways backfill on reconnect.

**Gateways unreachable** — the stores may genuinely be down.
[sgu-recovery.md](sgu-recovery.md).

**One store autonomous** is that store's broadband. **Fifty at once**
(`USSLPStoresAutonomousMany`) is the cloud side: the broker, its load balancer,
or the region. [mqtt-broker.md](mqtt-broker.md).

---

## A rising floor of undelivered updates

```promql
usslp_labels_pending_delivery                       # per store
usslp:label_delivery:success_ratio_by_store1h
```

`USSLPLabelsPendingDeliveryRising` fires on a *floor* that never drains, not on
a spike. It is the earliest signal available — earlier than the fleet SLO,
because a label with an authorised but unconfirmed update is still `active` in
the registry.

A spike during a store-wide fan-out is expected and drains. A floor means the
updates were authorised, published, and never acknowledged: the store's gateway
or its controller mesh.

---

## Mesh problems in one zone

```promql
sum by (sec, outcome) (rate(sec_mesh_deliveries_total[15m]))
sec_mesh_link_failure_risk{sec="sec-0007"}
rate(sec_mesh_reroutes_total{sec="sec-0007"}[1h])
histogram_quantile(0.99, sum by (le) (rate(sec_mesh_hops_bucket{sec="sec-0007"}[5m])))
```

Rising hop counts and rising reroutes in one zone means the radio path changed
physically: a shelf moved, a metal fixture added, a freezer door propped open, a
pallet parked in the aisle. Predictive healing routes around it; when it cannot,
somebody has to look at the aisle.

**High measured failure with low predicted risk** means the model is missing
something the radio is seeing — worth a bug, not just a truck roll.

---

## A quarantine spike

```promql
increase(registry_devices{state="quarantined"}[1h])
sum by (reason) (rate(registry_provision_rejected_total[15m]))
```

Quarantine is a deliberate act: a device failing authentication, or one an
operator pulled. A hundred in an hour is a fleet-wide event, and the usual cause
is a certificate expiry the key ceremony did not renew — which will also show up
as `USSLPProvisioningRejectionRate`.

---

## What NOT to do

**Do not restart store gateways to "reconnect" a fleet.** A gateway that is
autonomous is the only thing pricing that store's shelves. Restarting it takes
the store's broker down and flushes its upstream queue mid-write.

**Do not treat autonomous mode as an outage.** It is the design working: when
the cloud link drops the bridge stops and the local broker does not. That is the
entire mechanism behind zero label downtime during a WAN outage.
