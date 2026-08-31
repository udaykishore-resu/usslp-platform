#include "usslp_patch.h"

#include "../display/usslp_rle.h" /* usslp_uvarint: one varint reader, one set of overflow checks */

#include <string.h>

/* Chunk used to stream the base image through the digest and through copy
 * instructions. 256 bytes is one nRF52840 flash write block and keeps the
 * interpreter's stack under half a kilobyte. */
#define PATCH_CHUNK 256

/* A declared image size beyond any plausible firmware is refused before a byte
 * is written: the header is attacker-reachable on a device with a few hundred
 * kilobytes of RAM and one megabyte of flash. The platform's own limit is
 * 256 MiB; the label's is its slot. */
#define PATCH_MAX_IMAGE (512u * 1024u)

enum patch_op_state {
	PS_OPCODE = 0,
	PS_LENGTH,
	PS_OFFSET,
	PS_LITERAL,
};

struct patch_ctx {
	struct usslp_patch_io *io;
	struct usslp_sha256 target_hash;
	struct usslp_patch_stats stats;

	uint32_t target_size;
	uint32_t written;

	enum patch_op_state state;
	uint8_t opcode;
	/* Partially accumulated varint. */
	uint64_t varint;
	unsigned varint_shift;
	unsigned varint_bytes;
	uint64_t length;
	uint64_t remaining; /* literal bytes still to consume */

	int io_error;
};

static int emit_target(struct patch_ctx *p, const uint8_t *data, uint32_t len)
{
	int rc;

	if ((uint64_t)p->written + len > p->target_size) {
		return USSLP_ERR_MALFORMED;
	}
	rc = p->io->write_target(p->io->ctx, p->written, data, len);
	if (rc != 0) {
		p->io_error = rc;
		return USSLP_ERR_IO;
	}
	usslp_sha256_update(&p->target_hash, data, len);
	p->written += len;
	return USSLP_OK;
}

static int do_copy(struct patch_ctx *p, uint64_t offset, uint64_t length)
{
	uint8_t chunk[PATCH_CHUNK];

	if (offset + length > (uint64_t)p->io->base_len || offset + length < offset) {
		return USSLP_ERR_MALFORMED;
	}
	while (length > 0u) {
		uint32_t n = (length > PATCH_CHUNK) ? PATCH_CHUNK : (uint32_t)length;
		int rc;

		if (p->io->read_base(p->io->ctx, (uint32_t)offset, chunk, n) != 0) {
			p->io_error = USSLP_ERR_IO;
			return USSLP_ERR_IO;
		}
		rc = emit_target(p, chunk, n);
		if (rc != USSLP_OK) {
			return rc;
		}
		offset += n;
		length -= n;
	}
	p->stats.copies++;
	return USSLP_OK;
}

/*
 * The instruction interpreter, fed by the inflater one run of bytes at a time.
 *
 * It is a state machine rather than a loop over a decompressed buffer because
 * the decompressed instruction stream can be tens of kilobytes and the device
 * has nowhere to put it. The literal payload in particular is passed straight
 * from the inflater's staging buffer to flash without ever being assembled.
 */
static int patch_sink(void *ctx, const uint8_t *data, size_t len)
{
	struct patch_ctx *p = (struct patch_ctx *)ctx;
	size_t i = 0;

	while (i < len) {
		switch (p->state) {
		case PS_OPCODE:
			p->opcode = data[i++];
			if (p->opcode != 1u && p->opcode != 2u) {
				return USSLP_ERR_MALFORMED;
			}
			p->state = PS_LENGTH;
			p->varint = 0;
			p->varint_shift = 0;
			p->varint_bytes = 0;
			break;

		case PS_LENGTH:
		case PS_OFFSET: {
			uint8_t b = data[i++];

			if (p->varint_bytes == 9u && b > 1u) {
				return USSLP_ERR_MALFORMED;
			}
			if (p->varint_bytes > 9u) {
				return USSLP_ERR_MALFORMED;
			}
			p->varint |= (uint64_t)(b & 0x7fu) << p->varint_shift;
			p->varint_shift += 7u;
			p->varint_bytes++;
			if ((b & 0x80u) != 0u) {
				break;
			}
			if (p->state == PS_LENGTH) {
				p->length = p->varint;
				if (p->length > PATCH_MAX_IMAGE) {
					return USSLP_ERR_MALFORMED;
				}
				p->varint = 0;
				p->varint_shift = 0;
				p->varint_bytes = 0;
				if (p->opcode == 1u) {
					p->state = PS_OFFSET;
				} else {
					p->remaining = p->length;
					p->stats.literals++;
					p->state = PS_LITERAL;
					if (p->remaining == 0u) {
						p->state = PS_OPCODE;
					}
				}
			} else {
				int rc = do_copy(p, p->varint, p->length);

				if (rc != USSLP_OK) {
					return rc;
				}
				p->stats.copied_bytes += (uint32_t)p->length;
				p->state = PS_OPCODE;
			}
			break;
		}

		case PS_LITERAL: {
			size_t avail = len - i;
			uint32_t take = (avail > p->remaining) ? (uint32_t)p->remaining
							      : (uint32_t)avail;
			int rc = emit_target(p, &data[i], take);

			if (rc != USSLP_OK) {
				return rc;
			}
			p->stats.literal_bytes += take;
			i += take;
			p->remaining -= take;
			if (p->remaining == 0u) {
				p->state = PS_OPCODE;
			}
			break;
		}
		}
	}
	return USSLP_OK;
}

/* Parses the fixed header. On success *body is the offset of the DEFLATE body. */
static int parse_header(const uint8_t *patch, size_t patch_len, uint8_t base_digest[32],
			uint8_t target_digest[32], uint64_t *target_size, uint64_t *ops_len,
			size_t *body)
{
	size_t off = USSLP_DELTA_MAGIC_LEN;
	size_t used;

	if (patch == NULL || patch_len < USSLP_DELTA_HEADER_MIN) {
		return USSLP_ERR_MALFORMED;
	}
	if (memcmp(patch, USSLP_DELTA_MAGIC, USSLP_DELTA_MAGIC_LEN) != 0) {
		return USSLP_ERR_MALFORMED;
	}
	memcpy(base_digest, &patch[off], 32);
	off += 32;
	memcpy(target_digest, &patch[off], 32);
	off += 32;

	used = usslp_uvarint(&patch[off], patch_len - off, target_size);
	if (used == 0u) {
		return USSLP_ERR_MALFORMED;
	}
	off += used;
	used = usslp_uvarint(&patch[off], patch_len - off, ops_len);
	if (used == 0u) {
		return USSLP_ERR_MALFORMED;
	}
	off += used;
	if (*target_size > PATCH_MAX_IMAGE || *ops_len > PATCH_MAX_IMAGE * 2u) {
		return USSLP_ERR_MALFORMED;
	}
	*body = off;
	return USSLP_OK;
}

static int hash_base(struct usslp_patch_io *io, uint8_t out[USSLP_SHA256_DIGEST_LEN])
{
	struct usslp_sha256 ctx;
	uint8_t chunk[PATCH_CHUNK];
	uint32_t off = 0;

	usslp_sha256_init(&ctx);
	while (off < io->base_len) {
		uint32_t n = io->base_len - off;

		if (n > PATCH_CHUNK) {
			n = PATCH_CHUNK;
		}
		if (io->read_base(io->ctx, off, chunk, n) != 0) {
			usslp_sha256_final(&ctx, out);
			memset(out, 0, USSLP_SHA256_DIGEST_LEN);
			return USSLP_ERR_IO;
		}
		usslp_sha256_update(&ctx, chunk, n);
		off += n;
	}
	usslp_sha256_final(&ctx, out);
	return USSLP_OK;
}

int usslp_patch_inspect(const uint8_t *patch, size_t patch_len, struct usslp_patch_io *io,
			uint32_t *target_size_out,
			uint8_t target_digest_out[USSLP_SHA256_DIGEST_LEN])
{
	uint8_t want_base[32], want_target[32], got_base[32];
	uint64_t target_size, ops_len;
	size_t body;
	int rc;

	rc = parse_header(patch, patch_len, want_base, want_target, &target_size, &ops_len, &body);
	if (rc != USSLP_OK) {
		return rc;
	}
	rc = hash_base(io, got_base);
	if (rc != USSLP_OK) {
		return rc;
	}
	if (usslp_ct_memcmp(got_base, want_base, 32) != 0) {
		/* domain.ErrDeltaBaseMismatch. Applying it anyway would produce a
		 * plausible-looking image that has never been tested, on a device that
		 * has to be retrieved by hand if it does not boot. */
		return USSLP_ERR_INTEGRITY;
	}
	if (target_size_out != NULL) {
		*target_size_out = (uint32_t)target_size;
	}
	if (target_digest_out != NULL) {
		memcpy(target_digest_out, want_target, 32);
	}
	return USSLP_OK;
}

int usslp_patch_apply(const uint8_t *patch, size_t patch_len, struct usslp_patch_io *io,
		      uint8_t *window, struct usslp_patch_stats *stats)
{
	uint8_t want_base[32], want_target[32], got[32];
	uint64_t target_size, ops_len, produced = 0;
	size_t body;
	struct patch_ctx p;
	int rc;

	if (io == NULL || io->read_base == NULL || io->write_target == NULL || window == NULL) {
		return USSLP_ERR_INVAL;
	}
	rc = parse_header(patch, patch_len, want_base, want_target, &target_size, &ops_len, &body);
	if (rc != USSLP_OK) {
		return rc;
	}
	rc = hash_base(io, got);
	if (rc != USSLP_OK) {
		return rc;
	}
	if (usslp_ct_memcmp(got, want_base, 32) != 0) {
		return USSLP_ERR_INTEGRITY;
	}

	memset(&p, 0, sizeof(p));
	p.io = io;
	p.target_size = (uint32_t)target_size;
	p.state = PS_OPCODE;
	usslp_sha256_init(&p.target_hash);

	rc = usslp_inflate_raw(&patch[body], patch_len - body, window, ops_len, patch_sink, &p,
			       &produced, NULL);
	if (rc != USSLP_OK) {
		usslp_sha256_final(&p.target_hash, got);
		return rc == USSLP_ERR_IO ? USSLP_ERR_IO : rc;
	}
	if (produced != ops_len) {
		/* The header declares the length of the instruction stream. A body that
		 * inflates to a different length means the two disagree, which is a
		 * malformed patch rather than a harmless surplus. */
		usslp_sha256_final(&p.target_hash, got);
		return USSLP_ERR_MALFORMED;
	}
	if (p.state != PS_OPCODE) {
		usslp_sha256_final(&p.target_hash, got);
		return USSLP_ERR_MALFORMED; /* stream ended mid-instruction */
	}
	if (p.written != p.target_size) {
		usslp_sha256_final(&p.target_hash, got);
		return USSLP_ERR_MALFORMED;
	}

	usslp_sha256_final(&p.target_hash, got);
	if (usslp_ct_memcmp(got, want_target, 32) != 0) {
		/* domain.ErrDeltaResultMismatch: the last line of defence against a
		 * corrupted transfer and against a patch built to reference memory
		 * outside the base. */
		return USSLP_ERR_INTEGRITY;
	}
	p.stats.target_size = p.written;
	if (stats != NULL) {
		*stats = p.stats;
	}
	return USSLP_OK;
}
