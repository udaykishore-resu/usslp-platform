# Runbook — Price path latency

**Alerts:** `USSLPPricePathErrorBudgetBurnFast`, `...BurnSlow`, `...BurnTicket`,
`USSLPPricePathErrorBudgetLeak`

**SLO:** 99% of price updates reach the glass within 3 seconds. Error budget 1%.

**What is actually wrong when this fires:** somewhere a shelf is showing a price
the till does not agree with. Not "a metric is elevated" — a customer can walk up
to a shelf right now and see the wrong number. That is the urgency.

---

## What the number means

`usslp_price_update_e2e_seconds` is measured from the envelope's `RecordedAt` —
the moment USSLP took durable responsibility for the change — to the moment the
pixels settled. It is deliberately *not* measured from any internal handoff,
because that is the only number a retailer can verify by looking at a shelf.

So an elevated p99 always means real-world lateness. There is no measurement
artefact to rule out first.

---

## Localise it in one query

Open **USSLP — Price Path Latency by Hop**
(`deploy/observability/grafana/dashboards/price-path-latency.json`). The budget
from `docs/architecture/INTERFACE-CONTRACTS.md` §4:

```
POS -> UIG            <=   50 ms   instrumented: usslp:hop_uig_ingest:p99
UIG -> stream         <=   30 ms   not instrumented
stream -> Label Svc   <=  120 ms   partly: usslp:hop_label_fanout:p99
Label Svc -> broker   <=  100 ms   instrumented: usslp:hop_device_publish:p99
broker -> SGU -> SEC  <=  100 ms   not instrumented
SEC -> label          <=  400 ms   instrumented: usslp:hop_sec_delivery:p99
label refresh         <= 2000 ms   not instrumented (E-Ink waveform)
ACK back to cloud     <=  200 ms   not instrumented
                      ---------
                        3000 ms
```

The **residual panel** is the one to read first. It is the end-to-end p99 minus
the three cloud-side measured hops. If the residual has grown while every
measured hop is flat, the problem is at the far end of the mesh — inside stores,
not in the cloud — and this runbook is nearly finished; go to
[fleet-health.md](fleet-health.md).

---

## Cloud-side causes, in the order they occur

### 1. Consumer lag on `price-updates`

```promql
usslp:consumer_lag:by_topic_group{topic="price-updates"}
usslp:hop_event_handler:p99{topic="price-updates"}
```

Two readings, two different problems:

- **Lag rising, handler latency flat** — a throughput shortfall. The autoscaler
  should be fixing it; check whether the label-service HPA is pinned at its
  ceiling (`USSLPAutoscalerAtCeiling`). If it is, raise `replicas.max` in the
  region's values file. This is a capacity decision, not a bug.
- **Lag rising, handler latency also rising** — a code or dependency problem.
  The autoscaler will make it *worse*, because more consumers means more load on
  whatever is slow. Find the dependency before adding pods.

### 2. The MQTT publish hop

```promql
usslp:hop_device_publish:p99
usslp_mqtt_client_connected                     # 1 up, 0 down, per service
usslp_mqtt_broker_inflight_messages
rate(usslp_mqtt_client_ack_timeouts_total[5m])
```

A rising publish p99 with a rising broker in-flight count means subscribers are
not acknowledging — controllers or gateways, not the broker.
[mqtt-broker.md](mqtt-broker.md).

### 3. A fan-out that is genuinely large

```promql
histogram_quantile(0.99, sum by (le) (rate(usslp_price_fanout_batch_size_bucket[5m])))
```

A store-wide promotion touches up to 40,000 labels and legitimately takes tens
of seconds. Check whether the p99 spike coincides with a promotion activation
(`rate(usslp_promotion_fanout_total[5m])`). If it does, the SLO is being missed
by design and the conversation is with merchandising about activation windows,
not with engineering.

### 4. The pricing tier

```promql
histogram_quantile(0.99, sum by (le, tier) (rate(usslp_pricing_tier_seconds_bucket[5m])))
```

Budgets are 10 ms (Tier 1) and 15 ms (Tier 2), inside the Label Service's 120 ms
slice. A tier at 200 ms has not broken the SLO on its own but has spent slack
the slower hops downstream need.

### 5. A recent deploy

```bash
kubectl -n usslp rollout history deployment/usslp-label-service
kubectl argo rollouts get rollout usslp-label-service -n usslp
```

If the burn began within an hour of a rollout, roll back first and diagnose
afterwards. [rollback.md](rollback.md).

---

## Mitigations, in order of preference

1. **Roll back**, if a deploy correlates. Cheapest and most reversible.
2. **Scale out** the lagging consumer, if handler latency is flat.
3. **Raise the HPA ceiling** if it is pinned. A values change, a sync, minutes.
4. **Shed non-price work.** Analytics is `usslp-analytics` priority and is
   preemptible by design; cordoning the analytics node group returns capacity to
   the price path within a scheduling cycle.
5. **Defer batch promotions.** Pause the promotion sweep
   (`USSLP_PROMOTION_SWEEP_INTERVAL`) to stop new fan-outs starting while the
   backlog drains. This does not affect prices already on shelves.

---

## What NOT to do

**Do not restart the Label Service to "clear" lag.** A store-wide fan-out in
flight when SIGTERM arrives has up to 40,000 labels left to publish. The binary
drains for 20 seconds (a compile-time constant) and the pod's grace period is
45 — but a restart during a backlog adds a cold read-model rebuild to a service
that is already behind.

**Do not raise `terminationGracePeriodSeconds` to make a drain "safer".** It is
already longer than the app's own drain and the chart validates that. A longer
grace period only delays the moment the pod actually goes.

**Do not silence the alert because a promotion is running.** If a promotion
makes the platform miss its SLO, that is a fact about the platform worth
keeping, not noise.

---

## Afterwards

The burn-rate alerts are windowed, so they clear on their own once the ratio
drops — the short window in each pair exists precisely so they stop promptly.
Check the remaining budget before approving the next risky deploy:

**USSLP — SLOs and Error Budgets**, the "Price path" gauge. Under 25%
remaining is a freeze on non-essential price-path deploys until it recovers.
