/*
 * usslp_rle.h - the UFB2 image codec, decoder side.
 *
 * The controller renders and compresses; the label expands. The format is
 * defined in edge/sec/framebuffer.go (EncodeRLE / DecodeRLE) and this is the
 * firmware decoder that file's comment refers to when it says "it has to run in
 * a few hundred bytes of firmware".
 *
 * Format, after the 8-byte header ("UFB2", uint16 width, uint16 height, both
 * big endian), is a sequence of row groups:
 *
 *   <uvarint reps> <runs...>
 *
 * where the runs cover exactly one row of `width` pixels, and that row is
 * repeated `reps` times. A run is one header byte:
 *
 *   bit 7    long flag: the run length follows as a uvarint
 *   bits 6-4 ink state, 0-6
 *   bits 3-0 (short form only) length minus one, so 1-16
 *
 * Two properties of a shelf-label image drive that design and both were
 * measured rather than assumed: the image is mostly identical rows, and the runs
 * inside a row are short. A 296x128 BWR price render goes from 37,888 bytes raw
 * to a few hundred, which is the difference between an update costing forty
 * 802.15.4 fragments per hop and costing eight.
 *
 * The decoder is bounds-checked at every step. It is fed by the radio, from a
 * frame whose CRC has been checked but whose *contents* are attacker-influenced
 * on any deployment that has not moved to end-to-end attestation, so a run
 * length that overruns a row is a refusal, not a clamp.
 */

#ifndef USSLP_RLE_H
#define USSLP_RLE_H

#include "../usslp_portable.h"

/* Ink states, matching sec.Ink exactly. The value is what the display
 * controller's LUT is indexed by, so the numbering is hardware, not taste. */
enum usslp_ink {
	USSLP_INK_WHITE = 0,
	USSLP_INK_BLACK = 1,
	USSLP_INK_RED = 2,
	USSLP_INK_YELLOW = 3,
	USSLP_INK_GREEN = 4,
	USSLP_INK_BLUE = 5,
	USSLP_INK_ORANGE = 6,
	USSLP_INK_COUNT = 7,
};

/*
 * Decodes into a caller-owned one-byte-per-pixel buffer.
 *
 * out must hold at least width*height bytes, where the dimensions come from the
 * encoded header; usslp_rle_dimensions reads them without decoding so the caller
 * can check the window fits the panel before committing a buffer.
 *
 * Returns USSLP_OK, or USSLP_ERR_MALFORMED for any structural problem, or
 * USSLP_ERR_NOSPACE when the encoded image is larger than out_cap.
 */
int usslp_rle_decode(const uint8_t *in, size_t in_len, uint8_t *out, size_t out_cap,
		     uint16_t *width_out, uint16_t *height_out);

/* Reads the header only. Returns USSLP_ERR_MALFORMED for a bad magic or a
 * truncated header. */
int usslp_rle_dimensions(const uint8_t *in, size_t in_len, uint16_t *width, uint16_t *height);

/*
 * Reads a base-128 varint in Go's encoding/binary Uvarint format: seven bits per
 * byte, little-endian groups, high bit set on all but the last byte. Returns the
 * number of bytes consumed, or 0 on a truncated or over-long encoding.
 *
 * Exposed because the OTA patch format uses the same encoding, and one
 * implementation with one set of overflow checks is better than two.
 */
size_t usslp_uvarint(const uint8_t *in, size_t in_len, uint64_t *out);

#endif /* USSLP_RLE_H */
