# Runbook — Store Gateway Unit recovery

**Alerts:** `USSLPSGUUptimeBurnFast`, `...BurnSlow`, `USSLPSGURestartLoop`,
`USSLPStoreAutonomous`, `USSLPStoresAutonomousMany`,
`USSLPSGUUpstreamQueueNearFull`, `USSLPSGUBridgeFailures`

**SLO:** SGU uptime 99.9%. Budget 0.1%.

**This is the cloud-side view.** For work on the box itself — a person with a
laptop in a back office — go to [../edge/RUNBOOK.md](../edge/RUNBOOK.md), which
is written to be usable by someone who has been woken up in a store they have
never visited.

---

## Scale first

```promql
usslp:stores_autonomous:count
count(up{job=~"usslp.*", service="sgu"} == 0)
usslp:sgu_availability:ratio5m
```

| Pattern | Meaning | Where to work |
|---|---|---|
| One store | That store's broadband or hardware | Dispatch; edge runbook |
| Tens, geographically clustered | An ISP or a regional network event | Nothing to fix in the platform; track it |
| Tens, unclustered, sudden | **The cloud side** — broker, load balancer, region | [mqtt-broker.md](mqtt-broker.md) |
| Rising steadily over days | An update rolling out badly | See below |

A single store going autonomous is that store's broadband. Fifty at once is us.

---

## Restart loops the uptime SLI cannot see

```promql
resets(usslp_process_uptime_seconds{service="sgu"}[1h]) > 3
```

A gateway that restarts every four minutes has ~100% scrape availability and is
nonetheless broken. `usslp_process_uptime_seconds` is monotonic and resets to
near zero on every start, so `resets()` counts restarts directly — which is why
`USSLPSGURestartLoop` exists alongside the uptime burn-rate alerts.

Almost always one of: a corrupt write-ahead log that fails recovery, a full
disk, or a configuration change that removed a required variable. All three are
diagnosed on the box; edge runbook §5.

---

## Correlate with an update

```promql
count by (version) (usslp_process_uptime_seconds{service="sgu"})
```

If the failing gateways share a `version` label that the healthy ones do not,
this is a bad update reaching the fleet.

**Stop the rollout before recovering individual stores.** The update timer
spreads checks over four hours with `FixedRandomDelay`, so at 100,000 stores a
bad version reaches about 1% in the first two and a half minutes and the rest
over the following hours. Clearing the version pin in the manifest at
`USSLP_UPDATE_MANIFEST_URL` stops every store that has not yet checked. That is
one change, centrally, not 100,000.

Then recover the affected boxes:
`sudo /usr/local/lib/usslp/update.sh --rollback` — which needs no network,
because the previous version is still on disk.

---

## The queue is the thing with a deadline

```promql
usslp:sgu_upstream_queue_depth:max
topk(20, sgu_upstream_queue{measure="depth"})
```

`USSLPSGUUpstreamQueueNearFull` is a **page** where `USSLPStoreAutonomous` is
only a warning, and the difference is data loss. An autonomous store is pricing
correctly. A store whose upstream buffer has filled is *also* pricing correctly —
and the cloud's record of this outage now has gaps that no replay can recover.
Delivery acknowledgements, telemetry, mode transitions: gone.

Default cap is 50,000 entries / 256 MB. Raising it buys hours and fixes nothing;
edge runbook §2 has the procedure and the disk-space caveat.

---

## Bridge failures

```promql
sum by (store, direction, outcome) (rate(sgu_bridged_total[10m]))
```

`direction` separates downstream (cloud → store, so controllers only ever talk
to a broker inside the building) from upstream (store → cloud, buffered while
the WAN is down). Downstream failures mean prices are not reaching the store at
all; upstream failures mean the cloud is losing sight of it.

---

## After recovery: expect reconciliation conflicts

```promql
sum by (store, resolution) (rate(sgu_merge_conflicts_total[1h]))
```

A burst immediately after a store rejoins is **normal**: the store priced locally
while the cloud priced centrally, and reconciliation picks a winner. A sustained
rate outside an outage window means the local rules and the central prices
genuinely disagree, which is a merchandising question.

---

## What NOT to do

**Do not restart a gateway that is autonomous** unless the store is genuinely
broken. It is the only thing pricing that store's shelves, and the restart
flushes the queue holding the outage record.

**Do not delete `/var/lib/usslp` to "fix" a recovery loop** without reading edge
runbook §4 first. The write-ahead log and the checkpoint are a pair; removing one
leaves a store that cannot recover, and the upstream queue is lost permanently.
