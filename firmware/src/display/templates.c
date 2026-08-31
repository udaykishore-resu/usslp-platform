/*
 * The four render templates, drawn on the label.
 *
 * Normally they are not. In the platform's design the Shelf Edge Controller
 * renders — the cloud names a template, the controller computes the pixels, and
 * what crosses the mesh is a compressed image (edge/sec/render.go). That is the
 * right split: a new hardware tier with a different resolution ships without a
 * cloud release, and the label spends no energy on layout.
 *
 * This file exists for the case where there is no controller. A label whose zone
 * has lost its coordinator, or one being commissioned before it has ever
 * associated, still has to be able to put something legible on the glass:
 *
 *   - the commissioning screen, so a technician can see the serial and the
 *     provisioning state on a device that has no other display;
 *   - the last known price, redrawn after a full-clear cycle;
 *   - the fault screen, which is the difference between a label an operator can
 *     triage from the aisle and one they have to take back to the office.
 *
 * The geometry is proportional to the panel, so the same four templates serve
 * all three tiers, and it mirrors the controller's layout closely enough that a
 * locally drawn price and a controller-drawn one are not visibly different
 * fittings on the same shelf.
 *
 * The font is a 5x7 cell scaled by integer factors. Not a proportional font: a
 * scaled bitmap cell is a few hundred bytes of flash and no rasteriser, and the
 * one thing that matters about a price on a shelf edge is that the digits are
 * unambiguous at two metres, which a 5x7 cell at 8x scale does very well.
 */

#include "../app/price.h"
#include "eink.h"
#include "framebuffer.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_display, CONFIG_USSLP_LOG_LEVEL);

/* 5x7 glyphs, column-major, for the characters a label ever draws locally:
 * digits, a decimal point, the currency symbols the fleet uses, and the upper
 * case letters the fault and commissioning screens need. Anything outside the
 * set renders as a blank cell rather than as a wrong character, because a
 * mis-rendered digit on a price is worse than a gap. */
struct glyph {
	char c;
	uint8_t col[5];
};

static const struct glyph glyphs[] = {
	{ '0', { 0x3E, 0x51, 0x49, 0x45, 0x3E } },
	{ '1', { 0x00, 0x42, 0x7F, 0x40, 0x00 } },
	{ '2', { 0x42, 0x61, 0x51, 0x49, 0x46 } },
	{ '3', { 0x21, 0x41, 0x45, 0x4B, 0x31 } },
	{ '4', { 0x18, 0x14, 0x12, 0x7F, 0x10 } },
	{ '5', { 0x27, 0x45, 0x45, 0x45, 0x39 } },
	{ '6', { 0x3C, 0x4A, 0x49, 0x49, 0x30 } },
	{ '7', { 0x01, 0x71, 0x09, 0x05, 0x03 } },
	{ '8', { 0x36, 0x49, 0x49, 0x49, 0x36 } },
	{ '9', { 0x06, 0x49, 0x49, 0x29, 0x1E } },
	{ '.', { 0x00, 0x60, 0x60, 0x00, 0x00 } },
	{ ',', { 0x00, 0x50, 0x30, 0x00, 0x00 } },
	{ '-', { 0x08, 0x08, 0x08, 0x08, 0x08 } },
	{ '/', { 0x20, 0x10, 0x08, 0x04, 0x02 } },
	{ ':', { 0x00, 0x36, 0x36, 0x00, 0x00 } },
	{ ' ', { 0x00, 0x00, 0x00, 0x00, 0x00 } },
	{ '$', { 0x24, 0x2A, 0x7F, 0x2A, 0x12 } },
	{ 'A', { 0x7E, 0x11, 0x11, 0x11, 0x7E } },
	{ 'B', { 0x7F, 0x49, 0x49, 0x49, 0x36 } },
	{ 'C', { 0x3E, 0x41, 0x41, 0x41, 0x22 } },
	{ 'D', { 0x7F, 0x41, 0x41, 0x22, 0x1C } },
	{ 'E', { 0x7F, 0x49, 0x49, 0x49, 0x41 } },
	{ 'F', { 0x7F, 0x09, 0x09, 0x09, 0x01 } },
	{ 'G', { 0x3E, 0x41, 0x49, 0x49, 0x7A } },
	{ 'H', { 0x7F, 0x08, 0x08, 0x08, 0x7F } },
	{ 'I', { 0x00, 0x41, 0x7F, 0x41, 0x00 } },
	{ 'K', { 0x7F, 0x08, 0x14, 0x22, 0x41 } },
	{ 'L', { 0x7F, 0x40, 0x40, 0x40, 0x40 } },
	{ 'M', { 0x7F, 0x02, 0x0C, 0x02, 0x7F } },
	{ 'N', { 0x7F, 0x04, 0x08, 0x10, 0x7F } },
	{ 'O', { 0x3E, 0x41, 0x41, 0x41, 0x3E } },
	{ 'P', { 0x7F, 0x09, 0x09, 0x09, 0x06 } },
	{ 'R', { 0x7F, 0x09, 0x19, 0x29, 0x46 } },
	{ 'S', { 0x46, 0x49, 0x49, 0x49, 0x31 } },
	{ 'T', { 0x01, 0x01, 0x7F, 0x01, 0x01 } },
	{ 'U', { 0x3F, 0x40, 0x40, 0x40, 0x3F } },
	{ 'V', { 0x1F, 0x20, 0x40, 0x20, 0x1F } },
	{ 'W', { 0x3F, 0x40, 0x38, 0x40, 0x3F } },
	{ 'X', { 0x63, 0x14, 0x08, 0x14, 0x63 } },
	{ 'Y', { 0x07, 0x08, 0x70, 0x08, 0x07 } },
	{ 'Z', { 0x61, 0x51, 0x49, 0x45, 0x43 } },
};

/* A single scratch band. Drawing happens a band at a time so that no template
 * ever needs a full-panel one-byte-per-pixel buffer: on the 4.2-inch tier that
 * would be 120 KB. */
#define BAND_HEIGHT 8
static uint8_t band[USSLP_TEMPLATE_MAX_WIDTH * BAND_HEIGHT];

static const struct glyph *find_glyph(char c)
{
	for (unsigned i = 0; i < ARRAY_SIZE(glyphs); i++) {
		if (glyphs[i].c == c) {
			return &glyphs[i];
		}
	}
	return NULL;
}

/* Draws one character into a caller-provided pixel window. Returns the advance
 * in pixels. */
/*
 * x and y are signed so that a glyph can be drawn partly above the top of the
 * window. That is not a curiosity: the price band on the 4.2-inch panel is 84
 * pixels tall and the scratch buffer is 8 rows, so the band is drawn in slices
 * with the glyph origin above each slice. An unsigned origin would wrap and put
 * the digits nowhere.
 */
static uint16_t draw_char(uint8_t *win, uint16_t win_w, uint16_t win_h, int32_t x, int32_t y,
			  char c, uint8_t scale, uint8_t ink)
{
	const struct glyph *g = find_glyph(c);
	uint16_t advance = (uint16_t)(6u * scale);

	if (g == NULL) {
		return advance;
	}
	for (int32_t col = 0; col < 5; col++) {
		for (int32_t row = 0; row < 7; row++) {
			if ((g->col[col] & (1u << row)) == 0u) {
				continue;
			}
			for (int32_t sy = 0; sy < scale; sy++) {
				for (int32_t sx = 0; sx < scale; sx++) {
					int32_t px = x + col * scale + sx;
					int32_t py = y + row * scale + sy;

					if (px >= 0 && py >= 0 && px < (int32_t)win_w &&
					    py < (int32_t)win_h) {
						win[(size_t)py * win_w + (size_t)px] = ink;
					}
				}
			}
		}
	}
	return advance;
}

static uint16_t text_width(const char *s, uint8_t scale)
{
	return (uint16_t)(strlen(s) * 6u * scale);
}

static void draw_text(uint8_t *win, uint16_t win_w, uint16_t win_h, int32_t x, int32_t y,
		      const char *s, uint8_t scale, uint8_t ink)
{
	for (const char *p = s; *p != '\0'; p++) {
		x += draw_char(win, win_w, win_h, x, y, *p, scale, ink);
	}
}

/*
 * Renders one of the four templates into the framebuffer.
 *
 * The layouts, in proportional terms, so that the same code serves a 296x128
 * panel and a 600x448 one:
 *
 *   standard    SKU name across the top eighth, price centred in the middle
 *               half at the largest scale that fits, unit line along the bottom
 *   promo       as standard, with a badge block in the top right and the
 *               previous price struck through above the current one
 *   unit_price  as standard, with the per-unit comparison given equal weight to
 *               the SKU name, because that is what unit-pricing law requires to
 *               be legible rather than merely present
 *   clearance   as promo, with the badge inverted so it reads as a solid block
 *               from the end of the aisle
 */
int usslp_template_render(const struct usslp_render_request *req)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());
	uint16_t w = d->width;
	uint16_t price_scale;
	uint8_t badge_ink = USSLP_INK_BLACK;
	int rc;

	if (req == NULL || w > USSLP_TEMPLATE_MAX_WIDTH) {
		return USSLP_ERR_INVAL;
	}
	usslp_fb_clear();

	if (d->colors >= 3) {
		/* Red exists on this panel, and a promotional badge is the one thing on
		 * a shelf edge worth spending the second ink plane on. */
		badge_ink = USSLP_INK_RED;
	}

	/* The largest scale at which the price fits the middle half of the panel
	 * with a margin. Computed rather than tabulated because "9.99" and
	 * "1,299.00" are very different widths and a fixed scale would either
	 * overflow the panel or waste half of it. */
	price_scale = 1;
	while (price_scale < 12u) {
		if (text_width(req->price_text, (uint8_t)(price_scale + 1u)) > (w * 9u) / 10u) {
			break;
		}
		price_scale++;
	}

	/* Band 1: the SKU name, top eighth. */
	rc = USSLP_OK;
	{
		uint16_t band_h = BAND_HEIGHT;
		uint16_t y = (uint16_t)(d->height / 16u);

		memset(band, USSLP_INK_WHITE, (size_t)w * band_h);
		draw_text(band, w, band_h, 2, 0, req->name_text, 1, USSLP_INK_BLACK);
		rc |= usslp_fb_pack(band, w, band_h, 0, y);
	}

	/* Band 2: the price, centred in the middle half. */
	{
		uint16_t glyph_h = (uint16_t)(7u * price_scale);
		uint16_t y = (uint16_t)((d->height - glyph_h) / 2u);
		uint16_t x = (uint16_t)((w - text_width(req->price_text, (uint8_t)price_scale)) / 2u);
		uint16_t rows = glyph_h;

		if ((size_t)w * rows > sizeof(band)) {
			/* The price band is taller than the scratch buffer, so it is drawn
			 * in slices. The alternative — sizing the scratch for the largest
			 * panel's tallest band — would cost 19 KB of RAM to save this
			 * loop. */
			for (uint16_t off = 0; off < rows; off += BAND_HEIGHT) {
				uint16_t h = (uint16_t)MIN(BAND_HEIGHT, rows - off);

				memset(band, USSLP_INK_WHITE, (size_t)w * h);
				draw_text(band, w, h, x, -(int32_t)off, req->price_text,
					  (uint8_t)price_scale, USSLP_INK_BLACK);
				rc |= usslp_fb_pack(band, w, h, 0, (uint16_t)(y + off));
			}
		} else {
			memset(band, USSLP_INK_WHITE, (size_t)w * rows);
			draw_text(band, w, rows, x, 0, req->price_text, (uint8_t)price_scale,
				  USSLP_INK_BLACK);
			rc |= usslp_fb_pack(band, w, rows, 0, y);
		}
	}

	/* Band 3: the was-price, struck through, immediately above the price. Most
	 * jurisdictions require the previous price for a was/now claim, and require
	 * it to be visibly superseded rather than merely smaller. */
	if (req->show_was && req->was_text[0] != '\0') {
		uint16_t band_h = BAND_HEIGHT;
		uint16_t y = (uint16_t)(d->height / 2u - 7u * price_scale / 2u - band_h - 2u);
		uint16_t tw = text_width(req->was_text, 1);
		uint16_t x = (uint16_t)((w - tw) / 2u);

		memset(band, USSLP_INK_WHITE, (size_t)w * band_h);
		draw_text(band, w, band_h, x, 0, req->was_text, 1, USSLP_INK_BLACK);
		/* The strike. */
		for (uint16_t px = x; px < x + tw; px++) {
			band[3u * w + px] = USSLP_INK_BLACK;
		}
		rc |= usslp_fb_pack(band, w, band_h, 0, y);
	}

	/* Band 4: the badge, top right. */
	if (req->badge_text[0] != '\0') {
		uint16_t band_h = BAND_HEIGHT + 2u;
		uint16_t tw = (uint16_t)(text_width(req->badge_text, 1) + 4u);
		uint16_t x = (uint16_t)(w > tw + 2u ? w - tw - 2u : 0u);

		memset(band, USSLP_INK_WHITE, (size_t)w * band_h);
		if (req->template_code == USSLP_TEMPLATE_CLEARANCE) {
			/* Inverted: a solid block reads from the end of the aisle, which
			 * is the point of a clearance badge. */
			for (uint16_t row = 0; row < band_h; row++) {
				memset(&band[(size_t)row * w + x], badge_ink, tw);
			}
			draw_text(band, w, band_h, (uint16_t)(x + 2u), 1, req->badge_text, 1,
				  USSLP_INK_WHITE);
		} else {
			draw_text(band, w, band_h, (uint16_t)(x + 2u), 1, req->badge_text, 1,
				  badge_ink);
		}
		rc |= usslp_fb_pack(band, w, band_h, 0, 2);
	}

	/* Band 5: the unit price, bottom. Equal weight to the SKU name because
	 * EU and UK unit-pricing law requires the comparison to be legible, not
	 * merely present. */
	if (req->unit_text[0] != '\0') {
		uint16_t band_h = BAND_HEIGHT;
		uint16_t y = (uint16_t)(d->height - band_h - 2u);

		memset(band, USSLP_INK_WHITE, (size_t)w * band_h);
		draw_text(band, w, band_h, 2, 0, req->unit_text, 1, USSLP_INK_BLACK);
		rc |= usslp_fb_pack(band, w, band_h, 0, y);
	}

	return rc == USSLP_OK ? USSLP_OK : USSLP_ERR_INVAL;
}

/*
 * The commissioning screen. A technician clipping four hundred labels onto a
 * shelf needs to see, without a phone, which ones have taken their planogram
 * assignment and which have not.
 */
int usslp_template_commissioning(const char *serial, const char *state)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());
	uint16_t w = d->width;
	int rc = USSLP_OK;

	if (w > USSLP_TEMPLATE_MAX_WIDTH) {
		return USSLP_ERR_INVAL;
	}
	usslp_fb_clear();

	memset(band, USSLP_INK_WHITE, (size_t)w * BAND_HEIGHT);
	draw_text(band, w, BAND_HEIGHT, 2, 0, "USSLP LABEL", 1, USSLP_INK_BLACK);
	rc |= usslp_fb_pack(band, w, BAND_HEIGHT, 0, (uint16_t)(d->height / 8u));

	memset(band, USSLP_INK_WHITE, (size_t)w * BAND_HEIGHT);
	draw_text(band, w, BAND_HEIGHT, 2, 0, serial, 1, USSLP_INK_BLACK);
	rc |= usslp_fb_pack(band, w, BAND_HEIGHT, 0, (uint16_t)(d->height / 2u));

	memset(band, USSLP_INK_WHITE, (size_t)w * BAND_HEIGHT);
	draw_text(band, w, BAND_HEIGHT, 2, 0, state, 1, USSLP_INK_BLACK);
	rc |= usslp_fb_pack(band, w, BAND_HEIGHT, 0,
			    (uint16_t)(d->height - BAND_HEIGHT - 4u));
	return rc;
}

/*
 * The fault screen.
 *
 * Deliberately *not* used for a failed attestation. A label that cannot verify a
 * price keeps showing the last price it could verify — that is the contract, and
 * replacing a valid price with an error message would take a readable shelf edge
 * and make it unreadable in response to a problem the shopper did not cause.
 * This screen is for faults where the label has nothing valid to show at all: an
 * unprovisioned part, a panel that failed self-test, an exhausted cell detected
 * before the last refresh.
 */
int usslp_template_fault(const char *code, const char *detail)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());
	uint16_t w = d->width;
	int rc = USSLP_OK;

	if (w > USSLP_TEMPLATE_MAX_WIDTH) {
		return USSLP_ERR_INVAL;
	}
	usslp_fb_clear();

	memset(band, USSLP_INK_WHITE, (size_t)w * BAND_HEIGHT);
	draw_text(band, w, BAND_HEIGHT, 2, 0, code, 2,
		  d->colors >= 3 ? USSLP_INK_RED : USSLP_INK_BLACK);
	rc |= usslp_fb_pack(band, w, BAND_HEIGHT, 0, (uint16_t)(d->height / 3u));

	memset(band, USSLP_INK_WHITE, (size_t)w * BAND_HEIGHT);
	draw_text(band, w, BAND_HEIGHT, 2, 0, detail, 1, USSLP_INK_BLACK);
	rc |= usslp_fb_pack(band, w, BAND_HEIGHT, 0, (uint16_t)(d->height * 2u / 3u));
	return rc;
}
