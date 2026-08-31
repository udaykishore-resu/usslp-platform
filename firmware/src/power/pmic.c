#include "pmic.h"

#include "../display/eink.h"

#include <zephyr/device.h>
#include <zephyr/drivers/gpio.h>
#include <zephyr/drivers/i2c.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_power, CONFIG_USSLP_LOG_LEVEL);

#define PMIC_NODE DT_ALIAS(pmic0)
#define BOARD_NODE DT_PATH(usslp_board)

static const struct i2c_dt_spec pmic = I2C_DT_SPEC_GET(PMIC_NODE);
static const struct gpio_dt_spec panel_rail =
	GPIO_DT_SPEC_GET(BOARD_NODE, panel_power_gpios);

/* BQ25125 register map, the subset this firmware touches. */
#define REG_STATUS 0x00
#define REG_FAULT 0x01
#define REG_TS_CONTROL 0x02
#define REG_FAST_CHARGE 0x03
#define REG_TERM_PRECHG 0x04
#define REG_BATT_VOLTAGE 0x05
#define REG_SYS_VOUT 0x06
#define REG_LOAD_LDO 0x07
#define REG_PUSHBUTTON 0x08
#define REG_ILIM_UVLO 0x09
#define REG_VBAT_MONITOR 0x0A
#define REG_VIN_DPM 0x0B

/* VBAT_MONITOR returns the cell voltage as a fraction of the programmed battery
 * regulation voltage, in eight steps of 2% from 60% to 100%, plus a two-bit
 * fine field. It is a coarse reading by design: the part is built for a
 * rechargeable cell where the charger cares about thresholds, not for gauging a
 * primary cell. The firmware compensates by averaging several reads and by
 * treating the result as one of two independent estimates rather than as truth
 * (see the header). */
#define VBAT_MON_READ_TRIGGER 0x80
#define VBAT_REGULATION_MV 3200

static bool pmic_ready;

static int write_reg(uint8_t reg, uint8_t val)
{
	return i2c_reg_write_byte_dt(&pmic, reg, val);
}

static int read_reg(uint8_t reg, uint8_t *val)
{
	return i2c_reg_read_byte_dt(&pmic, reg, val);
}

int usslp_pmic_init(void)
{
	uint8_t status = 0;
	int rc;

	if (!i2c_is_ready_dt(&pmic)) {
		LOG_ERR("PMIC I2C not ready");
		return USSLP_ERR_IO;
	}
	if (!gpio_is_ready_dt(&panel_rail)) {
		return USSLP_ERR_IO;
	}
	rc = gpio_pin_configure_dt(&panel_rail, GPIO_OUTPUT_INACTIVE);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	rc = read_reg(REG_STATUS, &status);
	if (rc != 0) {
		LOG_ERR("PMIC did not answer (%d)", rc);
		return USSLP_ERR_IO;
	}

	/* A primary cell is never charged. Disabling the charge path is not a
	 * nicety: enabling it into a LiMnO2 cell is a fire risk, and the part powers
	 * up with charging enabled. This is the first thing the firmware does to the
	 * PMIC, before anything else can go wrong. */
	rc = write_reg(REG_FAST_CHARGE, 0x00);
	/* Disable the termination and pre-charge timers for the same reason. */
	rc |= write_reg(REG_TERM_PRECHG, 0x00);
	/* Undervoltage lockout at 2.0 V. Below it the boost cannot hold the E-Ink
	 * rails and a refresh would leave a half-driven image, so the label stops
	 * refreshing before it stops working. */
	rc |= write_reg(REG_ILIM_UVLO, 0x02);
	if (rc != 0) {
		LOG_ERR("configuring the PMIC failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	pmic_ready = true;
	LOG_INF("PMIC ready, status 0x%02x, charging disabled for the primary cell",
		status);
	return USSLP_OK;
}

int usslp_pmic_panel_rail(bool on)
{
	if (!pmic_ready) {
		return USSLP_ERR_IO;
	}
	return gpio_pin_set_dt(&panel_rail, on ? 1 : 0) == 0 ? USSLP_OK : USSLP_ERR_IO;
}

int usslp_pmic_radio_power(bool on)
{
	if (!pmic_ready) {
		return USSLP_ERR_IO;
	}
	/* The LDO feeding the radio co-processor. Cutting it is a last resort: a
	 * cold start costs a full mesh rejoin, which is tens of seconds of the
	 * zone's channel and a hole in the label's coverage. */
	return write_reg(REG_LOAD_LDO, on ? 0x80 : 0x00) == 0 ? USSLP_OK : USSLP_ERR_IO;
}

int usslp_pmic_read_vbat(uint16_t *millivolts)
{
	uint8_t raw = 0;
	int rc;

	if (!pmic_ready || millivolts == NULL) {
		return USSLP_ERR_IO;
	}
	if (usslp_eink_busy()) {
		/* The charge pump's 30 mA pulse drops the cell several hundred
		 * millivolts through its internal resistance. A gauge that sampled here
		 * would declare a healthy cell critical every time a price changed. */
		return USSLP_ERR_BUSY;
	}
	rc = write_reg(REG_VBAT_MONITOR, VBAT_MON_READ_TRIGGER);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	/* The conversion takes about 2 ms. */
	k_msleep(3);
	rc = read_reg(REG_VBAT_MONITOR, &raw);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	{
		/* Bits 7:5 are the coarse threshold in 2% steps from 60%, bits 4:3 the
		 * fine field in 0.5% steps. */
		unsigned coarse = (raw >> 5) & 0x07u;
		unsigned fine = (raw >> 3) & 0x03u;
		unsigned pct_x10 = 600u + coarse * 20u + fine * 5u;

		*millivolts = (uint16_t)(((uint32_t)VBAT_REGULATION_MV * pct_x10) / 1000u);
	}
	return USSLP_OK;
}
