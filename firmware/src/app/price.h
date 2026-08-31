/*
 * price.h - the price update handler: the one path in this firmware where
 * getting the order of operations wrong is a regulatory problem.
 *
 * The order, and why each step is where it is:
 *
 *   1. decode the frame            a frame that does not decode is dropped;
 *                                  the previous price stays on the glass
 *   2. check the sequence          INTERFACE-CONTRACTS section 6. Before the
 *                                  attestation, because a duplicate is the
 *                                  common case and verifying a signature costs
 *                                  13 ms of a coin cell's life
 *   3. verify the attestation      INTERFACE-CONTRACTS section 5. The digest is
 *                                  recomputed from the fields about to be
 *                                  rendered, never taken from the wire
 *   4. decode and load the image   still nothing on the glass; a failure here
 *                                  is recoverable
 *   5. persist the new sequence    BEFORE the waveform. See usslp_seq.h: a
 *                                  crash mid-refresh must lose the price, not
 *                                  strand the label showing one it has told the
 *                                  platform it is not showing
 *   6. drive the panel
 *   7. acknowledge with the measured refresh time
 *
 * Steps 2 and 3 are both refusals, and they are different refusals: a stale
 * sequence is the normal outcome of a duplicated mesh frame and is acknowledged
 * as such, while a failed attestation is a compliance alert. Collapsing them
 * into one "rejected" path would hide a tampering attempt inside the noise of
 * ordinary mesh duplication.
 */

#ifndef USSLP_PRICE_H
#define USSLP_PRICE_H

#include "../radio/usslp_wire.h"
#include "../usslp_portable.h"

/* Template codes on the wire, from sec.TemplateCode. */
enum usslp_template_code {
	USSLP_TEMPLATE_STANDARD = 0,
	USSLP_TEMPLATE_PROMO = 1,
	USSLP_TEMPLATE_UNIT_PRICE = 2,
	USSLP_TEMPLATE_CLEARANCE = 3,
};

/* The widest panel in the range, which bounds the local renderer's scratch
 * band. */
#define USSLP_TEMPLATE_MAX_WIDTH 600

/* What the local renderer needs. Only used when there is no controller to render
 * for us; see display/templates.c. */
struct usslp_render_request {
	uint8_t template_code;
	char price_text[16];
	char was_text[16];
	char badge_text[16];
	char name_text[32];
	char unit_text[32];
	bool show_was;
};

int usslp_template_render(const struct usslp_render_request *req);
int usslp_template_commissioning(const char *serial, const char *state);
int usslp_template_fault(const char *code, const char *detail);

/* Counters, reported in telemetry. Every one of these is a question an operator
 * asks about a shelf that is not behaving. */
struct usslp_price_stats {
	uint32_t frames;
	uint32_t applied;
	uint32_t stale;
	uint32_t bad_frame;
	uint32_t attestation_failed;
	uint32_t render_failed;
	uint32_t busy_dropped;
	int64_t displayed_sequence;
	int64_t displayed_price_minor;
	char displayed_currency[4];
	uint32_t image_hash;
	uint16_t last_refresh_ms;
};

int usslp_price_init(void);

/*
 * Handles one frame from the radio. Runs on the price thread, not on the radio
 * callback, because step 6 blocks for up to fifteen seconds and the radio stack
 * cannot be held that long.
 *
 * Returns USSLP_OK when the pixels changed. Everything else leaves the previous
 * image on the glass, which is the correct outcome for every failure this
 * function can have.
 */
int usslp_price_handle_frame(const uint8_t *frame, size_t len);

void usslp_price_stats(struct usslp_price_stats *out);

/* Formats a minor-unit amount the way canon.Money.Display does, for the local
 * renderer and the NFC record. Currencies absent from the table default to two
 * decimal places, matching canon.minorUnits. */
size_t usslp_format_money(int64_t amount_minor, const char *currency, char *out, size_t cap);

#endif /* USSLP_PRICE_H */
