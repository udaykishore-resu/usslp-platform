/*
 * usslp_crc32c.h - CRC-32 Castagnoli.
 *
 * The air protocol's image checksum is CRC-32C (edge/labelsim/wire.go uses
 * hash/crc32 with crc32.Castagnoli), so the firmware needs the same polynomial
 * with the same reflection, initial value and final inversion. The NVS records
 * use it too, for the different job of catching a half-written record after a
 * brownout.
 *
 * Implemented with the nibble-wise table: 16 entries and 64 bytes of flash
 * rather than 1 KiB for the byte-wise table. A price image is a few hundred
 * bytes, so the difference is a handful of microseconds per update against a
 * kilobyte of a budget that is measured in kilobytes.
 */

#ifndef USSLP_CRC32C_H
#define USSLP_CRC32C_H

#include "usslp_portable.h"

/* Standard CRC-32C: init 0xFFFFFFFF, reflected input and output, final xor
 * 0xFFFFFFFF. Matches Go's crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)). */
uint32_t usslp_crc32c(const void *data, size_t len);

/* Incremental form, for a payload arriving as mesh fragments. Seed the first
 * call with 0 and pass the previous return value thereafter. */
uint32_t usslp_crc32c_update(uint32_t crc, const void *data, size_t len);

#endif /* USSLP_CRC32C_H */
