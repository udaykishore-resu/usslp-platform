/*
 * SHA-256 against Go's crypto/sha256, with the block-boundary cases that a
 * hand-written padding routine gets wrong: 55, 56 and 64 bytes, and a length
 * that straddles two update() calls.
 */

#include "../src/crypto/usslp_sha256.h"
#include "test_util.h"
#include "test_vectors.h"

void test_sha256(void)
{
	uint8_t buf[256];
	uint8_t digest[32];
	char hex[65];

	printf("test_sha256\n");

	TEST("known-answer vectors from crypto/sha256");
	for (unsigned i = 0; i < USSLP_TEST_SHA_VECTORS; i++) {
		const struct sha_vector *v = &sha_vectors[i];

		memset(buf, v->fill[0] == '\0' ? 0 : v->fill[0], sizeof(buf));
		usslp_sha256(buf, (size_t)v->len, digest);
		usslp_test_hex(digest, 32, hex);
		/* The 3-byte vector is "abc" rather than "aaa"; check it separately. */
		if (v->len == 3) {
			usslp_sha256("abc", 3, digest);
			usslp_test_hex(digest, 32, hex);
			CHECK_EQ_S(hex, sha_abc_hex);
			continue;
		}
		CHECK_EQ_S(hex, v->digest_hex);
	}

	TEST("incremental update matches one-shot at every boundary");
	for (size_t total = 0; total <= 200; total++) {
		uint8_t one[32], inc[32];

		for (size_t i = 0; i < total; i++) {
			buf[i] = (uint8_t)(i * 37u + 11u);
		}
		usslp_sha256(buf, total, one);

		for (size_t split = 0; split <= total; split++) {
			struct usslp_sha256 ctx;

			usslp_sha256_init(&ctx);
			usslp_sha256_update(&ctx, buf, split);
			usslp_sha256_update(&ctx, buf + split, total - split);
			usslp_sha256_final(&ctx, inc);
			CHECK(memcmp(one, inc, 32) == 0);
		}
	}

	TEST("the context is wiped by final");
	{
		struct usslp_sha256 ctx;
		uint8_t zero[sizeof(ctx)];

		memset(zero, 0, sizeof(zero));
		usslp_sha256_init(&ctx);
		usslp_sha256_update(&ctx, "a secret price", 14);
		usslp_sha256_final(&ctx, digest);
		CHECK(memcmp(&ctx, zero, sizeof(ctx)) == 0);
	}
}
