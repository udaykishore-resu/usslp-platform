/*
 * usslp_wire.h - the Shelf Edge Controller to label air protocol.
 *
 * Byte-for-byte compatible with edge/labelsim/wire.go, which is the reference
 * implementation of the device side of this protocol. Frame types 1 (update),
 * 2 (ack) and 3 (telemetry) have exactly the layouts, field orders and
 * endianness of EncodeUpdate/EncodeAck/EncodeTelemetry, and tests/test_wire.c
 * decodes frames the Go encoder produced.
 *
 * Frame type 4, USSLP_FRAME_ATTESTED_UPDATE, carries the signed price tuple end
 * to end. See crypto/usslp_attest.h for why it exists — a compromised controller
 * is inside the trust boundary that frame type 1 depends on. It began as a
 * firmware-side extension and edge/sec and edge/labelsim have since implemented
 * it; interop was proved by compiling this decoder against the Go encoder and
 * round-tripping attest_vectors[1] with the digest matching byte for byte.
 *
 * Everything here decodes in place from a caller-owned buffer. There is no
 * allocation: the image field is a pointer into the received frame, and the
 * caller must not free that buffer until the framebuffer has been expanded.
 * That is not a micro-optimisation, it is the difference between one 5 KiB
 * receive buffer and two.
 */

#ifndef USSLP_WIRE_H
#define USSLP_WIRE_H

#include "../usslp_portable.h"
#include "../crypto/usslp_attest.h"

/* labelsim.WireVersion. A frame with an unknown version is discarded rather
 * than guessed at: misinterpreting a price frame is worse than missing one. */
#define USSLP_WIRE_VERSION 1

enum usslp_frame_type {
	USSLP_FRAME_UPDATE = 1,
	USSLP_FRAME_ACK = 2,
	USSLP_FRAME_TELEMETRY = 3,
	/* The end-to-end attested price update; see the file comment. */
	USSLP_FRAME_ATTESTED_UPDATE = 4,
};

/* labelsim update flag bits. */
#define USSLP_FLAG_REQUEST_PARTIAL (1u << 0)
#define USSLP_FLAG_LED_PULSE (1u << 1)

#define USSLP_UPDATE_HEADER_BYTES 33
#define USSLP_ACK_BYTES 20
#define USSLP_TELEMETRY_BYTES 24

struct usslp_update {
	int64_t sequence;
	int64_t price_minor;
	char currency[4]; /* three characters plus a NUL the wire does not carry */
	uint8_t flags;
	uint8_t template_code;
	uint32_t image_crc;
	uint16_t origin_x;
	uint16_t origin_y;
	const uint8_t *image; /* points into the caller's frame buffer */
	uint16_t image_len;
};

/*
 * Acknowledgement status.
 *
 * 0-2 are labelsim.AckStatus verbatim. 3 and 4 are additions, and the reason
 * they exist is that without them a label's *refusal to display* is
 * indistinguishable on the wire from a corrupted frame.
 *
 * That matters because the two have opposite runbooks. A bad frame is a radio
 * problem: check the link, check for interference, retry. An attestation refusal
 * is a compliance incident: a price that did not verify, which under
 * INTERFACE-CONTRACTS section 5 must be escalated. Reporting the second as the
 * first means either the controller infers compliance incidents from transport
 * errors — and raises false ones every time a frame is genuinely corrupted — or
 * it misses real ones.
 *
 * Backward compatibility: this is additive in a byte field with 251 unused
 * values, and labelsim.AckStatus.String maps everything it does not recognise to
 * "bad-frame". A controller that has not been updated therefore sees exactly
 * today's behaviour for these codes, and one that has been updated sees the
 * signal. The frame length does not change.
 */
enum usslp_ack_status {
	USSLP_ACK_APPLIED = 0,
	/* Not greater than the displayed sequence. The normal outcome of a
	 * duplicated mesh frame, and not an error. */
	USSLP_ACK_STALE_SEQUENCE = 1,
	/* The frame did not decode, or its image failed the reassembly checksum.
	 * A transport fault. */
	USSLP_ACK_BAD_FRAME = 2,
	/* The frame decoded and the label refused to display it because the
	 * attestation did not verify. The verdict is carried in the flags byte; see
	 * usslp_ack_flags below. This is a compliance incident. */
	USSLP_ACK_REFUSED_ATTESTATION = 3,
	/* The frame decoded and the label refused it because this build requires an
	 * end-to-end attestation and the controller sent an unattested type-1
	 * frame. A fleet configuration mismatch, not a compliance incident: the
	 * controller needs updating, and raising a compliance alert for every label
	 * in the zone would bury the real ones. */
	USSLP_ACK_REFUSED_UNATTESTED = 4,
};

/*
 * The ack flags byte (offset 13).
 *
 *   bit 0     partial refresh ran
 *   bit 1     a partial was requested and a full ran anyway (ghosting budget)
 *   bits 2-4  attestation verdict, as enum usslp_attest_verdict
 *   bits 5-7  reserved, sent as zero
 *
 * Bits 0 and 1 are labelsim's. The verdict occupies previously-unused bits of an
 * existing byte rather than extending the frame, so a decoder that reads only
 * bits 0 and 1 — which is what edge/labelsim does today — is unaffected.
 */
#define USSLP_ACK_FLAG_PARTIAL (1u << 0)
#define USSLP_ACK_FLAG_FORCED_FULL (1u << 1)
#define USSLP_ACK_VERDICT_SHIFT 2
#define USSLP_ACK_VERDICT_MASK 0x07u

struct usslp_ack {
	int64_t sequence;
	uint8_t status;
	uint16_t refresh_ms;
	bool partial;
	bool forced_full;
	/* enum usslp_attest_verdict. USSLP_ATTEST_OK on every status except
	 * USSLP_ACK_REFUSED_ATTESTATION. */
	uint8_t attest_verdict;
	uint16_t battery_mv;
	uint8_t battery_pct;
	int16_t temperature_centi_c;
};

struct usslp_telemetry {
	uint16_t battery_mv;
	uint8_t battery_pct;
	int16_t temperature_centi_c;
	uint8_t parent_lqi;
	int8_t parent_rssi;
	uint32_t refresh_count;
	uint32_t nfc_tap_count;
	uint32_t uptime_sec;
	bool tamper;
};

/*
 * The attested update, frame type 4.
 *
 * Layout (all integers big endian):
 *
 *   0    version (1)
 *   1    type (4)
 *   2    sequence            int64
 *  10    price_minor         int64
 *  18    currency            3 bytes
 *  21    flags               uint8
 *  22    template            uint8
 *  23    image_crc           uint32
 *  27    origin_x            uint16
 *  29    origin_y            uint16
 *  31    image_len           uint16
 *  33    effective_at        int64, seconds since the epoch, UTC
 *  41    alg                 uint8   (1 = Ed25519)
 *  42    kid                 28 bytes, not NUL terminated
 *  70    digest              32 bytes
 * 102    signature           64 bytes
 * 166    tenant_len          uint8
 * 167    store_len           uint8
 * 168    label_len           uint8
 * 169    sku_len             uint8
 * 170    promo_len           uint8
 * 171    identifiers, concatenated in that order, no separators
 *  ...   image
 *
 * The first 33 bytes are deliberately identical to a type 1 update, so a
 * controller can build one frame and truncate it for a legacy label, and so the
 * firmware's decoder shares its first half.
 */
#define USSLP_ATTESTED_HEADER_BYTES 171

struct usslp_attested_update {
	struct usslp_update update;
	struct usslp_attestation attestation;
	int64_t effective_at;
	/* Pointers into the frame, with explicit lengths, because the identifiers
	 * are not NUL terminated on the wire. usslp_attested_price_input copies
	 * them into a caller-provided scratch struct that is NUL terminated, which
	 * is what usslp_canon needs. */
	const uint8_t *tenant;
	uint8_t tenant_len;
	const uint8_t *store;
	uint8_t store_len;
	const uint8_t *label;
	uint8_t label_len;
	const uint8_t *sku;
	uint8_t sku_len;
	const uint8_t *promotion;
	uint8_t promotion_len;
};

/* Scratch space for turning the wire's length-prefixed identifiers into the
 * NUL-terminated ones the canonical encoder takes. Sized at the protocol's own
 * limit: five identifiers of at most 128 bytes. */
struct usslp_price_scratch {
	char tenant[USSLP_CANON_MAX_ID + 1];
	char store[USSLP_CANON_MAX_ID + 1];
	char label[USSLP_CANON_MAX_ID + 1];
	char sku[USSLP_CANON_MAX_ID + 1];
	char promotion[USSLP_CANON_MAX_ID + 1];
	char currency[4];
};

/* Reports the frame type without decoding, so a receiver can dispatch cheaply.
 * Returns false for a frame shorter than two bytes or with a version this
 * firmware does not speak. */
bool usslp_wire_kind(const uint8_t *buf, size_t len, uint8_t *type_out);

/* Decodes an update, verifying the image CRC exactly as labelsim.DecodeUpdate
 * does. USSLP_ERR_INTEGRITY means the reassembled image is corrupt. */
int usslp_wire_decode_update(const uint8_t *buf, size_t len, struct usslp_update *out);

int usslp_wire_decode_attested(const uint8_t *buf, size_t len, struct usslp_attested_update *out);

/* Fills a canonical-encoder input from a decoded attested frame, copying the
 * identifiers into scratch. Returns USSLP_ERR_MALFORMED if any identifier
 * contains a byte the platform's canon.ValidID forbids, because an identifier
 * carrying '/' or '#' is an attempt to escape an MQTT namespace and must not be
 * hashed as if it were legitimate. */
int usslp_attested_price_input(const struct usslp_attested_update *att,
			       struct usslp_price_scratch *scratch, struct usslp_price_input *out);

/* Encoders. They write to a caller buffer and return the number of bytes
 * written, or 0 if the buffer is too small. */
size_t usslp_wire_encode_ack(const struct usslp_ack *a, uint8_t *buf, size_t cap);
size_t usslp_wire_encode_telemetry(const struct usslp_telemetry *t, uint8_t *buf, size_t cap);

/* Encoders for the controller side of the protocol. The firmware needs them
 * because a mains-powered relay label re-frames traffic for its children, and
 * because the host tests round-trip every frame they decode. */
size_t usslp_wire_encode_update(const struct usslp_update *u, uint8_t *buf, size_t cap);

/*
 * Encodes an attested update, the exact inverse of usslp_wire_decode_attested.
 *
 * The identifier pointers in att alias the caller's storage and are not copied
 * until this call, so they must still be valid here. image_crc of 0 means
 * "compute it", matching usslp_wire_encode_update.
 *
 * Its existence matters beyond symmetry. Until there is an encoder on this side,
 * the only way to test the decoder is to hand-assemble a frame from offsets in a
 * test — and a test that hand-assembles the thing it is testing is a test that
 * can agree with itself while both are wrong. That is not hypothetical: the
 * first version of tests/test_wire.c advanced past the promotion field without
 * copying it, and passed for months because the vector it used had an empty
 * promotion.
 */
size_t usslp_wire_encode_attested_update(const struct usslp_attested_update *att, uint8_t *buf,
					 size_t cap);

/*
 * Fills a wire-shaped attested update from the canonical tuple, an attestation
 * and an image: the inverse of usslp_attested_price_input.
 *
 * The identifiers are validated with the same canon.ValidID rule the decoder
 * applies, so a frame this function agrees to build is a frame the decoder will
 * agree to parse. Pointers alias price's strings and the image; nothing is
 * copied until usslp_wire_encode_attested_update runs.
 */
int usslp_attested_from_price_input(const struct usslp_price_input *price,
				    const struct usslp_attestation *attestation, uint8_t flags,
				    uint8_t template_code, uint16_t origin_x, uint16_t origin_y,
				    const uint8_t *image, uint16_t image_len,
				    struct usslp_attested_update *out);

/* Decoders for the uplink frames, used by the relay build and by the tests. */
int usslp_wire_decode_ack(const uint8_t *buf, size_t len, struct usslp_ack *out);
int usslp_wire_decode_telemetry(const uint8_t *buf, size_t len, struct usslp_telemetry *out);

#endif /* USSLP_WIRE_H */
