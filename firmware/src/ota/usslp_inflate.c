/*
 * Raw DEFLATE decompression.
 *
 * The Huffman decoder is the canonical count/symbol formulation: for each code
 * length in turn, ask whether the bits read so far fall inside the range of
 * codes of that length. It decodes one bit at a time, which is slower than a
 * table-driven decoder and about a hundred times smaller, and an OTA that takes
 * four seconds instead of one on a device that spends the other 99.99% of its
 * life asleep is not a trade worth 4 KiB of flash.
 */

#include "usslp_inflate.h"

#include <string.h>

#define MAXBITS 15
#define MAXLCODES 286
#define MAXDCODES 30
#define MAXCODES (MAXLCODES + MAXDCODES)
#define FIXLCODES 288

/* Bytes staged before each call into the sink. Chosen so the flash writes the
 * patch interpreter ultimately performs land on page-sized boundaries more often
 * than not. */
#define STAGE_BYTES 128

struct huffman {
	short count[MAXBITS + 1];
	short symbol[MAXCODES];
};

struct inflate_state {
	const uint8_t *in;
	size_t in_len;
	size_t in_pos;
	uint32_t bitbuf;
	unsigned bitcnt;

	uint8_t *window;
	size_t window_pos;

	uint8_t stage[STAGE_BYTES];
	size_t stage_len;

	usslp_inflate_sink sink;
	void *ctx;
	uint64_t produced;
	uint64_t max_output;
	int sink_rc;
};

static int flush_stage(struct inflate_state *s)
{
	int rc;

	if (s->stage_len == 0u) {
		return USSLP_OK;
	}
	rc = s->sink(s->ctx, s->stage, s->stage_len);
	s->stage_len = 0;
	return rc;
}

static int emit(struct inflate_state *s, uint8_t b)
{
	if (s->produced >= s->max_output) {
		return USSLP_ERR_MALFORMED;
	}
	s->window[s->window_pos] = b;
	s->window_pos = (s->window_pos + 1u) & (USSLP_INFLATE_WINDOW - 1u);
	s->produced++;
	s->stage[s->stage_len++] = b;
	if (s->stage_len == STAGE_BYTES) {
		return flush_stage(s);
	}
	return USSLP_OK;
}

/* Returns -1 when the stream runs out, which is always malformed here: a
 * well-formed DEFLATE stream ends inside its final block. */
static int bits(struct inflate_state *s, unsigned need, uint32_t *out)
{
	uint32_t val = s->bitbuf;

	while (s->bitcnt < need) {
		if (s->in_pos >= s->in_len) {
			return -1;
		}
		val |= (uint32_t)s->in[s->in_pos++] << s->bitcnt;
		s->bitcnt += 8u;
	}
	s->bitbuf = val >> need;
	s->bitcnt -= need;
	*out = val & ((1u << need) - 1u);
	return 0;
}

static int decode_sym(struct inflate_state *s, const struct huffman *h)
{
	int len, code = 0, first = 0, index = 0;
	uint32_t b;

	for (len = 1; len <= MAXBITS; len++) {
		if (bits(s, 1, &b) < 0) {
			return -1;
		}
		code |= (int)b;
		{
			int count = h->count[len];

			if (code - count < first) {
				return h->symbol[index + (code - first)];
			}
			index += count;
			first += count;
			first <<= 1;
			code <<= 1;
		}
	}
	return -1;
}

/* Builds a canonical Huffman decoding table. Returns 0 for a complete code,
 * a positive value for an incomplete one and a negative value for an
 * over-subscribed one. */
static int construct(struct huffman *h, const short *length, int n)
{
	int symbol, len, left;
	short offs[MAXBITS + 1];

	for (len = 0; len <= MAXBITS; len++) {
		h->count[len] = 0;
	}
	for (symbol = 0; symbol < n; symbol++) {
		h->count[length[symbol]]++;
	}
	if (h->count[0] == n) {
		return 0;
	}
	left = 1;
	for (len = 1; len <= MAXBITS; len++) {
		left <<= 1;
		left -= h->count[len];
		if (left < 0) {
			return left;
		}
	}
	offs[1] = 0;
	for (len = 1; len < MAXBITS; len++) {
		offs[len + 1] = (short)(offs[len] + h->count[len]);
	}
	for (symbol = 0; symbol < n; symbol++) {
		if (length[symbol] != 0) {
			h->symbol[offs[length[symbol]]++] = (short)symbol;
		}
	}
	return left;
}

static const short len_base[29] = { 3,  4,  5,  6,  7,  8,  9,  10,  11,  13,
				    15, 17, 19, 23, 27, 31, 35, 43,  51,  59,
				    67, 83, 99, 115, 131, 163, 195, 227, 258 };
static const short len_extra[29] = { 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2,
				     2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0 };
static const short dist_base[30] = { 1,    2,    3,    4,    5,    7,     9,     13,
				     17,   25,   33,   49,   65,   97,    129,   193,
				     257,  385,  513,  769,  1025, 1537,  2049,  3073,
				     4097, 6145, 8193, 12289, 16385, 24577 };
static const short dist_extra[30] = { 0, 0, 0, 0, 1, 1, 2, 2,  3,  3,  4,  4,  5,  5,  6,
				      6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13 };

static int codes(struct inflate_state *s, const struct huffman *lencode,
		 const struct huffman *distcode)
{
	int sym;
	uint32_t b;

	for (;;) {
		sym = decode_sym(s, lencode);
		if (sym < 0) {
			return USSLP_ERR_MALFORMED;
		}
		if (sym < 256) {
			int rc = emit(s, (uint8_t)sym);

			if (rc != USSLP_OK) {
				return rc;
			}
			continue;
		}
		if (sym == 256) {
			return USSLP_OK;
		}
		sym -= 257;
		if (sym >= 29) {
			return USSLP_ERR_MALFORMED;
		}
		{
			unsigned length, dist;
			int dsym;

			if (bits(s, (unsigned)len_extra[sym], &b) < 0) {
				return USSLP_ERR_MALFORMED;
			}
			length = (unsigned)len_base[sym] + b;

			dsym = decode_sym(s, distcode);
			if (dsym < 0 || dsym >= 30) {
				return USSLP_ERR_MALFORMED;
			}
			if (bits(s, (unsigned)dist_extra[dsym], &b) < 0) {
				return USSLP_ERR_MALFORMED;
			}
			dist = (unsigned)dist_base[dsym] + b;
			if ((uint64_t)dist > s->produced) {
				/* A back-reference before the start of the stream. */
				return USSLP_ERR_MALFORMED;
			}
			while (length-- > 0u) {
				size_t src = (s->window_pos + USSLP_INFLATE_WINDOW - dist) &
					     (USSLP_INFLATE_WINDOW - 1u);
				int rc = emit(s, s->window[src]);

				if (rc != USSLP_OK) {
					return rc;
				}
			}
		}
	}
}

static int stored(struct inflate_state *s)
{
	unsigned len, nlen;

	/* Stored blocks are byte aligned. */
	s->bitbuf = 0;
	s->bitcnt = 0;
	if (s->in_pos + 4u > s->in_len) {
		return USSLP_ERR_MALFORMED;
	}
	len = (unsigned)s->in[s->in_pos] | ((unsigned)s->in[s->in_pos + 1u] << 8);
	nlen = (unsigned)s->in[s->in_pos + 2u] | ((unsigned)s->in[s->in_pos + 3u] << 8);
	s->in_pos += 4u;
	if ((len ^ 0xffffu) != nlen) {
		return USSLP_ERR_MALFORMED;
	}
	if (s->in_pos + len > s->in_len) {
		return USSLP_ERR_MALFORMED;
	}
	while (len-- > 0u) {
		int rc = emit(s, s->in[s->in_pos++]);

		if (rc != USSLP_OK) {
			return rc;
		}
	}
	return USSLP_OK;
}

/*
 * The fixed code tables are rebuilt on every fixed block rather than cached in
 * static storage. Building them is a few hundred operations against a flash
 * write that takes milliseconds, and keeping this module free of mutable statics
 * means the OTA thread and a future factory-test path can both call it without
 * anybody having to reason about which one owns the tables.
 */
static int fixed_block(struct inflate_state *s)
{
	struct huffman lencode, distcode;
	short lengths[FIXLCODES];
	int symbol;

	for (symbol = 0; symbol < 144; symbol++) {
		lengths[symbol] = 8;
	}
	for (; symbol < 256; symbol++) {
		lengths[symbol] = 9;
	}
	for (; symbol < 280; symbol++) {
		lengths[symbol] = 7;
	}
	for (; symbol < FIXLCODES; symbol++) {
		lengths[symbol] = 8;
	}
	construct(&lencode, lengths, FIXLCODES);
	for (symbol = 0; symbol < MAXDCODES; symbol++) {
		lengths[symbol] = 5;
	}
	construct(&distcode, lengths, MAXDCODES);
	return codes(s, &lencode, &distcode);
}

static int dynamic_block(struct inflate_state *s)
{
	static const short order[19] = { 16, 17, 18, 0, 8,  7, 9,  6, 10, 5,
					 11, 4,  12, 3, 13, 2, 14, 1, 15 };
	struct huffman lencode, distcode;
	short lengths[MAXCODES];
	unsigned nlen, ndist, ncode, index;
	uint32_t b;
	int err;

	if (bits(s, 5, &b) < 0) {
		return USSLP_ERR_MALFORMED;
	}
	nlen = b + 257u;
	if (bits(s, 5, &b) < 0) {
		return USSLP_ERR_MALFORMED;
	}
	ndist = b + 1u;
	if (bits(s, 4, &b) < 0) {
		return USSLP_ERR_MALFORMED;
	}
	ncode = b + 4u;
	if (nlen > MAXLCODES || ndist > MAXDCODES) {
		return USSLP_ERR_MALFORMED;
	}

	for (index = 0; index < ncode; index++) {
		if (bits(s, 3, &b) < 0) {
			return USSLP_ERR_MALFORMED;
		}
		lengths[order[index]] = (short)b;
	}
	for (; index < 19u; index++) {
		lengths[order[index]] = 0;
	}
	err = construct(&lencode, lengths, 19);
	if (err != 0) {
		return USSLP_ERR_MALFORMED; /* the code-length code must be complete */
	}

	index = 0;
	while (index < nlen + ndist) {
		int symbol = decode_sym(s, &lencode);
		unsigned len;

		if (symbol < 0) {
			return USSLP_ERR_MALFORMED;
		}
		if (symbol < 16) {
			lengths[index++] = (short)symbol;
			continue;
		}
		if (symbol == 16) {
			if (index == 0u) {
				return USSLP_ERR_MALFORMED;
			}
			len = (unsigned)lengths[index - 1u];
			if (bits(s, 2, &b) < 0) {
				return USSLP_ERR_MALFORMED;
			}
			symbol = (int)(3u + b);
		} else if (symbol == 17) {
			len = 0;
			if (bits(s, 3, &b) < 0) {
				return USSLP_ERR_MALFORMED;
			}
			symbol = (int)(3u + b);
		} else {
			len = 0;
			if (bits(s, 7, &b) < 0) {
				return USSLP_ERR_MALFORMED;
			}
			symbol = (int)(11u + b);
		}
		if (index + (unsigned)symbol > nlen + ndist) {
			return USSLP_ERR_MALFORMED;
		}
		while (symbol-- > 0) {
			lengths[index++] = (short)len;
		}
	}
	/* An incomplete code is legal only in the one case the format allows: a
	 * distance code with a single symbol, which is what a block with exactly one
	 * back-reference produces. Anything else incomplete is malformed. */
	err = construct(&lencode, lengths, (int)nlen);
	if (err < 0 || (err > 0 && (int)nlen != lencode.count[0] + 1)) {
		return USSLP_ERR_MALFORMED;
	}
	err = construct(&distcode, lengths + nlen, (int)ndist);
	if (err < 0 || (err > 0 && (int)ndist != distcode.count[0] + 1)) {
		return USSLP_ERR_MALFORMED;
	}
	return codes(s, &lencode, &distcode);
}

int usslp_inflate_raw(const uint8_t *in, size_t in_len, uint8_t *window, uint64_t max_output,
		      usslp_inflate_sink sink, void *ctx, uint64_t *produced_out,
		      size_t *consumed_out)
{
	struct inflate_state s;
	int rc = USSLP_OK;
	uint32_t last, type;

	if (in == NULL || window == NULL || sink == NULL) {
		return USSLP_ERR_INVAL;
	}
	memset(&s, 0, sizeof(s));
	s.in = in;
	s.in_len = in_len;
	s.window = window;
	s.sink = sink;
	s.ctx = ctx;
	s.max_output = max_output;

	do {
		uint32_t b;

		if (bits(&s, 1, &b) < 0) {
			return USSLP_ERR_MALFORMED;
		}
		last = b;
		if (bits(&s, 2, &b) < 0) {
			return USSLP_ERR_MALFORMED;
		}
		type = b;
		switch (type) {
		case 0:
			rc = stored(&s);
			break;
		case 1:
			rc = fixed_block(&s);
			break;
		case 2:
			rc = dynamic_block(&s);
			break;
		default:
			rc = USSLP_ERR_MALFORMED;
			break;
		}
		if (rc != USSLP_OK) {
			return rc;
		}
	} while (last == 0u);

	rc = flush_stage(&s);
	if (rc != USSLP_OK) {
		return rc;
	}
	if (produced_out != NULL) {
		*produced_out = s.produced;
	}
	if (consumed_out != NULL) {
		/* Whole bytes consumed. Bits still in the buffer belong to the final
		 * byte and are padding. */
		*consumed_out = s.in_pos;
	}
	return USSLP_OK;
}
