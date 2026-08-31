/*
 * The air protocol against frames the Go encoder produced.
 *
 * The frames in vectors.h come from labelsim.EncodeUpdate/EncodeAck/
 * EncodeTelemetry. If this suite passes, a label built from this firmware and a
 * controller built from edge/sec agree about what a price update looks like on
 * the air, which is the second thing after the attestation digest that would be
 * unrecoverable to get wrong.
 */

#include "../src/radio/usslp_wire.h"
#include "../src/usslp_crc32c.h"
#include "test_util.h"
#include "test_vectors.h"

void test_wire(void)
{
	struct usslp_update u;
	struct usslp_ack a;
	struct usslp_telemetry t;
	uint8_t buf[512];
	uint8_t type;

	printf("test_wire\n");

	TEST("CRC-32C matches hash/crc32 with the Castagnoli table");
	CHECK_EQ_I(usslp_crc32c("", 0), 0u);
	CHECK_EQ_I(usslp_crc32c("123456789", 9), 0xe3069283u);
	CHECK_EQ_I(usslp_crc32c(rle_vector, sizeof(rle_vector)), wire_update_crc);
	{
		/* Incremental must equal one-shot: a mesh payload arrives in fragments. */
		uint32_t crc = usslp_crc32c_update(0, rle_vector, 7);

		crc = usslp_crc32c_update(crc, rle_vector + 7, sizeof(rle_vector) - 7);
		CHECK_EQ_I(crc, wire_update_crc);
	}

	TEST("dispatch on the type byte");
	CHECK(usslp_wire_kind(wire_update, sizeof(wire_update), &type));
	CHECK_EQ_I(type, USSLP_FRAME_UPDATE);
	CHECK(usslp_wire_kind(wire_ack, sizeof(wire_ack), &type));
	CHECK_EQ_I(type, USSLP_FRAME_ACK);
	CHECK(usslp_wire_kind(wire_telemetry, sizeof(wire_telemetry), &type));
	CHECK_EQ_I(type, USSLP_FRAME_TELEMETRY);
	{
		/* A frame from a protocol version this label does not speak is not
		 * guessed at. */
		uint8_t alien[4] = { 99, 1, 0, 0 };

		CHECK(!usslp_wire_kind(alien, sizeof(alien), &type));
		CHECK(!usslp_wire_kind(alien, 1, &type));
	}

	TEST("decoding an update the Go encoder produced");
	CHECK_EQ_I(usslp_wire_decode_update(wire_update, sizeof(wire_update), &u), USSLP_OK);
	CHECK_EQ_I(u.sequence, 42);
	CHECK_EQ_I(u.price_minor, 249);
	CHECK_EQ_S(u.currency, "GBP");
	CHECK_EQ_I(u.flags, USSLP_FLAG_REQUEST_PARTIAL);
	CHECK_EQ_I(u.template_code, 1);
	CHECK_EQ_I(u.image_crc, wire_update_crc);
	CHECK_EQ_I(u.origin_x, 16);
	CHECK_EQ_I(u.origin_y, 32);
	CHECK_EQ_I(u.image_len, sizeof(rle_vector));
	CHECK(memcmp(u.image, rle_vector, sizeof(rle_vector)) == 0);

	TEST("re-encoding produces the same bytes");
	{
		size_t n = usslp_wire_encode_update(&u, buf, sizeof(buf));

		CHECK_EQ_I(n, sizeof(wire_update));
		CHECK(memcmp(buf, wire_update, sizeof(wire_update)) == 0);
	}

	TEST("a corrupted image is refused by the reassembly checksum");
	{
		uint8_t broken[sizeof(wire_update)];

		memcpy(broken, wire_update, sizeof(broken));
		broken[USSLP_UPDATE_HEADER_BYTES + 4] ^= 0x10u;
		CHECK_EQ_I(usslp_wire_decode_update(broken, sizeof(broken), &u),
			   USSLP_ERR_INTEGRITY);
	}

	TEST("a truncated frame is refused at every length");
	for (size_t n = 0; n < sizeof(wire_update); n++) {
		CHECK(usslp_wire_decode_update(wire_update, n, &u) != USSLP_OK);
	}

	TEST("a frame claiming more image than it carries is refused");
	{
		uint8_t lying[sizeof(wire_update)];

		memcpy(lying, wire_update, sizeof(lying));
		lying[31] = 0xff;
		lying[32] = 0xff;
		CHECK_EQ_I(usslp_wire_decode_update(lying, sizeof(lying), &u),
			   USSLP_ERR_MALFORMED);
	}

	TEST("acknowledgement encoding matches labelsim.EncodeAck");
	{
		struct usslp_ack want = {
			.sequence = 42,
			.status = USSLP_ACK_APPLIED,
			.refresh_ms = 300,
			.partial = true,
			.forced_full = false,
			.battery_mv = 2987,
			.battery_pct = 93,
			.temperature_centi_c = -1850,
		};
		size_t n = usslp_wire_encode_ack(&want, buf, sizeof(buf));

		CHECK_EQ_I(n, sizeof(wire_ack));
		CHECK(memcmp(buf, wire_ack, sizeof(wire_ack)) == 0);

		CHECK_EQ_I(usslp_wire_decode_ack(wire_ack, sizeof(wire_ack), &a), USSLP_OK);
		CHECK_EQ_I(a.sequence, 42);
		CHECK_EQ_I(a.status, USSLP_ACK_APPLIED);
		CHECK_EQ_I(a.refresh_ms, 300);
		CHECK(a.partial);
		CHECK(!a.forced_full);
		CHECK_EQ_I(a.battery_mv, 2987);
		CHECK_EQ_I(a.battery_pct, 93);
		/* A chiller label reports well below zero, and the field is signed. */
		CHECK_EQ_I(a.temperature_centi_c, -1850);
	}

	TEST("telemetry encoding matches labelsim.EncodeTelemetry");
	{
		struct usslp_telemetry want = {
			.battery_mv = 2987,
			.battery_pct = 93,
			.temperature_centi_c = 2150,
			.parent_lqi = 187,
			.parent_rssi = -62,
			.refresh_count = 1234,
			.nfc_tap_count = 7,
			.uptime_sec = 987654,
			.tamper = true,
		};
		size_t n = usslp_wire_encode_telemetry(&want, buf, sizeof(buf));

		CHECK_EQ_I(n, sizeof(wire_telemetry));
		CHECK(memcmp(buf, wire_telemetry, sizeof(wire_telemetry)) == 0);

		CHECK_EQ_I(usslp_wire_decode_telemetry(wire_telemetry, sizeof(wire_telemetry), &t),
			   USSLP_OK);
		CHECK_EQ_I(t.parent_rssi, -62);
		CHECK_EQ_I(t.refresh_count, 1234);
		CHECK_EQ_I(t.uptime_sec, 987654);
		CHECK(t.tamper);
	}

	TEST("every attestation vector round-trips through the type-4 frame");
	/*
	 * Over *every* vector, not just the first.
	 *
	 * The earlier version of this test hand-assembled a frame from byte offsets
	 * and ran it over one vector. It advanced past the promotion field without
	 * copying it, and passed anyway — because the vector it used has an empty
	 * promotion, so the nine bytes it failed to write were nine bytes that were
	 * supposed to be absent. The bug was invisible for exactly as long as the
	 * test's coverage was one vector wide.
	 *
	 * Two changes fix that class of gap rather than that instance of it. The
	 * frame is now built by the firmware's own encoder, so there is no offset
	 * arithmetic in this file to get wrong; and the loop covers all six vectors,
	 * including the one with a promotion, the one with a UTF-8 SKU, the one with
	 * a pre-epoch timestamp and the one at INT64-scale sequence.
	 */
	for (unsigned i = 0; i < USSLP_TEST_ATTEST_VECTORS; i++) {
		const struct attest_vector *v = &attest_vectors[i];
		struct usslp_price_input built;
		struct usslp_attestation attn;
		struct usslp_attested_update out;
		struct usslp_attested_update parsed;
		struct usslp_price_scratch scratch;
		struct usslp_price_input in;
		uint8_t frame[512];
		uint8_t reframe[512];
		uint8_t digest[32];
		char hex[65];
		size_t n;

		built.tenant = v->tenant;
		built.store = v->store;
		built.label = v->label;
		built.sku = v->sku;
		built.amount_minor = v->amount_minor;
		built.currency = v->currency;
		built.effective_at = v->effective_at_unix;
		built.sequence = v->sequence;
		built.promotion = v->promotion;

		memset(&attn, 0, sizeof(attn));
		attn.alg = USSLP_ATTEST_ALG_ED25519;
		memcpy(attn.kid, kid_a, USSLP_KID_BUF);
		usslp_test_unhex(v->digest_hex, attn.digest, 32);
		memcpy(attn.sig, sig_v0_by_a, 64);

		CHECK_EQ_I(usslp_attested_from_price_input(&built, &attn,
							   USSLP_FLAG_REQUEST_PARTIAL, 1, 16, 32,
							   rle_vector, sizeof(rle_vector), &out),
			   USSLP_OK);
		n = usslp_wire_encode_attested_update(&out, frame, sizeof(frame));
		CHECK(n > USSLP_ATTESTED_HEADER_BYTES);

		CHECK_EQ_I(usslp_wire_decode_attested(frame, n, &parsed), USSLP_OK);
		CHECK_EQ_I(parsed.update.sequence, v->sequence);
		CHECK_EQ_I(parsed.update.price_minor, v->amount_minor);
		CHECK_EQ_I(parsed.effective_at, v->effective_at_unix);
		CHECK_EQ_I(parsed.update.origin_x, 16);
		CHECK_EQ_I(parsed.update.origin_y, 32);
		CHECK_EQ_I(parsed.update.image_len, sizeof(rle_vector));
		CHECK_EQ_S(parsed.attestation.kid, kid_a);
		CHECK(memcmp(parsed.attestation.digest, attn.digest, 32) == 0);
		CHECK(memcmp(parsed.attestation.sig, sig_v0_by_a, 64) == 0);

		/* Each identifier arrives with its own bytes, at its own length. This
		 * is the assertion the old test could not make, because it never wrote
		 * the promotion in the first place. */
		CHECK_EQ_I(parsed.tenant_len, strlen(v->tenant));
		CHECK_EQ_I(parsed.store_len, strlen(v->store));
		CHECK_EQ_I(parsed.label_len, strlen(v->label));
		CHECK_EQ_I(parsed.sku_len, strlen(v->sku));
		CHECK_EQ_I(parsed.promotion_len, strlen(v->promotion));
		CHECK(memcmp(parsed.tenant, v->tenant, strlen(v->tenant)) == 0);
		CHECK(memcmp(parsed.sku, v->sku, strlen(v->sku)) == 0);
		CHECK(memcmp(parsed.promotion, v->promotion, strlen(v->promotion)) == 0);

		CHECK_EQ_I(usslp_attested_price_input(&parsed, &scratch, &in), USSLP_OK);
		CHECK_EQ_S(in.tenant, v->tenant);
		CHECK_EQ_S(in.store, v->store);
		CHECK_EQ_S(in.label, v->label);
		CHECK_EQ_S(in.sku, v->sku);
		CHECK_EQ_S(in.promotion, v->promotion);
		CHECK_EQ_S(in.currency, v->currency);
		CHECK_EQ_I(in.amount_minor, v->amount_minor);
		CHECK_EQ_I(in.sequence, v->sequence);
		CHECK_EQ_I(in.effective_at, v->effective_at_unix);

		/* The whole point: the identifiers that crossed the air produce the
		 * digest the platform signed. */
		CHECK_EQ_I(usslp_canon_price_digest(&in, digest), USSLP_OK);
		usslp_test_hex(digest, 32, hex);
		CHECK_EQ_S(hex, v->digest_hex);

		/* Re-encoding what was decoded must be byte-identical, which is what
		 * makes a relay able to re-frame traffic for its children without
		 * invalidating an attestation it cannot recompute. */
		CHECK_EQ_I(usslp_wire_encode_attested_update(&parsed, reframe, sizeof(reframe)),
			   n);
		CHECK(memcmp(frame, reframe, n) == 0);

		/* Truncation at every length, for every vector: the identifier block is
		 * variable length, so the boundary the decoder has to get right moves
		 * from vector to vector. */
		for (size_t trunc = 0; trunc < n; trunc++) {
			CHECK(usslp_wire_decode_attested(frame, trunc, &parsed) != USSLP_OK);
		}
	}

	TEST("a frame carrying a real signature verifies end to end from its bytes");
	{
		/*
		 * The strongest statement this suite can make: from the encoder, through
		 * the wire, through the decoder, through canonicalisation, to a
		 * signature Go actually produced. Vector 0 was signed by key A and
		 * vector 1 by key B, so both are checked and neither passes under the
		 * other's key.
		 */
		struct usslp_keyring ring;
		const struct {
			unsigned vector;
			const char *kid;
			const uint8_t *sig;
		} cases[] = {
			{ 0, kid_a, sig_v0_by_a },
			{ 1, kid_b, sig_v1_by_b },
		};

		usslp_keyring_init(&ring);
		CHECK_EQ_I(usslp_keyring_add(&ring, kid_a, pub_a, 0, 0, USSLP_KEY_ACTIVE),
			   USSLP_OK);
		CHECK_EQ_I(usslp_keyring_add(&ring, kid_b, pub_b, 0, 0, USSLP_KEY_ACTIVE),
			   USSLP_OK);

		for (unsigned c = 0; c < 2; c++) {
			const struct attest_vector *v = &attest_vectors[cases[c].vector];
			struct usslp_price_input built, in;
			struct usslp_attestation attn;
			struct usslp_attested_update out, parsed;
			struct usslp_price_scratch scratch;
			uint8_t frame[512];
			size_t n;

			built.tenant = v->tenant;
			built.store = v->store;
			built.label = v->label;
			built.sku = v->sku;
			built.amount_minor = v->amount_minor;
			built.currency = v->currency;
			built.effective_at = v->effective_at_unix;
			built.sequence = v->sequence;
			built.promotion = v->promotion;

			memset(&attn, 0, sizeof(attn));
			attn.alg = USSLP_ATTEST_ALG_ED25519;
			memcpy(attn.kid, cases[c].kid, USSLP_KID_BUF);
			usslp_test_unhex(v->digest_hex, attn.digest, 32);
			memcpy(attn.sig, cases[c].sig, 64);

			CHECK_EQ_I(usslp_attested_from_price_input(&built, &attn, 0, 0, 0, 0,
								   rle_vector,
								   sizeof(rle_vector), &out),
				   USSLP_OK);
			n = usslp_wire_encode_attested_update(&out, frame, sizeof(frame));
			CHECK(n > 0);
			CHECK_EQ_I(usslp_wire_decode_attested(frame, n, &parsed), USSLP_OK);
			CHECK_EQ_I(usslp_attested_price_input(&parsed, &scratch, &in), USSLP_OK);
			CHECK_EQ_I(usslp_attest_verify(&ring, &in, &parsed.attestation, 0),
				   USSLP_ATTEST_OK);

			/* And a single flipped bit anywhere in the price fields of the
			 * frame breaks it. Byte 10 is the top of price_minor. */
			{
				struct usslp_attested_update broken;
				struct usslp_price_input bad_in;
				uint8_t evil[512];

				memcpy(evil, frame, n);
				evil[17] ^= 0x01u; /* the low byte of price_minor */
				CHECK_EQ_I(usslp_wire_decode_attested(evil, n, &broken),
					   USSLP_OK);
				CHECK_EQ_I(usslp_attested_price_input(&broken, &scratch,
								      &bad_in),
					   USSLP_OK);
				CHECK_EQ_I(usslp_attest_verify(&ring, &bad_in,
							       &broken.attestation, 0),
					   USSLP_ATTEST_DIGEST_MISMATCH);
			}
		}
	}

	TEST("the encoder refuses what the decoder would refuse");
	{
		struct usslp_price_input bad;
		struct usslp_attestation attn;
		struct usslp_attested_update out;

		memset(&attn, 0, sizeof(attn));
		attn.alg = USSLP_ATTEST_ALG_ED25519;
		memcpy(attn.kid, kid_a, USSLP_KID_BUF);

		bad.tenant = "acme";
		bad.store = "store-001";
		bad.label = "lbl-0001";
		bad.sku = "SKU-1";
		bad.amount_minor = 249;
		bad.currency = "GBP";
		bad.effective_at = 0;
		bad.sequence = 1;
		bad.promotion = "";
		CHECK_EQ_I(usslp_attested_from_price_input(&bad, &attn, 0, 0, 0, 0, rle_vector,
							   sizeof(rle_vector), &out),
			   USSLP_OK);

		/* An identifier that would let a tenant address outside its own MQTT
		 * namespace must not be encodable, not merely undecodable: a relay that
		 * built such a frame would be manufacturing the attack. */
		bad.store = "store/../other";
		CHECK_EQ_I(usslp_attested_from_price_input(&bad, &attn, 0, 0, 0, 0, rle_vector,
							   sizeof(rle_vector), &out),
			   USSLP_ERR_MALFORMED);
		bad.store = "store-001";

		/* An empty identifier, which canon.ValidID forbids everywhere except the
		 * promotion. */
		bad.label = "";
		CHECK_EQ_I(usslp_attested_from_price_input(&bad, &attn, 0, 0, 0, 0, rle_vector,
							   sizeof(rle_vector), &out),
			   USSLP_ERR_MALFORMED);
		bad.label = "lbl-0001";

		/* A lower-case currency would hash differently from the platform's
		 * upper-cased one. */
		bad.currency = "gbp";
		CHECK_EQ_I(usslp_attested_from_price_input(&bad, &attn, 0, 0, 0, 0, rle_vector,
							   sizeof(rle_vector), &out),
			   USSLP_ERR_MALFORMED);
		bad.currency = "GBP";

		/* Over-long: canon.ValidID caps an identifier at 128 bytes. */
		{
			static char longid[USSLP_CANON_MAX_ID + 8];

			memset(longid, 'x', sizeof(longid) - 1);
			longid[sizeof(longid) - 1] = '\0';
			bad.sku = longid;
			CHECK_EQ_I(usslp_attested_from_price_input(&bad, &attn, 0, 0, 0, 0,
								   rle_vector,
								   sizeof(rle_vector), &out),
				   USSLP_ERR_MALFORMED);
		}
	}

	TEST("an identifier carrying an MQTT separator is refused on decode");
	{
		struct usslp_attested_update evil;
		struct usslp_price_scratch scratch;
		struct usslp_price_input in;
		static const uint8_t escape[] = "acme/+/#";

		memset(&evil, 0, sizeof(evil));
		memcpy(evil.update.currency, "GBP", 4);
		evil.tenant = escape;
		evil.tenant_len = 8;
		evil.store = (const uint8_t *)"s";
		evil.store_len = 1;
		evil.label = (const uint8_t *)"l";
		evil.label_len = 1;
		evil.sku = (const uint8_t *)"k";
		evil.sku_len = 1;
		evil.promotion = (const uint8_t *)"";
		evil.promotion_len = 0;
		CHECK_EQ_I(usslp_attested_price_input(&evil, &scratch, &in), USSLP_ERR_MALFORMED);
	}

	TEST("the ack carries a refusal reason distinct from a transport error");
	{
		/*
		 * The verdict rides in previously unused bits of the existing flags
		 * byte, so the frame stays 20 bytes and a decoder that reads only bits 0
		 * and 1 — which is what edge/labelsim does today — is unaffected. That
		 * backward compatibility is asserted here rather than assumed.
		 */
		struct usslp_ack refusal = {
			.sequence = 4242,
			.status = USSLP_ACK_REFUSED_ATTESTATION,
			.refresh_ms = 0,
			.partial = false,
			.forced_full = false,
			.attest_verdict = USSLP_ATTEST_DIGEST_MISMATCH,
			.battery_mv = 2900,
			.battery_pct = 88,
			.temperature_centi_c = 450,
		};
		struct usslp_ack back;
		uint8_t enc[USSLP_ACK_BYTES];

		CHECK_EQ_I(usslp_wire_encode_ack(&refusal, enc, sizeof(enc)), USSLP_ACK_BYTES);
		CHECK_EQ_I(usslp_wire_decode_ack(enc, sizeof(enc), &back), USSLP_OK);
		CHECK_EQ_I(back.status, USSLP_ACK_REFUSED_ATTESTATION);
		CHECK_EQ_I(back.attest_verdict, USSLP_ATTEST_DIGEST_MISMATCH);
		CHECK_EQ_I(back.sequence, 4242);
		/* Bits 0 and 1 still mean exactly what they meant. */
		CHECK(!back.partial);
		CHECK(!back.forced_full);
		CHECK_EQ_I(enc[13] & 0x03u, 0);

		/* Every verdict survives the three-bit field. */
		for (unsigned vd = USSLP_ATTEST_OK; vd <= USSLP_ATTEST_UNAVAILABLE; vd++) {
			struct usslp_ack each = refusal;

			each.attest_verdict = (uint8_t)vd;
			each.partial = true;
			each.forced_full = true;
			CHECK_EQ_I(usslp_wire_encode_ack(&each, enc, sizeof(enc)),
				   USSLP_ACK_BYTES);
			CHECK_EQ_I(usslp_wire_decode_ack(enc, sizeof(enc), &back), USSLP_OK);
			CHECK_EQ_I(back.attest_verdict, vd);
			CHECK(back.partial);
			CHECK(back.forced_full);
		}

		/* An applied ack still encodes byte-identically to what Go produced, so
		 * the addition has not disturbed the existing wire. */
		{
			struct usslp_ack applied = {
				.sequence = 42,
				.status = USSLP_ACK_APPLIED,
				.refresh_ms = 300,
				.partial = true,
				.forced_full = false,
				.attest_verdict = USSLP_ATTEST_OK,
				.battery_mv = 2987,
				.battery_pct = 93,
				.temperature_centi_c = -1850,
			};

			CHECK_EQ_I(usslp_wire_encode_ack(&applied, enc, sizeof(enc)),
				   sizeof(wire_ack));
			CHECK(memcmp(enc, wire_ack, sizeof(wire_ack)) == 0);
		}

		/* The unattested refusal is its own status: a fleet configuration
		 * mismatch, not a compliance incident. */
		{
			struct usslp_ack unatt = refusal;

			unatt.status = USSLP_ACK_REFUSED_UNATTESTED;
			unatt.attest_verdict = USSLP_ATTEST_OK;
			CHECK_EQ_I(usslp_wire_encode_ack(&unatt, enc, sizeof(enc)),
				   USSLP_ACK_BYTES);
			CHECK_EQ_I(usslp_wire_decode_ack(enc, sizeof(enc), &back), USSLP_OK);
			CHECK_EQ_I(back.status, USSLP_ACK_REFUSED_UNATTESTED);
			CHECK(back.status != USSLP_ACK_BAD_FRAME);
			CHECK(back.status != USSLP_ACK_REFUSED_ATTESTATION);
		}
	}
}
