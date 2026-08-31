#include "seq_store.h"

#include <string.h>
#include <zephyr/fs/nvs.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/storage/flash_map.h>

LOG_MODULE_DECLARE(usslp_price, CONFIG_USSLP_LOG_LEVEL);

/* NVS entry ids. Small integers, and stable forever: an id that changed meaning
 * between firmware versions would have a label reading a sequence out of a
 * ghosting counter. */
#define NVS_ID_SEQUENCE 0x0001
#define NVS_ID_GHOST 0x0002

#define STORAGE_PARTITION storage_partition

static struct nvs_fs fs;
static struct usslp_seq_state state;
static bool store_ready;
static K_MUTEX_DEFINE(store_lock);

int usslp_seq_store_init(struct usslp_ghost_state *ghost)
{
	const struct flash_area *fa;
	struct flash_pages_info info;
	uint8_t record[USSLP_SEQ_RECORD_LEN];
	uint8_t ghost_byte = 0;
	int rc;

	usslp_seq_init(&state);
	usslp_ghost_init(ghost);

	rc = flash_area_open(FIXED_PARTITION_ID(STORAGE_PARTITION), &fa);
	if (rc != 0) {
		LOG_ERR("storage partition unavailable (%d)", rc);
		return USSLP_ERR_IO;
	}
	fs.flash_device = flash_area_get_device(fa);
	fs.offset = fa->fa_off;
	flash_area_close(fa);
	if (fs.flash_device == NULL || !device_is_ready(fs.flash_device)) {
		return USSLP_ERR_IO;
	}
	rc = flash_get_page_info_by_offs(fs.flash_device, fs.offset, &info);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	fs.sector_size = (uint16_t)info.size;
	/* Six sectors of the 24 KiB partition. NVS needs at least two to garbage
	 * collect; six gives the wear levelling somewhere to go across a decade of
	 * price changes. */
	fs.sector_count = 6U;

	rc = nvs_mount(&fs);
	if (rc != 0) {
		LOG_ERR("NVS mount failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	store_ready = true;

	rc = nvs_read(&fs, NVS_ID_SEQUENCE, record, sizeof(record));
	if (rc == (int)sizeof(record)) {
		if (usslp_seq_decode(&state, record) != USSLP_OK) {
			/* A half-written record from a brownout during a commit. Treating
			 * it as absent is the safe direction: the label accepts the next
			 * update and rewrites the record, rather than rejecting everything
			 * forever or trusting a corrupted sequence. */
			LOG_WRN("the stored sequence record failed its checksum; treating "
				"this label as never having displayed a price");
			usslp_seq_init(&state);
		}
	} else if (rc == -ENOENT) {
		LOG_INF("no stored sequence: this label has never displayed a price");
	} else {
		LOG_WRN("reading the stored sequence failed (%d)", rc);
	}

	rc = nvs_read(&fs, NVS_ID_GHOST, &ghost_byte, sizeof(ghost_byte));
	if (rc == (int)sizeof(ghost_byte)) {
		ghost->partials_since_full = ghost_byte;
	}
	return USSLP_OK;
}

const struct usslp_seq_state *usslp_seq_store_state(void)
{
	return &state;
}

enum usslp_seq_verdict usslp_seq_store_check(int64_t candidate)
{
	enum usslp_seq_verdict v;

	k_mutex_lock(&store_lock, K_FOREVER);
	v = usslp_seq_check(&state, candidate);
	k_mutex_unlock(&store_lock);
	return v;
}

int usslp_seq_store_commit(int64_t sequence, struct usslp_ghost_state *ghost,
			   const struct usslp_refresh_plan *plan)
{
	uint8_t record[USSLP_SEQ_RECORD_LEN];
	struct usslp_seq_state next;
	struct usslp_ghost_state next_ghost;
	uint8_t ghost_byte;
	int rc;

	if (!store_ready) {
		return USSLP_ERR_IO;
	}
	k_mutex_lock(&store_lock, K_FOREVER);

	next = state;
	rc = usslp_seq_commit(&next, sequence);
	if (rc != USSLP_OK) {
		k_mutex_unlock(&store_lock);
		return rc;
	}
	next_ghost = *ghost;
	usslp_ghost_apply(&next_ghost, plan);

	usslp_seq_encode(&next, record);
	rc = nvs_write(&fs, NVS_ID_SEQUENCE, record, sizeof(record));
	if (rc < 0) {
		k_mutex_unlock(&store_lock);
		LOG_ERR("NVS write of the sequence failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	ghost_byte = next_ghost.partials_since_full;
	rc = nvs_write(&fs, NVS_ID_GHOST, &ghost_byte, sizeof(ghost_byte));
	if (rc < 0) {
		/* The sequence is committed and the ghosting counter is not. That
		 * asymmetry is the right one: on the next boot the counter reads low,
		 * the label runs a full refresh sooner than it needed to, and the only
		 * cost is energy. The reverse — a committed counter and a lost
		 * sequence — would let a stale price back onto the glass. */
		LOG_WRN("NVS write of the ghosting counter failed (%d); it will read "
			"conservatively after a reboot",
			rc);
	}

	/* Only now does the in-memory state move. A caller that sees USSLP_OK knows
	 * the flash has it. */
	state = next;
	*ghost = next_ghost;
	k_mutex_unlock(&store_lock);
	return USSLP_OK;
}

int usslp_seq_store_reset(struct usslp_ghost_state *ghost)
{
	int rc;

	if (!store_ready) {
		return USSLP_ERR_IO;
	}
	k_mutex_lock(&store_lock, K_FOREVER);
	usslp_seq_init(&state);
	usslp_ghost_init(ghost);
	rc = nvs_delete(&fs, NVS_ID_SEQUENCE);
	if (rc == 0) {
		rc = nvs_delete(&fs, NVS_ID_GHOST);
	}
	k_mutex_unlock(&store_lock);
	if (rc != 0) {
		return USSLP_ERR_IO;
	}
	LOG_WRN("sequence and ghosting state cleared; this label will accept the next "
		"price it is offered");
	return USSLP_OK;
}
