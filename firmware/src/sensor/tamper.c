#include "tamper.h"

#include "../power/power.h"

#include <zephyr/device.h>
#include <zephyr/drivers/gpio.h>
#include <zephyr/drivers/sensor.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_REGISTER(usslp_sensor, CONFIG_USSLP_LOG_LEVEL);

static const struct device *const accel = DEVICE_DT_GET(DT_ALIAS(accel0));
static const struct gpio_dt_spec led = GPIO_DT_SPEC_GET(DT_ALIAS(locator_led), gpios);

static bool tamper_latched;
static uint32_t tamper_events;
static struct k_work_delayable confirm_work;
static struct k_work_delayable led_work;

/* The gravity vector as measured at the last confirmed-good moment, in
 * milli-g. A label at rest on a shelf rail reads a stable vector; the question
 * the confirmation step asks is whether it still reads the same one a few
 * seconds after a shock. */
static int32_t reference_mg[3];
static bool have_reference;

/* Sustained reorientation threshold, in milli-g. 300 mg is about 17 degrees of
 * tilt, which a label on a rail does not do and a label in somebody's hand
 * always does. */
#define REORIENT_THRESHOLD_MG 300

static int read_vector(int32_t out[3])
{
	struct sensor_value v[3];
	int rc;

	rc = sensor_sample_fetch(accel);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	rc = sensor_channel_get(accel, SENSOR_CHAN_ACCEL_XYZ, v);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	for (unsigned i = 0; i < 3; i++) {
		/* sensor_value is m/s^2; 1 g is 9.80665. Converting to milli-g keeps
		 * the comparison integer. */
		int64_t mms2 = (int64_t)v[i].val1 * 1000000ll + v[i].val2;

		out[i] = (int32_t)(mms2 / 9807ll);
	}
	return USSLP_OK;
}

/*
 * The confirmation step, run a few seconds after a shock.
 *
 * A shock alone is not tamper. A freezer door slamming, a trolley hitting a
 * gondola end and a colleague restocking the shelf all produce one. What
 * distinguishes a label being unclipped is that the gravity vector is different
 * afterwards and stays different, so the interrupt only arms this check and the
 * check is what sets the latch.
 */
static void confirm_fn(struct k_work *work)
{
	int32_t now[3];
	int32_t delta = 0;

	ARG_UNUSED(work);

	if (read_vector(now) != USSLP_OK) {
		return;
	}
	if (!have_reference) {
		memcpy(reference_mg, now, sizeof(reference_mg));
		have_reference = true;
		return;
	}
	for (unsigned i = 0; i < 3; i++) {
		int32_t d = now[i] - reference_mg[i];

		delta += d < 0 ? -d : d;
	}
	if (delta >= REORIENT_THRESHOLD_MG) {
		if (!tamper_latched) {
			LOG_WRN("tamper: the label has been reoriented by %d mg and stayed "
				"there",
				delta);
		}
		tamper_latched = true;
		tamper_events++;
		memcpy(reference_mg, now, sizeof(reference_mg));
		/* A label somebody is handling is a label worth being reachable, and the
		 * platform wants the tamper flag promptly rather than at the next
		 * five-minute telemetry slot. */
		usslp_power_note_activity();
	}
}

static void motion_trigger(const struct device *dev, const struct sensor_trigger *trig)
{
	ARG_UNUSED(dev);
	ARG_UNUSED(trig);

	/* Do nothing here but schedule the confirmation. Reading the sensor in the
	 * trigger callback would sample the label mid-shock, which is exactly when
	 * the reading means least. */
	k_work_reschedule(&confirm_work, K_SECONDS(3));
}

int usslp_tamper_init(void)
{
	struct sensor_trigger trig = {
		.type = SENSOR_TRIG_DELTA,
		.chan = SENSOR_CHAN_ACCEL_XYZ,
	};
	struct sensor_value attr;
	int rc;

	k_work_init_delayable(&confirm_work, confirm_fn);

	if (!device_is_ready(accel)) {
		LOG_ERR("accelerometer not ready; tamper detection is off");
		return USSLP_ERR_IO;
	}

	/* 10 Hz. The lowest rate above power-down, and enough to see a label being
	 * unclipped: this is a "did somebody take this off the rail" question, not a
	 * motion-capture problem. */
	attr.val1 = 10;
	attr.val2 = 0;
	rc = sensor_attr_set(accel, SENSOR_CHAN_ACCEL_XYZ, SENSOR_ATTR_SAMPLING_FREQUENCY,
			     &attr);
	if (rc != 0) {
		LOG_WRN("could not set the accelerometer rate (%d)", rc);
	}

	/* The any-motion threshold, in m/s^2. 3.5 is about 0.36 g: high enough that
	 * shelf vibration and a passing trolley do not wake the MCU, low enough that
	 * a hand on the label does. Every spurious wake here costs the same as a
	 * beacon window, and there are a lot of trolleys. */
	attr.val1 = 3;
	attr.val2 = 500000;
	rc = sensor_attr_set(accel, SENSOR_CHAN_ACCEL_XYZ, SENSOR_ATTR_SLOPE_TH, &attr);
	if (rc != 0) {
		LOG_WRN("could not set the tamper threshold (%d)", rc);
	}

	rc = sensor_trigger_set(accel, &trig, motion_trigger);
	if (rc != 0) {
		LOG_ERR("could not arm the tamper trigger (%d)", rc);
		return USSLP_ERR_IO;
	}

	/* Take the resting orientation now, while the label is being commissioned
	 * and is presumably where it belongs. */
	if (read_vector(reference_mg) == USSLP_OK) {
		have_reference = true;
	}

	if (gpio_is_ready_dt(&led)) {
		(void)gpio_pin_configure_dt(&led, GPIO_OUTPUT_INACTIVE);
	}
	k_work_init_delayable(&led_work, NULL);
	LOG_INF("tamper detection armed");
	return USSLP_OK;
}

bool usslp_tamper_active(void)
{
	return tamper_latched;
}

void usslp_tamper_clear(void)
{
	if (tamper_latched) {
		LOG_INF("tamper latch cleared by a technician");
	}
	tamper_latched = false;
	(void)read_vector(reference_mg);
	have_reference = true;
}

uint32_t usslp_tamper_events(void)
{
	return tamper_events;
}

void usslp_locator_pulse(uint16_t duration_ms)
{
	int64_t deadline;

	if (!gpio_is_ready_dt(&led)) {
		return;
	}
	if (duration_ms > 30000u) {
		duration_ms = 30000u;
	}
	/*
	 * 10% duty at 5 Hz. Visible from the end of an aisle — the eye picks up a
	 * blink far better than a steady light — and a tenth of the charge of
	 * lighting it. Thirty seconds of this costs about 3 uAh, which is a few
	 * hours of the label's life rather than a fortnight.
	 */
	deadline = k_uptime_get() + duration_ms;
	while (k_uptime_get() < deadline) {
		gpio_pin_set_dt(&led, 1);
		k_msleep(20);
		gpio_pin_set_dt(&led, 0);
		k_msleep(180);
	}
}
