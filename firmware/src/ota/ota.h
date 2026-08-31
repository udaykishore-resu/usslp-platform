/*
 * ota.h - over-the-air update, over a Zigbee mesh, on a coin cell.
 *
 * The constraints that shape this, in order of how much they matter:
 *
 * 1. A label that does not boot has to be retrieved by hand. Forty thousand
 *    labels in a store, on shelf rails, behind stock. The cost of one bad
 *    rollout is not a support ticket, it is a truck and a week. Every check in
 *    this module exists because of that number.
 *
 * 2. The radio is the most expensive thing the device does. Transmitting a byte
 *    costs orders of magnitude more energy than storing one, and a full image is
 *    a few hundred kilobytes across a mesh shared with price updates. Hence the
 *    delta format (usslp_patch.h): a rollout that moves a fifth of the bytes
 *    costs a fifth of the battery, and battery is what the product is sold on.
 *
 * 3. The mesh is shared. A store-wide rollout that saturated the channel would
 *    stop prices reaching shelves, which is the one thing the platform promises.
 *    So the download is chunked, rate limited against the zone's own traffic, and
 *    gossiped between labels (usslp_chunkmap.h) rather than fanned out from the
 *    controller.
 *
 * The order of operations, and why nothing in it may be reordered:
 *
 *   a. the manifest arrives and is checked against this label's display tier
 *      and current version, before a single flash page is erased
 *   b. chunks are collected into the inactive slot, tracked by an exact bitmap
 *   c. when complete: the delta is applied (if it is a delta), and the result's
 *      SHA-256 is checked against the manifest
 *   d. the assembled image's MCUboot header and TLV trailer are checked in the
 *      inactive slot, before it is marked
 *   e. only then is the slot marked for swap and the device reset
 *   f. the new image runs with the watchdog armed and confirms itself only after
 *      it has joined the mesh and applied one price update; anything else and
 *      the watchdog fires and MCUboot reverts
 *
 * Step (d) needs its scope stated exactly, because it is easy to overclaim. The
 * *authenticity* check is MCUboot's, at boot, against a key held in the
 * bootloader; Zephyr does not expose that verifier to the application and moving
 * it here would mean moving the key here, which would defeat it. What step (d)
 * does is check that the assembled image is structurally an image: header magic,
 * declared size against what was written, TLV trailer present and well formed,
 * and the SHA-256 from step (c) against the manifest.
 *
 * That is worth doing even though it is not the security check, because a
 * corrupt transfer fails it. Without it a label with a corrupt image reboots,
 * fails MCUboot's verification, reverts, and has spent a full download and two
 * reboots of a coin cell to discover something it could have known while still
 * awake. On a fleet of forty thousand that is measurable battery, and on a label
 * near end of life it is the difference between an update and a dead label. A
 * *forged* image passes step (d) and fails at boot, which is the correct
 * division of labour.
 *
 * Step (f)'s definition of "confirmed" is the other one worth arguing about. The
 * weak version is "the image booted". That catches a corrupt flash and nothing
 * else: an image that boots and cannot join the mesh is exactly as unreachable
 * as one that does not boot. So confirmation requires the two things that make a
 * label useful — an association and an applied price — and until both have
 * happened the watchdog is armed.
 */

#ifndef USSLP_OTA_H
#define USSLP_OTA_H

#include "../crypto/usslp_sha256.h"
#include "../usslp_portable.h"

/* The rollout manifest, as it arrives over the cluster. */
struct usslp_ota_manifest {
	uint32_t rollout_id;
	/* Semantic version of the target image. */
	uint8_t version_major;
	uint8_t version_minor;
	uint16_t version_patch;
	/* The display tier the image was built for. An image for the wrong tier is
	 * refused: the waveform tables are per panel and the wrong waveform can mark
	 * a panel permanently. */
	uint8_t display_tier;
	/* Non-zero when the payload is a USDELTA1 patch against the running image
	 * rather than a whole image. */
	uint8_t is_delta;
	uint32_t payload_bytes;
	uint32_t image_bytes;
	uint8_t image_sha256[USSLP_SHA256_DIGEST_LEN];
	/* The version this delta applies to. Checked before anything is erased so a
	 * patch aimed at a different base never gets as far as the flash. */
	uint8_t base_major;
	uint8_t base_minor;
	uint16_t base_patch;
};

enum usslp_ota_state {
	USSLP_OTA_IDLE = 0,
	USSLP_OTA_DOWNLOADING,
	USSLP_OTA_ASSEMBLING,
	USSLP_OTA_VERIFYING,
	USSLP_OTA_READY,   /* verified and marked; waiting for a reset */
	USSLP_OTA_PENDING, /* running a new image that has not yet confirmed */
	USSLP_OTA_FAILED,
};

struct usslp_ota_status {
	enum usslp_ota_state state;
	uint32_t rollout_id;
	uint16_t chunks_total;
	uint16_t chunks_received;
	uint32_t bytes_written;
	uint32_t gossip_suppressed;
	int last_error;
};

int usslp_ota_init(void);

/* Accepts a manifest. Returns USSLP_ERR_UNSUPPORTED for an image built for a
 * different display tier, USSLP_ERR_STALE for a version not newer than the
 * running one, and USSLP_ERR_NOSPACE for an image larger than the slot. */
int usslp_ota_begin(const struct usslp_ota_manifest *m);

/* Accepts one chunk. Idempotent: a chunk already held is discarded, which is
 * what makes at-least-once mesh delivery workable for a transfer as well as for
 * a price. */
int usslp_ota_chunk(uint16_t index, const uint8_t *data, size_t len);

/* Reports whether an announcement should be re-broadcast to neighbours. See
 * usslp_chunkmap.h: a false positive here suppresses one redundant gossip path
 * and costs nothing, which is why a probabilistic filter is allowed for this and
 * for nothing else in the firmware. */
bool usslp_ota_should_relay(uint32_t rollout_id, uint16_t chunk_index);

/* Called when the bitmap is complete: assembles, verifies and marks. */
int usslp_ota_finish(void);

/*
 * Confirms the running image.
 *
 * Called only after the label has joined the mesh and applied one price update.
 * Until it is called the watchdog is armed and a reset reverts to the previous
 * slot. "It booted" is not confirmation: an image that boots and cannot join is
 * exactly as unreachable as one that does not boot.
 */
int usslp_ota_confirm(void);

/* True while running an image that has not yet confirmed itself. */
bool usslp_ota_pending(void);

void usslp_ota_status(struct usslp_ota_status *out);

/* Feeds the confirm-or-revert watchdog. Called from the main loop; when the
 * image is confirmed the watchdog channel is disarmed and this becomes a no-op.
 */
void usslp_ota_watchdog_feed(void);

#endif /* USSLP_OTA_H */
