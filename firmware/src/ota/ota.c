#include "ota.h"

#include "../crypto/devcert.h"
#include "../power/power.h"
#include "slots.h"
#include "usslp_chunkmap.h"
#include "usslp_patch.h"

#include <string.h>
#include <zephyr/dfu/mcuboot.h>
#include <zephyr/drivers/watchdog.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/storage/flash_map.h>
#include <zephyr/sys/reboot.h>

LOG_MODULE_REGISTER(usslp_ota, CONFIG_USSLP_LOG_LEVEL);

/*
 * The DEFLATE history window.
 *
 * 32 KiB, which is 12.5% of the device's RAM, and it exists only while an update
 * is being assembled. It is allocated from a dedicated k_heap rather than
 * declared static so that the 99.99% of the device's life that is not an update
 * does not pay for it: a static array would push the link-time RAM figure past
 * what the 4.2-inch tier's framebuffer leaves free.
 *
 * There is no way around the 32 KiB itself. DEFLATE back-references reach
 * 32,768 bytes by definition, and a decoder with a smaller window is a decoder
 * that fails on some valid streams — which on an update path means a rollout
 * that works in test and fails in the field on whichever build happens to
 * produce a long match.
 */
K_HEAP_DEFINE(ota_heap, USSLP_INFLATE_WINDOW + 512);

static struct usslp_ota_manifest manifest;
static struct usslp_ota_status status;
static struct usslp_chunk_bitmap chunks;
static struct usslp_bloom gossip;
static K_MUTEX_DEFINE(ota_lock);

static const struct device *wdt;
static int wdt_channel = -1;

int usslp_ota_init(void)
{
	memset(&status, 0, sizeof(status));
	usslp_bloom_init(&gossip);

	if (!boot_is_img_confirmed()) {
		/*
		 * Running an image that has been swapped in but not confirmed. Arm the
		 * watchdog: if this image cannot get far enough to confirm itself, the
		 * reset it causes reverts to the slot that was working.
		 */
		struct wdt_timeout_cfg cfg = {
			.window.min = 0,
			.window.max = CONFIG_USSLP_OTA_CONFIRM_TIMEOUT_S * 1000u,
			.callback = NULL,
			.flags = WDT_FLAG_RESET_SOC,
		};

		status.state = USSLP_OTA_PENDING;
		wdt = DEVICE_DT_GET(DT_NODELABEL(wdt0));
		if (device_is_ready(wdt)) {
			wdt_channel = wdt_install_timeout(wdt, &cfg);
			if (wdt_channel >= 0) {
				(void)wdt_setup(wdt, WDT_OPT_PAUSE_HALTED_BY_DBG);
				LOG_WRN("running an unconfirmed image; the watchdog will "
					"revert it in %d s unless it joins the mesh and "
					"applies a price",
					CONFIG_USSLP_OTA_CONFIRM_TIMEOUT_S);
			}
		} else {
			/* No watchdog means no automatic revert, which makes a bad image
			 * permanent. Refusing to run at all would be worse — the label is
			 * already booted and showing a price — so this is logged loudly and
			 * the platform sees it in telemetry. */
			LOG_ERR("no watchdog device: this unconfirmed image cannot be "
				"automatically reverted");
		}
	}
	return USSLP_OK;
}

bool usslp_ota_pending(void)
{
	return status.state == USSLP_OTA_PENDING;
}

void usslp_ota_watchdog_feed(void)
{
	if (wdt != NULL && wdt_channel >= 0) {
		(void)wdt_feed(wdt, wdt_channel);
	}
}

int usslp_ota_confirm(void)
{
	int rc;

	if (status.state != USSLP_OTA_PENDING) {
		return USSLP_OK;
	}
	rc = boot_write_img_confirmed();
	if (rc != 0) {
		LOG_ERR("confirming the image failed (%d); it will revert on the next "
			"reset",
			rc);
		return USSLP_ERR_IO;
	}
	status.state = USSLP_OTA_IDLE;
	if (wdt != NULL && wdt_channel >= 0) {
		/* Zephyr's watchdog API has no per-channel disarm on the nRF52840's
		 * WDT — the peripheral cannot be stopped once started — so the channel
		 * keeps being fed by the main loop for the life of the boot. That is
		 * harmless and is why the feed lives in the main loop rather than in the
		 * OTA path. */
		usslp_ota_watchdog_feed();
	}
	LOG_INF("image confirmed: joined the mesh and applied a price");
	return USSLP_OK;
}

static bool version_is_newer(const struct usslp_ota_manifest *m)
{
	const uint8_t cur_major = 1, cur_minor = 5;
	const uint16_t cur_patch = 0;

	if (m->version_major != cur_major) {
		return m->version_major > cur_major;
	}
	if (m->version_minor != cur_minor) {
		return m->version_minor > cur_minor;
	}
	return m->version_patch > cur_patch;
}

int usslp_ota_begin(const struct usslp_ota_manifest *m)
{
	int rc;

	if (m == NULL) {
		return USSLP_ERR_INVAL;
	}
	k_mutex_lock(&ota_lock, K_FOREVER);

	/*
	 * The tier check comes first, before the version check and long before any
	 * flash is touched. An image built for the 4.2-inch panel flashed onto a
	 * 2.9-inch one drives the wrong waveform tables, and the wrong waveform can
	 * bake a permanent shadow into a panel — a failure that is not visible for
	 * weeks and is not recoverable at all.
	 */
	if (m->display_tier != CONFIG_USSLP_DISPLAY_TIER) {
		LOG_ERR("refusing rollout %u: built for display tier %u, this label is "
			"tier %d",
			m->rollout_id, m->display_tier, CONFIG_USSLP_DISPLAY_TIER);
		k_mutex_unlock(&ota_lock);
		return USSLP_ERR_UNSUPPORTED;
	}
	if (!version_is_newer(m)) {
		LOG_INF("ignoring rollout %u: %u.%u.%u is not newer than the running "
			"image",
			m->rollout_id, m->version_major, m->version_minor, m->version_patch);
		k_mutex_unlock(&ota_lock);
		return USSLP_ERR_STALE;
	}
	if (m->image_bytes > usslp_slot_size()) {
		LOG_ERR("refusing rollout %u: a %u-byte image does not fit a %u-byte slot",
			m->rollout_id, m->image_bytes, (unsigned)usslp_slot_size());
		k_mutex_unlock(&ota_lock);
		return USSLP_ERR_NOSPACE;
	}

	rc = usslp_chunkmap_init(&chunks, m->payload_bytes);
	if (rc != USSLP_OK) {
		k_mutex_unlock(&ota_lock);
		return rc;
	}
	rc = usslp_slot_prepare();
	if (rc != USSLP_OK) {
		k_mutex_unlock(&ota_lock);
		return rc;
	}
	usslp_bloom_init(&gossip);

	manifest = *m;
	memset(&status, 0, sizeof(status));
	status.state = USSLP_OTA_DOWNLOADING;
	status.rollout_id = m->rollout_id;
	status.chunks_total = chunks.total;
	k_mutex_unlock(&ota_lock);

	LOG_INF("rollout %u accepted: %u.%u.%u, %u bytes in %u chunks%s", m->rollout_id,
		m->version_major, m->version_minor, m->version_patch, m->payload_bytes,
		chunks.total, m->is_delta ? " (delta)" : "");
	return USSLP_OK;
}

int usslp_ota_chunk(uint16_t index, const uint8_t *data, size_t len)
{
	int rc;

	k_mutex_lock(&ota_lock, K_FOREVER);
	if (status.state != USSLP_OTA_DOWNLOADING) {
		k_mutex_unlock(&ota_lock);
		return USSLP_ERR_BUSY;
	}
	if (usslp_chunkmap_has(&chunks, index)) {
		/* Already held. Idempotent by design: at-least-once delivery makes
		 * duplicates the common case for a transfer just as it does for a
		 * price. */
		k_mutex_unlock(&ota_lock);
		return USSLP_OK;
	}
	usslp_power_enter(USSLP_POWER_OTA);
	rc = usslp_slot_write((uint32_t)index * USSLP_OTA_CHUNK_BYTES, data, len);
	usslp_power_exit(USSLP_POWER_OTA);
	if (rc != USSLP_OK) {
		status.last_error = rc;
		k_mutex_unlock(&ota_lock);
		return rc;
	}
	usslp_chunkmap_set(&chunks, index);
	status.chunks_received = chunks.received;
	status.bytes_written += (uint32_t)len;
	k_mutex_unlock(&ota_lock);

	if (usslp_chunkmap_complete(&chunks)) {
		return usslp_ota_finish();
	}
	return USSLP_OK;
}

bool usslp_ota_should_relay(uint32_t rollout_id, uint16_t chunk_index)
{
	bool seen;

	k_mutex_lock(&ota_lock, K_FOREVER);
	seen = usslp_bloom_add(&gossip, usslp_bloom_chunk_key(rollout_id, chunk_index));
	if (seen) {
		status.gossip_suppressed++;
	}
	k_mutex_unlock(&ota_lock);
	return !seen;
}

/* The patch applier's view of flash. The base is the running image, which on the
 * nRF52840 is memory mapped and can be read directly; the target is the inactive
 * slot, written through the streaming writer. */
static int patch_read_base(void *ctx, uint32_t off, uint8_t *dst, uint32_t len)
{
	ARG_UNUSED(ctx);
	return usslp_slot_read_active(off, dst, len) == USSLP_OK ? 0 : -1;
}

static int patch_write_target(void *ctx, uint32_t off, const uint8_t *src, uint32_t len)
{
	ARG_UNUSED(ctx);
	return usslp_slot_write(off, src, len) == USSLP_OK ? 0 : -1;
}

int usslp_ota_finish(void)
{
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	int rc;

	k_mutex_lock(&ota_lock, K_FOREVER);
	status.state = USSLP_OTA_ASSEMBLING;
	k_mutex_unlock(&ota_lock);

	if (manifest.is_delta) {
		uint8_t *window = k_heap_alloc(&ota_heap, USSLP_INFLATE_WINDOW, K_NO_WAIT);
		struct usslp_patch_io io = {
			.read_base = patch_read_base,
			.write_target = patch_write_target,
			.ctx = NULL,
			.base_len = usslp_slot_active_size(),
		};
		struct usslp_patch_stats pstats;
		const uint8_t *patch;
		size_t patch_len;

		if (window == NULL) {
			LOG_ERR("no room for the DEFLATE window");
			rc = USSLP_ERR_NOSPACE;
			goto fail;
		}
		/*
		 * The patch was staged in the scratch partition, which is memory mapped,
		 * so the applier reads it in place. Then the *result* is written to the
		 * inactive slot — which means the slot has to be re-erased first,
		 * because it currently holds the patch's chunks rather than an image.
		 */
		patch = usslp_scratch_map(&patch_len);
		if (patch == NULL) {
			k_heap_free(&ota_heap, window);
			rc = USSLP_ERR_IO;
			goto fail;
		}
		rc = usslp_slot_prepare();
		if (rc == USSLP_OK) {
			usslp_power_enter(USSLP_POWER_OTA);
			rc = usslp_patch_apply(patch, patch_len, &io, window, &pstats);
			usslp_power_exit(USSLP_POWER_OTA);
		}
		k_heap_free(&ota_heap, window);
		if (rc != USSLP_OK) {
			LOG_ERR("delta application failed (%d)", rc);
			goto fail;
		}
		LOG_INF("delta applied: %u copies, %u literals, %u new bytes, %u total",
			pstats.copies, pstats.literals, pstats.literal_bytes,
			pstats.target_size);
	}

	k_mutex_lock(&ota_lock, K_FOREVER);
	status.state = USSLP_OTA_VERIFYING;
	k_mutex_unlock(&ota_lock);

	/* The assembled image's hash, against the manifest. This is not the
	 * signature check; it is the cheap one that catches a corrupt transfer
	 * before the expensive one runs. */
	rc = usslp_slot_digest(manifest.image_bytes, digest);
	if (rc != USSLP_OK) {
		goto fail;
	}
	if (usslp_ct_memcmp(digest, manifest.image_sha256, sizeof(digest)) != 0) {
		LOG_ERR("the assembled image does not match the manifest digest");
		rc = USSLP_ERR_INTEGRITY;
		goto fail;
	}

	/*
	 * The structural check. Not the authenticity check — that is MCUboot's, at
	 * boot, against a key this image cannot read (see slots.c). This catches a
	 * corrupt transfer while the label is still awake, instead of after two
	 * reboots and a failed swap of a coin cell.
	 */
	rc = usslp_slot_verify_signature();
	if (rc != USSLP_OK) {
		LOG_ERR("the assembled image is not a well-formed MCUboot image; not "
			"swapping");
		goto fail;
	}

	rc = usslp_slot_mark_for_swap();
	if (rc != USSLP_OK) {
		goto fail;
	}

	k_mutex_lock(&ota_lock, K_FOREVER);
	status.state = USSLP_OTA_READY;
	k_mutex_unlock(&ota_lock);
	LOG_INF("rollout %u verified and marked; resetting into the new image",
		manifest.rollout_id);

	/* The reset is deferred so the acknowledgement reaches the controller
	 * first: a rollout planner that never hears "ready" from a label counts it
	 * as failed and retries the whole transfer. */
	k_sleep(K_SECONDS(2));
	sys_reboot(SYS_REBOOT_COLD);
	return USSLP_OK;

fail:
	k_mutex_lock(&ota_lock, K_FOREVER);
	status.state = USSLP_OTA_FAILED;
	status.last_error = rc;
	k_mutex_unlock(&ota_lock);
	/* The inactive slot is left as it is rather than erased. Erasing it would
	 * cost another full erase cycle of the flash for no benefit: the next
	 * rollout erases it anyway, and MCUboot will not swap a slot that is not
	 * marked. */
	return rc;
}

void usslp_ota_status(struct usslp_ota_status *out)
{
	k_mutex_lock(&ota_lock, K_FOREVER);
	*out = status;
	k_mutex_unlock(&ota_lock);
}
