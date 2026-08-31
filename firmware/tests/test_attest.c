/*
 * The key ring and the attestation verifier.
 *
 * The contract being tested is one sentence from INTERFACE-CONTRACTS section 5:
 * a label never displays a price it cannot verify. Everything here is a way for
 * that to go wrong.
 */

#include "../src/crypto/usslp_attest.h"
#include "test_util.h"
#include "test_vectors.h"

static void fill_input(const struct attest_vector *v, struct usslp_price_input *in)
{
	in->tenant = v->tenant;
	in->store = v->store;
	in->label = v->label;
	in->sku = v->sku;
	in->amount_minor = v->amount_minor;
	in->currency = v->currency;
	in->effective_at = v->effective_at_unix;
	in->sequence = v->sequence;
	in->promotion = v->promotion;
}

void test_keyring(void)
{
	struct usslp_keyring ring;
	char kid[USSLP_KID_BUF];
	uint8_t other[32];

	printf("test_keyring\n");

	TEST("key identifiers are derived exactly as pki.KeyIDFor does");
	usslp_keyring_derive_kid(pub_a, kid);
	CHECK_EQ_S(kid, kid_a);
	usslp_keyring_derive_kid(pub_b, kid);
	CHECK_EQ_S(kid, kid_b);

	TEST("a key offered under an identifier its bytes do not produce is refused");
	usslp_keyring_init(&ring);
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_b, pub_a, 0, 0, USSLP_KEY_ACTIVE),
		   USSLP_ERR_AUTH);
	CHECK_EQ_I(usslp_keyring_len(&ring), 0);
	/* An attacker who can rewrite the ring in NVS still cannot bind their own
	 * key to the identifier the platform is signing with. */
	memcpy(other, pub_a, 32);
	other[0] ^= 0x01u;
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_a, other, 0, 0, USSLP_KEY_ACTIVE),
		   USSLP_ERR_AUTH);

	TEST("add, find, replace and remove");
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_a, pub_a, 0, 0, USSLP_KEY_ACTIVE), USSLP_OK);
	CHECK_EQ_I(usslp_keyring_len(&ring), 1);
	/* An empty kid means "derive it", which is what a factory record does. */
	CHECK_EQ_I(usslp_keyring_add(&ring, "", pub_b, 0, 0, USSLP_KEY_RETIRING), USSLP_OK);
	CHECK_EQ_I(usslp_keyring_len(&ring), 2);
	CHECK(usslp_keyring_find(&ring, kid_a) != NULL);
	CHECK(usslp_keyring_find(&ring, kid_b) != NULL);
	CHECK(usslp_keyring_find(&ring, "usslp-price-0000000000000000") == NULL);
	/* Replacing in place changes the window, not the count. */
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_a, pub_a, 100, 200, USSLP_KEY_RETIRING),
		   USSLP_OK);
	CHECK_EQ_I(usslp_keyring_len(&ring), 2);
	CHECK_EQ_I(usslp_keyring_find(&ring, kid_a)->not_after, 200);
	CHECK_EQ_I(usslp_keyring_remove(&ring, kid_a), USSLP_OK);
	CHECK_EQ_I(usslp_keyring_len(&ring), 1);
	CHECK(usslp_keyring_find(&ring, kid_a) == NULL);
	CHECK_EQ_I(usslp_keyring_remove(&ring, kid_a), USSLP_ERR_INVAL);

	TEST("validity windows, with zero meaning open ended");
	{
		struct usslp_ring_key k;

		memset(&k, 0, sizeof(k));
		k.status = USSLP_KEY_ACTIVE;
		k.not_before = 0;
		k.not_after = 0;
		CHECK(usslp_ring_key_valid_at(&k, 0));
		CHECK(usslp_ring_key_valid_at(&k, 1000000));
		k.not_before = 100;
		k.not_after = 200;
		CHECK(!usslp_ring_key_valid_at(&k, 99));
		CHECK(usslp_ring_key_valid_at(&k, 100));
		CHECK(usslp_ring_key_valid_at(&k, 200));
		CHECK(!usslp_ring_key_valid_at(&k, 201));
		k.status = USSLP_KEY_EMPTY;
		CHECK(!usslp_ring_key_valid_at(&k, 150));
	}

	TEST("the ring holds four keys and refuses a fifth");
	{
		uint8_t k[32];

		usslp_keyring_init(&ring);
		for (unsigned i = 0; i < 5; i++) {
			int rc;

			memcpy(k, pub_a, 32);
			k[31] = (uint8_t)i;
			rc = usslp_keyring_add(&ring, NULL, k, 0, 0, USSLP_KEY_ACTIVE);
			if (i < USSLP_KEYRING_SLOTS) {
				CHECK_EQ_I(rc, USSLP_OK);
			} else {
				CHECK_EQ_I(rc, USSLP_ERR_NOSPACE);
			}
		}
		CHECK_EQ_I(usslp_keyring_len(&ring), USSLP_KEYRING_SLOTS);
	}
}

void test_attest(void)
{
	struct usslp_keyring ring;
	struct usslp_price_input price;
	struct usslp_attestation att;

	printf("test_attest\n");

	usslp_keyring_init(&ring);
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_a, pub_a, 0, 0, USSLP_KEY_ACTIVE), USSLP_OK);
	CHECK_EQ_I(usslp_keyring_add(&ring, kid_b, pub_b, 1000, 2000, USSLP_KEY_RETIRING),
		   USSLP_OK);

	fill_input(&attest_vectors[0], &price);
	memset(&att, 0, sizeof(att));
	att.alg = USSLP_ATTEST_ALG_ED25519;
	memcpy(att.kid, kid_a, USSLP_KID_BUF);
	usslp_test_unhex(attest_vectors[0].digest_hex, att.digest, 32);
	memcpy(att.sig, sig_v0_by_a, 64);

	TEST("a genuine attestation verifies");
	CHECK_EQ_I(usslp_attest_verify(&ring, &price, &att, 0), USSLP_ATTEST_OK);
	CHECK(usslp_attest_ok(usslp_attest_verify(&ring, &price, &att, 0)));

	TEST("changing the price invalidates the attestation");
	{
		struct usslp_price_input tampered = price;

		tampered.amount_minor = 199; /* a shopper-visible markdown nobody signed */
		CHECK_EQ_I(usslp_attest_verify(&ring, &tampered, &att, 0),
			   USSLP_ATTEST_DIGEST_MISMATCH);
	}

	TEST("changing the sequence invalidates the attestation");
	{
		struct usslp_price_input tampered = price;

		tampered.sequence = price.sequence + 1;
		CHECK_EQ_I(usslp_attest_verify(&ring, &tampered, &att, 0),
			   USSLP_ATTEST_DIGEST_MISMATCH);
	}

	TEST("a valid signature over a different price does not authorise this one");
	{
		/* The attacker holds a genuine attestation for a cheaper SKU and pairs
		 * it with the expensive one. The transmitted digest is theirs and
		 * genuine; the recomputed digest is not. */
		struct usslp_attestation stolen = att;
		struct usslp_price_input other;

		fill_input(&attest_vectors[1], &other);
		usslp_test_unhex(attest_vectors[1].digest_hex, stolen.digest, 32);
		memcpy(stolen.sig, sig_v1_by_b, 64);
		memcpy(stolen.kid, kid_b, USSLP_KID_BUF);
		/* Against the price it was actually made for, and inside the key's
		 * window, it verifies. */
		CHECK_EQ_I(usslp_attest_verify(&ring, &other, &stolen, 1500), USSLP_ATTEST_OK);
		/* Against ours it does not. */
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &stolen, 1500),
			   USSLP_ATTEST_DIGEST_MISMATCH);
	}

	TEST("a signature by a key the ring does not know is refused");
	{
		struct usslp_attestation forged = att;

		memcpy(forged.kid, "usslp-price-ffffffffffffffff", USSLP_KID_BUF);
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &forged, 0),
			   USSLP_ATTEST_UNKNOWN_KID);
	}

	TEST("a genuine signature by the wrong known key is refused");
	{
		/* sig_v0_by_b really is key B's signature over this exact digest. The
		 * attestation claims key A, so it must fail — otherwise anyone holding
		 * any key in the ring could authorise any price. */
		struct usslp_attestation crossed = att;

		memcpy(crossed.sig, sig_v0_by_b, 64);
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &crossed, 0),
			   USSLP_ATTEST_BAD_SIGNATURE);
		/* And under key B's own identifier it does verify, which is what makes
		 * the previous assertion meaningful rather than vacuous. */
		memcpy(crossed.kid, kid_b, USSLP_KID_BUF);
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &crossed, 1500), USSLP_ATTEST_OK);
	}

	TEST("a flipped bit anywhere in the signature is refused");
	for (unsigned i = 0; i < 64; i++) {
		struct usslp_attestation broken = att;

		broken.sig[i] ^= 0x01u;
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &broken, 0),
			   USSLP_ATTEST_BAD_SIGNATURE);
	}

	TEST("a flipped bit anywhere in the transmitted digest is refused as tampering");
	for (unsigned i = 0; i < 32; i++) {
		struct usslp_attestation broken = att;

		broken.digest[i] ^= 0x80u;
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &broken, 0),
			   USSLP_ATTEST_DIGEST_MISMATCH);
	}

	TEST("an unsupported algorithm is refused before any key is touched");
	{
		struct usslp_attestation wrong = att;

		wrong.alg = 0;
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &wrong, 0), USSLP_ATTEST_BAD_ALG);
		wrong.alg = 2; /* an ECDSA attestation, say */
		CHECK_EQ_I(usslp_attest_verify(&ring, &price, &wrong, 0), USSLP_ATTEST_BAD_ALG);
	}

	TEST("a key outside its validity window is refused");
	{
		struct usslp_attestation b_att = att;
		struct usslp_price_input other;

		fill_input(&attest_vectors[1], &other);
		memcpy(b_att.kid, kid_b, USSLP_KID_BUF);
		usslp_test_unhex(attest_vectors[1].digest_hex, b_att.digest, 32);
		memcpy(b_att.sig, sig_v1_by_b, 64);

		CHECK_EQ_I(usslp_attest_verify(&ring, &other, &b_att, 1500), USSLP_ATTEST_OK);
		CHECK_EQ_I(usslp_attest_verify(&ring, &other, &b_att, 999),
			   USSLP_ATTEST_KEY_EXPIRED);
		CHECK_EQ_I(usslp_attest_verify(&ring, &other, &b_att, 2001),
			   USSLP_ATTEST_KEY_EXPIRED);
		/* A label with no clock reports now_unix == 0. The lenient entry point
		 * skips the window — the signature is the real control — and the strict
		 * one refuses. */
		CHECK_EQ_I(usslp_attest_verify(&ring, &other, &b_att, 0), USSLP_ATTEST_OK);
		CHECK_EQ_I(usslp_attest_verify_strict(&ring, &other, &b_att, 0),
			   USSLP_ATTEST_KEY_EXPIRED);
	}

	TEST("a price tuple that cannot be canonicalised is refused, not hashed");
	{
		struct usslp_price_input bad = price;

		bad.currency = "gbp";
		CHECK_EQ_I(usslp_attest_verify(&ring, &bad, &att, 0), USSLP_ATTEST_MALFORMED);
	}

	TEST("every verdict has a distinct name for the runbook");
	{
		const char *names[8];
		unsigned n = 0;

		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_OK);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_BAD_ALG);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_UNKNOWN_KID);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_KEY_EXPIRED);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_DIGEST_MISMATCH);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_BAD_SIGNATURE);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_MALFORMED);
		names[n++] = usslp_attest_verdict_str(USSLP_ATTEST_UNAVAILABLE);
		for (unsigned i = 0; i < n; i++) {
			CHECK(names[i][0] != '\0');
			for (unsigned j = i + 1; j < n; j++) {
				CHECK(strcmp(names[i], names[j]) != 0);
			}
		}
	}
}
