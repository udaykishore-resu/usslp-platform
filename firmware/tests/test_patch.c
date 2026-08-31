/*
 * The delta applier against a patch the Go tool produced.
 *
 * delta_patch in vectors.h is the output of domain.Diff over a 4,096-byte base
 * and a target that differs from it the way two firmware builds differ: an
 * edited string in the middle and a nine-byte insertion that shifts everything
 * after it. That insertion is the case a fixed-block differ cannot follow, and
 * the reason the format uses a rolling hash; it is also the case that finds an
 * off-by-one in an applier's copy-offset handling.
 */

#include "../src/ota/usslp_chunkmap.h"
#include "../src/ota/usslp_patch.h"
#include "test_util.h"
#include "test_vectors.h"

struct ram_io {
	const uint8_t *base;
	uint32_t base_len;
	uint8_t *target;
	uint32_t target_cap;
	uint32_t written;
	/* Set when write_target is called with a non-contiguous offset, which the
	 * flash writer on the device could not honour. */
	bool out_of_order;
	int fail_after; /* -1 for never */
};

static int ram_read(void *ctx, uint32_t off, uint8_t *dst, uint32_t len)
{
	struct ram_io *io = (struct ram_io *)ctx;

	if ((uint64_t)off + len > io->base_len) {
		return -1;
	}
	memcpy(dst, io->base + off, len);
	return 0;
}

static int ram_write(void *ctx, uint32_t off, const uint8_t *src, uint32_t len)
{
	struct ram_io *io = (struct ram_io *)ctx;

	if (off != io->written) {
		io->out_of_order = true;
	}
	if (io->fail_after >= 0 && (int)off >= io->fail_after) {
		return -1;
	}
	if ((uint64_t)off + len > io->target_cap) {
		return -1;
	}
	memcpy(io->target + off, src, len);
	io->written = off + len;
	return 0;
}

static void make_base(uint8_t *base, int len)
{
	for (int i = 0; i < len; i++) {
		base[i] = (uint8_t)((i * 31 + 7) & 0xff);
	}
}

void test_patch(void)
{
	static uint8_t window[USSLP_INFLATE_WINDOW];
	static uint8_t base[8192];
	static uint8_t out[8192];
	struct ram_io io;
	struct usslp_patch_io pio;
	struct usslp_patch_stats stats;
	uint32_t target_size;
	uint8_t target_digest[32];
	char hex[65];

	printf("test_patch\n");

	make_base(base, delta_base_len);

	memset(&io, 0, sizeof(io));
	io.base = base;
	io.base_len = (uint32_t)delta_base_len;
	io.target = out;
	io.target_cap = sizeof(out);
	io.fail_after = -1;
	pio.read_base = ram_read;
	pio.write_target = ram_write;
	pio.ctx = &io;
	pio.base_len = (uint32_t)delta_base_len;

	TEST("inspect reports the target without touching the inactive slot");
	CHECK_EQ_I(usslp_patch_inspect(delta_patch, sizeof(delta_patch), &pio, &target_size,
				       target_digest),
		   USSLP_OK);
	CHECK_EQ_I(target_size, (uint32_t)delta_target_len);
	usslp_test_hex(target_digest, 32, hex);
	CHECK_EQ_S(hex, delta_target_sha_hex);
	CHECK_EQ_I(io.written, 0);

	TEST("applying the patch reconstructs the target byte for byte");
	CHECK_EQ_I(usslp_patch_apply(delta_patch, sizeof(delta_patch), &pio, window, &stats),
		   USSLP_OK);
	CHECK_EQ_I(io.written, (uint32_t)delta_target_len);
	CHECK(memcmp(out, delta_target, (size_t)delta_target_len) == 0);
	CHECK(!io.out_of_order);
	CHECK_EQ_I(stats.target_size, (uint32_t)delta_target_len);
	CHECK(stats.copies >= 1);
	CHECK(stats.literals >= 1);
	CHECK_EQ_I(stats.copied_bytes + stats.literal_bytes, (uint32_t)delta_target_len);

	TEST("the patch is much smaller than the image, which is the point");
	/* 185 bytes to move a 4,106-byte image. On real firmware the ratio is worse
	 * than this synthetic case, but the shape is the same: transmitting a byte
	 * costs orders of magnitude more energy than storing one. */
	CHECK(sizeof(delta_patch) * 4u < (size_t)delta_target_len);

	TEST("a patch aimed at a different base is refused before anything is written");
	{
		uint8_t wrong[8192];

		make_base(wrong, delta_base_len);
		wrong[17] ^= 0x01u; /* one bit of one byte of the running image */
		io.base = wrong;
		io.written = 0;
		CHECK_EQ_I(usslp_patch_inspect(delta_patch, sizeof(delta_patch), &pio, NULL, NULL),
			   USSLP_ERR_INTEGRITY);
		CHECK_EQ_I(usslp_patch_apply(delta_patch, sizeof(delta_patch), &pio, window, NULL),
			   USSLP_ERR_INTEGRITY);
		CHECK_EQ_I(io.written, 0);
		io.base = base;
	}

	TEST("a corrupted patch body never produces a valid image");
	for (size_t i = 0; i < sizeof(delta_patch); i++) {
		uint8_t broken[sizeof(delta_patch)];
		int rc;

		memcpy(broken, delta_patch, sizeof(broken));
		broken[i] ^= 0x01u;
		io.written = 0;
		memset(out, 0, (size_t)delta_target_len);
		rc = usslp_patch_apply(broken, sizeof(broken), &pio, window, NULL);
		if (rc == USSLP_OK) {
			/* The only way a single-bit change can still succeed is if it
			 * landed somewhere that does not affect the output, which for this
			 * format means nowhere. Assert the image anyway: a "success" that
			 * produced different bytes would be the failure that costs a truck
			 * roll. */
			CHECK(memcmp(out, delta_target, (size_t)delta_target_len) == 0);
		} else {
			CHECK(rc == USSLP_ERR_MALFORMED || rc == USSLP_ERR_INTEGRITY);
		}
	}

	TEST("a truncated patch is refused at every length");
	for (size_t n = 0; n < sizeof(delta_patch); n++) {
		int rc;

		io.written = 0;
		rc = usslp_patch_apply(delta_patch, n, &pio, window, NULL);
		CHECK(rc != USSLP_OK);
	}

	TEST("a flash write failure is reported rather than leaving a half image");
	io.written = 0;
	io.fail_after = 512;
	CHECK_EQ_I(usslp_patch_apply(delta_patch, sizeof(delta_patch), &pio, window, NULL),
		   USSLP_ERR_IO);
	io.fail_after = -1;

	TEST("a hand-built patch exercises the instruction interpreter directly");
	{
		/* Two copies and a literal, laid out to straddle the inflater's 128-byte
		 * staging boundary so the state machine has to resume mid-varint and
		 * mid-literal. The stream is emitted as a stored DEFLATE block, which is
		 * legal and is what a producer emits for incompressible input. */
		uint8_t ops[600];
		uint8_t patch[900];
		size_t n = 0, p = 0;
		uint8_t base_d[32], target_d[32];
		uint8_t expect[600];
		size_t elen = 0;

		/* opCopy 200 bytes from offset 0 */
		ops[n++] = 1;
		ops[n++] = 200;
		ops[n++] = 1;
		ops[n++] = 0;
		/* opLiteral 300 bytes */
		ops[n++] = 2;
		ops[n++] = 0xac;
		ops[n++] = 0x02;
		for (unsigned i = 0; i < 300; i++) {
			ops[n++] = (uint8_t)(i * 13u + 5u);
		}
		/* opCopy 100 bytes from offset 1000 */
		ops[n++] = 1;
		ops[n++] = 100;
		ops[n++] = 0xe8;
		ops[n++] = 0x07;

		memcpy(expect + elen, base, 200);
		elen += 200;
		for (unsigned i = 0; i < 300; i++) {
			expect[elen++] = (uint8_t)(i * 13u + 5u);
		}
		memcpy(expect + elen, base + 1000, 100);
		elen += 100;

		usslp_sha256(base, (size_t)delta_base_len, base_d);
		usslp_sha256(expect, elen, target_d);

		memcpy(&patch[p], USSLP_DELTA_MAGIC, 8);
		p += 8;
		memcpy(&patch[p], base_d, 32);
		p += 32;
		memcpy(&patch[p], target_d, 32);
		p += 32;
		/* target size 600 as a uvarint */
		patch[p++] = 0xd8;
		patch[p++] = 0x04;
		/* ops length as a uvarint */
		patch[p++] = (uint8_t)((n & 0x7fu) | 0x80u);
		patch[p++] = (uint8_t)(n >> 7);
		/* one final stored DEFLATE block */
		patch[p++] = 0x01;
		patch[p++] = (uint8_t)(n & 0xffu);
		patch[p++] = (uint8_t)(n >> 8);
		patch[p++] = (uint8_t)(~n & 0xffu);
		patch[p++] = (uint8_t)((~n >> 8) & 0xffu);
		memcpy(&patch[p], ops, n);
		p += n;

		io.written = 0;
		io.out_of_order = false;
		CHECK_EQ_I(usslp_patch_apply(patch, p, &pio, window, &stats), USSLP_OK);
		CHECK_EQ_I(io.written, 600);
		CHECK(memcmp(out, expect, 600) == 0);
		CHECK_EQ_I(stats.copies, 2);
		CHECK_EQ_I(stats.literals, 1);
		CHECK_EQ_I(stats.literal_bytes, 300);
		CHECK_EQ_I(stats.copied_bytes, 300);
		CHECK(!io.out_of_order);

		TEST("a copy reaching past the end of the base is refused");
		{
			uint8_t evil[900];

			memcpy(evil, patch, p);
			/* rewrite the second copy's offset to the very end of the base */
			evil[p - n + 4 + 3 + 300 + 2] = 0x80;
			evil[p - n + 4 + 3 + 300 + 3] = 0x20; /* 4096 */
			io.written = 0;
			CHECK(usslp_patch_apply(evil, p, &pio, window, NULL) != USSLP_OK);
		}

		TEST("an unknown opcode is refused");
		{
			uint8_t evil[900];

			memcpy(evil, patch, p);
			evil[p - n] = 3;
			io.written = 0;
			CHECK_EQ_I(usslp_patch_apply(evil, p, &pio, window, NULL),
				   USSLP_ERR_MALFORMED);
		}

		TEST("instructions producing fewer bytes than declared are refused");
		{
			uint8_t evil[900];

			memcpy(evil, patch, p);
			/* declare 601 target bytes; the stream still produces 600 */
			evil[72] = 0xd9;
			io.written = 0;
			CHECK_EQ_I(usslp_patch_apply(evil, p, &pio, window, NULL),
				   USSLP_ERR_MALFORMED);
		}

		TEST("instructions producing more bytes than declared are refused");
		{
			uint8_t evil[900];

			memcpy(evil, patch, p);
			evil[72] = 0xd7; /* 599 */
			io.written = 0;
			CHECK_EQ_I(usslp_patch_apply(evil, p, &pio, window, NULL),
				   USSLP_ERR_MALFORMED);
		}
	}
}

void test_chunkmap(void)
{
	struct usslp_chunk_bitmap m;
	struct usslp_bloom b;

	printf("test_chunkmap\n");

	TEST("the chunk bitmap is exact");
	CHECK_EQ_I(usslp_chunkmap_init(&m, 100u * 1024u), USSLP_OK);
	CHECK_EQ_I(m.total, 200);
	CHECK(!usslp_chunkmap_complete(&m));
	CHECK_EQ_I(usslp_chunkmap_next_missing(&m), 0);
	CHECK(usslp_chunkmap_set(&m, 0));
	CHECK(!usslp_chunkmap_set(&m, 0)); /* a duplicate chunk is not new */
	CHECK(usslp_chunkmap_has(&m, 0));
	CHECK(!usslp_chunkmap_has(&m, 1));
	CHECK_EQ_I(usslp_chunkmap_next_missing(&m), 1);
	CHECK_EQ_I(m.received, 1);
	/* Out of range is neither held nor settable. */
	CHECK(!usslp_chunkmap_set(&m, 200));
	CHECK(!usslp_chunkmap_has(&m, 200));

	TEST("completion needs every chunk");
	for (uint16_t i = 1; i < 200; i++) {
		CHECK(usslp_chunkmap_set(&m, i));
	}
	CHECK(usslp_chunkmap_complete(&m));
	CHECK_EQ_I(usslp_chunkmap_next_missing(&m), -1);

	TEST("an image with no chunks, or more than the map holds, is refused");
	CHECK_EQ_I(usslp_chunkmap_init(&m, 0), USSLP_ERR_INVAL);
	CHECK_EQ_I(usslp_chunkmap_init(&m, USSLP_OTA_MAX_CHUNKS * USSLP_OTA_CHUNK_BYTES),
		   USSLP_OK);
	CHECK_EQ_I(usslp_chunkmap_init(&m, USSLP_OTA_MAX_CHUNKS * USSLP_OTA_CHUNK_BYTES + 1),
		   USSLP_ERR_INVAL);

	TEST("the gossip filter always remembers what it was told");
	/* The direction a Bloom filter guarantees, and the only direction this use
	 * depends on: once a key is added, it is never subsequently missed. A false
	 * *negative* would let the gossip flood, which is the failure the filter
	 * exists to prevent. */
	usslp_bloom_init(&b);
	{
		unsigned suppressed_early = 0;

		for (uint16_t i = 0; i < 200; i++) {
			uint64_t k = usslp_bloom_chunk_key(0xdeadbeefu, i);

			if (usslp_bloom_add(&b, k)) {
				/* A false positive on a first sighting. Harmless — one
				 * redundant path through the gossip graph does not happen —
				 * but it should be rare at this sizing. */
				suppressed_early++;
			}
			CHECK(usslp_bloom_maybe_contains(&b, k));
			CHECK(usslp_bloom_add(&b, k)); /* the re-broadcast is suppressed */
		}
		CHECK(suppressed_early <= 3);
		for (uint16_t i = 0; i < 200; i++) {
			CHECK(usslp_bloom_maybe_contains(&b, usslp_bloom_chunk_key(0xdeadbeefu, i)));
		}
		CHECK_EQ_I(b.inserted, 200 - suppressed_early);
	}

	TEST("a filter left over from a previous rollout cannot suppress this one");
	{
		unsigned false_positives = 0;

		for (uint16_t i = 0; i < 200; i++) {
			if (usslp_bloom_maybe_contains(&b, usslp_bloom_chunk_key(0xcafef00du, i))) {
				false_positives++;
			}
		}
		/* Some collisions are expected and harmless; a wholesale suppression is
		 * not. The design bound at 200 inserts, 1024 bits and 3 hashes is under
		 * one per cent. */
		CHECK(false_positives < 10);
	}

	TEST("the estimated false-positive rate tracks the observed fill");
	{
		uint32_t ppm_before, ppm_after;

		usslp_bloom_init(&b);
		ppm_before = usslp_bloom_fp_ppm(&b);
		CHECK_EQ_I(ppm_before, 0);
		for (uint16_t i = 0; i < 200; i++) {
			usslp_bloom_add(&b, usslp_bloom_chunk_key(1, i));
		}
		ppm_after = usslp_bloom_fp_ppm(&b);
		/* (1 - e^(-3*200/4096))^3 = 0.0025 */
		CHECK(ppm_after > 1200u && ppm_after < 5000u);
	}
}
