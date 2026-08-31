/*
 * eink.h - the electrophoretic panel.
 *
 * Three facts about this display drive everything in this header.
 *
 * 1. A refresh is slow and it is *blocking*. 1.5 s for a full waveform on the
 *    2.9-inch panel, 15 s on the colour one. While the waveform runs the charge
 *    pump is drawing 26-35 mA, which is thirty times anything else the device
 *    does, and the radio is off. A label mid-refresh is unreachable, and a frame
 *    arriving in that window is lost rather than queued — the simulator models
 *    this with mesh.SetBusyUntil and the firmware has to behave the same way or
 *    the latency numbers are fiction.
 *
 * 2. A partial refresh is short precisely because it does not drive the
 *    particles fully to their rails. Residue accumulates, and after enough
 *    consecutive partials the previous image is legible behind the current one.
 *    On a shelf label that ghost is a previous *price*: not a cosmetic defect
 *    but a weights-and-measures one, because a shopper can read two prices on
 *    one label. usslp_render_policy.h owns the counter that prevents it.
 *
 * 3. The panel is bistable. It holds its image with no power at all, for years.
 *    That is why a label with a dead cell still shows the last price it was
 *    given, and why "the label is blank" and "the label is stale" are different
 *    faults with different causes.
 */

#ifndef USSLP_EINK_H
#define USSLP_EINK_H

#include "../usslp_portable.h"
#include "usslp_render_policy.h"
#include "usslp_rle.h"

#include <zephyr/kernel.h>

/* Waveform selection. The lookup tables are per panel and per temperature; see
 * waveform.c for why the temperature term is not optional. */
enum usslp_waveform {
	USSLP_WAVEFORM_FULL = 0,
	USSLP_WAVEFORM_PARTIAL = 1,
	/* The clearing sequence run before a full refresh on a panel carrying
	 * accumulated residue. */
	USSLP_WAVEFORM_CLEAR = 2,
};

int usslp_eink_init(void);

/* True while a waveform is running. The radio layer consults this before
 * acknowledging: an ack sent mid-refresh would report a settle time the pixels
 * have not reached. */
bool usslp_eink_busy(void);

/*
 * Loads a decoded window into the framebuffer at (x, y).
 *
 * The pixels are one byte per pixel as usslp_rle_decode produces them; this
 * function packs them into the panel's bit planes. Separating the load from the
 * refresh matters because the load is cheap and interruptible and the refresh is
 * neither: a label can accept an image, discover the attestation fails, and
 * throw the load away without ever having touched the glass.
 */
int usslp_eink_load(const uint8_t *pixels, uint16_t w, uint16_t h, uint16_t x, uint16_t y);

/*
 * Drives the panel and blocks until the pixels have settled.
 *
 * Returns the measured duration in milliseconds, which is what goes into
 * canon.LabelDelivered via the ack: it is the last term of the three-second
 * budget and the only one a retailer can check by looking at a shelf.
 *
 * The caller is responsible for having already: verified the attestation,
 * persisted the sequence, and asked usslp_plan_refresh which waveform to use.
 * This function does not second-guess any of that; it drives what it is told to
 * drive, because a display driver that quietly upgraded a partial to a full
 * refresh would make the ghosting counter a lie.
 */
int usslp_eink_refresh(const struct usslp_refresh_plan *plan, uint16_t *elapsed_ms);

/*
 * Drives the panel to white. Used at commissioning and at decommissioning, and
 * by the field engineer's shell command, because a label showing a price for a
 * SKU that is no longer on the shelf is worse than a blank one.
 */
int usslp_eink_clear(void);

/* The panel this build drives. */
enum usslp_display_tier usslp_eink_tier(void);

/*
 * Temperature in hundredths of a degree, read from the panel controller's own
 * sensor rather than from the MCU die.
 *
 * The distinction is not pedantry. E-Ink waveform timing is strongly
 * temperature dependent — the same waveform that settles in 1.5 s at 20 C leaves
 * a visible grey at -5 C — and the panel in a chiller runs several degrees
 * colder than the MCU next to it, which is self-heating. Using the die
 * temperature to select a waveform is how a fleet ends up with an unreadable
 * freezer aisle.
 */
int usslp_eink_temperature(int16_t *centi_c);

/*
 * The waveform table for a panel at a temperature, from waveform.c.
 *
 * Returns NULL when the requested waveform does not exist in that temperature
 * band — which happens for a partial refresh below about -10 C, where a
 * single-phase drive does not complete and produces a smeared digit rather than
 * a fast one. The caller must then run a full refresh and report ForcedFull, so
 * that the controller's energy model learns it did not get what it asked for.
 */
const uint8_t *usslp_waveform_lut(enum usslp_display_tier tier, enum usslp_waveform wf,
				  int16_t centi_c, size_t *len);

/* How much longer than nominal a refresh takes at a temperature, in per cent.
 * Derived from the frame counts in the tables themselves rather than written
 * down separately, so the two cannot drift. */
unsigned usslp_waveform_time_scale_pct(int16_t centi_c);

#endif /* USSLP_EINK_H */
