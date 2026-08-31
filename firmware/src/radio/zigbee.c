/*
 * The Zigbee 3.0 stack binding.
 *
 * The stack is ZBOSS, from the nRF Connect SDK. The 802.15.4 PHY/MAC runs on the
 * CC2652P co-processor over a UART with hardware flow control, and the nRF52840
 * runs the network and application layers.
 *
 * Why the split, since the nRF52840 has a perfectly good 802.15.4 radio of its
 * own: the co-processor can hold a receive window open at 6.5 mA with the
 * nRF52840 in System OFF at 0.8 uA, and asserting CTS to wake it only when a
 * frame actually arrives. Beacon listening is more than half this device's
 * entire energy budget (power/usslp_budget.h), so moving it to a part that can
 * do it without waking the application MCU is the single largest saving in the
 * design. A single-chip build would be cheaper in bill of materials and would
 * not reach the battery target.
 *
 * The duty cycle is not the stack's. ZBOSS has its own sleepy-end-device
 * polling, and it is not used: the interval here is chosen by power.c from the
 * label's own measured workload and its target life, and the co-processor is
 * armed for one window at a time. Letting the stack's poll rate decide would
 * put the battery projection under the control of a Kconfig default.
 */

#include "radio.h"

#include "../app/telemetry.h"
#include "../crypto/devcert.h"
#include "../power/power.h"
#include "usslp_cluster.h"
#include "usslp_wire.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

#if defined(CONFIG_ZIGBEE)
#include <zboss_api.h>
#include <zboss_api_addons.h>
#include <zigbee/zigbee_app_utils.h>
#include <zigbee/zigbee_error_handler.h>
#endif

LOG_MODULE_REGISTER(usslp_radio, CONFIG_USSLP_LOG_LEVEL);

#define USSLP_DEVICE_ID 0x0402 /* ZCL device id: a manufacturer-specific sensor */
#define USSLP_DEVICE_VERSION 1

static usslp_radio_rx_cb rx_cb;
static bool joined;
static uint8_t parent_lqi;
static int8_t parent_rssi = -128;
static struct usslp_link_history parent_history;
static struct usslp_neighbour neighbours[USSLP_MAX_NEIGHBOURS];
static size_t neighbour_count;
static K_MUTEX_DEFINE(radio_lock);

/* Uplink queue. Small: a label has at most an ack and a telemetry report in
 * flight, and a deeper queue would only let a partitioned label accumulate stale
 * acknowledgements to deliver after the controller has already given up. */
K_MSGQ_DEFINE(uplink_q, 96, 4, 4);

struct uplink_item {
	uint8_t len;
	uint8_t data[95];
};

int usslp_radio_init(usslp_radio_rx_cb on_frame)
{
	const struct usslp_device_identity *id = usslp_devcert_identity();

	rx_cb = on_frame;
	usslp_link_history_init(&parent_history);

	if (id == NULL) {
		LOG_WRN("no device identity; not joining a network");
		return USSLP_ERR_UNSUPPORTED;
	}

#if defined(CONFIG_ZIGBEE)
	{
		zb_ieee_addr_t addr;

		for (unsigned i = 0; i < 8; i++) {
			addr[i] = (zb_uint8_t)(id->eui64 >> (i * 8));
		}
		zb_set_long_address(addr);
		/* An end device, not a router: a battery label that routed for its
		 * neighbours would have to keep its receiver on and would last months
		 * rather than years. The relay build sets this the other way. */
		if (IS_ENABLED(CONFIG_USSLP_ZIGBEE_ROUTER)) {
			zb_set_network_router_role(ZB_TRANSCEIVER_ALL_CHANNELS_MASK);
			zb_set_rx_on_when_idle(ZB_TRUE);
		} else {
			zb_set_network_ed_role(ZB_TRANSCEIVER_ALL_CHANNELS_MASK);
			zb_set_rx_on_when_idle(ZB_FALSE);
			/* The stack's own polling is disabled: the interval comes from
			 * power.c, which derives it from this label's measured workload and
			 * target life. */
			zb_set_ed_timeout(ED_AGING_TIMEOUT_256MIN);
			zb_set_keepalive_timeout(
				ZB_MILLISECONDS_TO_BEACON_INTERVAL(60000));
		}
		zb_set_nvram_erase_at_start(ZB_FALSE);
		zigbee_erase_persistent_storage(ZB_FALSE);
		usslp_cluster_init();
		zigbee_enable();
	}
#else
	LOG_WRN("built without the Zigbee stack; the radio is inert");
#endif
	return USSLP_OK;
}

bool usslp_radio_joined(void)
{
	return joined;
}

int usslp_radio_parent_link(uint8_t *lqi, int8_t *rssi_dbm)
{
	k_mutex_lock(&radio_lock, K_FOREVER);
	if (lqi != NULL) {
		*lqi = parent_lqi;
	}
	if (rssi_dbm != NULL) {
		*rssi_dbm = parent_rssi;
	}
	k_mutex_unlock(&radio_lock);
	return joined ? USSLP_OK : USSLP_ERR_IO;
}

size_t usslp_radio_neighbours(struct usslp_neighbour *out, size_t cap)
{
	size_t n;

	k_mutex_lock(&radio_lock, K_FOREVER);
	n = neighbour_count < cap ? neighbour_count : cap;
	memcpy(out, neighbours, n * sizeof(*out));
	k_mutex_unlock(&radio_lock);
	return n;
}

int usslp_radio_send_uplink(const uint8_t *frame, size_t len)
{
	struct uplink_item item;

	if (frame == NULL || len == 0u || len > sizeof(item.data)) {
		return USSLP_ERR_INVAL;
	}
	item.len = (uint8_t)len;
	memcpy(item.data, frame, len);
	if (k_msgq_put(&uplink_q, &item, K_NO_WAIT) != 0) {
		/* The queue is full, which means the label has been unable to transmit
		 * for a while. Dropping the oldest would deliver a stale acknowledgement
		 * after the controller has already retried; dropping this one is
		 * honest and is counted. */
		usslp_telemetry_note_uplink_drop();
		return USSLP_ERR_BUSY;
	}
	return USSLP_OK;
}

/*
 * The transmit worker. It runs on its own thread rather than in the radio
 * callback because a transmission has to wait for the medium, and holding the
 * stack's callback context through a CSMA backoff blocks reception.
 */
static void uplink_thread(void *a, void *b, void *c)
{
	struct uplink_item item;

	ARG_UNUSED(a);
	ARG_UNUSED(b);
	ARG_UNUSED(c);

	for (;;) {
		if (k_msgq_get(&uplink_q, &item, K_FOREVER) != 0) {
			continue;
		}
		if (!joined) {
			/* Not associated. The frame is dropped rather than held: a price
			 * acknowledgement that arrives after the controller has retried is
			 * worse than none, and a telemetry report is superseded by the next
			 * one in five minutes. */
			usslp_telemetry_note_uplink_drop();
			continue;
		}
		usslp_power_enter(USSLP_POWER_TX);
#if defined(CONFIG_ZIGBEE)
		{
			zb_bufid_t buf = zb_buf_get_out();

			if (buf != 0) {
				uint8_t *p = zb_buf_initial_alloc(buf, item.len);

				memcpy(p, item.data, item.len);
				ZB_ZCL_SEND_CMD(buf, 0x0000, ZB_APS_ADDR_MODE_16_ENDP_PRESENT,
						1, CONFIG_USSLP_ZIGBEE_ENDPOINT,
						ZB_AF_HA_PROFILE_ID, USSLP_CLUSTER_ID,
						NULL);
			}
		}
#endif
		/* Airtime for the accounting, from the same model the simulator uses. */
		k_usleep((uint32_t)usslp_airtime_us(item.len));
		usslp_power_exit(USSLP_POWER_TX);
	}
}

K_THREAD_DEFINE(usslp_uplink_tid, 1024, uplink_thread, NULL, NULL, NULL, 6, 0, 0);

/*
 * Called by the stack for each inbound application frame.
 */
void usslp_radio_on_frame(const uint8_t *data, size_t len)
{
	usslp_power_enter(USSLP_POWER_DATA_RX);
	usslp_power_note_activity();
	if (rx_cb != NULL) {
		rx_cb(data, len);
	}
	usslp_power_exit(USSLP_POWER_DATA_RX);
}

int usslp_radio_rejoin(void)
{
	LOG_INF("rejoining the mesh");
	joined = false;
#if defined(CONFIG_ZIGBEE)
	zb_bdb_reset_via_local_action(0);
#endif
	return USSLP_OK;
}

void usslp_radio_assess_links(void)
{
	struct usslp_link_assessment a;
	uint16_t mv;
	uint8_t pct;
	usslp_q16 battery_fraction;
	int64_t now_s = k_uptime_get() / 1000;

	if (!joined) {
		return;
	}
	k_mutex_lock(&radio_lock, K_FOREVER);
	usslp_link_history_add(&parent_history, (int32_t)now_s,
			       (usslp_q16)((int32_t)parent_lqi * USSLP_Q16_ONE),
			       (usslp_q16)((int32_t)parent_rssi * USSLP_Q16_ONE));
	k_mutex_unlock(&radio_lock);

	usslp_power_battery(&mv, &pct);
	battery_fraction = (usslp_q16)(((int32_t)pct * USSLP_Q16_ONE) / 100);

	/* The risk threshold: 0.7. Below it the model is not confident enough that
	 * the reroute is worth its cost — a reroute invalidates a route and costs a
	 * discovery round trip — and the reactive rule still covers a link that has
	 * actually failed. */
	a = usslp_assess_link(USSLP_HEAL_PREDICTIVE, &parent_history, battery_fraction, 1,
			      (usslp_q16)(USSLP_Q16_ONE * 7 / 10));
	if (a.act) {
		LOG_WRN("uplink assessed as failing (%s); rejoining", a.why);
		usslp_radio_rejoin();
	}
}

#if defined(CONFIG_ZIGBEE)
/*
 * The ZBOSS signal handler. Association, leave, and the steering outcome.
 */
void zboss_signal_handler(zb_bufid_t bufid)
{
	zb_zdo_app_signal_hdr_t *sig_hdr = NULL;
	zb_zdo_app_signal_type_t sig = zb_get_app_signal(bufid, &sig_hdr);
	zb_ret_t status = ZB_GET_APP_SIGNAL_STATUS(bufid);

	switch (sig) {
	case ZB_BDB_SIGNAL_DEVICE_FIRST_START:
	case ZB_BDB_SIGNAL_DEVICE_REBOOT:
	case ZB_BDB_SIGNAL_STEERING:
		if (status == RET_OK) {
			joined = true;
			LOG_INF("associated with the zone coordinator");
			usslp_telemetry_note_join();
		} else {
			joined = false;
			LOG_WRN("association failed (%d); retrying", (int)status);
		}
		break;
	case ZB_ZDO_SIGNAL_LEAVE:
		joined = false;
		LOG_WRN("left the network");
		break;
	default:
		break;
	}
	zigbee_default_signal_handler(bufid);
	if (bufid != 0) {
		zb_buf_free(bufid);
	}
}
#endif
