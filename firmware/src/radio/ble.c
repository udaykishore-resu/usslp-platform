/*
 * BLE 5.0: the secondary radio.
 *
 * It does three things and, importantly, does not do a fourth:
 *
 *   - commissioning. A technician's phone or handheld associates the label with
 *     a planogram slot before it has ever joined a mesh. Bounded to a window,
 *     because a label advertising a connectable commissioning service forever is
 *     a label anybody in the store can walk up to and talk to.
 *   - field diagnostics. A read-only characteristic carrying the state an
 *     engineer standing in the aisle needs: serial, sequence, battery, parent
 *     link, last attestation verdict.
 *   - the shopper beacon, when a retailer's app wants proximity. Non-connectable
 *     advertising, no identifiers beyond the store's own, at the lowest useful
 *     interval.
 *
 * What it does not do is carry a price. A price has to arrive over a path the
 * platform's mesh accounting can see and the controller can acknowledge, and a
 * phone-delivered price would be a price with no attestation chain and no
 * delivery record. The commissioning characteristic can set a label's identity;
 * it cannot set what the label displays.
 *
 * The antenna is shared with the 802.15.4 radio. Every BLE advertisement is
 * airtime the primary radio does not have, and an advertisement that collided
 * with a beacon window is a price update delayed by a resting interval — up to
 * thirty seconds. So the advertising interval is long, the commissioning window
 * is bounded, and a store that commissions with NFC only can turn this off in
 * Kconfig and get the airtime back.
 */

#include "radio.h"

#include "../app/price.h"
#include "../app/provision.h"
#include "../app/seq_store.h"
#include "../crypto/devcert.h"
#include "../power/power.h"

#include <string.h>
#include <zephyr/bluetooth/bluetooth.h>
#include <zephyr/bluetooth/gatt.h>
#include <zephyr/bluetooth/uuid.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_DECLARE(usslp_radio, CONFIG_USSLP_LOG_LEVEL);

/* USSLP label service: a random 128-bit UUID, not a 16-bit one, because these
 * are not standardised services and squatting on a 16-bit value that the SIG may
 * assign later is how a fleet acquires a field-upgrade problem. */
#define BT_UUID_USSLP_SERVICE_VAL                                                                  \
	BT_UUID_128_ENCODE(0x5f9b3400, 0x0001, 0x4b53, 0x9d55, 0x7553534c5000)
#define BT_UUID_USSLP_STATUS_VAL                                                                   \
	BT_UUID_128_ENCODE(0x5f9b3400, 0x0002, 0x4b53, 0x9d55, 0x7553534c5000)
#define BT_UUID_USSLP_COMMISSION_VAL                                                               \
	BT_UUID_128_ENCODE(0x5f9b3400, 0x0003, 0x4b53, 0x9d55, 0x7553534c5000)

static const struct bt_uuid_128 usslp_service_uuid = BT_UUID_INIT_128(BT_UUID_USSLP_SERVICE_VAL);
static const struct bt_uuid_128 usslp_status_uuid = BT_UUID_INIT_128(BT_UUID_USSLP_STATUS_VAL);
static const struct bt_uuid_128 usslp_commission_uuid =
	BT_UUID_INIT_128(BT_UUID_USSLP_COMMISSION_VAL);

static bool commissioning_open;
static struct k_work_delayable commissioning_close;

/* The diagnostics payload. Deliberately small and fixed: an engineer in an aisle
 * needs the six facts that distinguish the common faults, not a dump. */
struct __packed usslp_ble_status {
	char serial[24];
	int64_t displayed_sequence;
	uint16_t battery_mv;
	uint8_t battery_pct;
	uint8_t parent_lqi;
	int8_t parent_rssi;
	uint8_t joined;
	uint8_t tier;
	uint8_t attestation_mode;
	uint32_t attestation_failures;
	uint32_t image_hash;
};

static ssize_t read_status(struct bt_conn *conn, const struct bt_gatt_attr *attr, void *buf,
			   uint16_t len, uint16_t offset)
{
	const struct usslp_device_identity *id = usslp_devcert_identity();
	struct usslp_price_stats stats;
	struct usslp_ble_status st;

	ARG_UNUSED(conn);
	ARG_UNUSED(attr);

	memset(&st, 0, sizeof(st));
	if (id != NULL) {
		memcpy(st.serial, id->serial, sizeof(st.serial));
		st.tier = id->tier;
	}
	usslp_price_stats(&stats);
	st.displayed_sequence = stats.displayed_sequence;
	st.attestation_failures = stats.attestation_failed;
	st.image_hash = stats.image_hash;
	usslp_power_battery(&st.battery_mv, &st.battery_pct);
	(void)usslp_radio_parent_link(&st.parent_lqi, &st.parent_rssi);
	st.joined = usslp_radio_joined() ? 1u : 0u;
	st.attestation_mode = IS_ENABLED(CONFIG_USSLP_REQUIRE_ATTESTATION) ? 1u : 0u;

	return bt_gatt_attr_read(conn, attr, buf, len, offset, &st, sizeof(st));
}

static ssize_t write_commission(struct bt_conn *conn, const struct bt_gatt_attr *attr,
				const void *buf, uint16_t len, uint16_t offset, uint8_t flags)
{
	ARG_UNUSED(conn);
	ARG_UNUSED(attr);
	ARG_UNUSED(offset);
	ARG_UNUSED(flags);

	if (!commissioning_open) {
		/* Outside the window this characteristic does nothing. The window is
		 * opened by a physical action — an NFC tap or the technician's tool
		 * scanning the printed serial — so possession of the label is part of
		 * the authorisation. */
		return BT_GATT_ERR(BT_ATT_ERR_WRITE_NOT_PERMITTED);
	}
	/* The payload is a signed provisioning assignment; provision.c verifies it
	 * against the price-authority ring before it is applied, so a phone cannot
	 * assign a label to a planogram slot it was not authorised for. */
	if (usslp_provision_apply(buf, len) != USSLP_OK) {
		return BT_GATT_ERR(BT_ATT_ERR_VALUE_NOT_ALLOWED);
	}
	return (ssize_t)len;
}

BT_GATT_SERVICE_DEFINE(usslp_svc,
		       BT_GATT_PRIMARY_SERVICE(&usslp_service_uuid),
		       BT_GATT_CHARACTERISTIC(&usslp_status_uuid.uuid, BT_GATT_CHRC_READ,
					      BT_GATT_PERM_READ, read_status, NULL, NULL),
		       BT_GATT_CHARACTERISTIC(&usslp_commission_uuid.uuid, BT_GATT_CHRC_WRITE,
					      BT_GATT_PERM_WRITE, NULL, write_commission, NULL));

/* Non-connectable by default: the label is discoverable, and nothing more, until
 * somebody with physical access opens the commissioning window. */
static const struct bt_data ad_quiet[] = {
	BT_DATA_BYTES(BT_DATA_FLAGS, BT_LE_AD_NO_BREDR),
	BT_DATA_BYTES(BT_DATA_UUID128_ALL, BT_UUID_USSLP_SERVICE_VAL),
};

static void commissioning_close_fn(struct k_work *work)
{
	ARG_UNUSED(work);
	commissioning_open = false;
	(void)bt_le_adv_stop();
	(void)bt_le_adv_start(BT_LE_ADV_NCONN_IDENTITY, ad_quiet, ARRAY_SIZE(ad_quiet), NULL, 0);
	LOG_INF("commissioning window closed");
}

int usslp_ble_init(void)
{
	int rc;

	rc = bt_enable(NULL);
	if (rc != 0) {
		LOG_ERR("bt_enable failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	k_work_init_delayable(&commissioning_close, commissioning_close_fn);
	return usslp_ble_advertise(true);
}

int usslp_ble_advertise(bool on)
{
	int rc;

	if (!on) {
		return bt_le_adv_stop() == 0 ? USSLP_OK : USSLP_ERR_IO;
	}
	/*
	 * A one-second advertising interval, not the 100 ms a phone accessory would
	 * use. Advertising is airtime the 802.15.4 radio does not have, on a shared
	 * antenna, and an advertisement that collides with a beacon window costs a
	 * price update a whole resting interval. One second is still found in under
	 * three seconds by a scanning handheld.
	 */
	rc = bt_le_adv_start(BT_LE_ADV_PARAM(BT_LE_ADV_OPT_USE_IDENTITY, BT_GAP_ADV_SLOW_INT_MIN,
					     BT_GAP_ADV_SLOW_INT_MAX, NULL),
			     ad_quiet, ARRAY_SIZE(ad_quiet), NULL, 0);
	if (rc != 0 && rc != -EALREADY) {
		LOG_ERR("advertising failed to start (%d)", rc);
		return USSLP_ERR_IO;
	}
	return USSLP_OK;
}

int usslp_ble_commissioning_window(uint32_t seconds)
{
	static const struct bt_data ad_open[] = {
		BT_DATA_BYTES(BT_DATA_FLAGS, BT_LE_AD_GENERAL | BT_LE_AD_NO_BREDR),
		BT_DATA_BYTES(BT_DATA_UUID128_ALL, BT_UUID_USSLP_SERVICE_VAL),
	};
	int rc;

	if (seconds == 0u || seconds > 900u) {
		/* Fifteen minutes is longer than any commissioning task and short
		 * enough that a window left open by accident closes before the
		 * technician has left the store. */
		seconds = 300u;
	}
	(void)bt_le_adv_stop();
	rc = bt_le_adv_start(BT_LE_ADV_CONN_FAST_1, ad_open, ARRAY_SIZE(ad_open), NULL, 0);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	commissioning_open = true;
	k_work_reschedule(&commissioning_close, K_SECONDS(seconds));
	LOG_INF("commissioning window open for %u s", seconds);
	return USSLP_OK;
}
