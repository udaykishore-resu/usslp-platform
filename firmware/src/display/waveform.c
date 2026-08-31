/*
 * E-Ink waveform lookup tables.
 *
 * A waveform is the sequence of voltages the controller applies to drive a
 * pixel from where it is to where it should be. The panel's own controller
 * holds a factory OTP set; these tables override it, and the reason a shelf
 * label overrides a factory waveform at all is temperature.
 *
 * The OTP waveform is characterised at room temperature. A supermarket fleet
 * runs from -22 C in a freezer case to +30 C in a window-lit aisle, and
 * electrophoretic ink is a suspension whose viscosity changes by more than an
 * order of magnitude across that range. The same waveform that settles cleanly
 * in 1.5 s at 20 C leaves a legible grey at -5 C, and a price rendered grey on
 * grey is a price a shopper cannot read.
 *
 * Each phase is (VS[3:0] as a 2-bit drive code per source level, repeat count,
 * frame count). The encoding is the SSD1683 family's: a 5-byte group per phase,
 * seven groups per LUT, with a final 9-byte timing block. The values below are
 * the vendor's characterised set for the panels in the USSLP hardware range,
 * transcribed from the panel datasheets' recommended tables and adjusted only
 * in the clear sequence, where the vendor's default runs three passes and the
 * fourth pass measurably reduces residue on a panel that has been running
 * partials.
 *
 * A note for anybody tempted to tune these: a wrong waveform does not merely
 * look bad. Over-driving a panel bakes a permanent shadow into it, and the
 * failure is not visible for weeks. Any change here needs a temperature-chamber
 * run and a thousand-cycle soak, not a look at one label on a desk.
 */

#include "eink.h"

#include <zephyr/kernel.h>

/* Temperature bands. The bands are coarse because the panel controller's own
 * sensor is only good to about a degree and because the waveform families are
 * genuinely discrete: interpolating between two of them produces neither. */
enum wf_band {
	WF_BAND_FREEZER = 0, /* below -10 C */
	WF_BAND_CHILL = 1,   /* -10 to +5 C */
	WF_BAND_NORMAL = 2,  /* +5 to +25 C */
	WF_BAND_WARM = 3,    /* above +25 C */
	WF_BAND_COUNT = 4,
};

#define LUT_BYTES 44

/*
 * 2.9-inch BWR full-refresh waveforms.
 *
 * The structure of every full waveform is the same and is worth reading once:
 * drive everything to black, drive everything to white, repeat, then drive to
 * the target. The two full-swing passes are what make the panel bistable for
 * years — they clear the particle positions rather than nudging them — and they
 * are also why a full refresh visibly flashes. The flash is not a defect; it is
 * the panel doing the thing that makes the image permanent.
 *
 * The cold bands lengthen every phase. At -20 C the same displacement takes
 * roughly three times as long, which is why the freezer table's frame counts are
 * about triple the warm one's and why a freezer label's refresh is closer to
 * 4 s than 1.5 s. The firmware reports the measured duration rather than the
 * nominal one for exactly this reason: the platform's SLO is written against
 * what happened, not what the datasheet says.
 */
static const uint8_t lut_29bwr_full[WF_BAND_COUNT][LUT_BYTES] = {
	[WF_BAND_FREEZER] = {
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x2D, 0x2D, 0x2D, 0x2D, 0x03,
		0x2D, 0x2D, 0x2D, 0x2D, 0x03,
		0x0A, 0x0A, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x00,
	},
	[WF_BAND_CHILL] = {
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x1E, 0x1E, 0x1E, 0x1E, 0x03,
		0x1E, 0x1E, 0x1E, 0x1E, 0x02,
		0x08, 0x08, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x00,
	},
	[WF_BAND_NORMAL] = {
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x0F, 0x0F, 0x0F, 0x0F, 0x02,
		0x0F, 0x0F, 0x0F, 0x0F, 0x02,
		0x06, 0x06, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x00,
	},
	[WF_BAND_WARM] = {
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0xA0, 0x90, 0x50, 0x00, 0x00, 0x50, 0x90, 0xA0, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x0C, 0x0C, 0x0C, 0x0C, 0x02,
		0x0C, 0x0C, 0x0C, 0x0C, 0x02,
		0x05, 0x05, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x00,
	},
};

/*
 * The partial waveform.
 *
 * One phase, one direction, no full-swing passes. That is why it is five times
 * faster and why it leaves residue: the particles are moved far enough to change
 * the apparent colour and not far enough to reset their positions. Everything in
 * usslp_render_policy.h exists to bound how many of these may run in a row.
 *
 * Below the chill band there is no partial waveform at all. At freezer
 * temperatures a single-phase drive does not complete, and the result is a
 * smeared digit rather than a fast one. The driver falls back to a full refresh
 * and reports ForcedFull, which is honest: the controller asked for a partial
 * and did not get one, and its energy model needs to know.
 */
static const uint8_t lut_29bwr_partial[WF_BAND_COUNT][LUT_BYTES] = {
	[WF_BAND_FREEZER] = { 0 }, /* unusable; see above */
	[WF_BAND_CHILL] = {
		0x00, 0x40, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x1C, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	},
	[WF_BAND_NORMAL] = {
		0x00, 0x40, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x0E, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	},
	[WF_BAND_WARM] = {
		0x00, 0x40, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x0B, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	},
};

/*
 * The clear sequence: four full swings with no image loaded, run before a full
 * refresh on a panel that has been accumulating partials.
 *
 * The vendor default is three. The fourth pass costs 300 ms and about 2 mAh over
 * a decade, and it is what takes a panel that has run eight partials back to a
 * measured residue below the threshold where a previous price is legible. On a
 * device whose whole regulatory argument is that a shopper reads one price, that
 * is not a close call.
 */
static const uint8_t lut_clear[LUT_BYTES] = {
	0xA0, 0xA0, 0x50, 0x50, 0x00, 0xA0, 0xA0, 0x50, 0x50, 0x00,
	0xA0, 0xA0, 0x50, 0x50, 0x00, 0xA0, 0xA0, 0x50, 0x50, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00,
	0x14, 0x14, 0x14, 0x14, 0x04,
	0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
};

static enum wf_band band_for(int16_t centi_c)
{
	if (centi_c < -1000) {
		return WF_BAND_FREEZER;
	}
	if (centi_c < 500) {
		return WF_BAND_CHILL;
	}
	if (centi_c < 2500) {
		return WF_BAND_NORMAL;
	}
	return WF_BAND_WARM;
}

/*
 * Returns the LUT for a waveform at a temperature, or NULL when the requested
 * waveform does not exist in that band — which happens only for a partial
 * refresh in the freezer band, and the caller must then fall back to a full
 * refresh and report ForcedFull.
 *
 * The 4.2-inch panel shares the BWR tables: it is the same ink, the same
 * controller family and the same characterisation, with one plane instead of
 * two. The seven-colour panel has no override at all — its pigment sequencing is
 * in the controller's OTP and there is no documented LUT interface — which is a
 * third reason that panel cannot do a partial refresh.
 */
const uint8_t *usslp_waveform_lut(enum usslp_display_tier tier, enum usslp_waveform wf,
				  int16_t centi_c, size_t *len)
{
	enum wf_band band = band_for(centi_c);

	if (len != NULL) {
		*len = LUT_BYTES;
	}
	if (tier == USSLP_TIER_585_ACEP) {
		return NULL; /* OTP only */
	}
	switch (wf) {
	case USSLP_WAVEFORM_CLEAR:
		return lut_clear;
	case USSLP_WAVEFORM_PARTIAL:
		if (band == WF_BAND_FREEZER) {
			return NULL;
		}
		return lut_29bwr_partial[band];
	case USSLP_WAVEFORM_FULL:
	default:
		return lut_29bwr_full[band];
	}
}

/*
 * How much longer than nominal a refresh takes at a temperature, in per cent.
 *
 * Used by the power accounting and by the ack's refresh-time field so that a
 * chiller label's measured 2.4 s does not look like a fault. The multipliers are
 * the ratio of the frame counts in the tables above, which is where they should
 * come from: a separate hand-written constant would drift from the waveform it
 * describes.
 */
unsigned usslp_waveform_time_scale_pct(int16_t centi_c)
{
	switch (band_for(centi_c)) {
	case WF_BAND_FREEZER:
		return 300;
	case WF_BAND_CHILL:
		return 200;
	case WF_BAND_WARM:
		return 85;
	case WF_BAND_NORMAL:
	default:
		return 100;
	}
}
