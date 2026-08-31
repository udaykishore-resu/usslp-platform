/*
 * usslp_attest.h - "a label never displays a price it cannot verify."
 *
 * INTERFACE-CONTRACTS section 5, implemented on the device. The verifier
 * mirrors canon.Verify (platform/pkg/canon/attestation.go) and
 * pki.KeyRing.VerifyAt, with the same ordering of checks and the same
 * distinction between the two failure modes, because they have different
 * runbook entries:
 *
 *   - a digest mismatch means the price on the wire is not the price that was
 *     signed. Somebody rewrote a field in flight. That is tampering.
 *   - a signature failure means the digest is the signed one but the signature
 *     does not verify under the named key. That is a key problem — a stale ring,
 *     a rotation the label missed, a forged attestation.
 *
 * Both refuse the update. The difference is what the operator is told.
 *
 * The verifier NEVER trusts the transmitted digest. It recomputes the digest
 * from the fields it is about to render and compares. Trusting the transmitted
 * digest would let an attacker pair a genuine signature with a different price:
 * the signature would verify, over a digest of a price nobody is about to show.
 *
 * Where this sits in the tier model
 * ---------------------------------
 * INTERFACE-CONTRACTS puts attestation verification at the Shelf Edge
 * Controller, and edge/labelsim/wire.go says so explicitly: the JSON envelope
 * and the 64-byte signature stop at Tier 2, and what crosses the mesh is a
 * sequence number, a price and compressed pixels. That is a sound design and
 * this firmware implements it: USSLP_FRAME_UPDATE is accepted on that basis.
 *
 * It also leaves one hole, and this module is how the firmware closes it: a
 * compromised controller can author a price. The contract's own threat model
 * says an attacker who owns the store's broker cannot change a displayed price,
 * but a controller that has been replaced or rooted is inside the trust boundary
 * that claim depends on. So the firmware additionally accepts
 * USSLP_FRAME_ATTESTED_UPDATE, which carries the signed tuple end to end and is
 * verified here, on the glass, against a ring the label syncs itself.
 *
 * That frame type is a firmware-side extension: as of this writing
 * edge/labelsim and edge/sec speak frame types 1-3 only, and a deployment where
 * the controllers have not been updated must run with
 * CONFIG_USSLP_REQUIRE_ATTESTATION=n. The README says so in the same words.
 */

#ifndef USSLP_ATTEST_H
#define USSLP_ATTEST_H

#include "../usslp_portable.h"
#include "usslp_canon.h"
#include "usslp_ed25519.h"
#include "usslp_keyring.h"

/* canon.AttestationAlg. Ed25519 is the only algorithm accepted; an attestation
 * naming anything else is refused before any key is touched. On the wire the
 * algorithm is a single byte rather than the string the JSON envelope carries,
 * because a label has no use for a negotiation it is not allowed to lose. */
#define USSLP_ATTEST_ALG_ED25519 1

struct usslp_attestation {
	uint8_t alg;
	char kid[USSLP_KID_BUF];
	/* The digest as transmitted. It is compared against the locally recomputed
	 * one and is otherwise unused: the signature is verified over the *local*
	 * digest, never over this. */
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	uint8_t sig[USSLP_ED25519_SIGNATURE_LEN];
};

/* Verdicts. Every one of them except USSLP_ATTEST_OK means the previous image
 * stays on the glass. */
enum usslp_attest_verdict {
	USSLP_ATTEST_OK = 0,
	/* The attestation names an algorithm this firmware does not implement. */
	USSLP_ATTEST_BAD_ALG,
	/* The kid is not in the ring. Usually a missed rotation. */
	USSLP_ATTEST_UNKNOWN_KID,
	/* The key is in the ring but outside its validity window. */
	USSLP_ATTEST_KEY_EXPIRED,
	/* The transmitted digest is not the digest of the price we hold. */
	USSLP_ATTEST_DIGEST_MISMATCH,
	/* The digest is right and the signature over it is not. */
	USSLP_ATTEST_BAD_SIGNATURE,
	/* The price tuple could not be canonicalised at all. */
	USSLP_ATTEST_MALFORMED,
	/* The crypto backend is unavailable. Fails closed. */
	USSLP_ATTEST_UNAVAILABLE,
};

/*
 * Verifies an attestation against the price the caller is about to render.
 *
 * now_unix is used only for the key validity window and may be 0 on a label
 * that has not yet acquired time, in which case the window check is skipped —
 * a label with no clock must still be able to take a price, and the signature
 * is the real control. usslp_attest_verify_strict refuses instead, and is what
 * a deployment with a trusted time source configures.
 */
enum usslp_attest_verdict usslp_attest_verify(const struct usslp_keyring *ring,
					      const struct usslp_price_input *price,
					      const struct usslp_attestation *att, int64_t now_unix);

enum usslp_attest_verdict usslp_attest_verify_strict(const struct usslp_keyring *ring,
						     const struct usslp_price_input *price,
						     const struct usslp_attestation *att,
						     int64_t now_unix);

/* A short stable string for logs and for the compliance alert raised on
 * failure. Never NULL. */
const char *usslp_attest_verdict_str(enum usslp_attest_verdict v);

/* True only for USSLP_ATTEST_OK. Written as a function so that every caller
 * reads the same way and nobody writes `if (v != BAD_SIGNATURE)`. */
static inline bool usslp_attest_ok(enum usslp_attest_verdict v)
{
	return v == USSLP_ATTEST_OK;
}

#endif /* USSLP_ATTEST_H */
