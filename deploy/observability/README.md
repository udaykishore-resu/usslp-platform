# USSLP observability

```
prometheus/
  rules/                      CANONICAL. Recording rules, SLO alerts, component alerts.
grafana/dashboards/           Four dashboards, provisioned by the compose profiles.
otel/otel-collector.yaml      The collector, and an honest note about tracing.
verify-metrics.py             Checks every metric name AND label against the Go source.
```

`deploy/compose/config/prometheus-*.yml` hold the scrape configs; the Kubernetes
equivalent is the chart's ServiceMonitor.

---

## Where the rules live, and why there are two copies

`deploy/observability/prometheus/rules/*.yaml` is the **canonical source**. They
are plain Prometheus rule files, which is the form `promtool check rules` and the
compose profiles consume directly.

`deploy/helm/usslp/files/rules/` is a **generated copy** that the chart's
`PrometheusRule` template embeds with `.Files.Glob` — Helm's `.Files` cannot
reach outside a chart, so a copy is unavoidable. `make helm-sync-rules`
regenerates it and `make verify-rules` fails CI if the two have drifted.

One source, two consumers, and a check that keeps them honest.

---

## The verifier

```bash
make verify-metrics
python3 deploy/observability/verify-metrics.py --list    # every registered name
```

It extracts every metric registered through `obs.Registry`'s `Counter`, `Gauge`
and `Histogram` constructors — including the ones registered via string constants
in `platform/pkg/mqtt/metrics.go` and the namespaced families `pkg/kvstore`
builds from a caller-supplied prefix — and checks every name and every label
selector in the rules and dashboards against them.

**Why this earns its place.** A plausible metric name in an alert rule produces
an expression that evaluates to nothing, forever, silently. An alert that can
never fire looks exactly like coverage.

The label check catches the subtler version. A selector on a label the metric
does not declare matches *every* series, because an absent label compares equal
to `""` — so the alert either never fires or always fires. It caught two real
mistakes while these rules were being written: `usslp_promotion_conflicts` is
labelled `severity`, and `usslp_promotion_transitions_total` is labelled `to`,
not `state`.

---

## Why the alerts are burn rates

The naive version is "page when p99 > 3s for 5 minutes". It is wrong in both
directions at once.

**Too sensitive:** at 52,000 price updates a second, a brief regional blip
consuming 0.01% of a month's error budget still crosses a raw threshold. Paging
on that teaches people to close pages without reading them.

**Not sensitive enough:** a service that sits at 98.9% for a week never crosses a
"p99 > 3s" threshold at all — every individual window looks nearly fine — and has
burned the entire month's budget. Nobody is paged and the SLO is missed.

A burn rate asks one question instead: *at the rate we are currently failing, how
long until the whole month's budget is gone?*

```
burn_rate = observed_error_ratio / error_budget
```

Rate 1 exhausts the budget in exactly 30 days. Rate 14.4 exhausts 2% of it in an
hour.

Each alert pairs a long window with a short one — 14.4× over 1h confirmed by 5m,
6× over 6h confirmed by 30m. The long window says *this is real*; the short
window is what makes the alert **stop** once the burn does. Without it a
1h-window alert keeps firing for an hour after resolution, which is how people
learn to ignore the resolution notification too.

| SLO | Objective | Budget | Fast-burn threshold |
|---|---|---|---|
| Price update ≤ 3 s | 99% | 1% | 14.4% late |
| Label online | 99.5% | 0.5% | 7.2% offline |
| SGU uptime | 99.9% | 0.1% | 1.44% unreachable |
| Cloud API | 99.95% | 0.05% | 0.72% errors |
| OTA success | 99.7% | 0.3% | 4.32% failed |
| POS ingest ≤ 500 ms | 95% | 5% | 72% late |
| Attestation accuracy | 100% | **none** | any failure |

Attestation is the deliberate exception. A burn-rate alert says "you may fail
this much"; the attestation contract says a label never displays a price it
cannot verify, so the acceptable number is zero and there is nothing to burn.
It fires on any increase.

`USSLPSGURestartLoop` is the other non-burn-rate alert, and for a related reason:
a gateway that restarts every four minutes has ~100% scrape availability and is
nonetheless broken. `resets(usslp_process_uptime_seconds)` sees what the uptime
SLI structurally cannot.

---

## Dashboards

| File | Question it answers |
|---|---|
| `platform-overview.json` | Is the platform healthy? Four stat panels, then throughput, stream and MQTT. |
| `price-path-latency.json` | Where in the 3-second budget did the time go? Per hop, with a **residual** panel for the hops that are not instrumented. |
| `fleet-health.json` | Which stores are unhappy and why? A sortable per-store table plus mesh and OTA. |
| `slo-error-budget.json` | How much budget is left, and should we deploy? |

The residual panel on the price-path board is the most useful single thing here:
a residual that grows while every measured hop is flat means the problem is at
the far end of the mesh, not in the cloud.

---

## Tracing — read this before trusting a trace

`obs.NewRuntime` wires the tracer with `obs.LogExporter`
(`platform/pkg/obs/runtime.go`, `spanExporters`): spans go to the structured
log, and the OTel collector's `filelog` pipeline turns them back into spans.
That is a stopgap, not the tracing story — but it is a stopgap that works.

It did not, for a while, and the reason is worth keeping: the exporter wrote at
**debug** on the service's own logger, and `config.LoadService` defaults
`LOG_LEVEL` to `info` when the environment is `prod`. In a production cluster
the span lines were never emitted, the `filelog` pipeline received nothing, and
no trace backend held any USSLP data — a tracing outage with no symptom except
an empty UI.

Spans now have their own logger and their own two knobs, independent of
`USSLP_LOG_LEVEL`:

| | Default | What it does |
|---|---|---|
| `USSLP_SPAN_LOG_LEVEL` | `info` | Severity the span log is written at. `off` disables it. |
| `USSLP_SPAN_LOG_ONE_IN` | `100` (prod), `1` elsewhere | Writes one trace in N to the span log. |

The second exists because `Tracer.StartAlwaysSampled` bypasses head sampling on
the price path, provisioning and OTA — where every trace is evidence. At 52,000
price updates a second those are all of the spans, so what is *recorded* and
what is *written to the log bridge* have to be separately controllable. The
tracer's own sampling is untouched: an unsampled span still reaches no exporter.
Thinning is keyed on the trace id, so a trace arrives whole or not at all.

For an investigation, set `USSLP_SPAN_LOG_ONE_IN=1` on the one service under
investigation for as long as it takes.

The real fix is still an OTLP exporter in `pkg/obs`, at which point the
`filelog` receiver, its pipeline entry and both knobs are deleted together and
nothing else here changes.

The collector's tail sampling is worth understanding regardless: head sampling
cannot keep a trace *because* it turned out to be slow, since the decision is
made before the latency is known. Tail sampling waits for the whole trace, which
is exactly what a 3-second budget needs — the interesting traces are the slow
ones and the failed ones, and they are rare.

---

## Adding an alert

1. Add it to `deploy/observability/prometheus/rules/`.
2. Use a metric that exists. `verify-metrics.py --list` is the inventory.
3. Use a label the metric declares. The verifier checks this, and it is the
   mistake that produces an alert which always fires.
4. Give it a `runbook_url` pointing into `deploy/runbooks/`.
5. `make helm-sync-rules && make verify-deploy`.
6. Prefer a burn rate. Reach for a threshold only when the thing being measured
   genuinely has no budget — and say why in a comment, as the attestation and
   restart-loop alerts do.
