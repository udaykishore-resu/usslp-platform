#include "framebuffer.h"

#include "../usslp_crc32c.h"
#include "eink.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_display, CONFIG_USSLP_LOG_LEVEL);

#if CONFIG_USSLP_EINK_PLANES > 0
/* The single largest RAM allocation in the firmware. It is static rather than
 * heap because it is needed for the life of the device and a heap allocation of
 * this size would only move the failure from link time to run time. */
static uint8_t planes[CONFIG_USSLP_EINK_PLANES][CONFIG_USSLP_EINK_PLANE_BYTES];
#endif

/* Running hash for streaming mode, and the cached hash otherwise. */
static uint32_t stream_hash;

bool usslp_fb_streaming(void)
{
	return IS_ENABLED(CONFIG_USSLP_EINK_STREAMING);
}

bool usslp_fb_window_valid(const struct usslp_rect *r)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());

	if (r->x1 <= r->x0 || r->y1 <= r->y0) {
		return false;
	}
	return r->x1 <= d->width && r->y1 <= d->height;
}

void usslp_fb_clear(void)
{
#if CONFIG_USSLP_EINK_PLANES > 0
	/* Zero is white in both planes: the black plane is "drive this pixel to
	 * black" and the red plane is "drive this pixel to red". */
	memset(planes, 0, sizeof(planes));
#endif
	stream_hash = 0;
}

const uint8_t *usslp_fb_plane(unsigned index, size_t *len)
{
#if CONFIG_USSLP_EINK_PLANES > 0
	if (index >= CONFIG_USSLP_EINK_PLANES) {
		if (len != NULL) {
			*len = 0;
		}
		return NULL;
	}
	if (len != NULL) {
		*len = CONFIG_USSLP_EINK_PLANE_BYTES;
	}
	return planes[index];
#else
	(void)index;
	if (len != NULL) {
		*len = 0;
	}
	return NULL;
#endif
}

#if CONFIG_USSLP_EINK_PLANES > 0
static void set_bit(uint8_t *plane, uint32_t stride_bytes, uint16_t x, uint16_t y, bool on)
{
	uint32_t byte = (uint32_t)y * stride_bytes + (uint32_t)(x >> 3);
	uint8_t mask = (uint8_t)(0x80u >> (x & 7u));

	if (on) {
		plane[byte] |= mask;
	} else {
		plane[byte] &= (uint8_t)~mask;
	}
}
#endif

int usslp_fb_pack(const uint8_t *pixels, uint16_t w, uint16_t h, uint16_t x, uint16_t y)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());
	struct usslp_rect r = { .x0 = x, .y0 = y, .x1 = (uint16_t)(x + w), .y1 = (uint16_t)(y + h) };

	if (pixels == NULL || w == 0u || h == 0u) {
		return USSLP_ERR_INVAL;
	}
	if (!usslp_fb_window_valid(&r)) {
		/* The controller and the label disagree about the geometry. Drawing
		 * part of it would leave a half-updated price on the glass, which is
		 * worse than leaving the old one. */
		LOG_ERR("window %ux%u at (%u,%u) does not fit the %s panel", w, h, x, y,
			d->name);
		return USSLP_ERR_INVAL;
	}

	stream_hash = usslp_crc32c_update(stream_hash, pixels, (size_t)w * h);

#if CONFIG_USSLP_EINK_PLANES > 0
	{
		uint32_t stride = ((uint32_t)d->width + 7u) / 8u;

		for (uint16_t row = 0; row < h; row++) {
			for (uint16_t col = 0; col < w; col++) {
				uint8_t ink = pixels[(size_t)row * w + col];
				uint16_t px = (uint16_t)(x + col);
				uint16_t py = (uint16_t)(y + row);

				/* Plane 0 is black: every ink that is not white and not red
				 * drives black. On a two-colour panel that folds red into
				 * black, which keeps a promotional badge legible instead of
				 * dropping it. */
				bool black = ink != USSLP_INK_WHITE;
				bool red = false;

				if (CONFIG_USSLP_EINK_PLANES > 1 && ink == USSLP_INK_RED) {
					black = false;
					red = true;
				}
				set_bit(planes[0], stride, px, py, black);
				if (CONFIG_USSLP_EINK_PLANES > 1) {
					set_bit(planes[1], stride, px, py, red);
				}
			}
		}
	}
#else
	/* Streaming mode: the band goes straight to the panel controller. The
	 * transfer is the driver's job, so this function's contract there is just
	 * "the hash saw these bytes"; eink.c pushes the band in the same call
	 * sequence. */
	(void)d;
#endif
	return USSLP_OK;
}

uint32_t usslp_fb_hash(void)
{
#if CONFIG_USSLP_EINK_PLANES > 0
	uint32_t h = 0;

	for (unsigned i = 0; i < CONFIG_USSLP_EINK_PLANES; i++) {
		h = usslp_crc32c_update(h, planes[i], CONFIG_USSLP_EINK_PLANE_BYTES);
	}
	return h;
#else
	return stream_hash;
#endif
}
