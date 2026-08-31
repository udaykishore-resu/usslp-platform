/*
 * The monotonic sequence rule, and its survival across a reboot.
 */

#include "../src/app/usslp_seq.h"
#include "test_util.h"

void test_seq(void)
{
	struct usslp_seq_state s;
	uint8_t rec[USSLP_SEQ_RECORD_LEN];

	printf("test_seq\n");

	TEST("a fresh label accepts anything, including zero and a negative");
	usslp_seq_init(&s);
	CHECK(usslp_seq_never_displayed(&s));
	CHECK_EQ_I(usslp_seq_check(&s, 0), USSLP_SEQ_ACCEPT);
	CHECK_EQ_I(usslp_seq_check(&s, -5), USSLP_SEQ_ACCEPT);
	CHECK_EQ_I(usslp_seq_check(&s, INT64_MAX), USSLP_SEQ_ACCEPT);

	TEST("strictly greater is the rule: equal is stale");
	usslp_seq_init(&s);
	CHECK_EQ_I(usslp_seq_commit(&s, 10), USSLP_OK);
	CHECK_EQ_I(usslp_seq_check(&s, 11), USSLP_SEQ_ACCEPT);
	CHECK_EQ_I(usslp_seq_check(&s, 10), USSLP_SEQ_STALE);
	CHECK_EQ_I(usslp_seq_check(&s, 9), USSLP_SEQ_STALE);
	CHECK_EQ_I(usslp_seq_check(&s, INT64_MIN), USSLP_SEQ_STALE);

	TEST("a duplicated mesh frame is a no-op, not an error, and is counted");
	usslp_seq_init(&s);
	usslp_seq_commit(&s, 100);
	for (unsigned i = 0; i < 5; i++) {
		CHECK_EQ_I(usslp_seq_check(&s, 100), USSLP_SEQ_STALE);
	}
	CHECK_EQ_I(s.discarded, 5);
	CHECK_EQ_I(s.accepted, 1);
	CHECK_EQ_I(s.displayed, 100);

	TEST("checking never advances the sequence");
	usslp_seq_init(&s);
	usslp_seq_commit(&s, 7);
	CHECK_EQ_I(usslp_seq_check(&s, 8), USSLP_SEQ_ACCEPT);
	CHECK_EQ_I(s.displayed, 7);
	CHECK_EQ_I(usslp_seq_check(&s, 8), USSLP_SEQ_ACCEPT);
	CHECK_EQ_I(s.displayed, 7);

	TEST("commit refuses to move backwards even if the caller asks");
	usslp_seq_init(&s);
	usslp_seq_commit(&s, 50);
	CHECK_EQ_I(usslp_seq_commit(&s, 49), USSLP_ERR_STALE);
	CHECK_EQ_I(usslp_seq_commit(&s, 50), USSLP_ERR_STALE);
	CHECK_EQ_I(s.displayed, 50);
	CHECK_EQ_I(usslp_seq_commit(&s, 51), USSLP_OK);

	TEST("the record round-trips through NVS");
	usslp_seq_init(&s);
	usslp_seq_commit(&s, 1234567890123LL);
	usslp_seq_encode(&s, rec);
	{
		struct usslp_seq_state loaded;

		CHECK_EQ_I(usslp_seq_decode(&loaded, rec), USSLP_OK);
		CHECK_EQ_I(loaded.displayed, 1234567890123LL);
		CHECK(!usslp_seq_never_displayed(&loaded));
		/* And the reboot does what it exists to do: the retained price that a
		 * broker replays after a power cut is discarded. */
		CHECK_EQ_I(usslp_seq_check(&loaded, 1234567890123LL), USSLP_SEQ_STALE);
		CHECK_EQ_I(usslp_seq_check(&loaded, 1234567890124LL), USSLP_SEQ_ACCEPT);
	}

	TEST("negative and extreme sequences survive the record");
	{
		const int64_t values[] = { 0, -1, INT64_MIN + 1, INT64_MAX, -9007199254740993LL };

		for (unsigned i = 0; i < sizeof(values) / sizeof(values[0]); i++) {
			struct usslp_seq_state a, b;

			usslp_seq_init(&a);
			usslp_seq_commit(&a, values[i]);
			usslp_seq_encode(&a, rec);
			CHECK_EQ_I(usslp_seq_decode(&b, rec), USSLP_OK);
			CHECK_EQ_I(b.displayed, values[i]);
		}
	}

	TEST("a corrupted record reads as never-displayed, not as sequence zero");
	/* This is the safe direction. A label that read a corrupt record as
	 * sequence zero would reject every update whose sequence was negative, and
	 * a label that trusted it would accept a rollback. Treating it as absent
	 * means the next update is taken and the record is rewritten. */
	usslp_seq_init(&s);
	usslp_seq_commit(&s, 999);
	usslp_seq_encode(&s, rec);
	for (unsigned i = 0; i < USSLP_SEQ_RECORD_LEN; i++) {
		struct usslp_seq_state loaded;
		uint8_t broken[USSLP_SEQ_RECORD_LEN];

		memcpy(broken, rec, sizeof(broken));
		broken[i] ^= 0x40u;
		CHECK_EQ_I(usslp_seq_decode(&loaded, broken), USSLP_ERR_MALFORMED);
		CHECK(usslp_seq_never_displayed(&loaded));
	}

	TEST("an erased NVS page reads as never-displayed");
	{
		struct usslp_seq_state loaded;
		uint8_t erased[USSLP_SEQ_RECORD_LEN];

		memset(erased, 0xff, sizeof(erased));
		CHECK_EQ_I(usslp_seq_decode(&loaded, erased), USSLP_ERR_MALFORMED);
		CHECK(usslp_seq_never_displayed(&loaded));
		memset(erased, 0x00, sizeof(erased));
		CHECK_EQ_I(usslp_seq_decode(&loaded, erased), USSLP_ERR_MALFORMED);
		CHECK(usslp_seq_never_displayed(&loaded));
	}
}
