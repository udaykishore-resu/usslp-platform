/*
 * RF energy harvesting.
 *
 * The label carries a rectifier on its NFC antenna and a small storage cap. When
 * a reader gate, a shopper's phone or a stock-taking wand illuminates the label,
 * the rectifier makes a few tens of microwatts available and the PMIC's boost
 * takes it instead of the cell.
 *
 * The honest engineering position, which is why this file is short and its
 * output goes into telemetry rather than into the battery projection:
 *
 * Harvesting is real and it is small. A label at a store entrance under a
 * continuous EAS/RFID gate can harvest on the order of 100 uW, which against a
 * 20 uW average consumption is a genuine contribution and can extend that
 * label's life indefinitely. A label in the middle of an aisle, which is
 * where 95% of them are, sees a phone tap every few days and harvests something
 * like a microamp-hour a year — four parts in a million of the cell.
 *
 * So the platform's seven-to-ten-year claim is made *without* harvesting, and
 * this module exists to measure what actually arrives rather than to justify the
 * claim. usslp_harvest_nah goes into telemetry so a fleet planner can see which
 * store layouts benefit, and if a deployment turns out to harvest usefully that
 * is an observed fact rather than an assumption baked into a projection.
 *
 * Sampling costs more than most labels will ever harvest — the ADC and its
 * reference are about 700 uA while converting — so the rectifier is measured
 * once per telemetry interval, not continuously.
 */

#include "pmic.h"
#include "power.h"

#include <zephyr/drivers/adc.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_power, CONFIG_USSLP_LOG_LEVEL);

#define BOARD_NODE DT_PATH(usslp_board)

static const struct adc_dt_spec harvest_adc = ADC_DT_SPEC_GET_BY_NAME(BOARD_NODE, harvest_sense);

static uint64_t harvested_nah;
static int64_t last_poll_ms;
static bool adc_ready;

int usslp_harvest_init(void)
{
	int rc;

	if (!adc_is_ready_dt(&harvest_adc)) {
		LOG_WRN("harvest sense ADC not ready; harvesting will not be measured");
		return USSLP_ERR_IO;
	}
	rc = adc_channel_setup_dt(&harvest_adc);
	if (rc != 0) {
		LOG_WRN("harvest ADC channel setup failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	adc_ready = true;
	last_poll_ms = k_uptime_get();
	return USSLP_OK;
}

void usslp_harvest_poll(void)
{
	int16_t sample = 0;
	struct adc_sequence seq = {
		.buffer = &sample,
		.buffer_size = sizeof(sample),
	};
	int64_t now = k_uptime_get();
	int64_t elapsed_ms;
	int32_t mv;
	int rc;

	if (!adc_ready) {
		return;
	}
	elapsed_ms = now - last_poll_ms;
	last_poll_ms = now;

	rc = adc_sequence_init_dt(&harvest_adc, &seq);
	if (rc != 0) {
		return;
	}
	rc = adc_read_dt(&harvest_adc, &seq);
	if (rc != 0) {
		return;
	}
	mv = sample;
	rc = adc_raw_to_millivolts_dt(&harvest_adc, &mv);
	if (rc != 0) {
		return;
	}

	/*
	 * The rectifier's output sits across a 1 MOhm bleed, so the current it is
	 * delivering is mv / 1000 in microamps. Attributing the *whole* interval to
	 * the instantaneous reading is an overestimate whenever the field is
	 * intermittent, which it almost always is, so the sample is discounted to a
	 * tenth. That is deliberately pessimistic: a harvesting figure that flatters
	 * the deployment is worse than useless, because it would be used to justify
	 * a longer replacement interval than the cell can support.
	 */
	if (mv > 100) {
		uint64_t na = (uint64_t)mv; /* mv/1000 uA == mv nA */
		uint64_t nah = (na * (uint64_t)elapsed_ms) / (3600000ull * 10ull);

		harvested_nah += nah;
	}
}

uint64_t usslp_harvest_nah(void)
{
	return harvested_nah;
}
