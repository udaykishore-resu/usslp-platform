# 0006 — Signed CRLs rather than OCSP for a fleet that is mostly offline

**Status:** Accepted

---

## Context

USSLP issues certificates to every device in the fleet: labels from the
Manufacturing Sub-CA, controllers and gateways from the Shelf Controller Sub-CA,
cloud workloads from the Services CA. Some of those devices will be stolen,
cloned, decommissioned or found to be running compromised firmware, and the
platform needs a way to stop honouring their certificates.

The textbook answer for a modern PKI is OCSP with stapling. The reasoning behind
choosing otherwise is specific to this fleet's shape, and
`platform/pkg/pki/revocation.go` records it in full at the top of the file.

## Decision

**Revocation is by signed X.509 CRL, distributed to relying parties and enforced
from a local copy. There is no OCSP responder.**

`pki.RevocationList` builds and signs a CRL with RFC 5280 §5.3.1 reason codes;
`pki.Verify` consults it during chain verification, and it is a hard failure
(`ErrRevoked`), not a warning.

Five arguments, in the order they decided it.

**1. Devices are offline more than they are online.** A shelf label wakes for a
few hundred milliseconds, verifies an update and sleeps. A store whose broadband
is down keeps trading for hours on the gateway's local state
([0003](0003-edge-first-architecture.md)). OCSP asks a relying party to make a
network call to a third party at exactly the moment it is least able to. A CRL is
a file: the gateway syncs one during its normal maintenance window and enforces
revocation for the rest of the week with no connectivity at all.

**2. OCSP's failure mode is soft-fail, and soft-fail is not zero trust.** Every
production OCSP deployment eventually accepts certificates when the responder
times out, because the alternative is an outage triggered by someone else's
server. A security property that evaporates under load is not a security
property. A locally cached CRL either contains the serial or it does not, and the
answer is the same whether or not the WAN is up.

**3. Volume.** At 50 million devices and a 0.1% annual revocation rate the steady
state is on the order of 50,000 entries — roughly 1.5 MB of DER, which an SGU
stores without noticing, and which reduces to a 50,000-entry hash set of 128-bit
serials that a controller holds in about a megabyte of RAM. The same fleet under
OCSP generates a query per handshake, and after a regional power cut tens of
millions of devices reconnect within minutes and all want an answer from one
responder.

**4. Privacy and traffic analysis.** An OCSP responder learns which device talked
to which service and when. That log is a map of a retailer's store estate, held
by the platform operator, for no operational benefit.

**5. Verifiability with what the device already has.** A CRL is signed by the
same CA whose certificate is already provisioned on the device. No new trust
anchor, no responder certificate, no delegated signing key, and no extra thing to
get wrong in firmware shipped to 50 million units.

## Consequences

**Revocation is not instant, and the latency is bounded rather than eliminated.**
It takes effect when the next list is distributed — bounded by the CRL's
`NextUpdate` and by how often the edge syncs. Hours, not days, for a compromise.

That is acceptable **specifically because of
[0004](0004-end-to-end-price-attestation.md)**, and the two decisions have to be
read together. Revoking a *label* does not need to be instant: a label cannot
change a price whatever its certificate says, because prices are separately
signed by the price authority. The device certificate governs who may join the
mesh and speak; it does not govern who may author a price. Authority to speak and
authority to authorise are separate secrets, which is what makes a slower
revocation channel tolerable.

**Revocation that must be instant is handled by rotation, not by a list.** A
compromised sub-CA is answered by rotating the authority. A compromised price
authority key is removed from the ring entirely and every price it signed is
re-signed — `pki.KeyStatus` deliberately has no "revoked" state, because leaving a
compromised key listed in any state invites a verifier to accept it.

**Someone has to distribute the list.** There is a `pki.RevocationList` type and
`pki.Verify` consults it; the operational plumbing that gets a fresh CRL to
100,000 gateways on a schedule is a deployment concern that this tree provisions
(the gateway's maintenance window, the config directory) rather than automates.
`docs/architecture/roadmap.md` carries it.

**A stale CRL is a silent failure.** A gateway that has not synced a list in
three weeks enforces a three-week-old view of the world and looks entirely
healthy doing it. The list's `NextUpdate` is the signal that catches it, and
checking it is the relying party's job.

## Alternatives considered

**OCSP with stapling.** The right answer for the public web, where relying
parties are online by definition and responders are operated by CAs with
uptime obligations. Rejected here on all five grounds above; the soft-fail one is
decisive on its own.

**Short-lived certificates instead of revocation.** Genuinely attractive, and it
is what the platform already does for cloud services: `ServiceValidity` is 90
days, which is short enough that revocation is largely moot for that population.
Rejected for devices. A label's certificate is two years and a controller's one
year (`pki.ProductionProfile`), because renewal requires the device to be
reachable, and a label that is asleep, in a freezer, behind a stock pallet, or in
a store whose WAN has been down for a month is exactly the device that would fail
to renew. Certificate lifetime for a battery-powered device is a reachability
question, not a security preference.

**Delta CRLs.** Would reduce distribution cost. Not built. At 1.5 MB per full
list and a weekly sync, the saving does not currently justify the complexity or
the extra failure mode of a delta whose base is missing.

**No revocation at all**, relying entirely on the attestation separation to make
a stolen device harmless. Rejected: a device certificate still grants mesh
membership, telemetry publication and an acknowledgement channel, and a fleet with
no way to eject a device is a fleet that cannot respond to a compromise.
