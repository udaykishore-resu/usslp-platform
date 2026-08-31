#include "usslp_attest.h"

#include <string.h>

static enum usslp_attest_verdict verify_common(const struct usslp_keyring *ring,
					       const struct usslp_price_input *price,
					       const struct usslp_attestation *att, int64_t now_unix,
					       bool require_clock)
{
	uint8_t local[USSLP_SHA256_DIGEST_LEN];
	const struct usslp_ring_key *key;
	int rc;

	if (ring == NULL || price == NULL || att == NULL) {
		return USSLP_ATTEST_MALFORMED;
	}
	if (att->alg != USSLP_ATTEST_ALG_ED25519) {
		return USSLP_ATTEST_BAD_ALG;
	}

	/* Recompute first. The order matters for the operator, not the maths: a
	 * label that reports "digest mismatch" is reporting that the frame it holds
	 * disagrees with what was signed, and that is a different page in the
	 * runbook from "signature does not verify". canon.Verify orders the checks
	 * the same way for the same reason. */
	rc = usslp_canon_price_digest(price, local);
	if (rc != USSLP_OK) {
		return USSLP_ATTEST_MALFORMED;
	}
	if (usslp_ct_memcmp(local, att->digest, sizeof(local)) != 0) {
		memset(local, 0, sizeof(local));
		return USSLP_ATTEST_DIGEST_MISMATCH;
	}

	key = usslp_keyring_find(ring, att->kid);
	if (key == NULL) {
		memset(local, 0, sizeof(local));
		return USSLP_ATTEST_UNKNOWN_KID;
	}
	if (now_unix != 0 || require_clock) {
		if (!usslp_ring_key_valid_at(key, now_unix)) {
			memset(local, 0, sizeof(local));
			return USSLP_ATTEST_KEY_EXPIRED;
		}
	}

	/* Verify over the locally recomputed digest, which at this point is
	 * byte-equal to the transmitted one — but it is the local array that is
	 * passed, so that a future refactor which drops the comparison above cannot
	 * turn this into a verification of the attacker's own bytes. */
	rc = usslp_ed25519_verify(key->pub, local, sizeof(local), att->sig);
	memset(local, 0, sizeof(local));
	if (rc == USSLP_ERR_UNSUPPORTED) {
		return USSLP_ATTEST_UNAVAILABLE;
	}
	if (rc != USSLP_OK) {
		return USSLP_ATTEST_BAD_SIGNATURE;
	}
	return USSLP_ATTEST_OK;
}

enum usslp_attest_verdict usslp_attest_verify(const struct usslp_keyring *ring,
					      const struct usslp_price_input *price,
					      const struct usslp_attestation *att, int64_t now_unix)
{
	return verify_common(ring, price, att, now_unix, false);
}

enum usslp_attest_verdict usslp_attest_verify_strict(const struct usslp_keyring *ring,
						     const struct usslp_price_input *price,
						     const struct usslp_attestation *att,
						     int64_t now_unix)
{
	return verify_common(ring, price, att, now_unix, true);
}

const char *usslp_attest_verdict_str(enum usslp_attest_verdict v)
{
	switch (v) {
	case USSLP_ATTEST_OK:
		return "ok";
	case USSLP_ATTEST_BAD_ALG:
		return "unsupported-algorithm";
	case USSLP_ATTEST_UNKNOWN_KID:
		return "unknown-key-id";
	case USSLP_ATTEST_KEY_EXPIRED:
		return "key-outside-validity-window";
	case USSLP_ATTEST_DIGEST_MISMATCH:
		return "digest-mismatch";
	case USSLP_ATTEST_BAD_SIGNATURE:
		return "bad-signature";
	case USSLP_ATTEST_MALFORMED:
		return "malformed-price-tuple";
	case USSLP_ATTEST_UNAVAILABLE:
		return "crypto-unavailable";
	}
	return "unknown";
}
