/*
 * FIPS 180-4 SHA-256.
 *
 * Straightforward, unrolled only where the compiler will not do it for us, and
 * written for a Cortex-M4 with a barrel shifter: the rotates compile to single
 * ROR instructions and the message schedule is kept in a 64-word array rather
 * than a 16-word ring, because sixty-four words of stack is cheaper here than
 * the modulo arithmetic a ring costs on every one of the 48 expansion steps.
 */

#include "usslp_sha256.h"

#include <string.h>

static const uint32_t k256[64] = {
	0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu, 0x59f111f1u,
	0x923f82a4u, 0xab1c5ed5u, 0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u,
	0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u, 0xe49b69c1u, 0xefbe4786u,
	0x0fc19dc6u, 0x240ca1ccu, 0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
	0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u,
	0x06ca6351u, 0x14292967u, 0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u,
	0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u, 0xa2bfe8a1u, 0xa81a664bu,
	0xc24b8b70u, 0xc76c51a3u, 0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
	0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au,
	0x5b9cca4fu, 0x682e6ff3u, 0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u,
	0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u,
};

static inline uint32_t ror32(uint32_t v, unsigned n)
{
	return (v >> n) | (v << (32u - n));
}

static void sha256_compress(uint32_t state[8], const uint8_t block[64])
{
	uint32_t w[64];
	uint32_t a, b, c, d, e, f, g, h;

	for (unsigned i = 0; i < 16; i++) {
		w[i] = ((uint32_t)block[i * 4] << 24) | ((uint32_t)block[i * 4 + 1] << 16) |
		       ((uint32_t)block[i * 4 + 2] << 8) | (uint32_t)block[i * 4 + 3];
	}
	for (unsigned i = 16; i < 64; i++) {
		uint32_t s0 = ror32(w[i - 15], 7) ^ ror32(w[i - 15], 18) ^ (w[i - 15] >> 3);
		uint32_t s1 = ror32(w[i - 2], 17) ^ ror32(w[i - 2], 19) ^ (w[i - 2] >> 10);
		w[i] = w[i - 16] + s0 + w[i - 7] + s1;
	}

	a = state[0]; b = state[1]; c = state[2]; d = state[3];
	e = state[4]; f = state[5]; g = state[6]; h = state[7];

	for (unsigned i = 0; i < 64; i++) {
		uint32_t s1 = ror32(e, 6) ^ ror32(e, 11) ^ ror32(e, 25);
		uint32_t ch = (e & f) ^ ((~e) & g);
		uint32_t t1 = h + s1 + ch + k256[i] + w[i];
		uint32_t s0 = ror32(a, 2) ^ ror32(a, 13) ^ ror32(a, 22);
		uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
		uint32_t t2 = s0 + maj;

		h = g; g = f; f = e; e = d + t1;
		d = c; c = b; b = a; a = t1 + t2;
	}

	state[0] += a; state[1] += b; state[2] += c; state[3] += d;
	state[4] += e; state[5] += f; state[6] += g; state[7] += h;

	/* The schedule holds the message. Clearing it costs 64 stores and keeps a
	 * copy of the canonical price string off the stack after the digest is
	 * taken. */
	memset(w, 0, sizeof(w));
}

void usslp_sha256_init(struct usslp_sha256 *ctx)
{
	ctx->state[0] = 0x6a09e667u; ctx->state[1] = 0xbb67ae85u;
	ctx->state[2] = 0x3c6ef372u; ctx->state[3] = 0xa54ff53au;
	ctx->state[4] = 0x510e527fu; ctx->state[5] = 0x9b05688cu;
	ctx->state[6] = 0x1f83d9abu; ctx->state[7] = 0x5be0cd19u;
	ctx->bitlen = 0;
	ctx->buflen = 0;
	memset(ctx->buf, 0, sizeof(ctx->buf));
}

void usslp_sha256_update(struct usslp_sha256 *ctx, const void *data, size_t len)
{
	const uint8_t *p = (const uint8_t *)data;

	ctx->bitlen += (uint64_t)len * 8u;

	if (ctx->buflen > 0) {
		size_t need = USSLP_SHA256_BLOCK_LEN - ctx->buflen;
		size_t take = (len < need) ? len : need;
		memcpy(ctx->buf + ctx->buflen, p, take);
		ctx->buflen += take;
		p += take;
		len -= take;
		if (ctx->buflen == USSLP_SHA256_BLOCK_LEN) {
			sha256_compress(ctx->state, ctx->buf);
			ctx->buflen = 0;
		}
	}
	while (len >= USSLP_SHA256_BLOCK_LEN) {
		sha256_compress(ctx->state, p);
		p += USSLP_SHA256_BLOCK_LEN;
		len -= USSLP_SHA256_BLOCK_LEN;
	}
	if (len > 0) {
		memcpy(ctx->buf, p, len);
		ctx->buflen = len;
	}
}

void usslp_sha256_final(struct usslp_sha256 *ctx, uint8_t out[USSLP_SHA256_DIGEST_LEN])
{
	uint64_t bitlen = ctx->bitlen;
	uint8_t pad[USSLP_SHA256_BLOCK_LEN * 2];
	size_t padlen;

	memset(pad, 0, sizeof(pad));
	pad[0] = 0x80;
	/* Pad to 56 mod 64, then eight bytes of big-endian bit length. */
	padlen = (ctx->buflen < 56) ? (56 - ctx->buflen) : (120 - ctx->buflen);
	for (unsigned i = 0; i < 8; i++) {
		pad[padlen + i] = (uint8_t)(bitlen >> (56 - 8 * i));
	}
	/* usslp_sha256_update adds to bitlen; the padding must not, so the value is
	 * captured above and the context is discarded immediately after. */
	usslp_sha256_update(ctx, pad, padlen + 8);

	for (unsigned i = 0; i < 8; i++) {
		out[i * 4] = (uint8_t)(ctx->state[i] >> 24);
		out[i * 4 + 1] = (uint8_t)(ctx->state[i] >> 16);
		out[i * 4 + 2] = (uint8_t)(ctx->state[i] >> 8);
		out[i * 4 + 3] = (uint8_t)ctx->state[i];
	}
	memset(ctx, 0, sizeof(*ctx));
}

void usslp_sha256(const void *data, size_t len, uint8_t out[USSLP_SHA256_DIGEST_LEN])
{
	struct usslp_sha256 ctx;

	usslp_sha256_init(&ctx);
	usslp_sha256_update(&ctx, data, len);
	usslp_sha256_final(&ctx, out);
}
