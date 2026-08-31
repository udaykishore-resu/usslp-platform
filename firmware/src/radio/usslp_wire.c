#include "usslp_wire.h"

#include "../usslp_crc32c.h"

#include <string.h>

/*
 * The attestation verdict travels in three bits of the ack's flags byte. If a
 * ninth verdict is ever added, it would silently truncate to one of the existing
 * eight and the controller would be told the wrong reason a price was refused —
 * so the build breaks here instead.
 */
_Static_assert(USSLP_ATTEST_UNAVAILABLE <= USSLP_ACK_VERDICT_MASK,
	       "an attestation verdict no longer fits the three bits the ack flags reserve "
	       "for it; widen the field and update edge/labelsim to match");

static uint16_t rd16(const uint8_t *p)
{
	return (uint16_t)(((uint16_t)p[0] << 8) | (uint16_t)p[1]);
}

static uint32_t rd32(const uint8_t *p)
{
	return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) | ((uint32_t)p[2] << 8) |
	       (uint32_t)p[3];
}

static uint64_t rd64(const uint8_t *p)
{
	return ((uint64_t)rd32(p) << 32) | (uint64_t)rd32(p + 4);
}

static void wr16(uint8_t *p, uint16_t v)
{
	p[0] = (uint8_t)(v >> 8);
	p[1] = (uint8_t)v;
}

static void wr32(uint8_t *p, uint32_t v)
{
	p[0] = (uint8_t)(v >> 24);
	p[1] = (uint8_t)(v >> 16);
	p[2] = (uint8_t)(v >> 8);
	p[3] = (uint8_t)v;
}

static void wr64(uint8_t *p, uint64_t v)
{
	wr32(p, (uint32_t)(v >> 32));
	wr32(p + 4, (uint32_t)v);
}

bool usslp_wire_kind(const uint8_t *buf, size_t len, uint8_t *type_out)
{
	if (buf == NULL || len < 2u || buf[0] != USSLP_WIRE_VERSION) {
		return false;
	}
	if (type_out != NULL) {
		*type_out = buf[1];
	}
	return true;
}

/* Shared by the type 1 and type 4 decoders: the first 33 bytes are the same. */
static int decode_update_head(const uint8_t *buf, size_t len, uint8_t want_type,
			      struct usslp_update *out)
{
	if (len < USSLP_UPDATE_HEADER_BYTES) {
		return USSLP_ERR_MALFORMED;
	}
	if (buf[0] != USSLP_WIRE_VERSION || buf[1] != want_type) {
		return USSLP_ERR_MALFORMED;
	}
	out->sequence = (int64_t)rd64(&buf[2]);
	out->price_minor = (int64_t)rd64(&buf[10]);
	out->currency[0] = (char)buf[18];
	out->currency[1] = (char)buf[19];
	out->currency[2] = (char)buf[20];
	out->currency[3] = '\0';
	out->flags = buf[21];
	out->template_code = buf[22];
	out->image_crc = rd32(&buf[23]);
	out->origin_x = rd16(&buf[27]);
	out->origin_y = rd16(&buf[29]);
	out->image_len = rd16(&buf[31]);
	out->image = NULL;
	return USSLP_OK;
}

int usslp_wire_decode_update(const uint8_t *buf, size_t len, struct usslp_update *out)
{
	int rc;

	if (buf == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	memset(out, 0, sizeof(*out));
	rc = decode_update_head(buf, len, USSLP_FRAME_UPDATE, out);
	if (rc != USSLP_OK) {
		return rc;
	}
	if (len < (size_t)USSLP_UPDATE_HEADER_BYTES + (size_t)out->image_len) {
		return USSLP_ERR_MALFORMED;
	}
	out->image = &buf[USSLP_UPDATE_HEADER_BYTES];
	if (usslp_crc32c(out->image, out->image_len) != out->image_crc) {
		/* The mesh has its own frame CRC; this one covers reassembly across
		 * fragments, which is where a lost-and-retried fragment can leave a
		 * plausible-looking but wrong image. */
		return USSLP_ERR_INTEGRITY;
	}
	return USSLP_OK;
}

/*
 * Writes the fixed 33-byte head shared by the type 1 and type 4 updates, so
 * that the two encoders cannot drift apart in the way the two decoders cannot
 * either. Returns the CRC actually written, which the caller needs when it was
 * asked to compute one.
 */
static uint32_t encode_update_head(const struct usslp_update *u, uint8_t *buf, uint8_t type)
{
	uint32_t crc = u->image_crc;

	buf[0] = USSLP_WIRE_VERSION;
	buf[1] = type;
	wr64(&buf[2], (uint64_t)u->sequence);
	wr64(&buf[10], (uint64_t)u->price_minor);
	buf[18] = (uint8_t)u->currency[0];
	buf[19] = (uint8_t)u->currency[1];
	buf[20] = (uint8_t)u->currency[2];
	buf[21] = u->flags;
	buf[22] = u->template_code;
	if (crc == 0u) {
		crc = usslp_crc32c(u->image, u->image_len);
	}
	wr32(&buf[23], crc);
	wr16(&buf[27], u->origin_x);
	wr16(&buf[29], u->origin_y);
	wr16(&buf[31], u->image_len);
	return crc;
}

size_t usslp_wire_encode_update(const struct usslp_update *u, uint8_t *buf, size_t cap)
{
	size_t total;

	if (u == NULL || buf == NULL) {
		return 0;
	}
	total = (size_t)USSLP_UPDATE_HEADER_BYTES + (size_t)u->image_len;
	if (cap < total) {
		return 0;
	}
	if (u->currency[0] == '\0' || u->currency[1] == '\0' || u->currency[2] == '\0') {
		return 0;
	}
	(void)encode_update_head(u, buf, USSLP_FRAME_UPDATE);
	if (u->image_len > 0u && u->image != NULL) {
		memcpy(&buf[USSLP_UPDATE_HEADER_BYTES], u->image, u->image_len);
	}
	return total;
}

int usslp_wire_decode_attested(const uint8_t *buf, size_t len, struct usslp_attested_update *out)
{
	size_t idlen, need, off;
	int rc;

	if (buf == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	memset(out, 0, sizeof(*out));
	if (len < USSLP_ATTESTED_HEADER_BYTES) {
		return USSLP_ERR_MALFORMED;
	}
	rc = decode_update_head(buf, len, USSLP_FRAME_ATTESTED_UPDATE, &out->update);
	if (rc != USSLP_OK) {
		return rc;
	}
	out->effective_at = (int64_t)rd64(&buf[33]);
	out->attestation.alg = buf[41];
	memcpy(out->attestation.kid, &buf[42], USSLP_KID_LEN);
	out->attestation.kid[USSLP_KID_LEN] = '\0';
	memcpy(out->attestation.digest, &buf[70], USSLP_SHA256_DIGEST_LEN);
	memcpy(out->attestation.sig, &buf[102], USSLP_ED25519_SIGNATURE_LEN);

	out->tenant_len = buf[166];
	out->store_len = buf[167];
	out->label_len = buf[168];
	out->sku_len = buf[169];
	out->promotion_len = buf[170];
	idlen = (size_t)out->tenant_len + out->store_len + out->label_len + out->sku_len +
		out->promotion_len;
	need = (size_t)USSLP_ATTESTED_HEADER_BYTES + idlen + (size_t)out->update.image_len;
	if (len < need) {
		return USSLP_ERR_MALFORMED;
	}
	off = USSLP_ATTESTED_HEADER_BYTES;
	out->tenant = &buf[off];
	off += out->tenant_len;
	out->store = &buf[off];
	off += out->store_len;
	out->label = &buf[off];
	off += out->label_len;
	out->sku = &buf[off];
	off += out->sku_len;
	out->promotion = &buf[off];
	off += out->promotion_len;
	out->update.image = &buf[off];
	if (usslp_crc32c(out->update.image, out->update.image_len) != out->update.image_crc) {
		return USSLP_ERR_INTEGRITY;
	}
	return USSLP_OK;
}

/* canon.ValidID: non-empty, at most 128 bytes, and free of the separators the
 * MQTT and Kafka namespaces use. An identifier carrying one of those is not a
 * typo, it is an attempt to address outside a tenant's namespace. */
static bool valid_id_bytes(const uint8_t *p, size_t n, bool allow_empty)
{
	if (n == 0u) {
		return allow_empty;
	}
	if (n > USSLP_CANON_MAX_ID) {
		return false;
	}
	for (size_t i = 0; i < n; i++) {
		switch (p[i]) {
		case '/':
		case '#':
		case '+':
		case ' ':
		case '\t':
		case '\n':
		case '\r':
		case '\0':
		case ':':
			return false;
		default:
			break;
		}
	}
	return true;
}

static void copy_id(char *dst, const uint8_t *src, size_t n)
{
	if (n > 0u) {
		memcpy(dst, src, n);
	}
	dst[n] = '\0';
}

int usslp_attested_price_input(const struct usslp_attested_update *att,
			       struct usslp_price_scratch *scratch, struct usslp_price_input *out)
{
	if (att == NULL || scratch == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (!valid_id_bytes(att->tenant, att->tenant_len, false) ||
	    !valid_id_bytes(att->store, att->store_len, false) ||
	    !valid_id_bytes(att->label, att->label_len, false) ||
	    !valid_id_bytes(att->sku, att->sku_len, false) ||
	    !valid_id_bytes(att->promotion, att->promotion_len, true)) {
		return USSLP_ERR_MALFORMED;
	}
	copy_id(scratch->tenant, att->tenant, att->tenant_len);
	copy_id(scratch->store, att->store, att->store_len);
	copy_id(scratch->label, att->label, att->label_len);
	copy_id(scratch->sku, att->sku, att->sku_len);
	copy_id(scratch->promotion, att->promotion, att->promotion_len);
	memcpy(scratch->currency, att->update.currency, sizeof(scratch->currency));

	out->tenant = scratch->tenant;
	out->store = scratch->store;
	out->label = scratch->label;
	out->sku = scratch->sku;
	out->promotion = scratch->promotion;
	out->currency = scratch->currency;
	out->amount_minor = att->update.price_minor;
	out->effective_at = att->effective_at;
	out->sequence = att->update.sequence;
	return USSLP_OK;
}

int usslp_attested_from_price_input(const struct usslp_price_input *price,
				    const struct usslp_attestation *attestation, uint8_t flags,
				    uint8_t template_code, uint16_t origin_x, uint16_t origin_y,
				    const uint8_t *image, uint16_t image_len,
				    struct usslp_attested_update *out)
{
	const char *ids[5];
	size_t lens[5];

	if (price == NULL || attestation == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (price->tenant == NULL || price->store == NULL || price->label == NULL ||
	    price->sku == NULL || price->promotion == NULL || price->currency == NULL) {
		return USSLP_ERR_INVAL;
	}
	ids[0] = price->tenant;
	ids[1] = price->store;
	ids[2] = price->label;
	ids[3] = price->sku;
	ids[4] = price->promotion;
	for (unsigned i = 0; i < 5; i++) {
		size_t n = 0;

		while (n <= USSLP_CANON_MAX_ID && ids[i][n] != '\0') {
			n++;
		}
		if (n > USSLP_CANON_MAX_ID) {
			return USSLP_ERR_MALFORMED;
		}
		/* The wire carries each length in a single byte, and canon.ValidID caps
		 * an identifier at 128, so the byte is never the binding limit — but the
		 * check is written against the byte as well, because a future protocol
		 * revision that raised the canon limit would otherwise truncate here in
		 * silence. */
		if (n > 255u) {
			return USSLP_ERR_MALFORMED;
		}
		lens[i] = n;
	}
	/* The same validation the decoder applies, so a frame this function agrees
	 * to build is a frame the decoder will agree to parse. Only the promotion
	 * may be empty. */
	if (!valid_id_bytes((const uint8_t *)ids[0], lens[0], false) ||
	    !valid_id_bytes((const uint8_t *)ids[1], lens[1], false) ||
	    !valid_id_bytes((const uint8_t *)ids[2], lens[2], false) ||
	    !valid_id_bytes((const uint8_t *)ids[3], lens[3], false) ||
	    !valid_id_bytes((const uint8_t *)ids[4], lens[4], true)) {
		return USSLP_ERR_MALFORMED;
	}
	if (price->currency[0] < 'A' || price->currency[0] > 'Z' ||
	    price->currency[1] < 'A' || price->currency[1] > 'Z' ||
	    price->currency[2] < 'A' || price->currency[2] > 'Z' ||
	    price->currency[3] != '\0') {
		return USSLP_ERR_MALFORMED;
	}

	memset(out, 0, sizeof(*out));
	out->update.sequence = price->sequence;
	out->update.price_minor = price->amount_minor;
	memcpy(out->update.currency, price->currency, 4);
	out->update.flags = flags;
	out->update.template_code = template_code;
	out->update.origin_x = origin_x;
	out->update.origin_y = origin_y;
	out->update.image = image;
	out->update.image_len = image_len;
	out->update.image_crc = 0u; /* the encoder computes it */
	out->effective_at = price->effective_at;
	out->attestation = *attestation;
	out->tenant = (const uint8_t *)ids[0];
	out->tenant_len = (uint8_t)lens[0];
	out->store = (const uint8_t *)ids[1];
	out->store_len = (uint8_t)lens[1];
	out->label = (const uint8_t *)ids[2];
	out->label_len = (uint8_t)lens[2];
	out->sku = (const uint8_t *)ids[3];
	out->sku_len = (uint8_t)lens[3];
	out->promotion = (const uint8_t *)ids[4];
	out->promotion_len = (uint8_t)lens[4];
	return USSLP_OK;
}

size_t usslp_wire_encode_attested_update(const struct usslp_attested_update *att, uint8_t *buf,
					 size_t cap)
{
	size_t idlen, total, off;

	if (att == NULL || buf == NULL) {
		return 0;
	}
	if (att->update.currency[0] == '\0' || att->update.currency[1] == '\0' ||
	    att->update.currency[2] == '\0') {
		return 0;
	}
	idlen = (size_t)att->tenant_len + att->store_len + att->label_len + att->sku_len +
		att->promotion_len;
	total = (size_t)USSLP_ATTESTED_HEADER_BYTES + idlen + (size_t)att->update.image_len;
	if (cap < total) {
		return 0;
	}
	/* An identifier with a non-zero length and a NULL pointer would memcpy from
	 * nowhere. Refusing is right: the caller has built the struct by hand and got
	 * it wrong, which is exactly the mistake this encoder exists to remove from
	 * callers that do not have to. */
	if ((att->tenant_len > 0u && att->tenant == NULL) ||
	    (att->store_len > 0u && att->store == NULL) ||
	    (att->label_len > 0u && att->label == NULL) ||
	    (att->sku_len > 0u && att->sku == NULL) ||
	    (att->promotion_len > 0u && att->promotion == NULL) ||
	    (att->update.image_len > 0u && att->update.image == NULL)) {
		return 0;
	}

	memset(buf, 0, total);
	(void)encode_update_head(&att->update, buf, USSLP_FRAME_ATTESTED_UPDATE);
	wr64(&buf[33], (uint64_t)att->effective_at);
	buf[41] = att->attestation.alg;
	/* The kid is fixed width and NOT NUL terminated on the wire. The struct
	 * holds it NUL terminated, so a kid shorter than USSLP_KID_LEN — which the
	 * derivation never produces, but a hand-built struct might — is copied up to
	 * its terminator and the rest left as the zeroes memset put there. */
	{
		size_t n = 0;

		while (n < USSLP_KID_LEN && att->attestation.kid[n] != '\0') {
			n++;
		}
		memcpy(&buf[42], att->attestation.kid, n);
	}
	memcpy(&buf[70], att->attestation.digest, USSLP_SHA256_DIGEST_LEN);
	memcpy(&buf[102], att->attestation.sig, USSLP_ED25519_SIGNATURE_LEN);
	buf[166] = att->tenant_len;
	buf[167] = att->store_len;
	buf[168] = att->label_len;
	buf[169] = att->sku_len;
	buf[170] = att->promotion_len;

	/* Every field is copied through the same two lines, and the length is
	 * advanced by the same expression that sized the copy. The first version of
	 * this protocol's round-trip test hand-wrote five copy/advance pairs and got
	 * one of them wrong — it advanced past the promotion without copying it —
	 * which is the entire reason this function exists. */
	off = USSLP_ATTESTED_HEADER_BYTES;
	{
		const uint8_t *parts[6];
		size_t lens[6];

		parts[0] = att->tenant;
		lens[0] = att->tenant_len;
		parts[1] = att->store;
		lens[1] = att->store_len;
		parts[2] = att->label;
		lens[2] = att->label_len;
		parts[3] = att->sku;
		lens[3] = att->sku_len;
		parts[4] = att->promotion;
		lens[4] = att->promotion_len;
		parts[5] = att->update.image;
		lens[5] = att->update.image_len;

		for (unsigned i = 0; i < 6; i++) {
			if (lens[i] > 0u) {
				memcpy(&buf[off], parts[i], lens[i]);
				off += lens[i];
			}
		}
	}
	return total;
}

size_t usslp_wire_encode_ack(const struct usslp_ack *a, uint8_t *buf, size_t cap)
{
	uint8_t flags = 0;

	if (a == NULL || buf == NULL || cap < USSLP_ACK_BYTES) {
		return 0;
	}
	buf[0] = USSLP_WIRE_VERSION;
	buf[1] = USSLP_FRAME_ACK;
	wr64(&buf[2], (uint64_t)a->sequence);
	buf[10] = a->status;
	wr16(&buf[11], a->refresh_ms);
	if (a->partial) {
		flags |= USSLP_ACK_FLAG_PARTIAL;
	}
	if (a->forced_full) {
		flags |= USSLP_ACK_FLAG_FORCED_FULL;
	}
	flags |= (uint8_t)((a->attest_verdict & USSLP_ACK_VERDICT_MASK)
			   << USSLP_ACK_VERDICT_SHIFT);
	buf[13] = flags;
	wr16(&buf[14], a->battery_mv);
	buf[16] = a->battery_pct;
	wr16(&buf[17], (uint16_t)a->temperature_centi_c);
	/* labelsim.EncodeAck repeats the frame type in the last byte. Reproduced
	 * rather than reasoned about: the reference implementation writes it, so a
	 * decoder somewhere may one day check it. */
	buf[19] = USSLP_FRAME_ACK;
	return USSLP_ACK_BYTES;
}

int usslp_wire_decode_ack(const uint8_t *buf, size_t len, struct usslp_ack *out)
{
	if (buf == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (len < USSLP_ACK_BYTES || buf[0] != USSLP_WIRE_VERSION || buf[1] != USSLP_FRAME_ACK) {
		return USSLP_ERR_MALFORMED;
	}
	memset(out, 0, sizeof(*out));
	out->sequence = (int64_t)rd64(&buf[2]);
	out->status = buf[10];
	out->refresh_ms = rd16(&buf[11]);
	out->partial = (buf[13] & USSLP_ACK_FLAG_PARTIAL) != 0u;
	out->forced_full = (buf[13] & USSLP_ACK_FLAG_FORCED_FULL) != 0u;
	out->attest_verdict =
		(uint8_t)((buf[13] >> USSLP_ACK_VERDICT_SHIFT) & USSLP_ACK_VERDICT_MASK);
	out->battery_mv = rd16(&buf[14]);
	out->battery_pct = buf[16];
	out->temperature_centi_c = (int16_t)rd16(&buf[17]);
	return USSLP_OK;
}

size_t usslp_wire_encode_telemetry(const struct usslp_telemetry *t, uint8_t *buf, size_t cap)
{
	if (t == NULL || buf == NULL || cap < USSLP_TELEMETRY_BYTES) {
		return 0;
	}
	memset(buf, 0, USSLP_TELEMETRY_BYTES);
	buf[0] = USSLP_WIRE_VERSION;
	buf[1] = USSLP_FRAME_TELEMETRY;
	wr16(&buf[2], t->battery_mv);
	buf[4] = t->battery_pct;
	wr16(&buf[5], (uint16_t)t->temperature_centi_c);
	buf[7] = t->parent_lqi;
	buf[8] = (uint8_t)t->parent_rssi;
	wr32(&buf[9], t->refresh_count);
	wr32(&buf[13], t->nfc_tap_count);
	wr32(&buf[17], t->uptime_sec);
	buf[21] = t->tamper ? 1u : 0u;
	return USSLP_TELEMETRY_BYTES;
}

int usslp_wire_decode_telemetry(const uint8_t *buf, size_t len, struct usslp_telemetry *out)
{
	if (buf == NULL || out == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (len < USSLP_TELEMETRY_BYTES || buf[0] != USSLP_WIRE_VERSION ||
	    buf[1] != USSLP_FRAME_TELEMETRY) {
		return USSLP_ERR_MALFORMED;
	}
	memset(out, 0, sizeof(*out));
	out->battery_mv = rd16(&buf[2]);
	out->battery_pct = buf[4];
	out->temperature_centi_c = (int16_t)rd16(&buf[5]);
	out->parent_lqi = buf[7];
	out->parent_rssi = (int8_t)buf[8];
	out->refresh_count = rd32(&buf[9]);
	out->nfc_tap_count = rd32(&buf[13]);
	out->uptime_sec = rd32(&buf[17]);
	out->tamper = buf[21] != 0u;
	return USSLP_OK;
}
