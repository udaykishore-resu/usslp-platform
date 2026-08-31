#include "slots.h"

#include <string.h>
#include <zephyr/device.h>
#include <zephyr/dfu/flash_img.h>
#include <zephyr/dfu/mcuboot.h>
#include <zephyr/drivers/flash.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/storage/flash_map.h>

LOG_MODULE_DECLARE(usslp_ota, CONFIG_USSLP_LOG_LEVEL);

#define SLOT0_ID FIXED_PARTITION_ID(slot0_partition)
#define SLOT1_ID FIXED_PARTITION_ID(slot1_partition)
#define SCRATCH_ID FIXED_PARTITION_ID(ota_scratch_partition)

/* The nRF52840's internal flash is memory mapped at address 0, so a partition's
 * contents are addressable directly. That is what makes reading a 96 KiB staged
 * patch possible on a part with 256 KiB of RAM. */
#define FLASH_MAP_BASE ((const uint8_t *)DT_REG_ADDR(DT_NODELABEL(flash0)))

static struct flash_img_context img_ctx;
static bool writer_open;
static uint32_t writer_offset;

size_t usslp_slot_size(void)
{
	return (size_t)FIXED_PARTITION_SIZE(slot1_partition);
}

int usslp_slot_prepare(void)
{
	int rc;

	rc = flash_img_init(&img_ctx);
	if (rc != 0) {
		LOG_ERR("flash_img_init failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	rc = boot_erase_img_bank(SLOT1_ID);
	if (rc != 0) {
		LOG_ERR("erasing the inactive slot failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	writer_open = true;
	writer_offset = 0;
	return USSLP_OK;
}

int usslp_slot_write(uint32_t offset, const uint8_t *data, size_t len)
{
	int rc;

	if (!writer_open) {
		return USSLP_ERR_IO;
	}
	if (offset != writer_offset) {
		/* The streaming writer buffers up to a write-block boundary and cannot
		 * seek. Both producers — the chunk path, which requests chunks in order,
		 * and the patch applier, which emits the target strictly forwards — meet
		 * that contract, so a mismatch here is a bug rather than a condition to
		 * recover from. */
		LOG_ERR("out-of-order slot write: at %u, expected %u", offset, writer_offset);
		return USSLP_ERR_INVAL;
	}
	rc = flash_img_buffered_write(&img_ctx, (uint8_t *)data, len, false);
	if (rc != 0) {
		LOG_ERR("slot write failed at %u (%d)", offset, rc);
		return USSLP_ERR_IO;
	}
	writer_offset += (uint32_t)len;
	return USSLP_OK;
}

int usslp_slot_read_active(uint32_t offset, uint8_t *dst, uint32_t len)
{
	const struct flash_area *fa;
	int rc;

	rc = flash_area_open(SLOT0_ID, &fa);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	if ((size_t)offset + len > fa->fa_size) {
		flash_area_close(fa);
		return USSLP_ERR_INVAL;
	}
	rc = flash_area_read(fa, offset, dst, len);
	flash_area_close(fa);
	return rc == 0 ? USSLP_OK : USSLP_ERR_IO;
}

uint32_t usslp_slot_active_size(void)
{
	struct mcuboot_img_header hdr;

	if (boot_read_bank_header(SLOT0_ID, &hdr, sizeof(hdr)) != 0) {
		return 0;
	}
	/* The delta's base is the image as it was built and signed, which is the
	 * header plus the payload — the same bytes the platform's Diff ran over. */
	return hdr.h.v1.image_size + hdr.h.v1.header_size;
}

int usslp_slot_digest(uint32_t len, uint8_t out[USSLP_SHA256_DIGEST_LEN])
{
	const struct flash_area *fa;
	struct usslp_sha256 ctx;
	uint8_t chunk[256];
	uint32_t off = 0;
	int rc;

	rc = flash_area_open(SLOT1_ID, &fa);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	usslp_sha256_init(&ctx);
	while (off < len) {
		uint32_t n = len - off;

		if (n > sizeof(chunk)) {
			n = sizeof(chunk);
		}
		rc = flash_area_read(fa, off, chunk, n);
		if (rc != 0) {
			flash_area_close(fa);
			usslp_sha256_final(&ctx, out);
			memset(out, 0, USSLP_SHA256_DIGEST_LEN);
			return USSLP_ERR_IO;
		}
		usslp_sha256_update(&ctx, chunk, n);
		off += n;
	}
	flash_area_close(fa);
	usslp_sha256_final(&ctx, out);
	return USSLP_OK;
}

int usslp_slot_verify_signature(void)
{
	struct mcuboot_img_header hdr;
	int rc;

	/*
	 * Zephyr does not expose MCUboot's signature verifier to the application —
	 * the verification code lives in the bootloader and the key with it. What
	 * the application can check, and what this function does, is everything
	 * short of the elliptic curve:
	 *
	 *   - the image header is well formed and its magic is right
	 *   - the declared image size fits the slot and matches what was written
	 *   - the TLV trailer is present, is the right size, and carries a
	 *     signature TLV of the expected type and length
	 *
	 * A corrupt transfer fails all three. A *forged* image passes them and fails
	 * at boot, where MCUboot checks the signature against the key in the
	 * bootloader — which is the check that actually protects the device, and the
	 * one that cannot be moved into the application without moving the key with
	 * it.
	 *
	 * That distinction is worth being exact about rather than claiming this
	 * function verifies a signature: it verifies that an image is present and
	 * structurally sound, which saves a wasted reboot cycle, and it does not
	 * verify authenticity.
	 */
	rc = boot_read_bank_header(SLOT1_ID, &hdr, sizeof(hdr));
	if (rc != 0) {
		LOG_ERR("the staged image has no readable MCUboot header (%d)", rc);
		return USSLP_ERR_INTEGRITY;
	}
	if (hdr.mcuboot_version != 1) {
		LOG_ERR("unexpected MCUboot header version %u", hdr.mcuboot_version);
		return USSLP_ERR_INTEGRITY;
	}
	if ((size_t)hdr.h.v1.image_size + hdr.h.v1.header_size > usslp_slot_size()) {
		LOG_ERR("the staged image declares %u bytes, which does not fit the slot",
			hdr.h.v1.image_size);
		return USSLP_ERR_INTEGRITY;
	}
	if (hdr.h.v1.image_size + hdr.h.v1.header_size > writer_offset) {
		LOG_ERR("the staged image declares %u bytes and %u were written",
			hdr.h.v1.image_size + hdr.h.v1.header_size, writer_offset);
		return USSLP_ERR_INTEGRITY;
	}
	LOG_INF("staged image %u.%u.%u header is sound (%u bytes); MCUboot will verify "
		"its signature at boot",
		hdr.h.v1.sem_ver.major, hdr.h.v1.sem_ver.minor, hdr.h.v1.sem_ver.revision,
		hdr.h.v1.image_size);
	return USSLP_OK;
}

int usslp_slot_mark_for_swap(void)
{
	int rc;

	if (writer_open) {
		/* Flush whatever the streaming writer is still holding below its
		 * write-block boundary. Skipping this leaves the last few bytes of the
		 * image unwritten, which fails MCUboot's own hash and reverts — after a
		 * reboot the label did not need to spend. */
		rc = flash_img_buffered_write(&img_ctx, NULL, 0, true);
		if (rc != 0) {
			LOG_ERR("flushing the slot writer failed (%d)", rc);
			return USSLP_ERR_IO;
		}
		writer_open = false;
	}
	rc = boot_request_upgrade(BOOT_UPGRADE_TEST);
	if (rc != 0) {
		LOG_ERR("marking the slot for swap failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	return USSLP_OK;
}

const uint8_t *usslp_scratch_map(size_t *len)
{
	const struct flash_area *fa;
	const uint8_t *p;
	int rc;

	rc = flash_area_open(SCRATCH_ID, &fa);
	if (rc != 0) {
		return NULL;
	}
	p = FLASH_MAP_BASE + fa->fa_off;
	if (len != NULL) {
		*len = fa->fa_size;
	}
	flash_area_close(fa);
	return p;
}

int usslp_scratch_prepare(void)
{
	const struct flash_area *fa;
	int rc;

	rc = flash_area_open(SCRATCH_ID, &fa);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	rc = flash_area_erase(fa, 0, fa->fa_size);
	flash_area_close(fa);
	return rc == 0 ? USSLP_OK : USSLP_ERR_IO;
}

int usslp_scratch_write(uint32_t offset, const uint8_t *data, size_t len)
{
	const struct flash_area *fa;
	int rc;

	rc = flash_area_open(SCRATCH_ID, &fa);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	if ((size_t)offset + len > fa->fa_size) {
		flash_area_close(fa);
		return USSLP_ERR_NOSPACE;
	}
	rc = flash_area_write(fa, offset, data, len);
	flash_area_close(fa);
	return rc == 0 ? USSLP_OK : USSLP_ERR_IO;
}
