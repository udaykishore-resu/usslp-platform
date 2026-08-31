# Runbook — Cloud API availability

**Alerts:** `USSLPCloudAPIErrorBudgetBurnFast`, `...BurnSlow`, `...BurnTicket`,
`USSLPCloudAPIErrorBudgetLeak`, and the `usslp.component.api-gateway` group.

**SLO:** 99.95% of inbound requests succeed. Budget 0.05% — the tightest in the
platform, so the fast-burn threshold is only 0.72% errors.

---

## What "error" means here

`obs.StandardMetrics.ObserveRequest` sets `outcome="error"` whenever the handler
returned a non-nil error, and `"ok"` otherwise. This is the platform's own
verdict on the request, not an HTTP status-code heuristic layered on top of it —
so a 4xx the handler considers a normal refusal does not count against the SLO,
and a 200 the handler returned alongside an error does.

---

## Which service?

```promql
usslp:cloud_api:error_ratio_by_service1h
sum by (service, operation, outcome) (rate(usslp_requests_total[5m]))
```

Note the `service` label values are the binaries' own names — `uig`, not
`pos-integration-gw`; `pricing-service`, not `pricing-ai-service`.

---

## If it is the gateway

```promql
usslp_gateway_breaker_state{state="open"}
sum by (upstream, reason) (rate(usslp_gateway_upstream_failures_total[5m]))
sum by (operation) (rate(usslp_gateway_requests_total{outcome!="ok"}[5m]))
increase(usslp_gateway_panics_total[15m])
sum by (bucket) (rate(usslp_gateway_rate_limited_total[5m]))
```

**Every upstream the gateway names is a real service**, analytics included, so
there is no breaker here that is expected to sit open. Treat any of them as a
genuine upstream failure.

**A recovered panic** means a bug reached production. The gateway keeps serving,
which is why it is a warning rather than a page — and exactly why it must not be
allowed to become normal.

**Auth failures spiking** (`usslp_gateway_auth_total{outcome!="ok"}`) is either a
rotated credential a retailer has not picked up or credential stuffing. The two
look identical from the metric; the source addresses separate them.

---

## If it is a backend service

Work down: is it returning errors, or is it unreachable?

```promql
up{job=~"usslp.*"}                        # scrapeable at all
usslp_mqtt_client_connected               # broker link, per service
usslp:consumer_lag:by_topic_group         # behind on the stream
```

```bash
kubectl -n usslp get pods -l app.kubernetes.io/part-of=usslp
kubectl -n usslp port-forward svc/usslp-label-service 9090:9090
curl -s localhost:9090/readyz | jq        # names the failing dependency
```

`/readyz` names its checks. `/healthz` will answer 200 throughout — it only says
the process is scheduling goroutines, and that asymmetry is deliberate: a broker
blip removes a pod from the endpoint list rather than restarting it.

---

## Correlate with a deploy

```bash
kubectl argo rollouts list rollouts -n usslp
kubectl argo rollouts get rollout usslp-api-gateway -n usslp
```

The canary analysis should have caught a bad version at 5% — it gates on success
rate, latency, breaker state and panics. If the burn started *after* a rollout
completed, the regression is one the analysis window (five minutes at 5%, then
three at 25%) was too short to see. Roll back and lengthen the pause.

---

## Mitigations

1. **Roll back.** [rollback.md](rollback.md).
2. **Scale out**, if latency-driven rather than error-driven.
3. **Shed load deliberately** by tightening a rate-limit bucket on the noisiest
   tenant, rather than letting the gateway fail everyone equally.
4. **Fail an upstream open**, never. The breakers exist so that a broken
   dependency degrades one capability rather than the whole gateway.

---

## Afterwards

With a 0.05% budget there is very little room. Check
**USSLP — SLOs and Error Budgets** before approving the next deploy; under 25%
remaining should mean a freeze.
