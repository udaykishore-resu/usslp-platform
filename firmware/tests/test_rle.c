/*
 * The UFB2 image codec against an image the Go encoder produced, plus the
 * malformed cases. The decoder is fed by the radio, so every one of those cases
 * is reachable from the air on a deployment that has not moved to end-to-end
 * attestation.
 */

#include "../src/display/usslp_rle.h"
#include "test_util.h"
#include "test_vectors.h"

void test_rle(void)
{
	uint8_t out[4096];
	uint16_t w, h;

	printf("test_rle\n");

	TEST("uvarint decoding matches encoding/binary.Uvarint");
	{
		uint64_t v;
		const uint8_t one[] = { 0x01 };
		const uint8_t x300[] = { 0xac, 0x02 };
		const uint8_t max64[] = { 0xff, 0xff, 0xff, 0xff, 0xff,
					  0xff, 0xff, 0xff, 0xff, 0x01 };
		const uint8_t overlong[] = { 0xff, 0xff, 0xff, 0xff, 0xff,
					     0xff, 0xff, 0xff, 0xff, 0x02 };
		const uint8_t truncated[] = { 0x80 };

		CHECK_EQ_I(usslp_uvarint(one, 1, &v), 1);
		CHECK_EQ_I(v, 1);
		CHECK_EQ_I(usslp_uvarint(x300, 2, &v), 2);
		CHECK_EQ_I(v, 300);
		CHECK_EQ_I(usslp_uvarint(max64, 10, &v), 10);
		CHECK(v == UINT64_MAX);
		CHECK_EQ_I(usslp_uvarint(overlong, 10, &v), 0);
		CHECK_EQ_I(usslp_uvarint(truncated, 1, &v), 0);
		CHECK_EQ_I(usslp_uvarint(one, 0, &v), 0);
	}

	TEST("an image encoded by edge/sec decodes pixel for pixel");
	CHECK_EQ_I(usslp_rle_dimensions(rle_vector, sizeof(rle_vector), &w, &h), USSLP_OK);
	CHECK_EQ_I(w, rle_w);
	CHECK_EQ_I(h, rle_h);
	CHECK_EQ_I(usslp_rle_decode(rle_vector, sizeof(rle_vector), out, sizeof(out), &w, &h),
		   USSLP_OK);
	CHECK_EQ_I((int)w * (int)h, (int)sizeof(rle_expect_pix));
	CHECK(memcmp(out, rle_expect_pix, sizeof(rle_expect_pix)) == 0);

	TEST("a bad magic is refused");
	{
		uint8_t bad[sizeof(rle_vector)];

		memcpy(bad, rle_vector, sizeof(bad));
		bad[2] = 'X';
		CHECK_EQ_I(usslp_rle_decode(bad, sizeof(bad), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("a window larger than the caller's buffer is refused, not overrun");
	CHECK_EQ_I(usslp_rle_decode(rle_vector, sizeof(rle_vector), out, 10, NULL, NULL),
		   USSLP_ERR_NOSPACE);

	TEST("a truncated stream is refused at every length");
	for (size_t n = 0; n < sizeof(rle_vector); n++) {
		CHECK(usslp_rle_decode(rle_vector, n, out, sizeof(out), NULL, NULL) != USSLP_OK);
	}

	TEST("trailing bytes after a complete image are refused");
	{
		uint8_t extra[sizeof(rle_vector) + 1];

		memcpy(extra, rle_vector, sizeof(rle_vector));
		extra[sizeof(rle_vector)] = 0x00;
		CHECK_EQ_I(usslp_rle_decode(extra, sizeof(extra), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("a run that overflows its row is refused rather than clamped");
	{
		/* 4x1 panel, one run of ink 0 claiming 16 pixels. */
		const uint8_t evil[] = { 'U', 'F', 'B', '2', 0, 4, 0, 1, 0x01, 0x0f };

		CHECK_EQ_I(usslp_rle_decode(evil, sizeof(evil), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("a row group that overruns the panel is refused");
	{
		/* 4x1 panel, row group claiming 9 repeats. */
		const uint8_t evil[] = { 'U', 'F', 'B', '2', 0, 4, 0, 1, 0x09, 0x03 };

		CHECK_EQ_I(usslp_rle_decode(evil, sizeof(evil), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("a zero row-repeat count is refused rather than looping forever");
	{
		const uint8_t evil[] = { 'U', 'F', 'B', '2', 0, 4, 0, 1, 0x00, 0x03 };

		CHECK_EQ_I(usslp_rle_decode(evil, sizeof(evil), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("an ink state the panel does not have is refused");
	{
		/* ink 7 does not exist: sec.inkCount is 7, so states are 0..6. */
		const uint8_t evil[] = { 'U', 'F', 'B', '2', 0, 4, 0, 1, 0x01, 0x73 };

		CHECK_EQ_I(usslp_rle_decode(evil, sizeof(evil), out, sizeof(out), NULL, NULL),
			   USSLP_ERR_MALFORMED);
	}

	TEST("a zero-area window is legal and produces nothing");
	{
		const uint8_t empty[] = { 'U', 'F', 'B', '2', 0, 0, 0, 0 };

		CHECK_EQ_I(usslp_rle_decode(empty, sizeof(empty), out, sizeof(out), &w, &h),
			   USSLP_OK);
		CHECK_EQ_I(w, 0);
		CHECK_EQ_I(h, 0);
	}

	TEST("a long run decodes to the right length");
	{
		/* 300x1 panel: one long run of ink 1 covering the row. */
		uint8_t wide[] = { 'U',  'F',  'B',  '2', 0x01, 0x2c, 0x00, 0x01,
				   0x01, 0x90, 0xac, 0x02 };

		CHECK_EQ_I(usslp_rle_decode(wide, sizeof(wide), out, sizeof(out), &w, &h),
			   USSLP_OK);
		CHECK_EQ_I(w, 300);
		for (unsigned i = 0; i < 300; i++) {
			CHECK_EQ_I(out[i], USSLP_INK_BLACK);
		}
	}
}
