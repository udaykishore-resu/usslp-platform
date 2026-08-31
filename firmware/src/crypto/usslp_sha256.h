/*
 * usslp_sha256.h - FIPS 180-4 SHA-256.
 *
 * Zephyr can supply SHA-256 through PSA Crypto, and the OTA image hash uses
 * that path because the same hardware-accelerated driver verifies the MCUboot
 * signature. The price attestation digest deliberately does *not*: it uses this
 * implementation, on device and on the host test bench, so that the bytes the
 * host tests hash are hashed by the same code that runs on the shelf.
 *
 * That is not paranoia about PSA. It is that the attestation digest is the one
 * value in the system where a difference between "what we tested" and "what
 * ships" is unrecoverable in the field: a label that computes a digest one bit
 * differently from platform/pkg/canon will refuse every price it is ever sent,
 * and it will refuse them silently and correctly, which is the hardest kind of
 * failure to diagnose. Two kilobytes of flash buys the guarantee that the two
 * cannot diverge.
 */

#ifndef USSLP_SHA256_H
#define USSLP_SHA256_H

#include "../usslp_portable.h"

#define USSLP_SHA256_DIGEST_LEN 32
#define USSLP_SHA256_BLOCK_LEN 64

struct usslp_sha256 {
	uint32_t state[8];
	uint64_t bitlen;
	uint8_t buf[USSLP_SHA256_BLOCK_LEN];
	size_t buflen;
};

void usslp_sha256_init(struct usslp_sha256 *ctx);
void usslp_sha256_update(struct usslp_sha256 *ctx, const void *data, size_t len);

/* Writes the digest and wipes the context. Wiping matters here because the
 * context sits on the stack of the price thread, and the next thing that thread
 * does is drive a display update that can be interrupted. */
void usslp_sha256_final(struct usslp_sha256 *ctx, uint8_t out[USSLP_SHA256_DIGEST_LEN]);

/* One-shot convenience. */
void usslp_sha256(const void *data, size_t len, uint8_t out[USSLP_SHA256_DIGEST_LEN]);

#endif /* USSLP_SHA256_H */
