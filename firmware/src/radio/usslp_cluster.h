/*
 * usslp_cluster.h - the USSLP Zigbee application cluster.
 *
 * Zigbee has a Price cluster (0x0700, in the Smart Energy profile). It is not
 * used here, and the reason is worth writing down because "we invented our own
 * cluster" is normally a smell.
 *
 * The SE Price cluster models a utility tariff: it carries rate tiers, block
 * thresholds, and a price in a currency with a trailing-digit multiplier, and it
 * assumes a commissioned Smart Energy network with its own certificate
 * authority. A retail shelf price is a different object: it carries a sequence
 * number the device must enforce monotonicity on, a rendered image, a render
 * template, and — in this firmware — an Ed25519 attestation over a canonical
 * tuple. Encoding that into SE Price attributes would mean a private extension
 * to a standard cluster, which is the worst of both: not interoperable, and
 * confusing to anybody who reads the cluster id and expects a tariff.
 *
 * So USSLP defines a manufacturer-specific cluster in the private range, and the
 * label also implements Basic (0x0000), Identify (0x0003), Power Configuration
 * (0x0001) and OTA Upgrade (0x0019) so that a commissioning tool, a network
 * analyser and a generic Zigbee gateway see a well-formed device rather than a
 * black box.
 */

#ifndef USSLP_CLUSTER_H
#define USSLP_CLUSTER_H

#include "../usslp_portable.h"

/* Manufacturer code, from the Zigbee Alliance's assigned range. */
#define USSLP_MANUFACTURER_CODE 0x1337

/* The application cluster, in the manufacturer-specific range (0xFC00-0xFFFF).
 */
#define USSLP_CLUSTER_ID 0xFC10

/* Attributes. Read-only from the network's point of view except where noted:
 * the label's state is authoritative and a coordinator that could write it
 * could make a label lie about what it is displaying. */
enum usslp_cluster_attr {
	/* int64: the sequence of the price on the glass. INTERFACE-CONTRACTS
	 * section 6's counter, exposed so a controller rebuilding its cache after a
	 * reboot can ask rather than guess. */
	USSLP_ATTR_DISPLAYED_SEQUENCE = 0x0000,
	/* int64 and 3 characters: the price the label believes it is showing. */
	USSLP_ATTR_DISPLAYED_PRICE = 0x0001,
	USSLP_ATTR_DISPLAYED_CURRENCY = 0x0002,
	/* uint32: CRC-32C of the framebuffer. The cheapest answer to the question a
	 * merchandising audit asks — does the glass match the record — because the
	 * controller knows what image it sent. */
	USSLP_ATTR_IMAGE_HASH = 0x0003,
	/* uint8: partials since the last full refresh, so a controller can plan its
	 * diff threshold against the label's real ghosting budget rather than its
	 * own estimate of it. */
	USSLP_ATTR_PARTIALS_SINCE_FULL = 0x0004,
	/* uint8: display tier. An OTA image for the wrong tier is refused, and this
	 * is how the rollout planner knows before it sends one. */
	USSLP_ATTR_DISPLAY_TIER = 0x0005,
	/* uint32: projected life in thousandths of a year, from the label's own
	 * measured workload. */
	USSLP_ATTR_LIFE_MILLIYEARS = 0x0006,
	/* uint8: attestation mode. 0 = trusts the controller, 1 = requires
	 * end-to-end. A fleet audit reads this to find labels running the weaker
	 * mode. */
	USSLP_ATTR_ATTESTATION_MODE = 0x0007,
	/* uint32: attestation failures since boot. Non-zero is a compliance
	 * incident, not a statistic. */
	USSLP_ATTR_ATTESTATION_FAILURES = 0x0008,
	/* bool, writable: the tamper flag, cleared by a technician after
	 * inspection. The only writable attribute, and it clears a fault rather
	 * than asserting one. */
	USSLP_ATTR_TAMPER = 0x0009,
};

/* Commands, client to server (coordinator to label). */
enum usslp_cluster_cmd {
	/* Carries a price update. The payload is the air frame from
	 * radio/usslp_wire.h, so the cluster is a thin envelope and there is exactly
	 * one definition of the price wire format. */
	USSLP_CMD_PRICE_UPDATE = 0x00,
	/* Opens the activity window across a zone ahead of a price load. */
	USSLP_CMD_OPEN_WINDOW = 0x01,
	/* Pulses the locator LED so a picker can find one shelf edge among four
	 * hundred. */
	USSLP_CMD_IDENTIFY = 0x02,
	/* Pushes a signed price-authority key ring bundle. */
	USSLP_CMD_KEYRING = 0x03,
	/* Sets the wall clock from the controller's own. */
	USSLP_CMD_SET_TIME = 0x04,
	/* Clears the panel and the sequence: decommissioning. */
	USSLP_CMD_DECOMMISSION = 0x05,
	/* Requests an immediate telemetry report, for a technician standing in the
	 * aisle who does not want to wait five minutes. */
	USSLP_CMD_REPORT_NOW = 0x06,
};

/* Commands, server to client (label to coordinator). */
enum usslp_cluster_response {
	/* The acknowledgement frame from usslp_wire.h.
	 *
	 * Note for a controller implementation: the ack's status byte now carries
	 * two codes beyond labelsim's original three — 3 for a price refused because
	 * its attestation did not verify, 4 for one refused because this label
	 * requires an end-to-end attestation and was sent an unattested frame — and
	 * the flags byte carries the attestation verdict in bits 2-4. A controller
	 * that does not know them sees "bad frame", which is what it saw before, so
	 * the addition is safe in a mixed fleet. A controller that does know them
	 * can stop inferring compliance incidents from transport errors. */
	USSLP_RSP_ACK = 0x00,
	/* The telemetry frame from usslp_wire.h. */
	USSLP_RSP_TELEMETRY = 0x01,
	/* A compliance alert: an attestation that did not verify. Sent
	 * unsolicited and with a retry, because it is the one message whose loss
	 * matters more than the airtime of resending it. */
	USSLP_RSP_ATTESTATION_FAILURE = 0x02,
};

int usslp_cluster_init(void);

/* Handles one inbound cluster command. Returns USSLP_OK when the command was
 * understood; an unknown command id is not an error at this layer, it is a
 * newer controller talking to an older label and is answered with a default
 * response. */
int usslp_cluster_handle(uint8_t cmd, const uint8_t *payload, size_t len);

/* Sends a server-to-client command. */
int usslp_cluster_send(uint8_t cmd, const uint8_t *payload, size_t len);

#endif /* USSLP_CLUSTER_H */
