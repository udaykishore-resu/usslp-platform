/*
 * usslp_patch.h - the USDELTA1 binary patch applier.
 *
 * The producer is domain.Diff in platform/internal/ota/domain/delta.go. The
 * format:
 *
 *   "USDELTA1"          8 bytes
 *   base SHA-256       32 bytes
 *   target SHA-256     32 bytes
 *   target size        uvarint
 *   instruction length uvarint   (the *uncompressed* instruction stream)
 *   body               raw DEFLATE of the instruction stream
 *
 * and each instruction is
 *
 *   opCopy    (1) <uvarint length> <uvarint offset>   copy from the base image
 *   opLiteral (2) <uvarint length> <length bytes>     new material
 *
 * Why the label bothers
 * ---------------------
 * A firmware image is a few hundred kilobytes. Pushing one across a Zigbee mesh
 * that is also carrying price updates, at tens of kilobits per second, to a
 * device whose entire seven-year energy budget is a coin cell, is the most
 * expensive thing this product ever asks a label to do: transmitting a byte
 * costs orders of magnitude more energy than storing one. A rollout that moves a
 * fifth of the bytes does not merely finish five times sooner, it costs a fifth
 * of the battery, and battery is the resource the product is sold on.
 *
 * What this implementation refuses
 * --------------------------------
 * Every bound is checked, and a violation is a refusal rather than a clamp,
 * because the device that gets this wrong has to be physically retrieved:
 *
 *   - a patch whose base digest is not the image in the active slot
 *   - a copy instruction reaching past the end of the base
 *   - instructions producing more bytes than the header declares
 *   - a stream that ends having produced fewer
 *   - a reconstructed image whose SHA-256 is not the declared target
 *
 * The last of those is what makes the whole pipeline safe: whatever happened in
 * transit, the image that gets flashed is the image the signature was computed
 * over, or no image at all.
 *
 * Note that this check is *not* the signature check. usslp_patch_apply proves
 * the reconstruction is the intended target of this patch; ota.c separately
 * verifies the MCUboot image header and signature over the result before the
 * slot is marked for swap. A patch is an optimisation of the transport, never a
 * substitute for the authorisation.
 */

#ifndef USSLP_PATCH_H
#define USSLP_PATCH_H

#include "../crypto/usslp_sha256.h"
#include "../usslp_portable.h"
#include "usslp_inflate.h"

#define USSLP_DELTA_MAGIC "USDELTA1"
#define USSLP_DELTA_MAGIC_LEN 8
/* Fixed header before the two uvarints. */
#define USSLP_DELTA_HEADER_MIN (USSLP_DELTA_MAGIC_LEN + 64 + 2)

/*
 * Flash access, abstracted so the same interpreter serves the device and the
 * host tests. On the nRF52840 read_base is a memcpy from the memory-mapped
 * active slot and write_target is a flash_img_buffered_write into the inactive
 * one; in tests/test_patch.c both are RAM.
 *
 * write_target is always called with strictly increasing, contiguous offsets
 * starting at 0, which is what a streaming flash writer needs.
 */
struct usslp_patch_io {
	int (*read_base)(void *ctx, uint32_t offset, uint8_t *dst, uint32_t len);
	int (*write_target)(void *ctx, uint32_t offset, const uint8_t *src, uint32_t len);
	void *ctx;
	/* Size of the base image, used to bound every copy instruction. */
	uint32_t base_len;
};

struct usslp_patch_stats {
	uint32_t copies;
	uint32_t literals;
	uint32_t literal_bytes;
	uint32_t copied_bytes;
	uint32_t target_size;
};

/*
 * Verifies that base_digest matches the image behind io and returns the header's
 * declared target size and digest without applying anything. The OTA controller
 * calls this the moment a patch has finished downloading, so that a patch aimed
 * at a different base is rejected before a single flash page of the inactive
 * slot is erased.
 */
int usslp_patch_inspect(const uint8_t *patch, size_t patch_len, struct usslp_patch_io *io,
			uint32_t *target_size_out,
			uint8_t target_digest_out[USSLP_SHA256_DIGEST_LEN]);

/*
 * Applies the patch.
 *
 * window must point to USSLP_INFLATE_WINDOW bytes of scratch. stats may be NULL.
 *
 * Returns USSLP_OK, USSLP_ERR_MALFORMED for a structurally bad patch,
 * USSLP_ERR_INTEGRITY for a base or target digest mismatch, or USSLP_ERR_IO if
 * the flash callbacks failed.
 */
int usslp_patch_apply(const uint8_t *patch, size_t patch_len, struct usslp_patch_io *io,
		      uint8_t *window, struct usslp_patch_stats *stats);

#endif /* USSLP_PATCH_H */
