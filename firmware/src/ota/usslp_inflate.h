/*
 * usslp_inflate.h - raw DEFLATE (RFC 1951) decompression, streaming out.
 *
 * The USSLP delta format compresses its instruction stream with Go's
 * compress/flate at best compression (platform/internal/ota/domain/delta.go).
 * That choice is right for the platform — what survives the block matcher is
 * genuinely new code and strings, ordinary compressible data, and compressing it
 * typically halves it again — and it means the label needs an inflater.
 *
 * The shape here is dictated by the device, not by convenience:
 *
 *   - Input is a flat buffer. On the nRF52840 internal flash is memory mapped,
 *     so the staged patch in the scratch partition is addressable as a const
 *     pointer and there is no reason to copy it into RAM first.
 *   - Output is a callback, not a buffer. The inflated instruction stream is
 *     tens of kilobytes and the reconstructed image is hundreds; neither fits.
 *     The patch interpreter consumes bytes as they emerge and writes the image
 *     to the inactive slot as it goes.
 *   - The 32 KiB history window is caller-provided. DEFLATE back-references
 *     reach 32,768 bytes and there is no way around holding that; making the
 *     caller supply it means the OTA subsystem can put it in a buffer it already
 *     owns rather than this module claiming a static allocation that costs 32 KiB
 *     of the 256 KiB budget for the whole life of the device rather than for the
 *     ninety seconds an update takes.
 */

#ifndef USSLP_INFLATE_H
#define USSLP_INFLATE_H

#include "../usslp_portable.h"

#define USSLP_INFLATE_WINDOW 32768u

/* Called with each run of decompressed bytes in order. Returning anything other
 * than USSLP_OK aborts the inflation and is propagated to the caller. */
typedef int (*usslp_inflate_sink)(void *ctx, const uint8_t *data, size_t len);

/*
 * Inflates a raw DEFLATE stream.
 *
 * window must point to USSLP_INFLATE_WINDOW bytes and need not be initialised.
 * max_output bounds the number of bytes produced; exceeding it is
 * USSLP_ERR_MALFORMED rather than a truncation, because a patch header that
 * declares one size and carries another is malformed and a device with 256 KiB
 * of RAM must not discover that by running out of somewhere to put it.
 *
 * Returns USSLP_OK on a complete, well-formed stream, USSLP_ERR_MALFORMED on
 * anything else, or whatever the sink returned.
 */
int usslp_inflate_raw(const uint8_t *in, size_t in_len, uint8_t *window, uint64_t max_output,
		      usslp_inflate_sink sink, void *ctx, uint64_t *produced_out,
		      size_t *consumed_out);

#endif /* USSLP_INFLATE_H */
