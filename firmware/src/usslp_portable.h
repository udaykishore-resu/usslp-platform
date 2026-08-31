/*
 * usslp_portable.h - the boundary between the portable core and Zephyr.
 *
 * Every header under src/ that includes only this file is *portable*: it
 * compiles with a bare C11 toolchain, it has no Zephyr dependency, it does no
 * I/O, and it is covered by the host tests in firmware/tests. That set is
 * deliberately the set of things that would be catastrophic to get wrong:
 *
 *   - the price attestation canonical string and digest (crypto/usslp_canon.h)
 *   - the attestation verifier and key ring        (crypto/usslp_attest.h)
 *   - the monotonic sequence rule                  (app/usslp_seq.h)
 *   - the OTA delta applier                        (ota/usslp_patch.h)
 *   - the mesh routing cost and link-risk model    (radio/usslp_route.h)
 *   - the energy budget                            (power/usslp_budget.h)
 *   - the air-frame codec and the image codec      (radio/usslp_wire.h,
 *                                                   display/usslp_rle.h)
 *   - the refresh / ghosting policy                (display/usslp_render_policy.h)
 *
 * Anything that touches a peripheral lives in a .c file that includes Zephyr
 * headers directly and is *not* in that set. The split is what makes it
 * possible to state honestly which parts of this firmware have been executed
 * and which have only been compiled — see firmware/README.md.
 */

#ifndef USSLP_PORTABLE_H
#define USSLP_PORTABLE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* Result codes shared across the portable core. Negative is failure, and the
 * numeric values are stable because they are reported in telemetry and appear
 * in the platform's runbooks. */
enum usslp_status {
	USSLP_OK = 0,
	USSLP_ERR_INVAL = -1,       /* caller passed something impossible */
	USSLP_ERR_NOSPACE = -2,     /* output buffer too small */
	USSLP_ERR_MALFORMED = -3,   /* input is not well formed */
	USSLP_ERR_UNSUPPORTED = -4, /* well formed, but this build cannot do it */
	USSLP_ERR_INTEGRITY = -5,   /* hash or CRC mismatch */
	USSLP_ERR_AUTH = -6,        /* signature or key failure */
	USSLP_ERR_STALE = -7,       /* sequence rule rejected it */
	USSLP_ERR_IO = -8,          /* flash / bus failure */
	USSLP_ERR_BUSY = -9,        /* the panel or radio is mid-operation */
};

/* usslp_ct_memcmp compares in constant time with respect to the *contents* of
 * the two buffers, returning 0 when they are equal.
 *
 * It is used for digest and key-identifier comparisons. An early-exit memcmp on
 * a digest is a genuine oracle here: an attacker with write access to the mesh
 * can submit a price update repeatedly and time the rejection, recovering the
 * expected digest a byte at a time and then searching for a price whose digest
 * matches a signature they already hold. The cost of avoiding that is thirty-two
 * XORs.
 *
 * Declared inline in the header because it is used by several translation units
 * and is four lines; static inline avoids a cross-module call in the hot path
 * without needing link-time optimisation. */
static inline int usslp_ct_memcmp(const void *a, const void *b, size_t len)
{
	const uint8_t *pa = (const uint8_t *)a;
	const uint8_t *pb = (const uint8_t *)b;
	uint8_t diff = 0;
	for (size_t i = 0; i < len; i++) {
		diff |= (uint8_t)(pa[i] ^ pb[i]);
	}
	/* Fold any non-zero difference to 1 without branching on it: for diff in
	 * 0..255, (diff + 255) >> 8 is 0 exactly when diff is 0. */
	return (int)(((uint32_t)diff + 0xffu) >> 8);
}

/* usslp_ct_streq compares two NUL-terminated strings of bounded length in a
 * way that does not leak the position of the first difference. It still leaks
 * the lengths, which is fine: key identifiers are public. */
static inline bool usslp_ct_streq(const char *a, const char *b, size_t max)
{
	size_t la = 0, lb = 0;
	while (la < max && a[la] != '\0') {
		la++;
	}
	while (lb < max && b[lb] != '\0') {
		lb++;
	}
	if (la != lb) {
		return false;
	}
	return usslp_ct_memcmp(a, b, la) == 0;
}

/* Rounded integer division, half away from zero. Every rounding decision in the
 * portable core goes through this so that the C and Go models cannot disagree
 * about a boundary case: canon.roundDiv in platform/pkg/canon/money.go is the
 * same rule. */
static inline int64_t usslp_round_div(int64_t num, int64_t den)
{
	if (den == 0) {
		return 0;
	}
	if ((num < 0) != (den < 0)) {
		return (num - den / 2) / den;
	}
	return (num + den / 2) / den;
}

#endif /* USSLP_PORTABLE_H */
