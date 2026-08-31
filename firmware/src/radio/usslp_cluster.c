#include "usslp_cluster.h"

#include "../app/price.h"
#include "../app/seq_store.h"
#include "../app/telemetry.h"
#include "../crypto/devcert.h"
#include "../display/eink.h"
#include "../power/power.h"
#include "../sensor/tamper.h"
#include "radio.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_radio, CONFIG_USSLP_LOG_LEVEL);

static int64_t rd_i64(const uint8_t *p)
{
	uint64_t v = 0;

	for (unsigned i = 0; i < 8; i++) {
		v = (v << 8) | p[i];
	}
	return (int64_t)v;
}

int usslp_cluster_init(void)
{
	/* Attribute registration with the ZBOSS stack happens in zigbee.c, where the
	 * device context and endpoint descriptors live; this module owns the
	 * semantics rather than the plumbing. */
	return USSLP_OK;
}

int usslp_cluster_handle(uint8_t cmd, const uint8_t *payload, size_t len)
{
	switch (cmd) {
	case USSLP_CMD_PRICE_UPDATE:
		/* Straight through to the price handler, which owns the whole ordered
		 * sequence of checks. Nothing about a price is decided here: a second
		 * place that could accept or reject an update is a second place that
		 * could get the order wrong. */
		return usslp_price_handle_frame(payload, len);

	case USSLP_CMD_OPEN_WINDOW: {
		uint32_t seconds = 60;

		if (len >= 2u) {
			seconds = ((uint32_t)payload[0] << 8) | payload[1];
		}
		if (seconds > 3600u) {
			seconds = 3600u; /* an hour is already a long price load */
		}
		usslp_power_open_window(seconds * 1000u);
		LOG_INF("zone wake: fast listening for %u s", seconds);
		return USSLP_OK;
	}

	case USSLP_CMD_IDENTIFY: {
		uint16_t ms = 3000;

		if (len >= 2u) {
			ms = (uint16_t)(((uint16_t)payload[0] << 8) | payload[1]);
		}
		/* The LED is a measurable battery cost — a lit indicator shortens a
		 * seven-year cell — so it is pulsed, bounded, and only ever on request.
		 */
		usslp_locator_pulse(ms);
		return USSLP_OK;
	}

	case USSLP_CMD_KEYRING:
		return usslp_price_keyring_update(payload, len);

	case USSLP_CMD_SET_TIME:
		if (len < 8u) {
			return USSLP_ERR_MALFORMED;
		}
		usslp_power_set_unix_time(rd_i64(payload));
		return USSLP_OK;

	case USSLP_CMD_DECOMMISSION: {
		struct usslp_ghost_state ghost;
		int rc;

		/* Clearing the panel is part of decommissioning, not an afterthought: a
		 * label showing a price for a SKU that is no longer on the shelf is
		 * worse than a blank one, and a label that has forgotten its sequence
		 * must not keep displaying what it has forgotten. */
		rc = usslp_seq_store_reset(&ghost);
		if (rc == USSLP_OK) {
			rc = usslp_eink_clear();
		}
		return rc;
	}

	case USSLP_CMD_REPORT_NOW:
		usslp_telemetry_report_now();
		return USSLP_OK;

	default:
		/* A newer controller talking to an older label. Not an error: answered
		 * with a default response and logged once, because a fleet mid-rollout
		 * will generate a lot of these and they are not faults. */
		LOG_DBG("unknown cluster command 0x%02x", cmd);
		return USSLP_ERR_UNSUPPORTED;
	}
}

int usslp_cluster_send(uint8_t cmd, const uint8_t *payload, size_t len)
{
	uint8_t frame[96];

	if (len + 1u > sizeof(frame)) {
		return USSLP_ERR_NOSPACE;
	}
	frame[0] = cmd;
	memcpy(&frame[1], payload, len);
	return usslp_radio_send_uplink(frame, len + 1u);
}
