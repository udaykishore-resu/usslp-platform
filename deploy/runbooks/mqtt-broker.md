# Runbook — MQTT broker

**Alerts:** `USSLPMQTTClientDisconnected`, `USSLPMQTTAckTimeouts`,
`USSLPMQTTBrokerDroppingMessages`, `USSLPMQTTBrokerInflightBacklog`,
`USSLPStoresAutonomousMany`

**Why this is a page:** the Label Service publishes every price update over this
link. A broker outage does not corrupt anything — labels hold their prices, and
stores fall back to autonomous mode, which is the design working — but for as
long as it lasts, no price change reaches any shelf anywhere.

---

## Is it the broker, or the link to it?

```promql
usslp_mqtt_client_connected              # 1 up, 0 down, per USSLP service
usslp:mqtt_broker_connections:total
usslp:stores_autonomous:count
```

- **Services disconnected AND stores autonomous** — the broker or its load
  balancer.
- **Services connected, stores autonomous** — the stores' path to the broker,
  not the broker. Usually the NLB or DNS.
- **Services disconnected, stores fine** — the cluster-side path only. The
  stores are still being served by their own gateways.

```bash
kubectl -n usslp get pods -l app.kubernetes.io/component=mqtt-broker
kubectl -n usslp exec usslp-mqtt-broker-0 -- /opt/emqx/bin/emqx ctl status
kubectl -n usslp exec usslp-mqtt-broker-0 -- /opt/emqx/bin/emqx ctl cluster status
```

---

## Cluster membership

EMQX is a StatefulSet of five with a PodDisruptionBudget minimum of three,
because its session store needs a majority to rebalance safely. A split below
three risks losing persistent sessions — **and a lost session is an
un-acknowledged QoS 1 price update: a price that never reached a shelf and that
nobody knows never reached it.**

```bash
kubectl -n usslp get statefulset usslp-mqtt-broker
kubectl -n usslp get pdb usslp-mqtt-broker
kubectl -n usslp logs usslp-mqtt-broker-0 | grep -i 'cluster\|mnesia\|partition'
```

If the cluster has split, **do not** delete pods to force a re-form. Bring the
minority side back and let EMQX heal; deleting a pod discards the sessions it was
holding.

---

## Messages being dropped

```promql
sum by (reason) (rate(usslp_mqtt_broker_dropped_total[5m]))
usslp_mqtt_broker_inflight_messages
usslp_mqtt_broker_retained_messages
rate(usslp_mqtt_client_ack_timeouts_total[5m])
```

**In-flight climbing** means subscribers are not acknowledging QoS 1/2 messages.
During a store-wide promotion a transient spike is expected; sustained means a
controller or a gateway has stopped acknowledging. Go to
[fleet-health.md](fleet-health.md).

**Ack timeouts on a client** mean a publish was abandoned — a price update the
platform believes it sent and the edge never saw. The label's sequence number
makes the eventual retry safe, but until then that label is stale.

---

## Retained messages are load-bearing

`EMQX_RETAINER__ENABLE` must stay on and `MSG_EXPIRY_INTERVAL` must stay `0s`.

A controller rebooting after a power cut recovers the current price of every
label in its zone from the retained messages on the local broker, without a
round trip to a cloud that may be unreachable
(`docs/architecture/INTERFACE-CONTRACTS.md` §3). Expiring them, or clearing them
to reclaim memory, means every controller that reboots afterwards comes up
knowing nothing — and cannot ask.

```promql
usslp:mqtt_broker_retained:total
```

A sudden drop in this number is a configuration change somebody made, and it is
worth finding before the next power cut does.

---

## Recovery order

1. Restore the broker.
2. Watch services reconnect: `usslp_mqtt_client_connected` returns to 1.
   `CleanSession` is false, so the broker replays each service's
   un-acknowledged QoS 1 messages — the acknowledgements that arrived while the
   link was flapping are not lost.
3. Watch stores rejoin: `usslp:stores_autonomous:count` falls. The gateways take
   4 successful probes over 15 seconds before re-bridging, deliberately slower
   than the failure path, because a flapping link treated as up produces a
   bridge that starts and stops repeatedly.
4. Expect a reconciliation burst: `rate(sgu_merge_conflicts_total[1h])`. Normal.
5. Expect an upstream flush: the queues drain and the cloud's record backfills.
   Check none of them hit their cap while the outage lasted —
   `usslp:sgu_upstream_queue_depth:max` — because anything beyond the cap is
   permanently gone.

---

## What NOT to do

**Do not restart the whole StatefulSet at once.** `OrderedReady` and the PDB
exist because bringing five EMQX nodes up simultaneously races cluster discovery
and can produce two partitions that both believe they are the cluster.

**Do not clear retained messages.** See above.

**Do not enable `EMQX_ALLOW_ANONYMOUS` to get devices reconnecting.** Every
device credential is issued scoped to `usslp/{tenant}/#`, which is what makes
tenant isolation a single ACL rule. Anonymous access removes it for everybody.
