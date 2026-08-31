/*
 * framebuffer.h - the label's packed image buffer.
 *
 * The Shelf Edge Controller keeps one byte per pixel (edge/sec/framebuffer.go),
 * and says so explicitly: it is a Linux box with memory to spare and the
 * operations it cares about are clearer that way. The label cannot afford that.
 * A 296x128 panel at one byte per pixel is 37,888 bytes; packed into two
 * one-bit planes it is 9,472, and on a part with 256 KB shared between two radio
 * stacks, an OTA window and the E-Ink buffer, that difference decides whether
 * the 4.2-inch tier fits at all.
 *
 * So the wire format is one byte per pixel, the decoder produces one byte per
 * pixel into a small window buffer, and this module packs the window into the
 * planes the panel controller is actually loaded with. The packing is the last
 * step before the SPI transfer and nothing downstream of it ever sees a pixel.
 */

#ifndef USSLP_FRAMEBUFFER_H
#define USSLP_FRAMEBUFFER_H

#include "../usslp_portable.h"
#include "usslp_render_policy.h"

/* A rectangular region, inclusive-exclusive, matching sec.Rect. */
struct usslp_rect {
	uint16_t x0, y0, x1, y1;
};

/* Reports whether a window lies entirely inside the fitted panel. A window that
 * does not is refused rather than clipped: the controller and the label disagree
 * about the geometry, and drawing part of it would leave a half-updated price. */
bool usslp_fb_window_valid(const struct usslp_rect *r);

/*
 * Packs a one-byte-per-pixel window into the panel's bit planes at (x, y).
 *
 * On a BWR panel the black plane takes ink states other than white and red, and
 * the red plane takes red; the panel controller ORs them. On a plain black and
 * white panel every non-white ink becomes black, which is the right degradation:
 * a promotional layout rendered for a BWR label and delivered to a BW one
 * remains legible rather than losing its badge entirely.
 */
int usslp_fb_pack(const uint8_t *pixels, uint16_t w, uint16_t h, uint16_t x, uint16_t y);

/* Fills the whole buffer with white, which is what an E-Ink panel is at rest. */
void usslp_fb_clear(void);

/* The packed planes, for the SPI transfer. plane 0 is black, plane 1 is red. */
const uint8_t *usslp_fb_plane(unsigned index, size_t *len);

/*
 * A 32-bit hash of the current buffer contents.
 *
 * Reported in telemetry so the platform can tell "the label applied the update"
 * from "the label says it applied the update": the controller knows what image
 * it sent and can compare. It is the cheapest possible answer to the question a
 * merchandising audit actually asks, which is whether the glass matches the
 * record.
 */
uint32_t usslp_fb_hash(void);

/*
 * Streaming mode, for the seven-colour panel.
 *
 * 600x448 at four bits per pixel is 134 KB and does not fit. In streaming mode
 * there is no local framebuffer at all: bands are packed and pushed to the panel
 * controller's own RAM as they arrive from the mesh. usslp_fb_pack still works,
 * one band at a time, and usslp_fb_hash returns a running hash of what was
 * pushed rather than of a buffer that does not exist.
 *
 * The consequence is that the colour panel cannot do a partial refresh even in
 * principle: there is nothing local to diff against. That is the second reason,
 * alongside the pigment-stack waveform, and it is why SupportsPartial is false
 * for the tier in both this firmware and the simulator.
 */
bool usslp_fb_streaming(void);

#endif /* USSLP_FRAMEBUFFER_H */
