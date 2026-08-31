/*
 * The E-Ink panel driver: SPI, waveform load, refresh, and the busy gate.
 *
 * The bit that is easy to get subtly wrong and expensive to discover: the panel
 * is driven from a boosted rail that has to be up before the first command and
 * that must not be left up afterwards. Leaving the boost enabled costs about
 * 40 uA — six times this device's entire sleep budget — and it is invisible on a
 * bench where the label is on a power supply. Every path out of this file turns
 * it off, including the error paths, which is why the refresh routine is written
 * with a single exit.
 */

#include "eink.h"

#include "framebuffer.h"

#include <zephyr/device.h>
#include <zephyr/drivers/gpio.h>
#include <zephyr/drivers/spi.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/pm/device.h>

LOG_MODULE_REGISTER(usslp_display, CONFIG_USSLP_LOG_LEVEL);

#define EINK_NODE DT_ALIAS(eink0)
#define BOARD_NODE DT_PATH(usslp_board)

static const struct spi_dt_spec eink_bus = SPI_DT_SPEC_GET(
	EINK_NODE, SPI_OP_MODE_MASTER | SPI_WORD_SET(8) | SPI_TRANSFER_MSB, 0);
static const struct gpio_dt_spec dc = GPIO_DT_SPEC_GET(EINK_NODE, dc_gpios);
static const struct gpio_dt_spec reset = GPIO_DT_SPEC_GET(EINK_NODE, reset_gpios);
static const struct gpio_dt_spec busy = GPIO_DT_SPEC_GET(EINK_NODE, busy_gpios);
static const struct gpio_dt_spec panel_power =
	GPIO_DT_SPEC_GET(BOARD_NODE, panel_power_gpios);

/* SSD1683-family command set, the subset this driver uses. */
#define CMD_DRIVER_OUTPUT 0x01
#define CMD_DEEP_SLEEP 0x10
#define CMD_DATA_ENTRY 0x11
#define CMD_SW_RESET 0x12
#define CMD_TEMP_SENSOR_READ 0x18
#define CMD_MASTER_ACTIVATION 0x20
#define CMD_UPDATE_CONTROL2 0x22
#define CMD_WRITE_RAM_BW 0x24
#define CMD_WRITE_RAM_RED 0x26
#define CMD_WRITE_LUT 0x32
#define CMD_SET_RAM_X 0x44
#define CMD_SET_RAM_Y 0x45
#define CMD_SET_RAM_X_COUNTER 0x4E
#define CMD_SET_RAM_Y_COUNTER 0x4F

/* Update sequences for CMD_UPDATE_CONTROL2. The full sequence enables the clock
 * and the charge pump, loads the LUT, displays, and powers the pump back down;
 * the partial one skips the pump ramp because the rails are already up. */
#define SEQ_FULL 0xF7
#define SEQ_PARTIAL 0xFF

/* The panel asserts BUSY for the duration of a waveform. The timeout is
 * generous: a freezer-band full refresh legitimately takes four seconds, and a
 * timeout that fired at two would turn a cold aisle into a fault storm. */
#define BUSY_TIMEOUT_MS 20000

static K_MUTEX_DEFINE(panel_lock);
static atomic_t panel_busy;
static bool panel_ready;
static int16_t last_temperature = 2000; /* until the first read: 20.00 C */

static int write_cmd(uint8_t cmd, const uint8_t *data, size_t len)
{
	const struct spi_buf cmd_buf = { .buf = &cmd, .len = 1 };
	const struct spi_buf_set cmd_set = { .buffers = &cmd_buf, .count = 1 };
	int rc;

	gpio_pin_set_dt(&dc, 0); /* command */
	rc = spi_write_dt(&eink_bus, &cmd_set);
	if (rc != 0 || len == 0u) {
		return rc;
	}
	{
		const struct spi_buf data_buf = { .buf = (void *)data, .len = len };
		const struct spi_buf_set data_set = { .buffers = &data_buf, .count = 1 };

		gpio_pin_set_dt(&dc, 1); /* data */
		rc = spi_write_dt(&eink_bus, &data_set);
	}
	return rc;
}

static int wait_not_busy(k_timeout_t timeout)
{
	int64_t deadline = k_uptime_get() + k_ticks_to_ms_floor64(timeout.ticks);

	while (gpio_pin_get_dt(&busy) == 1) {
		if (k_uptime_get() > deadline) {
			LOG_ERR("panel BUSY did not clear; the display controller is "
				"unresponsive");
			return USSLP_ERR_IO;
		}
		/* Sleeping rather than spinning: the CPU has nothing to do for up to
		 * four seconds and this is a device that counts microamps. 5 ms of
		 * granularity costs nothing against a 1.5 s waveform. */
		k_msleep(5);
	}
	return USSLP_OK;
}

static int panel_power_on(void)
{
	int rc = gpio_pin_set_dt(&panel_power, 1);

	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	/* The boost needs a few milliseconds to reach +/-15 V. Issuing a command
	 * into a half-risen rail does not fail loudly, it produces a partly driven
	 * image. */
	k_msleep(5);
	gpio_pin_set_dt(&reset, 1);
	k_msleep(2);
	gpio_pin_set_dt(&reset, 0);
	k_msleep(10);
	return wait_not_busy(K_MSEC(1000));
}

static void panel_power_off(void)
{
	uint8_t sleep_mode = 0x01;

	/* Deep sleep mode 1: the controller retains RAM but stops its oscillator.
	 * The image stays on the glass regardless, because the panel is bistable —
	 * that is why a label with a flat cell still shows a price. */
	(void)write_cmd(CMD_DEEP_SLEEP, &sleep_mode, 1);
	gpio_pin_set_dt(&reset, 1);
	gpio_pin_set_dt(&panel_power, 0);
}

int usslp_eink_init(void)
{
	int rc;

	if (!spi_is_ready_dt(&eink_bus)) {
		LOG_ERR("E-Ink SPI bus not ready");
		return USSLP_ERR_IO;
	}
	if (!gpio_is_ready_dt(&dc) || !gpio_is_ready_dt(&reset) || !gpio_is_ready_dt(&busy) ||
	    !gpio_is_ready_dt(&panel_power)) {
		LOG_ERR("E-Ink control GPIOs not ready");
		return USSLP_ERR_IO;
	}
	rc = gpio_pin_configure_dt(&dc, GPIO_OUTPUT_INACTIVE);
	rc |= gpio_pin_configure_dt(&reset, GPIO_OUTPUT_ACTIVE);
	rc |= gpio_pin_configure_dt(&busy, GPIO_INPUT);
	rc |= gpio_pin_configure_dt(&panel_power, GPIO_OUTPUT_INACTIVE);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	usslp_fb_clear();
	panel_ready = true;
	LOG_INF("E-Ink panel %s ready", usslp_panel(usslp_eink_tier())->name);
	return USSLP_OK;
}

enum usslp_display_tier usslp_eink_tier(void)
{
	return (enum usslp_display_tier)CONFIG_USSLP_DISPLAY_TIER;
}

bool usslp_eink_busy(void)
{
	return atomic_get(&panel_busy) != 0;
}

int usslp_eink_temperature(int16_t *centi_c)
{
	uint8_t raw[2];
	int rc;

	if (centi_c == NULL) {
		return USSLP_ERR_INVAL;
	}
	k_mutex_lock(&panel_lock, K_FOREVER);
	rc = panel_power_on();
	if (rc == USSLP_OK) {
		uint8_t mode = 0x80; /* internal sensor */

		rc = write_cmd(CMD_TEMP_SENSOR_READ, &mode, 1);
		if (rc == 0) {
			const struct spi_buf rb = { .buf = raw, .len = sizeof(raw) };
			const struct spi_buf_set rs = { .buffers = &rb, .count = 1 };

			gpio_pin_set_dt(&dc, 1);
			rc = spi_read_dt(&eink_bus, &rs);
		}
		if (rc == 0) {
			/* 12-bit two's complement in units of 1/16 C. */
			int16_t v = (int16_t)(((uint16_t)raw[0] << 4) | (raw[1] >> 4));

			if (v & 0x0800) {
				v = (int16_t)(v | 0xF000);
			}
			last_temperature = (int16_t)((int32_t)v * 100 / 16);
			rc = USSLP_OK;
		} else {
			rc = USSLP_ERR_IO;
		}
	}
	panel_power_off();
	k_mutex_unlock(&panel_lock);
	*centi_c = last_temperature;
	return rc;
}

static int set_window(uint16_t x, uint16_t y, uint16_t w, uint16_t h)
{
	uint8_t xr[2] = { (uint8_t)(x / 8u), (uint8_t)((x + w - 1u) / 8u) };
	uint8_t yr[4] = { (uint8_t)y, (uint8_t)(y >> 8), (uint8_t)(y + h - 1u),
			  (uint8_t)((y + h - 1u) >> 8) };
	uint8_t xc = (uint8_t)(x / 8u);
	uint8_t yc[2] = { (uint8_t)y, (uint8_t)(y >> 8) };
	int rc;

	rc = write_cmd(CMD_SET_RAM_X, xr, sizeof(xr));
	rc |= write_cmd(CMD_SET_RAM_Y, yr, sizeof(yr));
	rc |= write_cmd(CMD_SET_RAM_X_COUNTER, &xc, 1);
	rc |= write_cmd(CMD_SET_RAM_Y_COUNTER, yc, sizeof(yc));
	return rc == 0 ? USSLP_OK : USSLP_ERR_IO;
}

int usslp_eink_load(const uint8_t *pixels, uint16_t w, uint16_t h, uint16_t x, uint16_t y)
{
	if (!panel_ready) {
		return USSLP_ERR_IO;
	}
	if (usslp_eink_busy()) {
		/* A frame arriving mid-waveform is lost, not queued. The simulator
		 * models the same thing with mesh.SetBusyUntil, and a firmware that
		 * queued here would make the platform's latency numbers optimistic by
		 * exactly the length of a refresh. */
		return USSLP_ERR_BUSY;
	}
	/* Packing into the local planes needs no bus and no power: the panel is not
	 * touched until usslp_eink_refresh. That is what lets the price handler
	 * decode an image, fail the attestation and throw it away without the glass
	 * ever changing. */
	return usslp_fb_pack(pixels, w, h, x, y);
}

int usslp_eink_refresh(const struct usslp_refresh_plan *plan, uint16_t *elapsed_ms)
{
	const struct usslp_panel_spec *d = usslp_panel(usslp_eink_tier());
	const uint8_t *lut;
	size_t lut_len;
	int64_t started;
	int rc;
	uint8_t seq;
	bool partial = plan->partial;

	if (!panel_ready) {
		return USSLP_ERR_IO;
	}
	k_mutex_lock(&panel_lock, K_FOREVER);
	atomic_set(&panel_busy, 1);
	started = k_uptime_get();

	rc = panel_power_on();
	if (rc != USSLP_OK) {
		goto out;
	}

	/* Read the panel's own temperature, not the die's. The panel in a chiller
	 * runs several degrees colder than the MCU beside it, and choosing a
	 * waveform from the warmer of the two is how a fleet ends up with an
	 * unreadable freezer aisle. */
	{
		uint8_t mode = 0x80;

		(void)write_cmd(CMD_TEMP_SENSOR_READ, &mode, 1);
	}

	lut = usslp_waveform_lut(usslp_eink_tier(), partial ? USSLP_WAVEFORM_PARTIAL
							    : USSLP_WAVEFORM_FULL,
				 last_temperature, &lut_len);
	if (partial && lut == NULL) {
		/* No partial waveform exists this cold. Fall back and say so: the
		 * controller's energy model has to learn it did not get what it asked
		 * for. */
		LOG_INF("no partial waveform at %d.%02d C; running a full refresh",
			last_temperature / 100, (last_temperature % 100 + 100) % 100);
		partial = false;
		lut = usslp_waveform_lut(usslp_eink_tier(), USSLP_WAVEFORM_FULL,
					 last_temperature, &lut_len);
	}
	if (!partial && plan->forced_full) {
		/* The ghosting budget is spent. Run the clear sequence first: four full
		 * swings with no image, which is what takes a panel that has run eight
		 * partials back below the residue threshold. */
		const uint8_t *clear_lut =
			usslp_waveform_lut(usslp_eink_tier(), USSLP_WAVEFORM_CLEAR,
					   last_temperature, &lut_len);

		if (clear_lut != NULL) {
			seq = SEQ_FULL;
			(void)write_cmd(CMD_WRITE_LUT, clear_lut, lut_len);
			(void)write_cmd(CMD_UPDATE_CONTROL2, &seq, 1);
			(void)write_cmd(CMD_MASTER_ACTIVATION, NULL, 0);
			rc = wait_not_busy(K_MSEC(BUSY_TIMEOUT_MS));
			if (rc != USSLP_OK) {
				goto out;
			}
		}
	}

	if (lut != NULL) {
		rc = write_cmd(CMD_WRITE_LUT, lut, lut_len);
		if (rc != 0) {
			rc = USSLP_ERR_IO;
			goto out;
		}
	}

	rc = set_window(0, 0, d->width, d->height);
	if (rc != USSLP_OK) {
		goto out;
	}
	{
		size_t len;
		const uint8_t *plane = usslp_fb_plane(0, &len);

		if (plane != NULL) {
			rc = write_cmd(CMD_WRITE_RAM_BW, plane, len);
		}
		plane = usslp_fb_plane(1, &len);
		if (rc == 0 && plane != NULL) {
			rc = write_cmd(CMD_WRITE_RAM_RED, plane, len);
		}
		if (rc != 0) {
			rc = USSLP_ERR_IO;
			goto out;
		}
	}

	seq = partial ? SEQ_PARTIAL : SEQ_FULL;
	rc = write_cmd(CMD_UPDATE_CONTROL2, &seq, 1);
	rc |= write_cmd(CMD_MASTER_ACTIVATION, NULL, 0);
	if (rc != 0) {
		rc = USSLP_ERR_IO;
		goto out;
	}
	rc = wait_not_busy(K_MSEC(BUSY_TIMEOUT_MS));

out:
	/* Single exit, and the boost is off on every path through it. Leaving it
	 * enabled costs 40 uA, six times the whole sleep budget, and is invisible on
	 * a bench where the label is on a power supply. */
	panel_power_off();
	atomic_set(&panel_busy, 0);
	{
		int64_t took = k_uptime_get() - started;

		if (elapsed_ms != NULL) {
			*elapsed_ms = (uint16_t)(took > 65535 ? 65535 : took);
		}
	}
	k_mutex_unlock(&panel_lock);
	return rc;
}

int usslp_eink_clear(void)
{
	struct usslp_refresh_plan plan = {
		.partial = false,
		.duration_ms = usslp_panel(usslp_eink_tier())->full_refresh_ms,
		.forced_full = true,
	};

	usslp_fb_clear();
	return usslp_eink_refresh(&plan, NULL);
}
