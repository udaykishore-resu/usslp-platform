# USSLP — Deployment and Operations

Everything needed to run USSLP, from a laptop to three production regions.

**Read section 2 before drawing conclusions from anything else.** It says
plainly what is exercised end to end today and what is a provisioned production
target waiting for an adapter. The distinction matters more here than in most
systems, because the platform's whole shape is ports and adapters and several
of the adapters are the next piece of work rather than this one.

---

## 1. Layout

```
deploy/
  docker/          one parameterised Dockerfile + build script for every binary
  compose/         dev (pure Go) and prod-like (real infrastructure) profiles
  helm/usslp/      the cloud tier chart, plus five environment values files
  istio/           STRICT mTLS, connection pools, outlier detection, retries
  policy/          Gatekeeper constraint templates and Kyverno policies
  argocd/          ApplicationSet per region × environment, Argo Rollouts canaries
  observability/   Prometheus scrape + rules, four Grafana dashboards, OTel
  edge/            systemd units, K3s manifests, installer, updater, runbook
  terraform/       EKS, MSK, Aurora, ElastiCache, S3, KMS, IAM/IRSA, networking
  runbooks/        the procedures the alert rules link to
.github/workflows/ CI and release
Makefile           what CI runs, and what you can run
```

---

## 2. What is real, and what is a documented production target

### Real, exercised, load-bearing

**Every USSLP binary.** `api-gateway`, `uig`, `label-service`, `device-registry`,
`ota-service`, `pricing-service`, `promotion-service`, `analytics-service`,
`sgu`, `sec`, `labelsim`. All build and run; the dev compose profile starts them
and they talk to each other.

**The MQTT tier.** `platform/pkg/mqtt` is a complete MQTT 3.1.1 implementation —
protocol name `MQTT`, protocol level 4, see `platform/pkg/mqtt/packet.go`. The
services speak to EMQX over the wire exactly as they would in production:
retained messages, QoS 1 for prices, QoS 2 for OTA triggers, sessions that
survive a reconnect. The prod-like profile exercises all of it against a real
broker.

**The metrics surface.** `obs.Registry` writes the Prometheus text exposition
format directly (`platform/pkg/obs/metrics.go`). No exporter, no adapter, no
push gateway — Prometheus scrapes the process. Every metric name in every alert
rule and every dashboard is checked against the Go source in CI
(`make verify-metrics`).

**The stream catalogue.** Partition counts and retentions come from
`canon.AllStreams()`. The Helm chart, the compose topic job and the MSK module
each transcribe it, and `make verify-topics` fails if any of the three has
drifted by a single partition.

### Provisioned, not yet wired

**The event stream across processes.** `pkg/eventlog` is an embedded,
file-backed log whose consumer-group coordination lives in memory
(`platform/pkg/eventlog/consumer.go`). Two processes must not share one log
directory. So in the dev compose profile the UIG's `price-updates` records do
**not** reach the Label Service's consumer — each service has its own log. The
services are wired over MQTT and HTTP, which is real; the cross-service event
stream is not. The Kafka adapter behind `pkg/eventbus.Bus` is the documented
production port, and the prod-like profile provisions the Kafka it will target.

**PostgreSQL, ClickHouse and Redis.** Nothing in the Go tree connects to any of
them. The event store and every read model are on `pkg/kvstore`, an embedded LSM
store. The prod-like profile and the Terraform provision all three with the
schemas, roles and parameters the documented port expects, so the adapters have
something to be written against.

**OTLP tracing.** `obs.NewRuntime` wires the tracer with `obs.LogExporter` —
spans go to the structured log and the OTel collector's `filelog` bridge
reconstructs them, which is a stopgap and is documented as one. The span log has
its own level and its own rate (`USSLP_SPAN_LOG_LEVEL`, `USSLP_SPAN_LOG_ONE_IN`)
precisely so that production's `info` application default no longer silences it,
which is what it used to do.

### Absent

Nothing. Every service the chart declares has a binary behind it.

`analytics-service` was the last entry under this heading and no longer belongs
here: `platform/cmd/analytics-service/main.go` exists, builds, is constructed by
`usslpd`, and is now `enabled: true` in the chart and present in both the CI and
release image matrices. The `api-gateway`'s analytics upstream therefore reaches
a service that answers, and
`usslp_gateway_breaker_state{upstream="analytics",state="open"}` firing is now a
real fault rather than the expected steady state — the Argo analysis template
that tolerated one open breaker has been tightened accordingly.

---

## 3. Running it

### The dev profile — pure Go, no infrastructure

```bash
make dev            # or: docker compose -f deploy/compose/docker-compose.dev.yml up --build
make dev-logs
make dev-down
```

| | |
|---|---|
| api-gateway | http://localhost:8080 — admin **9080** |
| uig | http://localhost:8081 — admin 9081 |
| label-service | http://localhost:8082 — admin 9082 |
| ota-service | http://localhost:8084 — admin 9084 |
| pricing-service | http://localhost:8085 — admin 9085 |
| promotion-service | http://localhost:8086 — admin 9086 |
| analytics-service | http://localhost:8087 — admin 9087 |
| SGU diagnostics | http://localhost:8090/status |
| label simulator | http://localhost:8099/store |
| store MQTT | localhost:1883 |
| cloud MQTT | localhost:1884 |

Two services are opt-in because they cannot start without key material the
repository has no command to produce:

```bash
docker compose -f deploy/compose/docker-compose.dev.yml --profile registry up device-registry
docker compose -f deploy/compose/docker-compose.dev.yml --profile edge     up sec
docker compose -f deploy/compose/docker-compose.dev.yml --profile obs      up prometheus
```

`device-registry` calls `pki.Load(USSLP_REGISTRY_PKI_DIR)` and refuses to start
without a hierarchy — correct, since a registry that enrolled devices without
verifying them would be worse than no registry. `sec` declares
`USSLP_KEYRING_FILE` required. Populate `deploy/compose/pki/` from the key
ceremony first.

Watch the offline story work:

```bash
docker compose -f deploy/compose/docker-compose.dev.yml pause cloud-broker
curl -s http://localhost:8090/mode      # flips to autonomous in ~5 seconds
curl -s http://localhost:8090/queue     # the upstream buffer fills
docker compose -f deploy/compose/docker-compose.dev.yml unpause cloud-broker
```

### The prod-like profile — real infrastructure

```bash
cp deploy/compose/.env.example deploy/compose/.env   # edit it
make prod-like
```

Kafka in KRaft mode with every topic from `canon.AllStreams()`, EMQX,
PostgreSQL 16, ClickHouse, Redis, Prometheus, Grafana, Jaeger, an OTel
collector, Kafka Connect, and the USSLP services against them. Grafana at
:3000, Prometheus :9190, Jaeger :16686, EMQX dashboard :18083.

### Kubernetes

```bash
helm upgrade --install usslp deploy/helm/usslp \
  --namespace usslp --create-namespace \
  --values deploy/helm/usslp/values.yaml \
  --values deploy/helm/usslp/values-prod-use1.yaml
```

In practice ArgoCD does this — see `deploy/argocd/applicationset.yaml`.
Production Applications track `release-*` tags with auto-sync **off**.

Apply the mesh policy and the admission policies separately; they have a
different blast radius from a service release and should not ride along with an
image bump:

```bash
kubectl apply -f deploy/istio/
kubectl apply -f deploy/policy/gatekeeper/constrainttemplates.yaml
kubectl apply -f deploy/policy/gatekeeper/constraints.yaml   # one residency constraint per cluster
kubectl apply -f deploy/policy/kyverno/policies.yaml
```

### The edge

```bash
make build
sudo deploy/edge/install.sh --store store-0001 --tenant acme \
     --region us-east-1 --controllers 25 \
     --cloud-broker tls://mqtt.us-east-1.usslp.example:8883
```

Or K3s: `k3s kubectl apply -f deploy/edge/k3s/`. Both are supported;
`deploy/edge/k3s/usslp-sgu.yaml` explains when each is right.

### Infrastructure

```bash
cd deploy/terraform/regions/us-east-1
terraform init && terraform plan
terraform output helm_values     # the values that replace the REPLACE-ME placeholders
```

---

## 4. Verification

```bash
make verify          # everything CI checks
make verify-deploy   # just the deployment layer — no network, no Go cache needed
```

| Target | What it proves |
|---|---|
| `yaml-check` | every YAML and JSON file under `deploy/` and `.github/` parses |
| `helm-lint` | `helm lint --strict` and `helm template` against all five values files, plus a structural checker for environments with no helm |
| `verify-rules` | the chart's copy of the Prometheus rules matches the canonical one; `promtool check rules` on the PromQL |
| `verify-metrics` | every metric name **and every label** in the rules and dashboards is registered in the Go tree |
| `verify-topics` | the four transcriptions of the stream catalogue all match `canon.AllStreams()` |
| `tf-check` | `terraform fmt -check` and `validate`, or a structural HCL checker |
| `shell-check` | `bash -n` and shellcheck |

`verify-metrics` is the one that has repeatedly earned its place. A plausible
metric name in an alert rule produces an expression that evaluates to nothing,
forever, silently — an alert that can never fire looks exactly like coverage.
The label check catches the subtler version: a selector on a label the metric
does not declare matches *every* series, because an absent label compares equal
to `""`, so the alert either never fires or always does. It caught two real
mistakes while this layer was being written.

---

## 5. Operational runbook index

| Situation | Runbook |
|---|---|
| Price updates are slow — `USSLPPricePathErrorBudgetBurn*` | [runbooks/price-path-latency.md](runbooks/price-path-latency.md) |
| Cloud API errors — `USSLPCloudAPIErrorBudgetBurn*` | [runbooks/cloud-api-availability.md](runbooks/cloud-api-availability.md) |
| POS ingest is slow or refusing — `USSLPPOSIngest*`, `USSLPUIG*` | [runbooks/pos-ingest.md](runbooks/pos-ingest.md) |
| A price could not be signed or verified — `USSLPAttestationFailure`, `USSLPControllerComplianceRefusal` | [runbooks/attestation-failure.md](runbooks/attestation-failure.md) |
| Labels offline, stores autonomous — `USSLPLabelAvailability*`, `USSLPStores*` | [runbooks/fleet-health.md](runbooks/fleet-health.md) |
| A store gateway is down or looping — `USSLPSGU*` | [runbooks/sgu-recovery.md](runbooks/sgu-recovery.md), and [edge/RUNBOOK.md](edge/RUNBOOK.md) for on-site work |
| An OTA rollout is failing — `USSLPOTA*` | [runbooks/ota-rollout.md](runbooks/ota-rollout.md) |
| The broker is dropping or disconnecting — `USSLPMQTT*` | [runbooks/mqtt-broker.md](runbooks/mqtt-broker.md) |
| A deploy went wrong | [runbooks/rollback.md](runbooks/rollback.md) |

Every `runbook_url` annotation in
`deploy/observability/prometheus/rules/*.yaml` points into that table.

---

## 6. Things that will bite you

Collected because each of these was found by reading the Go source rather than
by assuming, and each would otherwise have been a silent misconfiguration.

**Three binaries default to `127.0.0.1`.** `device-registry` binds
`127.0.0.1:8081` and `127.0.0.1:9101`; `ota-service` binds `127.0.0.1:8082` and
`127.0.0.1:9102`; the SGU's diagnostics port defaults to `127.0.0.1:8080`. All
correct for an appliance, all useless in a pod. The chart and both compose files
override every one of them.

**`api-gateway`'s admin port is `:9080`, not `:9090`.** It is the only binary
that overrides the shared default. The Helm chart pins it to 9090 so the shared
ServiceMonitor, probes and NetworkPolicy need no special case; the dev compose
file leaves it at 9080 and points Prometheus there.

**`USSLP_UIG_ADDR` means two different things.** To the `uig` binary it is its
own listen address. To `api-gateway` it is the URL of the UIG upstream. They
never collide because each process has its own environment — but a ConfigMap
shared between them would silently break one.

**`label-service`'s drain is a compile-time constant.**
`const shutdownGrace = 20 * time.Second` in
`platform/cmd/label-service/main.go`. There is no environment variable.
`terminationGracePeriodSeconds` must exceed it; the chart's `usslp.validate`
helper fails the render if it does not.

**Three names for the pricing service.** The workload is `pricing-ai-service`
(the capacity table's name), the image is `pricing-service` (the binary), and
the metric `service` label is `"pricing-service"` (the `serviceName` constant).
Each is correct in its own place; a query or a canary analysis that uses the
wrong one silently matches nothing. Same shape for `pos-integration-gw`/`uig`.

**Some services start "successfully" while refusing to do their job.**
`label-service` with no `USSLP_PRICE_AUTHORITY_DIR` generates an ephemeral
signing key whose attestations verify against no key ring in the field.
`ota-service` with no `USSLP_OTA_SIGNING_KEYS` refuses every artifact upload.
Both log it once, at boot, and then look healthy.

**Readiness and liveness are not interchangeable.** `/healthz` answers 200
unconditionally — it asks only whether the process is scheduling goroutines.
`/readyz` runs the dependency checks. Registering a dependency on liveness is
how a five-second broker wobble becomes a cluster-wide restart storm, which is
why `INTERFACE-CONTRACTS` §7 forbids it and why nothing here does it.
