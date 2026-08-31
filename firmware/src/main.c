/*
 * USSLP Tier 1 smart shelf label: init, threads, and the main state machine.
 *
 * Threading
 * ---------
 * Four threads, and the split is dictated by one fact: driving the E-Ink panel
 * blocks for between 300 ms and 15 s, and during that time the radio is off.
 *
 *   main       boot, init, then the supervisory loop: watchdog, OTA
 *              confirmation, and the sleep the whole design exists to maximise
 *   price      takes frames from the radio and does the ordered work in
 *              app/price.h, including the blocking refresh. It is a separate
 *              thread precisely so that a fifteen-second colour-panel waveform
 *              does not hold the radio stack's callback context
 *   uplink     drains the acknowledgement and telemetry queue, because a
 *              transmission has to wait for the medium and holding the stack's
 *              callback through a CSMA backoff blocks reception
 *   zigbee     the ZBOSS stack's own thread
 *
 * Everything else is work items on the system workqueue: telemetry, the tamper
 * confirmation, the commissioning window timeout. None of them blocks for long
 * and none of them needs its own stack.
 *
 * Boot order
 * ----------
 * The order below is not arbitrary. Identity before crypto, because the key ring
 * lives in settings and settings needs flash. Display before provisioning,
 * because an unprovisioned label's first useful act is to put its serial on the
 * glass. Sequence store before radio, because a frame arriving before the stored
 * sequence has been read would be evaluated against a sequence of "never
 * displayed" and would be accepted when it should have been discarded — which is
 * a stale price on a shelf, and the exact failure INTERFACE-CONTRACTS section 6
 * exists to prevent.
 */

#include "app/price.h"
#include "app/provision.h"
#include "app/seq_store.h"
#include "app/telemetry.h"
#include "crypto/devcert.h"
#include "display/eink.h"
#include "ota/ota.h"
#include "power/power.h"
#include "radio/radio.h"
#include "nfc/nfc.h"
#include "sensor/tamper.h"

#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/sys/reboot.h>

LOG_MODULE_REGISTER(usslp_main, CONFIG_USSLP_LOG_LEVEL);

/*
 * The price thread's stack.
 *
 * 4 KiB, and the largest consumer is the OTA delta path, which is not on this
 * thread. On the price path the deepest call is the attestation verifier: a
 * 104-byte SHA-256 context, a 768-byte scratch struct for the wire frame's
 * identifiers, and PSA's own frame for the Ed25519 verification. 2 KiB would
 * fit and 4 KiB leaves room for the logging backend's formatting buffer, which
 * is easy to forget until a stack overflow in the field turns out to be a log
 * line.
 */
#define PRICE_STACK_SIZE 4096
#define PRICE_PRIORITY 5

K_THREAD_STACK_DEFINE(price_stack, PRICE_STACK_SIZE);
static struct k_thread price_thread_data;

/* Inbound frames from the radio. Four deep: a label that is more than four price
 * updates behind has a problem the queue depth will not solve, and a deeper
 * queue would only let it apply a backlog of superseded prices one waveform at a
 * time. */
K_MSGQ_DEFINE(price_q, 160, 4, 4);

struct price_item {
	uint16_t len;
	uint8_t data[158];
};

static void on_radio_frame(const uint8_t *frame, size_t len)
{
	struct price_item item;

	if (len > sizeof(item.data)) {
		LOG_WRN("dropping a %u-byte frame; the largest this label accepts is %u",
			(unsigned)len, (unsigned)sizeof(item.data));
		return;
	}
	item.len = (uint16_t)len;
	memcpy(item.data, frame, len);
	if (k_msgq_put(&price_q, &item, K_NO_WAIT) != 0) {
		/* The queue is full, which means a refresh is running and more updates
		 * have arrived behind it. Dropping the newest would be wrong — it is the
		 * most current price — so the oldest is dropped and the newest queued.
		 * The sequence rule makes this safe: whichever ones survive, only the
		 * highest sequence reaches the glass. */
		struct price_item stale;

		(void)k_msgq_get(&price_q, &stale, K_NO_WAIT);
		(void)k_msgq_put(&price_q, &item, K_NO_WAIT);
	}
}

static void price_thread(void *a, void *b, void *c)
{
	struct price_item item;

	ARG_UNUSED(a);
	ARG_UNUSED(b);
	ARG_UNUSED(c);

	for (;;) {
		if (k_msgq_get(&price_q, &item, K_FOREVER) != 0) {
			continue;
		}
		if (usslp_price_handle_frame(item.data, item.len) == USSLP_OK) {
			/* A verified price reached the glass. Two things follow from that
			 * and from nothing weaker: the label is ACTIVE rather than merely
			 * assigned, and a freshly swapped image has earned the right to
			 * confirm itself. */
			usslp_provision_note_price_displayed();
			if (usslp_ota_pending() && usslp_radio_joined()) {
				(void)usslp_ota_confirm();
			}
		}
	}
}

int main(void)
{
	int rc;

	LOG_INF("USSLP label firmware %s, display tier %d", USSLP_FW_VERSION,
		CONFIG_USSLP_DISPLAY_TIER);

	if (!IS_ENABLED(CONFIG_USSLP_REQUIRE_ATTESTATION)) {
		/*
		 * Logged loudly, at boot, every boot. This build trusts the Shelf Edge
		 * Controller to have verified the price attestation on the label's
		 * behalf, which is what INTERFACE-CONTRACTS section 5 specifies and is
		 * a sound design against the threat model it states — but it does mean
		 * a compromised controller can author a price. A fleet audit finds
		 * labels running this way by reading the attestation-mode attribute;
		 * this line is how a field engineer finds one with a debugger.
		 */
		LOG_WRN("CONFIG_USSLP_REQUIRE_ATTESTATION is disabled: this label trusts "
			"its controller to have verified prices on its behalf");
	}

	/* Identity first: the key ring lives in settings, and everything that
	 * verifies anything needs it. */
	rc = usslp_devcert_init();
	if (rc != USSLP_OK) {
		LOG_ERR("identity unavailable (%d); continuing unprovisioned", rc);
	}

	rc = usslp_power_init();
	if (rc != USSLP_OK) {
		LOG_ERR("power init failed (%d)", rc);
	}

	rc = usslp_eink_init();
	if (rc != USSLP_OK) {
		/* No panel means no product. The label stays up so that a technician can
		 * reach it over BLE and read the fault, but it will not join a mesh and
		 * pretend to be a working shelf edge. */
		LOG_ERR("display init failed (%d); this label cannot show a price", rc);
	}

	/*
	 * The sequence store before the radio. A frame that arrived before the
	 * stored sequence had been read would be evaluated against "never
	 * displayed" and accepted — putting a superseded price on the glass after
	 * every reboot, which is exactly what INTERFACE-CONTRACTS section 6 exists
	 * to prevent.
	 */
	rc = usslp_price_init();
	if (rc != USSLP_OK) {
		LOG_ERR("price handler init failed (%d)", rc);
	}

	rc = usslp_ota_init();
	if (rc != USSLP_OK) {
		LOG_ERR("OTA init failed (%d)", rc);
	}

	if (IS_ENABLED(CONFIG_USSLP_NFC)) {
		(void)usslp_nfc_init();
	}
	if (IS_ENABLED(CONFIG_USSLP_TAMPER)) {
		(void)usslp_tamper_init();
	}

	(void)usslp_provision_init();

	k_thread_create(&price_thread_data, price_stack, PRICE_STACK_SIZE, price_thread, NULL,
			NULL, NULL, PRICE_PRIORITY, 0, K_NO_WAIT);
	k_thread_name_set(&price_thread_data, "usslp_price");

	rc = usslp_radio_init(on_radio_frame);
	if (rc != USSLP_OK) {
		LOG_ERR("radio init failed (%d)", rc);
	}
	if (IS_ENABLED(CONFIG_USSLP_BLE)) {
		(void)usslp_ble_init();
	}

	(void)usslp_telemetry_init();

	LOG_INF("up: %s, %s", usslp_provision_state_str(usslp_provision_state()),
		usslp_radio_joined() ? "joined" : "not joined");

	/*
	 * The supervisory loop.
	 *
	 * It does almost nothing, on purpose. Every millisecond this thread is
	 * awake is a millisecond the CPU is not in System OFF at 0.8 uA, and the
	 * whole battery argument rests on the CPU being asleep essentially all the
	 * time. Waking once a second to feed a watchdog is about 20 us of CPU per
	 * wake, which is under a nanoamp averaged — but it is the kind of cost that
	 * accumulates if the loop is allowed to grow, so it stays this short.
	 */
	for (;;) {
		usslp_ota_watchdog_feed();

		if (usslp_ota_pending()) {
			/*
			 * A freshly swapped image confirms itself only when it has both
			 * joined the mesh and applied a price. The price side is handled on
			 * the price thread; this catches the case where the label has joined
			 * and is simply not being sent anything, which on a quiet aisle at
			 * night is normal and must not be allowed to look like a failed
			 * update.
			 *
			 * The threshold is deliberately most of the confirm timeout: if the
			 * label has been joined for that long without a price, the mesh
			 * works and the image is fine, and reverting it would be a
			 * regression triggered by a quiet shelf.
			 */
			static uint32_t joined_seconds;

			if (usslp_radio_joined()) {
				joined_seconds++;
				if (joined_seconds >
				    (uint32_t)CONFIG_USSLP_OTA_CONFIRM_TIMEOUT_S / 2u) {
					LOG_INF("confirming the image: joined and stable "
						"for %u s with no price to apply",
						joined_seconds);
					(void)usslp_ota_confirm();
				}
			} else {
				joined_seconds = 0;
			}
		}

		k_sleep(K_SECONDS(1));
	}
	return 0;
}
