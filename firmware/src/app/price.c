#include "price.h"

#include "../crypto/devcert.h"
#include "../display/eink.h"
#include "../display/framebuffer.h"
#include "../display/usslp_render_policy.h"
#include "../display/usslp_rle.h"
#include "../nfc/nfc.h"
#include "../power/power.h"
#include "../radio/radio.h"
#include "seq_store.h"
#include "telemetry.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_REGISTER(usslp_price, CONFIG_USSLP_LOG_LEVEL);

/*
 * The decoded-window scratch buffer.
 *
 * Sized for the largest *window* a partial refresh legitimately carries rather
 * than for a whole panel. A price changing from 2.49 to 1.99 moves one band of
 * pixels; a full refresh carries the panel, and on the two buffered tiers the
 * panel at one byte per pixel is 37,888 or 120,000 bytes, which is why the full
 * path decodes and packs a band at a time rather than materialising the image.
 *
 * 8 KiB covers a full-width band 27 rows deep on the 2.9-inch panel and 20 on
 * the 4.2-inch one, which is comfortably more than the tallest price field.
 */
#define WINDOW_SCRATCH_BYTES 8192
static uint8_t window_scratch[WINDOW_SCRATCH_BYTES];

static struct usslp_price_stats stats;
static struct usslp_ghost_state ghost;
static K_MUTEX_DEFINE(price_lock);

int usslp_price_init(void)
{
	int rc;

	memset(&stats, 0, sizeof(stats));
	memcpy(stats.displayed_currency, "   ", 4);
	rc = usslp_seq_store_init(&ghost);
	if (rc != USSLP_OK) {
		return rc;
	}
	stats.displayed_sequence = usslp_seq_store_state()->displayed;
	LOG_INF("price handler ready; displayed sequence %lld, %u partials since the "
		"last full refresh",
		(long long)stats.displayed_sequence, ghost.partials_since_full);
	return USSLP_OK;
}

size_t usslp_format_money(int64_t amount_minor, const char *currency, char *out, size_t cap)
{
	/* canon.minorUnits: the zero- and three-decimal currencies are the ones
	 * that break naive formatting, and a label that renders 1000 JPY as 10.00
	 * is a weights-and-measures fault, not a cosmetic one. */
	static const char *const zero_dp[] = { "JPY", "KRW", "VND", "CLP", "ISK", "PYG",
					       "RWF", "UGX", "VUV", "XAF", "XOF", "XPF",
					       "KMF", "DJF" };
	static const char *const three_dp[] = { "BHD", "IQD", "JOD", "KWD",
						"LYD", "OMR", "TND" };
	static const struct {
		const char *code;
		const char *symbol;
	} symbols[] = {
		{ "USD", "$" }, { "EUR", "E" }, { "GBP", "L" }, { "INR", "R" },
		{ "JPY", "Y" }, { "CNY", "Y" }, { "AUD", "A$" }, { "CAD", "C$" },
	};
	unsigned exp = 2;
	const char *symbol = NULL;
	bool neg = amount_minor < 0;
	uint64_t mag = neg ? (~(uint64_t)amount_minor + 1u) : (uint64_t)amount_minor;
	uint64_t div = 1;
	char tmp[32];
	size_t n = 0;

	if (out == NULL || cap == 0u || currency == NULL) {
		return 0;
	}
	for (unsigned i = 0; i < ARRAY_SIZE(zero_dp); i++) {
		if (strncmp(currency, zero_dp[i], 3) == 0) {
			exp = 0;
		}
	}
	for (unsigned i = 0; i < ARRAY_SIZE(three_dp); i++) {
		if (strncmp(currency, three_dp[i], 3) == 0) {
			exp = 3;
		}
	}
	for (unsigned i = 0; i < ARRAY_SIZE(symbols); i++) {
		if (strncmp(currency, symbols[i].code, 3) == 0) {
			symbol = symbols[i].symbol;
		}
	}
	for (unsigned i = 0; i < exp; i++) {
		div *= 10u;
	}

	if (neg) {
		tmp[n++] = '-';
	}
	if (symbol != NULL) {
		for (const char *p = symbol; *p != '\0'; p++) {
			tmp[n++] = *p;
		}
	}
	{
		uint64_t whole = mag / div;
		uint64_t frac = mag % div;
		char digits[24];
		size_t d = 0;

		do {
			digits[d++] = (char)('0' + (whole % 10u));
			whole /= 10u;
		} while (whole != 0u && d < sizeof(digits));
		while (d > 0u && n < sizeof(tmp) - 1u) {
			tmp[n++] = digits[--d];
		}
		if (exp > 0u) {
			tmp[n++] = '.';
			for (unsigned i = exp; i > 0u && n < sizeof(tmp) - 1u; i--) {
				uint64_t place = 1;

				for (unsigned j = 1; j < i; j++) {
					place *= 10u;
				}
				tmp[n++] = (char)('0' + ((frac / place) % 10u));
			}
		}
	}
	if (symbol == NULL) {
		/* Unknown currency: fall back to the ISO code, as canon.Money.Display
		 * does, rather than showing a bare number that could be any currency. */
		if (n < sizeof(tmp) - 4u) {
			tmp[n++] = ' ';
			tmp[n++] = currency[0];
			tmp[n++] = currency[1];
			tmp[n++] = currency[2];
		}
	}
	if (n >= cap) {
		n = cap - 1u;
	}
	memcpy(out, tmp, n);
	out[n] = '\0';
	return n;
}

/* Turns a verdict into the compliance event the platform expects. A failed
 * attestation is not a log line, it is an alert with a runbook entry. */
static void raise_compliance_alert(enum usslp_attest_verdict v, int64_t sequence)
{
	LOG_ERR("REFUSING price update seq=%lld: attestation %s. The previous price "
		"remains on the glass.",
		(long long)sequence, usslp_attest_verdict_str(v));
	/* The telemetry reporter picks this up on its next uplink; it is also
	 * visible within three missed heartbeats even if the uplink is lost, which
	 * is the property INTERFACE-CONTRACTS section 5 relies on. */
	usslp_telemetry_note_attestation_failure(v);
}

/*
 * Verifies an attested frame. Split out so that the two ingest paths — the
 * legacy image-only frame and the attested one — have exactly one place where
 * the decision to render is made.
 */
static enum usslp_attest_verdict verify_attested(const struct usslp_attested_update *att)
{
	struct usslp_price_scratch scratch;
	struct usslp_price_input price;
	enum usslp_attest_verdict verdict;
	int rc;

	rc = usslp_attested_price_input(att, &scratch, &price);
	if (rc != USSLP_OK) {
		raise_compliance_alert(USSLP_ATTEST_MALFORMED, att->update.sequence);
		return USSLP_ATTEST_MALFORMED;
	}
	if (IS_ENABLED(CONFIG_USSLP_ATTESTATION_STRICT_CLOCK)) {
		verdict = usslp_attest_verify_strict(usslp_price_keyring(), &price,
						     &att->attestation, usslp_power_unix_time());
	} else {
		verdict = usslp_attest_verify(usslp_price_keyring(), &price, &att->attestation,
					      usslp_power_unix_time());
	}
	if (!usslp_attest_ok(verdict)) {
		raise_compliance_alert(verdict, att->update.sequence);
		return verdict;
	}
	return USSLP_ATTEST_OK;
}

/* Decodes the image and loads it, without touching the glass. */
static int load_image(const struct usslp_update *u)
{
	uint16_t w = 0, h = 0;
	int rc;

	rc = usslp_rle_dimensions(u->image, u->image_len, &w, &h);
	if (rc != USSLP_OK) {
		return rc;
	}
	if ((size_t)w * h > sizeof(window_scratch)) {
		/* A window bigger than the scratch buffer. On the buffered tiers this
		 * only happens for a full-panel image, which the controller sends at the
		 * origin; the streaming tier never buffers at all. Refusing is right:
		 * a partially loaded image is a partially updated price. */
		LOG_ERR("image window %ux%u exceeds the %u-byte decode buffer", w, h,
			(unsigned)sizeof(window_scratch));
		return USSLP_ERR_NOSPACE;
	}
	rc = usslp_rle_decode(u->image, u->image_len, window_scratch, sizeof(window_scratch), &w,
			      &h);
	if (rc != USSLP_OK) {
		return rc;
	}
	return usslp_eink_load(window_scratch, w, h, u->origin_x, u->origin_y);
}

static void send_ack(int64_t sequence, enum usslp_ack_status status,
		     enum usslp_attest_verdict verdict, const struct usslp_refresh_plan *plan,
		     uint16_t refresh_ms)
{
	struct usslp_ack ack;
	uint8_t frame[USSLP_ACK_BYTES];
	uint16_t mv;
	uint8_t pct;
	int16_t centi_c = 0;

	memset(&ack, 0, sizeof(ack));
	ack.sequence = sequence;
	ack.status = (uint8_t)status;
	ack.attest_verdict = (uint8_t)verdict;
	ack.refresh_ms = refresh_ms;
	if (plan != NULL) {
		ack.partial = plan->partial;
		ack.forced_full = plan->forced_full;
	}
	usslp_power_battery(&mv, &pct);
	ack.battery_mv = mv;
	ack.battery_pct = pct;
	(void)usslp_eink_temperature(&centi_c);
	ack.temperature_centi_c = centi_c;

	if (usslp_wire_encode_ack(&ack, frame, sizeof(frame)) == USSLP_ACK_BYTES) {
		usslp_radio_send_uplink(frame, USSLP_ACK_BYTES);
	}
}

int usslp_price_handle_frame(const uint8_t *frame, size_t len)
{
	struct usslp_attested_update att;
	struct usslp_update *u;
	struct usslp_refresh_plan plan;
	uint8_t type;
	uint16_t refresh_ms = 0;
	bool attested = false;
	int rc;

	if (!usslp_wire_kind(frame, len, &type)) {
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.frames++;
		stats.bad_frame++;
		k_mutex_unlock(&price_lock);
		return USSLP_ERR_MALFORMED;
	}

	/* Counted here rather than after the decode, so that every frame this label
	 * was handed appears in the total — including the ones a refusal path
	 * returns early on. A frames counter that misses its own refusals makes the
	 * ratio an operator computes from it wrong in the direction that hides a
	 * problem. */
	k_mutex_lock(&price_lock, K_FOREVER);
	stats.frames++;
	k_mutex_unlock(&price_lock);

	memset(&att, 0, sizeof(att));
	switch (type) {
	case USSLP_FRAME_ATTESTED_UPDATE:
		rc = usslp_wire_decode_attested(frame, len, &att);
		attested = true;
		break;
	case USSLP_FRAME_UPDATE:
		rc = usslp_wire_decode_update(frame, len, &att.update);
		if (rc == USSLP_OK && IS_ENABLED(CONFIG_USSLP_REQUIRE_ATTESTATION)) {
			/*
			 * This build does not trust the controller to have verified on its
			 * behalf. The frame is decoded first anyway, purely so the refusal
			 * can name the sequence it is refusing: an ack with sequence 0 tells
			 * the controller only that something went wrong, while one carrying
			 * the sequence lets it correlate the refusal with the update it
			 * sent and stop retrying that particular one.
			 *
			 * The status is REFUSED_UNATTESTED, not REFUSED_ATTESTATION. This is
			 * a fleet configuration mismatch — the controller speaks frame type
			 * 1 to a label that requires type 4 — and raising a compliance alert
			 * for it would bury the real ones under every label in the zone.
			 */
			k_mutex_lock(&price_lock, K_FOREVER);
			stats.attestation_failed++;
			k_mutex_unlock(&price_lock);
			LOG_WRN("refusing an unattested price frame at sequence %lld: this "
				"build requires end-to-end attestation and the controller "
				"sent a type-%u frame",
				(long long)att.update.sequence, type);
			send_ack(att.update.sequence, USSLP_ACK_REFUSED_UNATTESTED,
				 USSLP_ATTEST_OK, NULL, 0);
			return USSLP_ERR_AUTH;
		}
		break;
	default:
		rc = USSLP_ERR_MALFORMED;
		break;
	}

	if (rc != USSLP_OK) {
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.bad_frame++;
		k_mutex_unlock(&price_lock);
		LOG_WRN("frame did not decode (%d)", rc);
		send_ack(0, USSLP_ACK_BAD_FRAME, USSLP_ATTEST_OK, NULL, 0);
		return rc;
	}
	u = &att.update;

	/* The label is in its active window from the moment a frame lands, whatever
	 * happens to the frame: something is going on in this aisle. */
	usslp_power_note_activity();

	/*
	 * Step 2: the sequence rule, before the signature.
	 *
	 * A duplicated mesh frame is the common case — that is what at-least-once
	 * delivery means — and an Ed25519 verification is 13 ms of receiver-off CPU
	 * time. Checking the cheap invariant first costs nothing in safety, because
	 * a stale update is discarded either way, and saves the cell a verification
	 * per duplicate.
	 */
	if (usslp_seq_store_check(u->sequence) == USSLP_SEQ_STALE) {
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.stale++;
		k_mutex_unlock(&price_lock);
		LOG_DBG("discarding sequence %lld, not greater than the displayed %lld",
			(long long)u->sequence, (long long)stats.displayed_sequence);
		send_ack(u->sequence, USSLP_ACK_STALE_SEQUENCE, USSLP_ATTEST_OK, NULL, 0);
		return USSLP_ERR_STALE;
	}

	/* Step 3: the attestation. */
	if (attested) {
		enum usslp_attest_verdict verdict = verify_attested(&att);

		if (!usslp_attest_ok(verdict)) {
			k_mutex_lock(&price_lock, K_FOREVER);
			stats.attestation_failed++;
			k_mutex_unlock(&price_lock);
			/*
			 * A distinct status, and the verdict with it. Reporting this as a
			 * bad frame would make a compliance incident indistinguishable from
			 * a corrupted radio frame, and the two have opposite runbooks: one
			 * is "check the link", the other is "a price that did not verify
			 * was offered to a shelf". The verdict tells the operator which
			 * kind — a stale key ring reads as unknown-key-id, actual tampering
			 * as digest-mismatch — without a round trip to ask.
			 */
			send_ack(u->sequence, USSLP_ACK_REFUSED_ATTESTATION, verdict, NULL, 0);
			return USSLP_ERR_AUTH;
		}
	}

	/* Step 4: decode and load. Still nothing on the glass. */
	rc = load_image(u);
	if (rc == USSLP_ERR_BUSY) {
		/* A frame that arrived while a waveform was running. It is lost, not
		 * queued — the same behaviour the simulator models with
		 * mesh.SetBusyUntil — and the controller will retry. */
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.busy_dropped++;
		k_mutex_unlock(&price_lock);
		return rc;
	}
	if (rc != USSLP_OK) {
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.render_failed++;
		k_mutex_unlock(&price_lock);
		LOG_ERR("image decode failed (%d); keeping the previous price", rc);
		send_ack(u->sequence, USSLP_ACK_BAD_FRAME, USSLP_ATTEST_OK, NULL, 0);
		return rc;
	}

	plan = usslp_plan_refresh(usslp_eink_tier(),
				  (u->flags & USSLP_FLAG_REQUEST_PARTIAL) != 0u, &ghost);

	/*
	 * Step 5: persist BEFORE the waveform.
	 *
	 * A brownout during a 1.5-second refresh is a real event on a coin cell
	 * driving a charge pump. Persisting first means such a crash loses this
	 * price change and the retry is accepted. Persisting after would leave the
	 * new pixels on the glass with the old sequence in NVS, and the retry would
	 * then be discarded as stale — a label showing a price it has told the
	 * platform it is not showing, which is precisely the state the whole
	 * attestation apparatus exists to make impossible.
	 */
	rc = usslp_seq_store_commit(u->sequence, &ghost, &plan);
	if (rc != USSLP_OK) {
		LOG_ERR("persisting sequence %lld failed (%d); refusing to render",
			(long long)u->sequence, rc);
		send_ack(u->sequence, USSLP_ACK_BAD_FRAME, USSLP_ATTEST_OK, NULL, 0);
		return rc;
	}

	/* Step 6: the glass. */
	rc = usslp_eink_refresh(&plan, &refresh_ms);
	if (rc != USSLP_OK) {
		k_mutex_lock(&price_lock, K_FOREVER);
		stats.render_failed++;
		k_mutex_unlock(&price_lock);
		LOG_ERR("panel refresh failed (%d)", rc);
		send_ack(u->sequence, USSLP_ACK_BAD_FRAME, USSLP_ATTEST_OK, &plan, refresh_ms);
		return rc;
	}

	k_mutex_lock(&price_lock, K_FOREVER);
	stats.applied++;
	stats.displayed_sequence = u->sequence;
	stats.displayed_price_minor = u->price_minor;
	memcpy(stats.displayed_currency, u->currency, 4);
	stats.image_hash = usslp_fb_hash();
	stats.last_refresh_ms = refresh_ms;
	k_mutex_unlock(&price_lock);

	/* The NFC record follows the glass, never leads it: a shopper tapping a
	 * label must not read a price the panel is not showing. */
	usslp_nfc_publish_price(u->price_minor, u->currency, u->sequence);

	if (plan.forced_full) {
		LOG_INF("ghosting budget spent; ran a full refresh where a partial was "
			"requested");
	}
	LOG_INF("applied sequence %lld in %u ms (%s)", (long long)u->sequence, refresh_ms,
		plan.partial ? "partial" : "full");

	/* Step 7. */
	send_ack(u->sequence, USSLP_ACK_APPLIED, USSLP_ATTEST_OK, &plan, refresh_ms);
	return USSLP_OK;
}

void usslp_price_stats(struct usslp_price_stats *out)
{
	k_mutex_lock(&price_lock, K_FOREVER);
	*out = stats;
	k_mutex_unlock(&price_lock);
}
