/*
 * slots.h - MCUboot A/B slot handling.
 *
 * The flash layout is in boards/usslp_label_nrf52840.overlay: 48 KiB of
 * bootloader, two 424 KiB application slots, a 96 KiB scratch partition for the
 * staged patch, and NVS.
 *
 * A note on why there are two full-size slots on a 1 MB part, when swap-move
 * would let MCUboot get away with one slot plus a sector: swap-move rewrites the
 * *running* slot during the swap, so a power loss mid-swap leaves the device
 * dependent on MCUboot's recovery path to reassemble an image from two half
 * slots. On a device that has to be physically retrieved if it does not boot,
 * spending 424 KiB of flash to make the running image immutable until the new
 * one has been verified is the right trade. The label never needs the space for
 * anything else.
 */

#ifndef USSLP_SLOTS_H
#define USSLP_SLOTS_H

#include "../crypto/usslp_sha256.h"
#include "../usslp_portable.h"

/* Erases the inactive slot and opens the streaming writer. */
int usslp_slot_prepare(void);

/* Writes at an offset in the inactive slot. Offsets must be non-decreasing and
 * contiguous, which is what the streaming flash writer requires and what both
 * the chunk path and the patch applier produce. */
int usslp_slot_write(uint32_t offset, const uint8_t *data, size_t len);

/* Reads from the *running* image, which is the base a delta patch applies to. */
int usslp_slot_read_active(uint32_t offset, uint8_t *dst, uint32_t len);

/* The size of the running image, from its MCUboot header. */
uint32_t usslp_slot_active_size(void);

/* The size of one application slot. */
size_t usslp_slot_size(void);

/* SHA-256 over the first len bytes of the inactive slot. */
int usslp_slot_digest(uint32_t len, uint8_t out[USSLP_SHA256_DIGEST_LEN]);

/*
 * Verifies the MCUboot image header and its Ed25519 signature over the image in
 * the inactive slot.
 *
 * MCUboot verifies it again at boot, and that is the check that actually
 * protects the device. This one exists so that a bad image is discovered while
 * the label is awake rather than after two reboots and a failed swap. See ota.h.
 */
int usslp_slot_verify_signature(void);

/* Marks the inactive slot for a test swap. The image must confirm itself after
 * the reset or MCUboot reverts on the one after. */
int usslp_slot_mark_for_swap(void);

/* Maps the staged patch in the scratch partition. The nRF52840's internal flash
 * is memory mapped, so this is a pointer rather than a copy — which is what lets
 * the patch applier read a 96 KiB patch on a device with 256 KiB of RAM. */
const uint8_t *usslp_scratch_map(size_t *len);

/* Writes into the scratch partition, for a staged delta patch. */
int usslp_scratch_prepare(void);
int usslp_scratch_write(uint32_t offset, const uint8_t *data, size_t len);

#endif /* USSLP_SLOTS_H */
