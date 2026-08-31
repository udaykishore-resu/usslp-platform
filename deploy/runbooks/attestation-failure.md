# Runbook — Attestation failure

**Alerts:** `USSLPAttestationFailure` (cloud side),
`USSLPControllerComplianceRefusal` (edge side)

**SLO:** price attestation accuracy 100%. **No error budget.** This is the one
alert in the platform that is not a burn rate, because the acceptable number of
unverifiable prices reaching a shelf is zero and there is nothing to burn.

---

## The contract, in three sentences

A label never displays a price it cannot verify.

The Label Service computes `canon.AttestationInput`, signs the SHA-256 of its
canonical string with the tenant's Ed25519 price-authority key, and the
signature travels inside `canon.PriceUpdated.Attestation`. The Shelf Edge
Controller **recomputes** the digest from the update it is holding — never from
the transmitted digest — and verifies it against the key ring it last synced.

Failure means the update is dropped, the previous price stays on the glass, and
a compliance alert is raised.

**So a compromised controller, a corrupted mesh frame, or an attacker with write
access to a store's broker cannot change a displayed price. They can only
prevent one from changing.** That is what these alerts are.

---

## First: which side failed?

```promql
sum by (reason) (increase(usslp_attestation_failures_total[1h]))   # cloud: cannot SIGN
sum by (sec, reason) (increase(sec_compliance_alerts_total[1h]))   # edge: cannot VERIFY
```

They are different incidents with different responses.

---

## Cloud side — `usslp_attestation_failures_total`

The Label Service could not sign a price. Nothing was published; every affected
label keeps its previous price. The till and the shelf are now diverging, and
will keep diverging for as long as this lasts.

**This is a full stop on the price path, not a degradation.**

### Diagnose

```bash
kubectl -n usslp logs -l app.kubernetes.io/component=label-service --tail=200 | grep -i 'authority\|attest\|sign\|kid'
kubectl -n usslp exec deploy/usslp-label-service -- ls -la /run/secrets/price-authority
```

| `reason` | Cause | Fix |
|---|---|---|
| key material unreadable | The ExternalSecret did not reconcile, or the projected file's mode is wrong | `kubectl -n usslp get externalsecret usslp-label-service-price-authority -o yaml` and look at its status |
| key expired | The price authority's certificate passed its validity window | Key ceremony. Not something to work around. |
| tenant has no key | A new tenant was onboarded without a price-authority key | The onboarding step was skipped; issue the key. |
| (service started with a warning about an ephemeral key) | `USSLP_PRICE_AUTHORITY_DIR` is unset | See below — this is the dangerous one |

### The ephemeral-key trap

With no `USSLP_PRICE_AUTHORITY_DIR`, `label-service` does **not** fail. It
generates an ephemeral Ed25519 key, logs a warning once at boot, and then looks
completely healthy — while signing every price with a key no controller in the
field has ever seen. Every controller will refuse every update.

The symptom is not this alert. It is
`USSLPControllerComplianceRefusal` firing fleet-wide with an unknown key id,
while the cloud side reports nothing wrong at all. Check the boot log:

```bash
kubectl -n usslp logs -l app.kubernetes.io/component=label-service | head -50 | grep -i ephemeral
```

The chart mounts the key ring from an ExternalSecret in every environment except
dev, where it is deliberately absent.

---

## Edge side — `sec_compliance_alerts_total`

A controller could not verify a price against its key ring. The update was
dropped and the previous price stays on the glass.

**Two causes, and they need opposite responses. Separate them before doing
anything.**

### (a) A key-ring sync problem — an operational fault

**Shape:** fleet-wide or store-wide; started shortly after a key rotation;
`reason` indicates an unknown key id.

The controllers are refusing *valid* prices because they do not have the new key
yet. Prices are frozen across whatever part of the fleet is stale, and nothing
on a shelf looks wrong — the old price is simply still there.

```bash
# On a controller:
stat -c '%y %n' /etc/usslp/secrets/keyring.json
journalctl -u 'usslp-sec@sec-0001' --since '2 hours ago' | grep -i 'keyring\|verify\|kid'
```

Check how much of the fleet is stale before treating it as one store's problem:

```promql
count(count by (sec) (sec_compliance_alerts_total))
```

**Fix:** push the current key ring, restart the affected controllers, confirm
prices flow. A controller restart is safe — it rebuilds its zone state from the
retained messages on the store's broker, which is precisely why price updates
are retained.

### (b) A signature that genuinely does not verify — a security incident

**Shape:** isolated to one store or one controller; no recent rotation.

**Do NOT "fix" this by pushing a new key ring.** That would authorise whatever
produced the bad signature.

1. **Leave the controller running.** It is refusing the updates, which is
   correct, and the prices currently displayed are the last verified ones.
2. **Capture before restarting anything.** The journal, the controller's
   `/metrics`, the store's broker state. `deploy/edge/RUNBOOK.md` §10 has the
   capture script.
3. **Escalate.** The attestation is retained in the `audit-log` stream for the
   statutory period (365 days, `canon.StreamAudit`) and is what the
   investigation is built on.
4. **Do not** restart the store gateway. Its broker holds the retained messages
   that are part of the evidence.

The threat model is explicit: an attacker with write access to a store's broker
can stop prices changing and cannot change one. If this is (b), the mechanism
worked.

---

## Verifying recovery

```promql
increase(usslp_attestation_failures_total[10m])   # must be 0
increase(sec_compliance_alerts_total[10m])        # must be 0
rate(usslp_price_updates_total{outcome="applied"}[5m])  # must be non-zero
```

The third one matters as much as the first two. Attestation failures stopping
because prices stopped being attempted is not recovery.

---

## Why there is no error budget here

Every other SLO in the platform gets a multi-window burn-rate alert, because for
those SLOs the question is "how fast are we consuming the amount of failure we
have agreed to tolerate". Here the agreed tolerance is zero: a price that cannot
be verified must not reach a shelf, and one that does is a regulatory exposure
regardless of how rare it is. So this alert fires on any increase, with a short
`for:` to suppress a single scrape-boundary artefact, and pages.
