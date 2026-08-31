# USSLP compose profiles

Two profiles, and they answer different questions.

**`dev`** answers "does the platform work". Pure Go, nothing but binaries out of
this repository, boots in seconds on a laptop.

**`prod-like`** answers "what does the production topology look like, and does
the platform behave against real infrastructure". Kafka, EMQX, PostgreSQL,
ClickHouse, Redis, Prometheus, Grafana, Jaeger.

---

## dev

```bash
docker compose -f deploy/compose/docker-compose.dev.yml up --build
# or: make dev
```

### What runs

| Container | Host ports | Notes |
|---|---|---|
| `api-gateway` | 8080, admin **9080** | its admin default is 9080, not 9090 |
| `uig` | 8081, admin 9081 | |
| `label-service` | 8082, admin 9082 | ephemeral signing key — see below |
| `ota-service` | 8084, admin 9084 | uploads refused — see below |
| `pricing-service` | 8085, admin 9085 | |
| `promotion-service` | 8086, admin 9086 | |
| `analytics-service` | 8087, admin 9087 | its own event log — see below |
| `store-gateway` (sgu) | 1883 broker, 8090 diagnostics, 9090 admin | |
| `cloud-broker` (sgu) | 1884 broker, 9091 admin | stands in for EMQX |
| `labelsim` | 8099 status, 9099 admin | 4 controllers × 250 labels |

Opt-in:

```bash
docker compose -f deploy/compose/docker-compose.dev.yml --profile registry up device-registry
docker compose -f deploy/compose/docker-compose.dev.yml --profile edge     up sec
docker compose -f deploy/compose/docker-compose.dev.yml --profile obs      up prometheus
```

### Four things that are not what they look like

**1. The event stream does not cross process boundaries.**
`pkg/eventlog` is an embedded log whose consumer-group coordination lives in
memory (`platform/pkg/eventlog/consumer.go`), so two processes must not share one
log directory. Each service therefore has its own event-log volume, which means
the UIG's `price-updates` records do **not** reach the Label Service's consumer
here. The services are wired over MQTT and HTTP, which is real; the event stream
is per-process, which is a property of the embedded log rather than a
configuration mistake. The Kafka adapter behind `pkg/eventbus.Bus` is the
production port.

**2. `cloud-broker` is the SGU binary.** The repository has no standalone broker
command and the SGU embeds `pkg/mqtt`'s broker, so an SGU instance stands in for
EMQX and gives the store gateway's cloud bridge something to bridge to. It logs
"no cloud broker configured: this store will run permanently autonomous" on
start, which is expected — it has no cloud of its own.

**3. `label-service` signs with a key nobody has published.** With no
`USSLP_PRICE_AUTHORITY_DIR` it generates an ephemeral Ed25519 key and warns that
attestations signed with it verify against no key ring in the field. Correct for
a laptop, catastrophic anywhere else.

**4. `ota-service` refuses every upload.** With no `USSLP_OTA_SIGNING_KEYS` it
starts normally and logs the refusal once, at boot. Rollout planning, cohorts and
quiet hours are all exercisable; uploading an artifact is not.

**5. `analytics-service` reports on a stream it cannot see.** It runs, serves its
reports and answers the gateway, but like every service here it opens its own
`pkg/eventlog` directory, so the rows it would project from `price-updates`,
`label-delivery`, `label-telemetry` and `device-events` never reach it. That is
the cross-service stream limitation in point 1, not a fault in the service: the
prod-like profile and `usslpd` both give it a shared log and the reports fill in.

### Why device-registry and sec are opt-in

Neither can start without key material this repository has no command to
produce. `device-registry` calls `pki.Load(USSLP_REGISTRY_PKI_DIR)` and refuses
to start without a hierarchy — which is correct: a registry that enrolled devices
without verifying them would be worse than no registry. `sec` declares
`USSLP_KEYRING_FILE` required.

Populate `deploy/compose/pki/` (bind-mounted read-only at `/etc/usslp/pki`) with
a hierarchy from `pki.Bootstrap` + `Save`, and a key ring at `keyring.json`.

### Watch the offline story

```bash
docker compose -f deploy/compose/docker-compose.dev.yml pause cloud-broker
curl -s http://localhost:8090/mode      # autonomous within ~5 seconds
curl -s http://localhost:8090/queue     # the upstream buffer fills
curl -s http://localhost:9090/readyz | jq   # still ready — deliberately
docker compose -f deploy/compose/docker-compose.dev.yml unpause cloud-broker
curl -s http://localhost:8090/reconciliation | jq
```

Readiness stays green throughout, and that is the point: a store gateway has no
load balancer in front of it and nowhere else for its controllers to go, so an
unreachable cloud must not make it look unhealthy.

The WAN detector is tuned aggressively here (2s probes, fail after 2) so the flip
happens inside a demo. Production defaults are in
`deploy/edge/config/sgu.env.template`, and they are asymmetric on purpose.

---

## prod-like

```bash
cp deploy/compose/.env.example deploy/compose/.env    # edit it
docker compose -f deploy/compose/docker-compose.prod-like.yml --profile all up --build -d
# or: make prod-like
```

| | |
|---|---|
| Grafana | http://localhost:3000 (all four dashboards provisioned) |
| Prometheus | http://localhost:9190 |
| Jaeger | http://localhost:16686 |
| EMQX dashboard | http://localhost:18083 |
| Kafka | localhost:9092 |
| Kafka Connect | http://localhost:8083 |
| PostgreSQL | localhost:5432 |
| ClickHouse | http://localhost:8123 |
| Redis | localhost:6379 |

Sub-profiles: `--profile infra`, `--profile obs`, `--profile usslp`.

### What is genuinely exercised here

**The MQTT tier.** `platform/pkg/mqtt` is a complete MQTT 3.1.1 client —
protocol name `MQTT`, level 4 (`platform/pkg/mqtt/packet.go`) — so the services
talk to EMQX exactly as they would in production. Retained messages, QoS 1 for
prices, QoS 2 for OTA, sessions surviving a reconnect: all real.

**The topic catalogue.** The `kafka-topics` job creates all eleven streams with
the partition counts and retentions from `canon.AllStreams()` — 5,472 partitions
in total, including `price-updates` at 1,024 and `label-telemetry` at 2,048.
`make verify-topics` proves the transcription matches the Go source.

### What is provisioned but not connected

Kafka, PostgreSQL, ClickHouse and Redis all run, correctly configured, and
nothing in the Go tree connects to any of them. The services still use their
in-tree embedded event log and kvstore. That is deliberate and it is the honest
shape of the work: the ports exist behind `pkg/eventbus.Bus` and the repository
interfaces, the adapters do not, and this profile provisions the infrastructure
those adapters will be written against.

When they land, the change here is environment variables — not services.

### Resource appetite

`--profile all` wants roughly 16 GB of RAM and 8 cores. Every service has
`deploy.resources` limits, so it degrades rather than thrashing, but Kafka and
ClickHouse in particular are not laptop-shaped. Use `--profile infra` alone if
you only need the infrastructure.
