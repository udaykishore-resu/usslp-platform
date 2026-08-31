#include "telemetry.h"

#include "../display/eink.h"
#include "../nfc/nfc.h"
#include "../ota/ota.h"
#include "../power/pmic.h"
#include "../power/power.h"
#include "../radio/radio.h"
#include "../radio/usslp_wire.h"
#include "../sensor/tamper.h"
#include "price.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/random/random.h>

LOG_MODULE_REGISTER(usslp_telemetry, CONFIG_USSLP_LOG_LEVEL);

static uint32_t attestation_failures;
static uint32_t uplink_drops;
static uint32_t joins;
static enum usslp_attest_verdict last_verdict = USSLP_ATTEST_OK;
static int64_t boot_ms;

static void report_fn(struct k_work *work);
static K_WORK_DELAYABLE_DEFINE(report_work, report_fn);

/* The jittered interval. Without the jitter every label in a zone reports on the
 * same second and turns an idle channel into a five-minute spike. */
static k_timeout_t next_interval(void)
{
	uint32_t base_ms = (uint32_t)CONFIG_USSLP_TELEMETRY_INTERVAL_S * 1000u;
	uint32_t span = base_ms / 5u; /* +/- 20% */
	uint32_t jitter = sys_rand32_get() % (span * 2u + 1u);

	return K_MSEC(base_ms - span + jitter);
}

static void report_fn(struct k_work *work)
{
	struct usslp_telemetry frame;
	struct usslp_price_stats price;
	uint8_t buf[USSLP_TELEMETRY_BYTES];
	uint16_t mv;
	uint8_t pct;
	int16_t centi_c = 2000;
	uint8_t lqi = 0;
	int8_t rssi = -128;

	ARG_UNUSED(work);

	usslp_price_stats(&price);
	usslp_power_battery(&mv, &pct);
	(void)usslp_eink_temperature(&centi_c);
	(void)usslp_radio_parent_link(&lqi, &rssi);
	usslp_harvest_poll();

	memset(&frame, 0, sizeof(frame));
	frame.battery_mv = mv;
	frame.battery_pct = pct;
	frame.temperature_centi_c = centi_c;
	frame.parent_lqi = lqi;
	frame.parent_rssi = rssi;
	frame.refresh_count = price.applied;
	frame.nfc_tap_count = usslp_nfc_taps();
	frame.uptime_sec = (uint32_t)((k_uptime_get() - boot_ms) / 1000);
	frame.tamper = usslp_tamper_active();

	if (usslp_wire_encode_telemetry(&frame, buf, sizeof(buf)) == USSLP_TELEMETRY_BYTES) {
		(void)usslp_radio_send_uplink(buf, USSLP_TELEMETRY_BYTES);
	}

	/* The link assessment runs on the same cadence, which is also the sampling
	 * cadence the predictive model's coefficients were fitted against. Sampling
	 * more often would not make the model better and would cost beacon windows.
	 */
	usslp_radio_assess_links();

	/* A daily retune. Not more often: the workload estimate needs a day of
	 * evidence to be worth anything, and changing the resting interval churns
	 * the mesh's expectations about when this label is reachable. */
	{
		static int64_t last_retune_ms;
		int64_t now = k_uptime_get();

		if (now - last_retune_ms > 86400000ll) {
			last_retune_ms = now;
			usslp_power_retune();
		}
	}

	if (attestation_failures > 0u) {
		/* Repeated on every report until it is acknowledged, because this is the
		 * one signal whose loss matters more than the airtime of resending it.
		 * INTERFACE-CONTRACTS section 5's guarantee — that an attacker can
		 * suppress a price change but not author one, and that suppression is
		 * visible within three missed heartbeats — depends on this reaching the
		 * platform. */
		LOG_WRN("%u attestation failures since boot; last verdict: %s",
			attestation_failures, usslp_attest_verdict_str(last_verdict));
	}

	k_work_reschedule(&report_work, next_interval());
}

int usslp_telemetry_init(void)
{
	boot_ms = k_uptime_get();
	(void)usslp_harvest_init();
	/* The first report is jittered too, so a store powering up after an outage
	 * does not have forty thousand labels report in the same second. */
	k_work_reschedule(&report_work, next_interval());
	return USSLP_OK;
}

void usslp_telemetry_report_now(void)
{
	k_work_reschedule(&report_work, K_NO_WAIT);
}

void usslp_telemetry_note_attestation_failure(enum usslp_attest_verdict verdict)
{
	attestation_failures++;
	last_verdict = verdict;
	/* Reported immediately rather than at the next slot. A price that did not
	 * verify is a compliance incident, and the difference between hearing about
	 * it now and hearing about it in five minutes is the difference between an
	 * operator who can act and one who is reading history. */
	usslp_telemetry_report_now();
}

void usslp_telemetry_note_uplink_drop(void)
{
	uplink_drops++;
}

void usslp_telemetry_note_join(void)
{
	joins++;
	/*
	 * An association is one of the two conditions for confirming a freshly
	 * swapped image. The other is an applied price, checked in the main loop:
	 * "it booted" is not confirmation, because an image that boots and cannot
	 * join is exactly as unreachable as one that does not boot.
	 */
	LOG_INF("joined (%u times since boot)", joins);
}

uint32_t usslp_telemetry_updates_per_day_milli(void)
{
	struct usslp_price_stats price;
	int64_t up_ms = k_uptime_get() - boot_ms;
	uint64_t per_day_milli;

	if (up_ms < 3600000ll) {
		/* Less than an hour of evidence. A label that took three updates in its
		 * first ten minutes is being commissioned, not repriced every three
		 * minutes, and extrapolating from that would have it choose a resting
		 * interval measured in minutes. */
		return 10000u;
	}
	usslp_price_stats(&price);
	per_day_milli = ((uint64_t)price.applied * 86400000ull * 1000ull) / (uint64_t)up_ms;
	if (per_day_milli < 100ull) {
		per_day_milli = 100ull; /* never model a label as taking no updates */
	}
	if (per_day_milli > 200000ull) {
		per_day_milli = 200000ull; /* 200 a day is already pathological */
	}
	return (uint32_t)per_day_milli;
}
