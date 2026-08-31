# Runbook — Rolling back

Three deployment mechanisms, three rollback procedures. Pick by what you are
rolling back.

**The rule that applies to all three:** roll back first, diagnose afterwards. The
failing version is preserved in every case — aborted canary pods stay for 30
minutes, the previous edge binary stays on disk, ArgoCD keeps history — so
nothing is lost by reverting quickly.

---

## 1. A canary that is still in progress

`api-gateway`, `pos-integration-gw` and `label-service` deploy through Argo
Rollouts: 5% → analysis → 25% → analysis → 100%.

```bash
kubectl argo rollouts get rollout usslp-label-service -n usslp --watch
kubectl argo rollouts abort  usslp-label-service -n usslp
```

Abort shifts 100% of traffic back to stable immediately. It usually is not
needed: every step has an inline analysis, and a failed measurement aborts on its
own within seconds.

The aborted canary's pods stay for 30 minutes
(`abortScaleDownDelaySeconds: 1800`). They serve no traffic; they exist so that
whoever is debugging at 3 a.m. has the failing pods to look at rather than a
description of them.

```bash
kubectl -n usslp logs -l rollouts-pod-template-hash=<canary-hash> --tail=200
kubectl argo rollouts undo usslp-label-service -n usslp   # after an abort, to be explicit
```

---

## 2. A completed deploy

For a plain Deployment (`device-registry`, `ota-service`, `pricing-ai-service`,
`promotion-service`):

```bash
kubectl -n usslp rollout undo deployment/usslp-device-registry
kubectl -n usslp rollout status deployment/usslp-device-registry
```

Through ArgoCD, which is the path that leaves Git and the cluster agreeing:

```bash
argocd app history usslp-prod-use1
argocd app rollback usslp-prod-use1 <revision>
```

Production Applications have auto-sync **off**, so a rollback is not undone by
the next reconcile. Follow it with a revert commit and a new `release-*` tag;
until then the cluster is running something Git does not point at.

---

## 3. An edge update

```bash
sudo /usr/local/lib/usslp/update.sh --rollback
sudo /usr/local/lib/usslp/update.sh --list          # what is installed
sudo /usr/local/lib/usslp/update.sh --version v1.4.1
```

The updater already rolls back on its own: it swaps the symlink, restarts, polls
`/readyz` for `USSLP_READY_TIMEOUT`, and reverts if the new version does not
become ready. So if a box is broken *and* the update ran, the likely story is
that the new version became ready and then failed — which automatic rollback
cannot catch.

The previous version is still in `/usr/local/lib/usslp/<version>/`, so a rollback
needs **no network**. That matters: the most common reason to roll back a store
gateway is that it can no longer reach the network.

**Then stop the rest of the fleet getting it.** Clear the version pin in the
manifest at `USSLP_UPDATE_MANIFEST_URL`. The timer spreads checks over four
hours, so at 100,000 stores a bad version reaches about 1% in the first two and a
half minutes and the rest over the following hours — one central change stops
every store that has not yet checked.

---

## What a rollback does not undo

**Event-stream records.** Everything published is published. Consumers are
idempotent (`Envelope.IdempotencyKey`, per-aggregate `Version`, per-label
`Sequence`), so replay is safe, but a bad price that was accepted and fanned out
is on shelves until a corrected one replaces it.

**Prices already on glass.** Rolling back the Label Service does not roll back
what labels are displaying. Publish the correction.

**Firmware already flashed.** Not recoverable over the air. Rolling back the OTA
service stops further dispatch; the devices that took the image need
`/v1/ota/jobs/<id>/rollback`, and `boot_failed` devices have already fallen back
on their own. [ota-rollout.md](ota-rollout.md).

**Schema or partition changes.** A partition count that changed re-maps every key
and destroys per-key ordering — which is why `pkg/eventlog` refuses the change
outright and the provisioning Job uses `--if-not-exists`, never `--alter`.

---

## Afterwards

1. Confirm the SLIs recovered — not just that the pods are Running.
2. Note how much error budget the incident cost:
   **USSLP — SLOs and Error Budgets**.
3. If the canary analysis did not catch it, ask why. The usual answer is that
   the regression takes longer to appear than the analysis window (five minutes
   at 5%, three at 25%), and the fix is a longer pause rather than more metrics.
