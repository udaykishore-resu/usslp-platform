/*
 * The fuel gauge: two independent estimates and their disagreement.
 *
 * See pmic.h for why there is no coulomb counter. The short version is that a
 * counter costs 3-5 uA continuously — most of this device's entire budget — to
 * measure a quantity it would then get wrong by more than the answer over ten
 * years of accumulated offset.
 *
 * So there are two estimates, and the interesting output is a third thing:
 *
 *   voltage  -> state of charge, through the inverse of the LiMnO2 discharge
 *               curve the platform's model uses. Accurate near the cliff, which
 *               is where the alert threshold is, and nearly useless in the flat
 *               middle.
 *   ledger   -> state of charge, from the firmware's own accumulated
 *               nanoamp-hours divided by the temperature-derated capacity.
 *               Accurate in the middle, and drifts because it cannot see
 *               self-discharge or a cell that was already partly used.
 *   divergence between them, which is neither and is the most useful of the
 *               three: a cell that stops tracking the model is a cold-damaged or
 *               counterfeit cell, and averaging the two estimates would destroy
 *               exactly that signal.
 *
 * The reported percentage takes the voltage estimate below 20% — near the cliff
 * the voltage is the honest one — and the ledger estimate above it, because in
 * the flat region the ledger is the only one that moves at all.
 */

#include "../display/eink.h"
#include "pmic.h"
#include "power.h"

#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_power, CONFIG_USSLP_LOG_LEVEL);

/* A short moving average over the raw readings. The PMIC's monitor is coarse
 * and a single sample can jump a whole step; four samples costs nothing because
 * the gauge is read once per telemetry interval. */
#define GAUGE_HISTORY 4

static uint16_t history[GAUGE_HISTORY];
static uint8_t history_len;
static uint8_t history_pos;
static int8_t divergence_pp;

/* The inverse of usslp_battery_mv: state of charge from a voltage. Written as a
 * search over the forward curve rather than as a second closed form, so the two
 * cannot disagree — a separately derived inverse is a classic place for a
 * firmware and a fleet model to drift apart by a few per cent and then argue
 * about which is right. */
static uint32_t dod_ppm_from_mv(uint16_t mv)
{
	uint32_t lo = 0, hi = 1000000;

	if (mv >= usslp_battery_mv(0)) {
		return 0;
	}
	if (mv <= usslp_battery_mv(1000000)) {
		return 1000000;
	}
	while (hi - lo > 1000u) {
		uint32_t mid = lo + (hi - lo) / 2u;

		if (usslp_battery_mv(mid) > mv) {
			lo = mid;
		} else {
			hi = mid;
		}
	}
	return lo;
}

void usslp_gauge_read(uint16_t *millivolts, uint8_t *percent)
{
	struct usslp_power_ledger ledger;
	struct usslp_projection projection;
	uint16_t mv = 0;
	uint32_t sum = 0;
	uint32_t dod_voltage_ppm;
	uint32_t dod_ledger_ppm = 0;
	uint32_t reported_ppm;

	if (usslp_pmic_read_vbat(&mv) == USSLP_OK && mv > 0u) {
		history[history_pos] = mv;
		history_pos = (uint8_t)((history_pos + 1u) % GAUGE_HISTORY);
		if (history_len < GAUGE_HISTORY) {
			history_len++;
		}
	}
	if (history_len == 0u) {
		/* No reading yet — the panel was busy, or the PMIC is not answering.
		 * Report the fresh-cell voltage rather than zero: a spurious
		 * device.battery.critical across a whole store because one I2C
		 * transaction was refused would be worse than a stale reading. */
		if (millivolts != NULL) {
			*millivolts = usslp_battery_mv(0);
		}
		if (percent != NULL) {
			*percent = 100;
		}
		return;
	}
	for (unsigned i = 0; i < history_len; i++) {
		sum += history[i];
	}
	mv = (uint16_t)(sum / history_len);

	dod_voltage_ppm = dod_ppm_from_mv(mv);

	usslp_power_ledger(&ledger);
	usslp_power_projection(&projection);
	if (projection.usable_capacity_uah > 0u) {
		uint64_t used_nah = ledger.sleep_nah + ledger.beacon_nah + ledger.data_rx_nah +
				    ledger.tx_nah + ledger.refresh_nah + ledger.nfc_nah +
				    ledger.ota_nah;
		uint64_t capacity_nah = (uint64_t)projection.usable_capacity_uah * 1000ull;

		if (used_nah >= capacity_nah) {
			dod_ledger_ppm = 1000000u;
		} else {
			dod_ledger_ppm = (uint32_t)((used_nah * 1000000ull) / capacity_nah);
		}
	}

	{
		int32_t d = (int32_t)(dod_voltage_ppm / 10000u) - (int32_t)(dod_ledger_ppm / 10000u);

		if (d > 127) {
			d = 127;
		}
		if (d < -128) {
			d = -128;
		}
		divergence_pp = (int8_t)d;
	}

	/* Below 20% remaining the voltage estimate is the honest one: that is where
	 * the curve has slope. Above it the ledger is the only estimate that moves.
	 */
	reported_ppm = dod_voltage_ppm > 800000u ? dod_voltage_ppm : dod_ledger_ppm;

	if (millivolts != NULL) {
		*millivolts = mv;
	}
	if (percent != NULL) {
		uint32_t remaining = 1000000u - (reported_ppm > 1000000u ? 1000000u : reported_ppm);

		*percent = (uint8_t)((remaining + 5000u) / 10000u);
	}
}

int8_t usslp_gauge_model_divergence(void)
{
	return divergence_pp;
}
