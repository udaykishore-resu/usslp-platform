# 0005 — Ed25519 rather than ECDSA P-256 for price attestation

**Status:** Accepted

---

## Context

[0004](0004-end-to-end-price-attestation.md) puts a signature verification on the
critical path of every price change, twice: once on a Linux controller and once
on a Cortex-M4F running from a coin cell that has to last seven to ten years. The
choice of signature algorithm therefore has to satisfy three constraints at once.

- **The label's energy budget.** The whole steady-state budget is 6.584 µA
  (`firmware/README.md`). A verification that costs materially more than a few
  milliamp-milliseconds per update is a verification that shortens the fleet's
  life.
- **The 802.15.4 frame.** `mesh.MaxFrameBytes` is 127. Every byte of signature is
  a byte of airtime on a channel shared by the whole zone, and the attested frame
  already fragments.
- **Implementability without a hardware RNG on the verifying side, and without
  subtlety.** The firmware is written once, shipped to 50 million units, and
  cannot be recalled.

ECDSA P-256 was the obvious default. The platform already uses it: `pki.Profile`
issues label leaf certificates as `ecdsa-p256`, service leaves as `ecdsa-p256`,
and tenant JWTs are ES256 (`pki.JWTAlgorithm`). Using it for attestation too
would have been one fewer primitive in the firmware.

## Decision

**Price attestation is Ed25519, and it is the only algorithm accepted.**
`canon.AttestationAlg = "Ed25519"`, and `canon.Verify` refuses anything else
rather than negotiating.

The reasons, in the order they decided it — the comment on `AttestationAlg`
states all three:

**1. Verification is constant time without special care.** ECDSA verification is
constant-time only if the implementation is written to be, and the failure mode
of getting that wrong on a device an attacker can hold in their hand is a
side-channel against a key that authorises prices. Ed25519's design removes the
class rather than mitigating it.

**2. Signatures are 64 bytes**, and the public key is 32. The equivalent DER
ECDSA signature is 70–72 bytes with variable-length integers, which is both
larger on a 127-byte frame and requires a DER parser in firmware — another piece
of attacker-facing parsing code shipped to 50 million units.

**3. A Cortex-M4F verifies one in about 13 ms**, which fits inside the label's
power budget. That figure is a blueprint number, not a measurement: nothing in
this tree has been timed on hardware, and `firmware/README.md` says so under
*Not verified*.

Signing is deterministic — Ed25519 derives its nonce from the message and the
private key — so the signing side needs no entropy source at signing time. That
matters less for the cloud, which has one, than for the delegated store-scoped
authority a Store Gateway Unit may hold
([0003](0003-edge-first-architecture.md)), which is a fanless box in a stock room
where nonce reuse under a poor RNG would leak the key.

**The rest of the PKI stays where it is.** This decision is scoped to price
attestation. Certificates remain ECDSA P-256 at the leaves and RSA at the top of
the hierarchy for reasons recorded in `pki/profile.go`: the root and
intermediates must remain verifiable by whatever validates them in 2045,
including hardware that predates widespread Ed25519 support, and a 90-day service
leaf is rotated often enough that long-horizon algorithm risk does not apply.
Having two primitives in the firmware — P-256 for the device certificate, Ed25519
for attestation — is a real cost, paid deliberately.

## Consequences

**64-byte signatures still do not fit the frame.** The attested frame is 199
bytes larger than a plain price update, of which the signature is 64, the digest
32 and the key identifier 28. It fragments across 802.15.4 frames, which is the
measured cost recorded in [0004](0004-end-to-end-price-attestation.md).

**A second primitive in the firmware.** `firmware/src/crypto/usslp_attest.c`
sits on PSA for Curve25519 field arithmetic. The host tests do not exercise that
arithmetic: `tests/fake_ed25519.c` is an oracle over triples that Go's
`crypto/ed25519` actually produced, so the verifier's *decisions* are tested
against genuine signatures while the field arithmetic remains PSA's
responsibility and PSA's test suite. That is the correct division and it is
stated at the top of the file.

**SHA-256 is compiled in rather than taken from PSA**, even though PSA has it.
Two kilobytes of flash buys the guarantee that the code the host tests hash with
is the code that runs on the shelf.

**Verification is tested to the bit.** `firmware/tests` asserts that a genuine
attestation verifies; that a price change, a sequence change, every single-bit
signature flip (64) and every digest flip (32) is refused; that a *genuine*
signature by the wrong known key is refused; and that an unknown key identifier
and an expired key are refused. That is 25,961 host assertions in total across
the portable core, clean under ASan and UBSan and under two compilers.

**Key identifiers are self-authenticating.** `pki.KeyIDFor` derives the
identifier as a truncated SHA-256 of the public key itself, so an attacker cannot
publish a ring entry claiming an existing `kid` with a substituted key — the
identifier would not match the bytes. Sixteen hex characters is enough because
the identifier selects among a handful of keys, and forging one requires a
preimage rather than a birthday collision.

## Alternatives considered

**ECDSA P-256.** Would have meant one primitive instead of two, and hardware
acceleration exists for it on more parts. Rejected on the constant-time and DER
grounds above, and on signature size.

**RSA-2048 signatures.** Verification is actually fast (small public exponent),
which makes this less obviously wrong than it looks. Rejected on size: a 256-byte
signature on a 127-byte frame is four fragments before anything else is carried.

**Ed448 or P-384.** A 192-bit security level rather than 128. Rejected: the
attestation's secret has a rotation overlap of 30 days and a retention horizon
measured in years, not decades, and 128-bit security is the right level for it.
`pki.KeyECDSAP384` exists in the profile for deployments whose regulator mandates
192-bit at the certificate leaf; that is a different question from the
attestation algorithm.

**Negotiating the algorithm on the wire.** Rejected. `canon.Verify` checks the
algorithm field against a constant and refuses anything else; it never uses that
field to *select* a verification strategy. That is the same discipline
`pki/jwt.go` applies to the JWT `alg` header, and for the same reason: algorithm
selection from attacker-controlled input is the root of a whole vulnerability
class.
