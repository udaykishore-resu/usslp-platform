/*
 * Minimal test harness for the portable core.
 *
 * No framework. The whole point of these tests is that they build and run with
 * nothing but a C compiler, on a machine that has never heard of Zephyr, so that
 * "the attestation encoder agrees with the platform" is a claim somebody can
 * check in ten seconds rather than a claim about a build nobody can reproduce.
 */

#ifndef USSLP_TEST_UTIL_H
#define USSLP_TEST_UTIL_H

#include <inttypes.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

extern int usslp_test_failures;
extern int usslp_test_checks;
extern const char *usslp_test_current;

#define CHECK(cond)                                                                                \
	do {                                                                                       \
		usslp_test_checks++;                                                               \
		if (!(cond)) {                                                                     \
			usslp_test_failures++;                                                     \
			printf("  FAIL %s:%d in %s: %s\n", __FILE__, __LINE__,                     \
			       usslp_test_current, #cond);                                         \
		}                                                                                  \
	} while (0)

#define CHECK_EQ_I(got, want)                                                                      \
	do {                                                                                       \
		long long g_ = (long long)(got);                                                   \
		long long w_ = (long long)(want);                                                  \
		usslp_test_checks++;                                                               \
		if (g_ != w_) {                                                                    \
			usslp_test_failures++;                                                     \
			printf("  FAIL %s:%d in %s: %s = %lld, want %lld\n", __FILE__, __LINE__,   \
			       usslp_test_current, #got, g_, w_);                                  \
		}                                                                                  \
	} while (0)

#define CHECK_EQ_S(got, want)                                                                      \
	do {                                                                                       \
		const char *g_ = (got);                                                            \
		const char *w_ = (want);                                                           \
		usslp_test_checks++;                                                               \
		if (g_ == NULL || strcmp(g_, w_) != 0) {                                           \
			usslp_test_failures++;                                                     \
			printf("  FAIL %s:%d in %s:\n    got  %s\n    want %s\n", __FILE__,        \
			       __LINE__, usslp_test_current, g_ == NULL ? "(null)" : g_, w_);      \
		}                                                                                  \
	} while (0)

/* Absolute tolerance comparison for the fixed-point models, printed with enough
 * digits to see where a divergence starts. */
#define CHECK_NEAR(got, want, tol)                                                                 \
	do {                                                                                       \
		double g_ = (double)(got);                                                         \
		double w_ = (double)(want);                                                        \
		double t_ = (double)(tol);                                                         \
		double d_ = g_ > w_ ? g_ - w_ : w_ - g_;                                           \
		usslp_test_checks++;                                                               \
		if (!(d_ <= t_)) {                                                                 \
			usslp_test_failures++;                                                     \
			printf("  FAIL %s:%d in %s: %s = %.9f, want %.9f (delta %.3g > %.3g)\n",   \
			       __FILE__, __LINE__, usslp_test_current, #got, g_, w_, d_, t_);      \
		}                                                                                  \
	} while (0)

#define TEST(name)                                                                                 \
	do {                                                                                       \
		usslp_test_current = (name);                                                       \
		printf("  - %s\n", (name));                                                        \
	} while (0)

/* Parses lowercase hex into a byte buffer. Returns the number of bytes. */
static inline size_t usslp_test_unhex(const char *hex, uint8_t *out, size_t cap)
{
	size_t n = 0;

	while (hex[0] != '\0' && hex[1] != '\0' && n < cap) {
		int hi = hex[0] <= '9' ? hex[0] - '0' : (hex[0] | 0x20) - 'a' + 10;
		int lo = hex[1] <= '9' ? hex[1] - '0' : (hex[1] | 0x20) - 'a' + 10;

		out[n++] = (uint8_t)((hi << 4) | lo);
		hex += 2;
	}
	return n;
}

static inline void usslp_test_hex(const uint8_t *b, size_t n, char *out)
{
	static const char d[] = "0123456789abcdef";

	for (size_t i = 0; i < n; i++) {
		out[i * 2] = d[b[i] >> 4];
		out[i * 2 + 1] = d[b[i] & 0x0f];
	}
	out[n * 2] = '\0';
}

/* Each suite. */
void test_canon(void);
void test_sha256(void);
void test_keyring(void);
void test_attest(void);
void test_seq(void);
void test_wire(void);
void test_rle(void);
void test_render_policy(void);
void test_route(void);
void test_power(void);
void test_patch(void);
void test_chunkmap(void);

#endif /* USSLP_TEST_UTIL_H */
