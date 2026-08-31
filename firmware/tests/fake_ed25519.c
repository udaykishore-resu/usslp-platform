/*
 * The host build's Ed25519 backend: an oracle over real signatures.
 *
 * The firmware delegates curve arithmetic to PSA (src/crypto/psa_backend.c) and
 * there is no value in reimplementing Ed25519 here just so a test can call it —
 * a second implementation would be a second thing that can be wrong, and it is
 * not the thing these tests are about.
 *
 * What these tests are about is the verifier's *decisions*, and for that an
 * oracle is not merely adequate, it is more honest than a local implementation
 * would be. The table below holds (public key, message, signature) triples that
 * Go's crypto/ed25519 actually produced, generated alongside the attestation
 * vectors. A triple in the table verifies; anything else does not — which is
 * exactly what a correct Ed25519 verifier does, with overwhelming probability,
 * for the inputs these tests use.
 *
 * The consequence, stated so nobody reads more into a green test run than is
 * there: firmware/tests exercises key resolution, validity windows, digest
 * recomputation, constant-time comparison and the refuse-to-render path against
 * genuine signatures. It does not exercise field arithmetic on Curve25519.
 */

#include "../src/crypto/usslp_ed25519.h"
#include "test_util.h"
#include "test_vectors.h"

struct oracle_entry {
	const uint8_t *pub;
	const uint8_t *sig;
};

static const struct oracle_entry oracle[] = {
	/* vector 0's digest, signed by key A */
	{ pub_a, sig_v0_by_a },
	/* vector 1's digest, signed by key B */
	{ pub_b, sig_v1_by_b },
	/* vector 0's digest, signed by key B: a genuine signature under the wrong
	 * key, which is what a substituted-kid attack looks like from the
	 * verifier's side. */
	{ pub_b, sig_v0_by_b },
};

/* Filled on first use from the attestation vectors so the digests are never
 * written down twice. */
static uint8_t oracle_msg[3][32];
static bool oracle_ready;

static void oracle_init(void)
{
	if (oracle_ready) {
		return;
	}
	usslp_test_unhex(attest_vectors[0].digest_hex, oracle_msg[0], 32);
	usslp_test_unhex(attest_vectors[1].digest_hex, oracle_msg[1], 32);
	usslp_test_unhex(attest_vectors[0].digest_hex, oracle_msg[2], 32);
	oracle_ready = true;
}

int usslp_ed25519_verify(const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN], const uint8_t *msg,
			 size_t msg_len, const uint8_t sig[USSLP_ED25519_SIGNATURE_LEN])
{
	oracle_init();

	if (pub == NULL || msg == NULL || sig == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (msg_len != 32u) {
		/* The attestation always signs a 32-byte digest. Anything else is a
		 * caller bug, not a signature to check. */
		return USSLP_ERR_INVAL;
	}
	for (unsigned i = 0; i < sizeof(oracle) / sizeof(oracle[0]); i++) {
		if (memcmp(pub, oracle[i].pub, 32) == 0 &&
		    memcmp(msg, oracle_msg[i], 32) == 0 &&
		    memcmp(sig, oracle[i].sig, 64) == 0) {
			return USSLP_OK;
		}
	}
	return USSLP_ERR_AUTH;
}
