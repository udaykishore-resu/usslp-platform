/*
 * The canonical price encoding against the Go implementation.
 *
 * This suite is the reason the rest of the firmware is worth writing. Every
 * vector here was produced by calling
 * canon.AttestationInput.CanonicalString/Digest in
 * platform/pkg/canon/attestation.go, and the assertions compare the *bytes*,
 * not merely the hash of them, so a divergence is reported as the string it
 * actually is rather than as an opaque digest mismatch.
 */

#include "../src/crypto/usslp_canon.h"
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

static void test_int_formatting(void)
{
	char buf[32];

	TEST("int64 formatting matches strconv.FormatInt");
	CHECK_EQ_I(usslp_canon_format_i64(0, buf, sizeof(buf)), 1);
	CHECK_EQ_S(buf, "0");
	CHECK_EQ_I(usslp_canon_format_i64(249, buf, sizeof(buf)), 3);
	CHECK_EQ_S(buf, "249");
	CHECK_EQ_I(usslp_canon_format_i64(-1250, buf, sizeof(buf)), 5);
	CHECK_EQ_S(buf, "-1250");
	usslp_canon_format_i64(INT64_MAX, buf, sizeof(buf));
	CHECK_EQ_S(buf, "9223372036854775807");
	/* INT64_MIN is the case a naive negate gets wrong, and a sequence number is
	 * an int64 that an adversary controls. */
	usslp_canon_format_i64(INT64_MIN, buf, sizeof(buf));
	CHECK_EQ_S(buf, "-9223372036854775808");
	/* A buffer one byte too small must refuse rather than truncate. */
	CHECK_EQ_I(usslp_canon_format_i64(-1250, buf, 5), 0);
	CHECK_EQ_I(usslp_canon_format_i64(-1250, buf, 6), 5);
}

static void test_timestamps(void)
{
	char buf[32];

	TEST("RFC 3339 UTC formatting matches time.Format(time.RFC3339)");
	CHECK_EQ_I(usslp_canon_format_rfc3339_utc(0, buf, sizeof(buf)), 20);
	CHECK_EQ_S(buf, "1970-01-01T00:00:00Z");
	usslp_canon_format_rfc3339_utc(1741944413, buf, sizeof(buf));
	CHECK_EQ_S(buf, "2025-03-14T09:26:53Z");
	usslp_canon_format_rfc3339_utc(2147483647, buf, sizeof(buf));
	CHECK_EQ_S(buf, "2038-01-19T03:14:07Z");
	/* Pre-epoch: the case a truncating division gets wrong by a day. */
	usslp_canon_format_rfc3339_utc(-1, buf, sizeof(buf));
	CHECK_EQ_S(buf, "1969-12-31T23:59:59Z");
	usslp_canon_format_rfc3339_utc(-14182940, buf, sizeof(buf));
	CHECK_EQ_S(buf, "1969-07-20T20:17:40Z");
	/* Leap day, and the day after a century that is not a leap year. */
	usslp_canon_format_rfc3339_utc(951782400, buf, sizeof(buf));
	CHECK_EQ_S(buf, "2000-02-29T00:00:00Z");
	usslp_canon_format_rfc3339_utc(4107542400LL, buf, sizeof(buf));
	CHECK_EQ_S(buf, "2100-03-01T00:00:00Z");
	/* Years outside 0000-9999 have no four-digit rendering; refusing is the
	 * only safe answer. */
	CHECK_EQ_I(usslp_canon_format_rfc3339_utc(300000000000LL, buf, sizeof(buf)), 0);
	CHECK_EQ_I(usslp_canon_format_rfc3339_utc(0, buf, 20), 0);
}

static void test_vectors_against_go(void)
{
	char out[USSLP_CANON_MAX_LEN];
	char hex[USSLP_SHA256_DIGEST_LEN * 2 + 1];
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	uint8_t via_string[USSLP_SHA256_DIGEST_LEN];
	size_t len;

	TEST("canonical string and digest match platform/pkg/canon");
	for (unsigned i = 0; i < USSLP_TEST_ATTEST_VECTORS; i++) {
		const struct attest_vector *v = &attest_vectors[i];
		struct usslp_price_input in;

		fill_input(v, &in);

		CHECK_EQ_I(usslp_canon_price_string(&in, out, sizeof(out), &len), USSLP_OK);
		CHECK_EQ_S(out, v->canonical);
		CHECK_EQ_I(len, strlen(v->canonical));

		CHECK_EQ_I(usslp_canon_price_digest(&in, digest), USSLP_OK);
		usslp_test_hex(digest, sizeof(digest), hex);
		CHECK_EQ_S(hex, v->digest_hex);

		/* The streaming digest must be the digest of the string it claims to
		 * hash. Without this the optimisation could drift silently. */
		usslp_sha256(out, len, via_string);
		CHECK(memcmp(digest, via_string, sizeof(digest)) == 0);
	}
}

static void test_rejections(void)
{
	struct usslp_price_input in;
	char out[USSLP_CANON_MAX_LEN];
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	char longid[USSLP_CANON_MAX_ID + 8];

	fill_input(&attest_vectors[0], &in);

	TEST("malformed inputs are refused rather than encoded differently");
	{
		struct usslp_price_input bad = in;

		bad.currency = "gb";
		CHECK_EQ_I(usslp_canon_price_string(&bad, out, sizeof(out), NULL),
			   USSLP_ERR_INVAL);
		bad.currency = "GBPX";
		CHECK_EQ_I(usslp_canon_price_string(&bad, out, sizeof(out), NULL),
			   USSLP_ERR_INVAL);
		/* Lower case would hash differently from the platform, which upper-cases
		 * in canon.NewMoney. Refuse rather than silently disagree. */
		bad.currency = "gbp";
		CHECK_EQ_I(usslp_canon_price_string(&bad, out, sizeof(out), NULL),
			   USSLP_ERR_INVAL);
	}
	{
		struct usslp_price_input bad = in;

		bad.promotion = NULL;
		CHECK_EQ_I(usslp_canon_price_string(&bad, out, sizeof(out), NULL),
			   USSLP_ERR_INVAL);
	}
	{
		struct usslp_price_input bad = in;

		memset(longid, 'x', sizeof(longid) - 1);
		longid[sizeof(longid) - 1] = '\0';
		bad.sku = longid;
		CHECK_EQ_I(usslp_canon_price_string(&bad, out, sizeof(out), NULL),
			   USSLP_ERR_INVAL);
	}

	TEST("a rejected input yields no digest at all");
	{
		struct usslp_price_input bad = in;
		uint8_t zero[USSLP_SHA256_DIGEST_LEN] = { 0 };

		bad.currency = "gbp";
		CHECK_EQ_I(usslp_canon_price_digest(&bad, digest), USSLP_ERR_INVAL);
		CHECK(memcmp(digest, zero, sizeof(zero)) == 0);
	}

	TEST("a too-small buffer is refused, not truncated");
	CHECK_EQ_I(usslp_canon_price_string(&in, out, 10, NULL), USSLP_ERR_NOSPACE);
	CHECK_EQ_S(out, "");
	/* Exactly enough, and one short. */
	{
		size_t want = strlen(attest_vectors[0].canonical);

		CHECK_EQ_I(usslp_canon_price_string(&in, out, want + 1, NULL), USSLP_OK);
		CHECK_EQ_I(usslp_canon_price_string(&in, out, want, NULL), USSLP_ERR_NOSPACE);
	}
}

static void test_sensitivity(void)
{
	struct usslp_price_input a, b;
	uint8_t da[32], db[32];

	TEST("every signed field changes the digest");
	fill_input(&attest_vectors[0], &a);
	usslp_canon_price_digest(&a, da);

#define MUTATE(field, value)                                                                       \
	do {                                                                                       \
		b = a;                                                                             \
		b.field = (value);                                                                 \
		usslp_canon_price_digest(&b, db);                                                  \
		CHECK(memcmp(da, db, 32) != 0);                                                    \
	} while (0)

	MUTATE(tenant, "acme2");
	MUTATE(store, "store-002");
	MUTATE(label, "lbl-0002");
	MUTATE(sku, "SKU-12346");
	MUTATE(amount_minor, 250);
	MUTATE(currency, "USD");
	MUTATE(effective_at, attest_vectors[0].effective_at_unix + 1);
	MUTATE(sequence, 2);
	MUTATE(promotion, "p1");
#undef MUTATE

	TEST("field boundaries are unambiguous");
	/* Moving a character across a separator must change the digest: if the
	 * encoder ever dropped the '|' the two would collide, and an attacker could
	 * turn store "a" label "bc" into store "ab" label "c". */
	{
		struct usslp_price_input x = a, y = a;

		x.store = "ab";
		x.label = "c";
		y.store = "a";
		y.label = "bc";
		usslp_canon_price_digest(&x, da);
		usslp_canon_price_digest(&y, db);
		CHECK(memcmp(da, db, 32) != 0);
	}
}

void test_canon(void)
{
	printf("test_canon\n");
	test_int_formatting();
	test_timestamps();
	test_vectors_against_go();
	test_rejections();
	test_sensitivity();
}
