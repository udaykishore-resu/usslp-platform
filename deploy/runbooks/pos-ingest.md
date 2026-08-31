# Runbook — POS ingest

**Alerts:** `USSLPPOSIngestErrorBudgetBurn*`, `USSLPUIGLatencyBudgetExceeded`,
`USSLPUIGPublishFailures`, `USSLPUIGCircuitBreakerOpen`, `USSLPUIGQuarantineSpike`

**SLO:** 95% of POS deliveries durably published within 500 ms. Budget 5%.

**The self-amplifying risk:** the gateway acknowledges a POS the instant a change
is durable, because a POS that is kept waiting retries — and a retry is more work
for everyone than a fast 202. When ingest slows, retries add load, which slows it
further. Break that loop first and diagnose second.

---

## Is it one retailer or all of them?

This is the first question of every UIG incident, which is why
`usslp_uig_ingest_total` carries a `tenant` label despite the cardinality cost.

```promql
sum by (adapter) (rate(usslp_uig_ingest_total[5m]))
sum by (tenant, outcome) (rate(usslp_uig_ingest_total[5m]))
usslp:pos_ingest_latency:p95_by_adapter
```

- **One adapter** — that integration. Clover fetches the object back and Oracle
  Retail is SOAP, so both are latency-sensitive to the retailer's own systems.
- **One tenant across adapters** — that retailer's traffic pattern changed.
  Check `usslp_uig_rate_limited_total{tenant=...}`.
- **Everything** — the gateway or what is beneath it.

---

## The gateway's own budget

The retailer-facing SLO is 500 ms. The gateway's internal slice of the price path
is **50 ms**, and it counts its own breaches:

```promql
sum by (adapter) (rate(usslp_uig_latency_budget_exceeded_total[5m]))
  / sum by (adapter) (rate(usslp_uig_ingest_total[5m]))
```

This is the early warning for the SLO. Over 1% for ten minutes means the gateway
is eating slack the slower hops downstream will need, well before a retailer
notices anything.

---

## Causes, in the order they occur

### Durable publish failing

```promql
rate(usslp_uig_publish_failures_total[5m])
```

Non-zero means the gateway cannot make a change durable. It is the top of the
price path; nothing downstream can compensate. Check the event log or, once the
Kafka adapter lands, broker availability and `min.insync.replicas`.

### A circuit breaker open

```promql
usslp_uig_circuit_breaker_state == 2      # 0 closed, 1 half-open, 2 open
```

The gateway has stopped calling a failing dependency. Deliveries needing it are
refused fast rather than slowly, which is correct — and that retailer is not
getting price changes through that path. Fix the dependency; the breaker closes
itself.

### Rate limiting

```promql
sum by (adapter, tenant) (rate(usslp_uig_rate_limited_total[5m]))
```

Either a retailer changed their call pattern or the per-binding limit is too
tight for legitimate traffic. Both need a human decision. The limit is
per-binding; raising it globally (`USSLP_UIG_DEFAULT_RATE_PER_SECOND`) to solve
one retailer's problem removes the protection from all of them.

### Quarantine spike

```promql
sum by (adapter, reason) (rate(usslp_uig_quarantined_total[5m]))
```

Almost always a retailer's integration changing shape: a new field, a changed
code page, a fixed-width file whose columns moved. The raw body is retained:

```bash
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  https://api.<region>.usslp.example/v1/deliveries/<tenant>
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  https://api.<region>.usslp.example/v1/replay/<tenant>/<delivery_id>
```

Fix the mapping, then replay. Nothing is lost while it is quarantined —
`USSLP_UIG_DELIVERY_RETENTION` bounds how long, so a spike that goes unhandled
for days does eventually become loss.

### Guardrail rejections — not an engineering problem

```promql
sum by (tenant) (rate(usslp_price_guardrail_rejections_total[15m]))
```

A change refused as a suspected data error: a decimal point in the wrong place,
a currency mix-up, cents where the feed meant units. **These never reached a
shelf, which is the guardrail working.** Route it to merchandising, not to
engineering, and do not raise `USSLP_PRICE_GUARDRAIL_FACTOR` to make it stop.

---

## Mitigations

1. **Scale out.** The HPA is RPS-based with a zero-second scale-up window
   precisely so a retry storm is met with capacity. Check whether it is at its
   ceiling.
2. **Rate-limit the noisiest binding**, deliberately and temporarily, to protect
   everybody else. A refused delivery is retried; a platform that is down is not.
3. **Roll back**, if a deploy correlates. [rollback.md](rollback.md).

---

## What NOT to do

**Do not disable the idempotency guard to "reduce latency".** The 24-hour window
is fixed by `INTERFACE-CONTRACTS` §6. Without it every POS retry becomes a
duplicated price change, and the retries are exactly what is happening during
this incident.

**Do not widen the guardrail factor to clear rejections.** The rejections are the
system refusing to put a wrong price on a shelf.
