#include "usslp_rle.h"

#include <string.h>

#define RLE_LONG_FLAG 0x80u
#define RLE_INK_SHIFT 4u
#define RLE_HEADER_BYTES 8u

size_t usslp_uvarint(const uint8_t *in, size_t in_len, uint64_t *out)
{
	uint64_t v = 0;
	unsigned shift = 0;

	for (size_t i = 0; i < in_len; i++) {
		uint8_t b = in[i];

		if (i == 9u) {
			/* Ten bytes is the maximum for a 64-bit value, and the tenth may
			 * only carry a single bit. Go's binary.Uvarint returns an error
			 * here; so do we, rather than wrapping. */
			if (b > 1u) {
				return 0;
			}
		}
		if (i > 9u) {
			return 0;
		}
		v |= (uint64_t)(b & 0x7fu) << shift;
		if ((b & 0x80u) == 0u) {
			*out = v;
			return i + 1u;
		}
		shift += 7u;
	}
	return 0;
}

int usslp_rle_dimensions(const uint8_t *in, size_t in_len, uint16_t *width, uint16_t *height)
{
	if (in == NULL || in_len < RLE_HEADER_BYTES) {
		return USSLP_ERR_MALFORMED;
	}
	if (in[0] != 'U' || in[1] != 'F' || in[2] != 'B' || in[3] != '2') {
		return USSLP_ERR_MALFORMED;
	}
	if (width != NULL) {
		*width = (uint16_t)(((uint16_t)in[4] << 8) | in[5]);
	}
	if (height != NULL) {
		*height = (uint16_t)(((uint16_t)in[6] << 8) | in[7]);
	}
	return USSLP_OK;
}

int usslp_rle_decode(const uint8_t *in, size_t in_len, uint8_t *out, size_t out_cap,
		     uint16_t *width_out, uint16_t *height_out)
{
	uint16_t w, h;
	size_t i, y;
	int rc;

	if (out == NULL) {
		return USSLP_ERR_INVAL;
	}
	rc = usslp_rle_dimensions(in, in_len, &w, &h);
	if (rc != USSLP_OK) {
		return rc;
	}
	if (width_out != NULL) {
		*width_out = w;
	}
	if (height_out != NULL) {
		*height_out = h;
	}
	if (w == 0u || h == 0u) {
		/* A zero-area window is legal and encodes to a bare header: it is what
		 * a diff that found no changed pixels produces. */
		return USSLP_OK;
	}
	if ((size_t)w * (size_t)h > out_cap) {
		return USSLP_ERR_NOSPACE;
	}

	i = RLE_HEADER_BYTES;
	y = 0;
	while (y < h) {
		uint64_t reps;
		size_t used;
		size_t x = 0;
		uint8_t *row = out + (size_t)y * w;

		used = usslp_uvarint(&in[i], in_len - i, &reps);
		if (used == 0u || reps == 0u) {
			return USSLP_ERR_MALFORMED;
		}
		i += used;

		while (x < w) {
			uint8_t hdr;
			uint8_t ink;
			uint64_t n;

			if (i >= in_len) {
				return USSLP_ERR_MALFORMED;
			}
			hdr = in[i++];
			ink = (uint8_t)((hdr >> RLE_INK_SHIFT) & 0x07u);
			if (ink >= USSLP_INK_COUNT) {
				return USSLP_ERR_MALFORMED;
			}
			if ((hdr & RLE_LONG_FLAG) != 0u) {
				used = usslp_uvarint(&in[i], in_len - i, &n);
				if (used == 0u) {
					return USSLP_ERR_MALFORMED;
				}
				i += used;
			} else {
				n = (uint64_t)(hdr & 0x0fu) + 1u;
			}
			if (n == 0u || n > (uint64_t)(w - x)) {
				return USSLP_ERR_MALFORMED;
			}
			memset(row + x, ink, (size_t)n);
			x += (size_t)n;
		}

		if (reps > (uint64_t)(h - y)) {
			return USSLP_ERR_MALFORMED;
		}
		for (uint64_t r = 1; r < reps; r++) {
			memcpy(out + (size_t)(y + r) * w, row, w);
		}
		y += (size_t)reps;
	}
	/* Trailing bytes after a complete image mean the encoder and this decoder
	 * disagree about the format. Accepting them would let a crafted frame smuggle
	 * data past a length check somewhere else. */
	if (i != in_len) {
		return USSLP_ERR_MALFORMED;
	}
	return USSLP_OK;
}
