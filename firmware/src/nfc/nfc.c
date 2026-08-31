#include "nfc.h"

#include "../app/price.h"
#include "../crypto/devcert.h"
#include "../power/power.h"

#include <stdio.h>
#include <string.h>
#include <zephyr/device.h>
#include <zephyr/drivers/gpio.h>
#include <zephyr/drivers/i2c.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_REGISTER(usslp_nfc, CONFIG_USSLP_LOG_LEVEL);

#define NFC_NODE DT_ALIAS(nfctag0)

static const struct i2c_dt_spec tag = I2C_DT_SPEC_GET(NFC_NODE);
static const struct gpio_dt_spec gpo = GPIO_DT_SPEC_GET(NFC_NODE, gpo_gpios);

/* ST25DV user memory starts at 0x0000 on the user I2C address. The capability
 * container occupies the first four bytes and must be written once, at
 * manufacture; the firmware writes it defensively on first boot because a tag
 * with a blank CC is invisible to a phone and the failure looks like a hardware
 * fault. */
#define CC_ADDR 0x0000
#define NDEF_ADDR 0x0004

static struct gpio_callback gpo_cb;
static uint32_t tap_count;
static bool nfc_ready;

/* An I2C write to the tag's EEPROM takes about 5 ms per 4-byte page and must not
 * overlap an RF access. The ST25DV arbitrates and returns a NACK, so a write is
 * retried rather than assumed. */
static int tag_write(uint16_t addr, const uint8_t *data, size_t len)
{
	uint8_t buf[34];
	size_t off = 0;

	while (off < len) {
		size_t n = len - off;
		int rc;
		int tries = 0;

		if (n > 32u) {
			n = 32u;
		}
		buf[0] = (uint8_t)((addr + off) >> 8);
		buf[1] = (uint8_t)(addr + off);
		memcpy(&buf[2], data + off, n);
		do {
			rc = i2c_write_dt(&tag, buf, n + 2u);
			if (rc == 0) {
				break;
			}
			/* A NACK here almost always means a phone is holding the RF
			 * interface. Backing off and retrying is right; failing would leave
			 * the tag carrying a stale price. */
			k_msleep(6);
		} while (++tries < 5);
		if (rc != 0) {
			return USSLP_ERR_IO;
		}
		k_msleep(6);
		off += n;
	}
	return USSLP_OK;
}

static void gpo_handler(const struct device *dev, struct gpio_callback *cb, uint32_t pins)
{
	ARG_UNUSED(dev);
	ARG_UNUSED(cb);
	ARG_UNUSED(pins);

	tap_count++;
	/* A label somebody is standing in front of is a label worth being able to
	 * update quickly, so a tap opens the activity window. The read itself is
	 * field powered and does not need the MCU at all. */
	usslp_power_note_activity();
}

int usslp_nfc_init(void)
{
	/* NFC Forum Type 5 capability container: magic 0xE1, version 1.0 with read
	 * access granted and write access denied over RF, memory size in blocks,
	 * and the MBREAD feature bit. Write access is denied deliberately: a tag a
	 * phone can write to is a tag anybody can put a different price into. */
	static const uint8_t cc[4] = { 0xE1, 0x40, 0x40, 0x01 };
	uint8_t existing[4] = { 0 };
	int rc;

	if (!i2c_is_ready_dt(&tag)) {
		LOG_ERR("NFC tag I2C not ready");
		return USSLP_ERR_IO;
	}
	rc = i2c_burst_read_dt(&tag, CC_ADDR, existing, sizeof(existing));
	if (rc == 0 && existing[0] != 0xE1) {
		LOG_WRN("the NFC capability container is blank; writing it");
		rc = tag_write(CC_ADDR, cc, sizeof(cc));
		if (rc != USSLP_OK) {
			return rc;
		}
	}

	if (gpio_is_ready_dt(&gpo)) {
		rc = gpio_pin_configure_dt(&gpo, GPIO_INPUT);
		rc |= gpio_pin_interrupt_configure_dt(&gpo, GPIO_INT_EDGE_TO_ACTIVE);
		if (rc == 0) {
			gpio_init_callback(&gpo_cb, gpo_handler, BIT(gpo.pin));
			gpio_add_callback(gpo.port, &gpo_cb);
		}
	}
	nfc_ready = true;
	return usslp_nfc_publish_commissioning();
}

/* Builds a Type 5 NDEF message: a URI record and a text record. */
static size_t build_ndef(uint8_t *out, size_t cap, const char *uri_body, const char *text)
{
	size_t uri_len = strlen(uri_body);
	size_t text_len = strlen(text);
	size_t payload = 0;
	size_t n = 0;

	/* TLV: type 0x03 (NDEF message), then the length, then the message. */
	payload = (3u + 1u + uri_len) + (3u + 3u + text_len);
	if (payload + 4u > cap || payload > 254u) {
		return 0;
	}
	out[n++] = 0x03;
	out[n++] = (uint8_t)payload;

	/* URI record: MB=1, ME=0, SR=1, TNF=1 (well known), type 'U'. The 0x04
	 * prefix byte is "https://", which is what saves the eight bytes that matter
	 * on a tag with 512 bytes of user memory. */
	out[n++] = 0x91;
	out[n++] = 0x01;
	out[n++] = (uint8_t)(uri_len + 1u);
	out[n++] = 'U';
	out[n++] = 0x04;
	memcpy(&out[n], uri_body, uri_len);
	n += uri_len;

	/* Text record: MB=0, ME=1, SR=1, TNF=1, type 'T', language "en". */
	out[n++] = 0x51;
	out[n++] = 0x01;
	out[n++] = (uint8_t)(text_len + 3u);
	out[n++] = 'T';
	out[n++] = 0x02;
	out[n++] = 'e';
	out[n++] = 'n';
	memcpy(&out[n], text, text_len);
	n += text_len;

	out[n++] = 0xFE; /* terminator TLV */
	return n;
}

int usslp_nfc_publish_price(int64_t price_minor, const char *currency, int64_t sequence)
{
	const struct usslp_device_identity *id = usslp_devcert_identity();
	char uri[128];
	char text[64];
	char money[24];
	uint8_t ndef[224];
	size_t n;

	if (!nfc_ready) {
		return USSLP_ERR_IO;
	}
	usslp_format_money(price_minor, currency, money, sizeof(money));

	/* The URI carries the serial rather than the SKU. The label knows its own
	 * serial for certain; the SKU it holds is whatever the last update said, and
	 * a shopper following a link to the wrong product page is a worse failure
	 * than one following a link that the retailer's own service resolves. */
	snprintf(uri, sizeof(uri), "usslp.example/l/%s", id != NULL ? id->serial : "UNKNOWN");
	snprintf(text, sizeof(text), "%s seq %lld", money, (long long)sequence);

	n = build_ndef(ndef, sizeof(ndef), uri, text);
	if (n == 0u) {
		return USSLP_ERR_NOSPACE;
	}
	return tag_write(NDEF_ADDR, ndef, n);
}

int usslp_nfc_publish_commissioning(void)
{
	const struct usslp_device_identity *id = usslp_devcert_identity();
	char uri[128];
	char text[64];
	uint8_t ndef[224];
	size_t n;

	if (!nfc_ready) {
		return USSLP_ERR_IO;
	}
	snprintf(uri, sizeof(uri), "usslp.example/provision/%s",
		 id != NULL ? id->serial : "UNKNOWN");
	snprintf(text, sizeof(text), "UNPROVISIONED %s", id != NULL ? id->serial : "");
	n = build_ndef(ndef, sizeof(ndef), uri, text);
	if (n == 0u) {
		return USSLP_ERR_NOSPACE;
	}
	return tag_write(NDEF_ADDR, ndef, n);
}

uint32_t usslp_nfc_taps(void)
{
	return tap_count;
}
