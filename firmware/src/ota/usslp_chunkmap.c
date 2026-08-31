#include "usslp_chunkmap.h"

#include <string.h>

int usslp_chunkmap_init(struct usslp_chunk_bitmap *m, uint32_t image_bytes)
{
	uint32_t chunks;

	if (m == NULL) {
		return USSLP_ERR_INVAL;
	}
	memset(m, 0, sizeof(*m));
	chunks = (image_bytes + USSLP_OTA_CHUNK_BYTES - 1u) / USSLP_OTA_CHUNK_BYTES;
	if (chunks == 0u || chunks > USSLP_OTA_MAX_CHUNKS) {
		return USSLP_ERR_INVAL;
	}
	m->total = (uint16_t)chunks;
	return USSLP_OK;
}

bool usslp_chunkmap_has(const struct usslp_chunk_bitmap *m, uint16_t index)
{
	if (index >= m->total) {
		return false;
	}
	return (m->bits[index >> 3] & (uint8_t)(1u << (index & 7u))) != 0u;
}

bool usslp_chunkmap_set(struct usslp_chunk_bitmap *m, uint16_t index)
{
	uint8_t mask;

	if (index >= m->total) {
		return false;
	}
	mask = (uint8_t)(1u << (index & 7u));
	if ((m->bits[index >> 3] & mask) != 0u) {
		return false;
	}
	m->bits[index >> 3] |= mask;
	m->received++;
	return true;
}

bool usslp_chunkmap_complete(const struct usslp_chunk_bitmap *m)
{
	return m->total > 0u && m->received == m->total;
}

int usslp_chunkmap_next_missing(const struct usslp_chunk_bitmap *m)
{
	for (uint16_t i = 0; i < m->total; i++) {
		if (!usslp_chunkmap_has(m, i)) {
			return (int)i;
		}
	}
	return -1;
}

void usslp_bloom_init(struct usslp_bloom *b)
{
	memset(b, 0, sizeof(*b));
}

/*
 * Two independent 64-bit mixes of the key, then k = h1 + i*h2 (Kirsch-Mitzenmacher
 * double hashing). Two hashes give k as good a distribution as k independent ones
 * for filters of this size, and on a Cortex-M4 without a hardware multiplier
 * penalty it is two multiplies rather than three.
 */
static void bloom_hashes(uint64_t key, uint32_t *h1, uint32_t *h2)
{
	uint64_t x = key;

	/* splitmix64 finalisation: cheap, and it decorrelates the low bits that a
	 * chunk index would otherwise dominate. */
	x += 0x9e3779b97f4a7c15ull;
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9ull;
	x = (x ^ (x >> 27)) * 0x94d049bb133111ebull;
	x = x ^ (x >> 31);
	*h1 = (uint32_t)(x & 0xffffffffu);
	*h2 = (uint32_t)(x >> 32) | 1u; /* odd, so the stride never revisits a slot */
}

bool usslp_bloom_maybe_contains(const struct usslp_bloom *b, uint64_t key)
{
	uint32_t h1, h2;

	bloom_hashes(key, &h1, &h2);
	for (unsigned i = 0; i < USSLP_BLOOM_HASHES; i++) {
		uint32_t bit = (h1 + i * h2) % USSLP_BLOOM_BITS;

		if ((b->bits[bit >> 3] & (uint8_t)(1u << (bit & 7u))) == 0u) {
			return false;
		}
	}
	return true;
}

bool usslp_bloom_add(struct usslp_bloom *b, uint64_t key)
{
	uint32_t h1, h2;
	bool present = true;

	bloom_hashes(key, &h1, &h2);
	for (unsigned i = 0; i < USSLP_BLOOM_HASHES; i++) {
		uint32_t bit = (h1 + i * h2) % USSLP_BLOOM_BITS;
		uint8_t mask = (uint8_t)(1u << (bit & 7u));

		if ((b->bits[bit >> 3] & mask) == 0u) {
			present = false;
			b->bits[bit >> 3] |= mask;
		}
	}
	if (!present && b->inserted < UINT16_MAX) {
		b->inserted++;
	}
	return present;
}

uint32_t usslp_bloom_fp_ppm(const struct usslp_bloom *b)
{
	/* p = (set_fraction)^k, computed from the observed fill rather than from
	 * the nominal insert count: the observed fill is what actually determines
	 * the rate, and it costs 128 byte-popcounts to measure exactly. */
	uint32_t set = 0;
	uint64_t frac_ppm;
	uint64_t p;

	for (unsigned i = 0; i < USSLP_BLOOM_BYTES; i++) {
		uint8_t v = b->bits[i];

		while (v != 0u) {
			set += (uint32_t)(v & 1u);
			v >>= 1;
		}
	}
	frac_ppm = (uint64_t)set * 1000000ull / USSLP_BLOOM_BITS;
	p = frac_ppm;
	for (unsigned i = 1; i < USSLP_BLOOM_HASHES; i++) {
		p = p * frac_ppm / 1000000ull;
	}
	return (uint32_t)p;
}

uint64_t usslp_bloom_chunk_key(uint32_t rollout_id, uint16_t chunk_index)
{
	return ((uint64_t)rollout_id << 16) | (uint64_t)chunk_index;
}
