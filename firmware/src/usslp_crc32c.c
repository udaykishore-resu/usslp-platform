#include "usslp_crc32c.h"

/* Reflected Castagnoli polynomial, nibble-wise. Entry i is the CRC of the
 * 4-bit value i shifted into an all-zero register. */
static const uint32_t crc32c_nibble[16] = {
	0x00000000u, 0x105ec76fu, 0x20bd8edeu, 0x30e349b1u, 0x417b1dbcu, 0x5125dad3u,
	0x61c69362u, 0x7198540du, 0x82f63b78u, 0x92a8fc17u, 0xa24bb5a6u, 0xb21572c9u,
	0xc38d26c4u, 0xd3d3e1abu, 0xe330a81au, 0xf36e6f75u,
};

uint32_t usslp_crc32c_update(uint32_t crc, const void *data, size_t len)
{
	const uint8_t *p = (const uint8_t *)data;
	uint32_t reg = ~crc;

	for (size_t i = 0; i < len; i++) {
		reg ^= p[i];
		reg = (reg >> 4) ^ crc32c_nibble[reg & 0x0fu];
		reg = (reg >> 4) ^ crc32c_nibble[reg & 0x0fu];
	}
	return ~reg;
}

uint32_t usslp_crc32c(const void *data, size_t len)
{
	return usslp_crc32c_update(0u, data, len);
}
