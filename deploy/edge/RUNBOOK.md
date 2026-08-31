# USSLP Edge — Failure Recovery Runbook

For the Store Gateway Unit and the Shelf Edge Controllers. Written to be usable
by someone who has been woken up, in a store they have never visited, over a
link they cannot rely on.

**The first thing to know:** a store whose cloud link is down is *not* an
outage. The gateway runs the store's MQTT broker and an offline pricing brain;
the controllers only ever talk to a broker inside the building. When the cloud
link drops the bridge stops and the local broker does not. Labels keep their
prices and the store keeps trading. That is the design, not a degraded mode —
see `docs/architecture/INTERFACE-CONTRACTS.md` §3.

So before doing anything: **find out whether the shelves are wrong, or only the
cloud's picture of them.** Those need completely different responses, and the
second one has no urgency at all.

---

## 0. Triage in ninety seconds

```bash
# On the gateway box:
systemctl status usslp-sgu.service
curl -s http://127.0.0.1:9090/readyz | jq          # what the gateway thinks
curl -s http://127.0.0.1:8090/status  | jq         # what the store looks like
curl -s http://127.0.0.1:8090/mode                 # autonomous or not
curl -s http://127.0.0.1:8090/queue   | jq         # how much is buffered
```

| What you see | What it means | Go to |
|---|---|---|
| `readyz` 200, mode autonomous, queue small | The WAN is down. The store is fine. | [§1](#1-the-store-is-autonomous) |
| `readyz` 200, mode autonomous, queue near 50,000 | The WAN has been down a long time and the outage record is about to be lost | [§2](#2-the-upstream-buffer-is-filling) |
| `readyz` 503, `store-broker` failing | The broker is not listening. **The store's labels are frozen.** | [§3](#3-the-store-broker-is-not-listening) |
| `readyz` 503, `durable-store` failing | The state store will not write | [§4](#4-the-durable-store-is-unwritable) |
| Unit in `activating (auto-restart)` | Crash loop | [§5](#5-the-gateway-is-crash-looping) |
| Gateway fine, one zone's labels stale | A controller is down or its mesh is broken | [§6](#6-a-shelf-edge-controller-is-down) |
| Prices refused, `sec_compliance_alerts_total` rising | Attestation is failing | [§7](#7-attestation-failures) |
| Everything fine but the last update just ran | A bad update | [§8](#8-a-bad-update) |

`readyz` names the failing check, so read its body rather than guessing:

```json
{"status":"not ready","checks":{
  "_startup":"ok",
  "store-broker":"the store's MQTT broker is not listening",
  "durable-store":"ok",
  "upstream-buffer":"ok"}}
```

---

## 1. The store is autonomous

**Symptom.** `sgu_store_mode` is 1. `USSLPStoreAutonomous` has fired.

**What is happening.** The gateway's WAN detector saw the cloud broker fail its
probe often enough for long enough (by default 3 failures sustained over 12
seconds) and stopped the bridge. The local broker did not stop. Controllers
never noticed. Labels are being priced from the store's local rules.

**Urgency.** Low. Nothing on a shelf is wrong. What is accumulating is a gap in
the cloud's record of this store, which the gateway will backfill from its
upstream queue when the link returns.

**What to check, in order:**

1. **Is it one store or many?**
   ```promql
   usslp:stores_autonomous:count
   ```
   One store is that store's broadband. Fifty at once is the cloud side —
   the broker, its load balancer, or the region — and this runbook is the wrong
   one; go to `deploy/runbooks/mqtt-broker.md`.

2. **Is the WAN actually down, or is it the broker?**
   ```bash
   # From the gateway box:
   ping -c3 1.1.1.1                                   # is there internet at all
   getent hosts mqtt.us-east-1.usslp.example          # is DNS working
   openssl s_client -connect mqtt.us-east-1.usslp.example:8883 -brief </dev/null
   ```
   Internet up + DNS up + TLS refused means the broker or its certificate, not
   the store.

3. **Are the credentials still valid?** A device credential that expired looks
   exactly like a WAN outage from the store's side. Check the journal:
   ```bash
   journalctl -u usslp-sgu.service --since '1 hour ago' | grep -i 'connect\|auth\|refused'
   ```

**Recovery.** When the link returns the detector needs 4 successful probes
sustained over 15 seconds before it re-bridges — deliberately slower than the
failure path, because a flapping link treated as up produces a bridge that
starts and stops repeatedly, and every flip produces reconciliation conflicts.
Watch `sgu_merge_conflicts_total` afterwards: a burst is normal, a sustained
rate means the store's local rules and the central prices genuinely disagree.

**Do not** restart the gateway to "fix" autonomous mode. That takes down the
only thing pricing the store's shelves.

---

## 2. The upstream buffer is filling

**Symptom.** `sgu_upstream_queue{measure="depth"}` above 45,000 (default cap
50,000). `USSLPSGUUpstreamQueueNearFull` has fired.

**Why this one is a page and §1 is not.** Full is not "unready" — the store is
still pricing correctly, and the gateway's own readiness check says so in those
words. But it is the one queue state that *loses data*: once the buffer fills,
the cloud's record of this outage has gaps that no replay can recover. Delivery
acknowledgements, telemetry, mode transitions — gone.

**Immediate mitigation** (buys hours, does not fix anything):

```bash
# Raise the cap. Requires a restart, which is safe here: the queue is on disk.
sudoedit /etc/usslp/sgu.env      # USSLP_QUEUE_MAX_ENTRIES, USSLP_QUEUE_MAX_MB
systemctl restart usslp-sgu.service
curl -s http://127.0.0.1:9090/readyz | jq
```

Check free disk first — `df -h /var/lib/usslp`. Raising the cap past what the
disk holds turns a data-loss problem into a full-disk problem, which also takes
the durable store down.

**Real fix.** Restore the WAN. §1.

---

## 3. The store broker is not listening

**Symptom.** `readyz` reports `store-broker: the store's MQTT broker is not
listening`.

**This is the real outage.** No broker means no controller can receive an
update. Labels keep displaying their last price — they are E-Ink, they hold
without power — so the shelves are not blank, they are *stale*. If a price
changed in the POS in the last few minutes, the till and the shelf now disagree,
which is a compliance exposure with a clock on it.

**Diagnose:**

```bash
ss -tlnp | grep 1883                      # is anything listening
journalctl -u usslp-sgu.service -n 100 --no-pager | grep -i 'bind\|address'
```

Two causes account for almost all of these:

1. **The port is taken.** Something else on the box bound 1883 — a mosquitto
   left over from a proof of concept is the classic. `ss -tlnp` names it.
   ```bash
   systemctl stop mosquitto && systemctl disable mosquitto
   systemctl restart usslp-sgu.service
   ```

2. **`USSLP_BROKER_ADDR` was narrowed to 127.0.0.1.** The broker is listening,
   just not where the controllers can reach it. It must be `0.0.0.0:1883` unless
   every controller is on this box. The systemd unit's `IPAddressAllow` is what
   restricts who can reach it, not the bind address.

**Verify recovery from a controller's point of view, not the gateway's:**

```bash
# From a controller box, or any host on the store LAN:
nc -zv <gateway-lan-ip> 1883
```

---

## 4. The durable store is unwritable

**Symptom.** `readyz` reports `durable-store: the durable store is not writable`.

**Causes, in the order they actually occur:**

1. **The disk is full.**
   ```bash
   df -h /var/lib/usslp
   du -sh /var/lib/usslp/*
   ```
   Usually the upstream queue (§2) or an event log that never rotated. Do
   **not** delete files from `/var/lib/usslp` by hand — the write-ahead log and
   the checkpoint are a pair, and removing one leaves a store that cannot
   recover. Raise the disk, or if that is impossible, see the destructive reset
   at the end of this section.

2. **Permissions.** After a manual file copy, or a restore from backup taken as
   root:
   ```bash
   chown -R usslp:usslp /var/lib/usslp
   chmod 0700 /var/lib/usslp
   systemctl restart usslp-sgu.service
   ```

3. **A corrupt write-ahead log after an unclean power loss.** The store
   re-validates every segment tail on open and truncates damaged ones, so this
   usually recovers on its own — but it can take minutes on a large store, and
   the startup probe allows five minutes for exactly that. Watch it rather than
   intervening:
   ```bash
   journalctl -u usslp-sgu.service -f | grep -i 'recover\|truncat\|segment'
   ```

**Destructive last resort.** Only when the store cannot recover and cannot be
given more disk:

```bash
systemctl stop usslp-sgu.service
mv /var/lib/usslp /var/lib/usslp.broken.$(date +%s)
install -d -m 0700 -o usslp -g usslp /var/lib/usslp
systemctl start usslp-sgu.service
```

**What this costs:** the upstream queue (every unsent acknowledgement and
telemetry batch — permanently lost) and the store's replica of the label
directory (rebuilt from the cloud on reconnect, which requires the WAN to be
up). **Do not do this while the store is autonomous.** Keep the `.broken`
directory for the postmortem.

---

## 5. The gateway is crash-looping

**Symptom.** `systemctl status` shows `activating (auto-restart)`.
`USSLPSGURestartLoop` has fired — `resets(usslp_process_uptime_seconds)` catches
this, and the scrape-based uptime SLI cannot: a gateway restarting every four
minutes has ~100% scrape availability and is nonetheless broken.

```bash
journalctl -u usslp-sgu.service -n 200 --no-pager
```

| Log line | Cause | Fix |
|---|---|---|
| `config: USSLP_STORE_ID is required; USSLP_TENANT_ID is required` | The env file was reset or never written | Restore `/etc/usslp/sgu.env`. The loader reports **every** missing variable at once, so fix them all in one edit. |
| `bind ...: address already in use` | §3 | |
| `opening the durable store` / `creating data directory` | §4 | |
| `_FILE=/etc/usslp/secrets/...: no such file` | A secret file is missing | The loader reads `NAME_FILE` before `NAME`; a broken path is a hard error, not a fallback. Restore the file or unset the `_FILE` variable. |
| Killed, exit 137, shortly after start | `MemoryMax=2G` in the unit | Check whether the store is genuinely larger than the unit allows before raising it. |

**If the loop started right after an update,** go to §8 — do not debug, roll
back first.

**Stop the loop while you work:**

```bash
systemctl stop usslp-sgu.service
# The labels hold their prices. The store is stale, not blank. Do not leave it
# stopped longer than it takes to fix.
```

---

## 6. A Shelf Edge Controller is down

**Symptom.** One zone's labels are stale; the rest of the store is fine.
`USSLPMeshDeliveryFailures` may have fired for that `sec`.

```bash
systemctl status 'usslp-sec@sec-0007.service'
journalctl -u 'usslp-sec@sec-0007.service' -n 100 --no-pager
curl -s http://127.0.0.1:9090/readyz | jq     # on the controller's box
```

The controller registers two readiness checks: `gateway-link` and `zone-mesh`.

**`gateway-link` failing.** The controller cannot reach the store's broker.
Check §3 first — if the broker is down every controller will show this. If only
one does, it is that controller's network.

**`zone-mesh` failing.** The controller is connected to the broker but its
Zigbee mesh is degraded. This is usually physical:

```promql
sec_mesh_link_failure_risk{sec="sec-0007"}          # what the model predicts
rate(sec_mesh_reroutes_total{sec="sec-0007"}[1h])   # how much it is re-routing
histogram_quantile(0.99, sum by (le) (rate(sec_mesh_hops_bucket{sec="sec-0007"}[5m])))
```

Rising hop counts and rising reroutes in one zone means the radio path changed:
a shelf moved, a metal fixture added, a freezer door propped open, a pallet
parked in the aisle. Predictive healing routes around it; if it cannot, somebody
has to look at the aisle.

**Restarting one controller is safe.** It rebuilds its zone state from the
retained messages on the store's broker — which is exactly why price updates are
retained. The 15-second drain plus the unit's 30-second stop timeout means the
zone is unmanaged for well under a minute, and the labels hold their prices
throughout.

```bash
systemctl restart 'usslp-sec@sec-0007.service'
```

**Restarting all of them at once is not safe** and the K3s DaemonSet's
`maxUnavailable: 1` exists to prevent it. Twenty-five controllers restarting
together takes every shelf off the mesh at the same moment.

---

## 7. Attestation failures

**Symptom.** `sec_compliance_alerts_total` rising.
`USSLPControllerComplianceRefusal` has fired. Prices are not changing in one or
more zones, and nothing looks broken.

**What is happening.** The controller recomputes the SHA-256 digest from the
update it is holding — never from the transmitted digest — and verifies the
Ed25519 signature against the key ring it last synced. It failed. The update was
dropped and the previous price stays on the glass.

**This is the system working.** A compromised controller, a corrupted mesh
frame, or an attacker with write access to the store's broker cannot change a
displayed price. They can only prevent one from changing.

**Two causes, and they need opposite responses.** Separate them first:

```promql
sum by (reason) (increase(sec_compliance_alerts_total[1h]))
```

**(a) A key-ring sync problem.** Fleet-wide or store-wide, started shortly after
a key rotation, `reason` indicates an unknown key ID. This is an operational
problem: the controllers are refusing valid prices because they do not have the
new key yet.

```bash
# On the controller box:
ls -l /etc/usslp/secrets/keyring.json
stat -c '%y %n' /etc/usslp/secrets/keyring.json     # how old is it
```

Push the current key ring, restart the controller, confirm prices flow. Check
whether the *whole fleet* is stale before treating it as one store's problem.

**(b) A signature that genuinely does not verify.** Isolated to one store or one
controller, no recent rotation. **This is a security incident, not a config
problem.** Do not "fix" it by pushing a new key ring — that would authorise
whatever produced the bad signature.

- Leave the controller running. It is refusing the updates, which is what you
  want, and the previous prices are correct.
- Capture the journal and the store's broker state before anything is restarted.
- Escalate. The attestation record is retained in `audit-log` for the statutory
  period and is what the investigation will be built on.

**If `usslp_attestation_failures_total` is rising instead** — that is the
*cloud* side, the Label Service failing to sign rather than a controller failing
to verify. Different runbook:
`deploy/runbooks/attestation-failure.md`. The usual cause is the price-authority
key material being unreadable or expired at `USSLP_PRICE_AUTHORITY_DIR`.

---

## 8. A bad update

**Symptom.** The gateway or the controllers started failing within an hour of
`usslp-update.timer` firing.

```bash
journalctl -u usslp-update.service -n 100 --no-pager
/usr/local/lib/usslp/update.sh --list
```

**The updater rolls back on its own.** It swaps the `current` symlink, restarts,
and polls `/readyz` for `USSLP_READY_TIMEOUT` seconds (60 by default). If the
new version does not become ready it swaps back and exits non-zero. So if the
box is broken *and* the update ran, the most likely story is that the new
version became ready and then failed — which automatic rollback cannot catch.

**Roll back by hand:**

```bash
sudo /usr/local/lib/usslp/update.sh --rollback
# or to a specific version that is still on disk:
sudo /usr/local/lib/usslp/update.sh --version v1.4.1
```

The previous version is still in `/usr/local/lib/usslp/<version>/`, so a
rollback needs **no network** — which matters, because the most common reason to
roll back a store gateway is that it can no longer reach the network.

**Then stop the rest of the fleet getting it.** The timer spreads checks over
four hours with `FixedRandomDelay`, so at 100,000 stores a bad version reaches
about 1% of the fleet in the first two and a half minutes and the rest over the
following hours. Clear the version pin centrally — the manifest at
`USSLP_UPDATE_MANIFEST_URL` — and the stores that have not yet checked will not
take it. That is one change, not 100,000.

**Hold one store back for investigation:**

```bash
sudoedit /etc/usslp/update.env    # set USSLP_TARGET_VERSION=v1.4.1
```

---

## 9. Total loss — rebuilding a gateway from nothing

New hardware, or a box whose disk failed.

```bash
# 1. Install. Binaries and units; does not touch /etc or /var if they survived.
sudo deploy/edge/install.sh \
  --store store-0001 --tenant acme --region us-east-1 \
  --controllers 25 --cloud-broker tls://mqtt.us-east-1.usslp.example:8883

# 2. Restore the credentials and the key ring. These come from the platform's
#    key ceremony and the installer will not generate them: a box that mints its
#    own price-authority key can authorise its own prices, which is exactly what
#    attestation exists to prevent.
sudo install -m 0400 -o root -g root mqtt-username /etc/usslp/secrets/
sudo install -m 0400 -o root -g root mqtt-password /etc/usslp/secrets/
sudo install -m 0440 -o root -g usslp keyring.json /etc/usslp/secrets/

# 3. Start and watch.
sudo systemctl restart usslp-sgu.service
curl -s http://127.0.0.1:9090/readyz | jq
```

**What comes back on its own:** the label directory and every label's current
price, rebuilt from the cloud once the bridge comes up; the controllers'
zone state, rebuilt from the retained messages on the store's broker.

**What does not:** anything that was in the old box's upstream queue. If the old
disk is readable at all, copy `/var/lib/usslp` across *before* first start —
the gateway will replay it.

**Order matters.** Start the gateway and wait for `/readyz` before starting the
controllers. Twenty-five controllers retrying against a broker that is not
listening yet is noise that will hide whatever the real problem is.

---

## 10. What to capture before you fix anything

If there is any chance this becomes a postmortem — and an attestation failure
always is:

```bash
D=/tmp/usslp-capture-$(date +%s); mkdir -p "$D"
systemctl status usslp-sgu.service          > "$D/status.txt" 2>&1
journalctl -u usslp-sgu.service --since '6 hours ago' --no-pager > "$D/journal.txt"
journalctl -u 'usslp-sec@*' --since '6 hours ago' --no-pager    > "$D/journal-sec.txt"
curl -s http://127.0.0.1:9090/metrics       > "$D/metrics.txt"
curl -s http://127.0.0.1:9090/readyz        > "$D/readyz.json"
curl -s http://127.0.0.1:8090/status        > "$D/store-status.json"
curl -s http://127.0.0.1:8090/queue         > "$D/queue.json"
curl -s http://127.0.0.1:8090/reconciliation > "$D/reconciliation.json"
df -h; ls -la /var/lib/usslp                > "$D/disk.txt" 2>&1
tar czf "$D.tar.gz" -C /tmp "$(basename "$D")"
```

`/etc/usslp/secrets/` is **not** in that list and must not be added to it.

---

## Reference

| | |
|---|---|
| Gateway diagnostics | `http://127.0.0.1:8090` — `/status` `/mode` `/queue` `/secs` `/labels` `/inventory` `/reconciliation` `/promotions` `/rules` `/pos/price` |
| Gateway admin | `http://127.0.0.1:9090` — `/metrics` `/healthz` `/readyz` |
| Controller admin | `http://<controller>:9090` — same three |
| Units | `usslp-sgu.service`, `usslp-sec@<id>.service`, `usslp-edge.target`, `usslp-update.timer` |
| Config | `/etc/usslp/sgu.env`, `/etc/usslp/sec.env`, `/etc/usslp/sec-<id>.env`, `/etc/usslp/update.env` |
| Secrets | `/etc/usslp/secrets/` — root-owned, 0700 |
| State | `/var/lib/usslp` — the durable store and the upstream queue |
| Binaries | `/usr/local/lib/usslp/<version>/`, `current` symlink, `/usr/local/bin` |

**Liveness vs readiness, once more, because it is the thing people get wrong
here:** `/healthz` answers 200 unconditionally — it asks only whether the
process is scheduling goroutines. `/readyz` runs the dependency checks. The
gateway's readiness deliberately does **not** include the cloud: there is no load
balancer in front of a store gateway and nowhere else for its controllers to go,
so an unreachable cloud must not make the process look unhealthy. The cloud's
reachability is a metric and a diagnostics line, where it is information rather
than a verdict.
