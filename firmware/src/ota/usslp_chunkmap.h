/*
 * usslp_chunkmap.h - what the label has, and what it has already heard about.
 *
 * A staged OTA download over the mesh is a gossip problem. Several hundred
 * labels in a zone want the same image, they can hear each other as well as
 * their parent, and the channel is one shared 250 kbps. Two different things
 * have to be tracked and they need two different data structures, which is why
 * this file has two:
 *
 * 1. usslp_chunk_bitmap - which chunks this label actually holds.
 *
 *    Exact. One bit per chunk, 512 chunks of 512 bytes covering a 256 KiB
 *    image in 64 bytes of RAM. It has to be exact because it decides what to
 *    request and when the download is complete, and a false positive here means
 *    a hole in the image that the digest check catches only after the whole
 *    transfer has been paid for.
 *
 * 2. usslp_bloom - which chunk *announcements* have already been relayed.
 *
 *    Approximate, and that is fine. Labels re-broadcast "I have chunk N" so a
 *    label out of range of the parent can find a sibling that has it. Without
 *    dedupe that gossip floods: each announcement is re-broadcast by every
 *    neighbour that hears it, and a zone of five hundred labels turns one
 *    announcement into thousands of frames. The Bloom filter suppresses the
 *    repeats.
 *
 *    A false positive suppresses a re-broadcast that would have been useful.
 *    That costs one redundant path through the gossip graph and nothing else;
 *    the requester still asks its parent, and the download still completes. This
 *    is the only place in the firmware where a probabilistic structure is
 *    allowed, and it is allowed precisely because its failure mode is "slightly
 *    slower", not "wrong image".
 *
 *    Using a Bloom filter for (1) instead — which is the tempting simplification
 *    — would mean a false positive marks a chunk as held that was never
 *    received. The label would report the download complete, fail the image
 *    digest, and start again from scratch, on battery, forever.
 */

#ifndef USSLP_CHUNKMAP_H
#define USSLP_CHUNKMAP_H

#include "../usslp_portable.h"

/* 512-byte chunks: four 802.15.4 fragments each, so a lost chunk costs four
 * retransmissions and not forty. */
#define USSLP_OTA_CHUNK_BYTES 512
#define USSLP_OTA_MAX_CHUNKS 1024
#define USSLP_CHUNK_BITMAP_BYTES (USSLP_OTA_MAX_CHUNKS / 8)

struct usslp_chunk_bitmap {
	uint8_t bits[USSLP_CHUNK_BITMAP_BYTES];
	uint16_t total;    /* chunks in this image */
	uint16_t received; /* how many bits are set */
};

int usslp_chunkmap_init(struct usslp_chunk_bitmap *m, uint32_t image_bytes);
bool usslp_chunkmap_has(const struct usslp_chunk_bitmap *m, uint16_t index);
/* Returns true when this call set a bit that was previously clear. */
bool usslp_chunkmap_set(struct usslp_chunk_bitmap *m, uint16_t index);
bool usslp_chunkmap_complete(const struct usslp_chunk_bitmap *m);
/* Index of the lowest missing chunk, or -1 when complete. This is what the
 * download loop requests next; going in order rather than at random keeps the
 * flash writes sequential, which matters because the nRF52840's flash controller
 * stalls the CPU for the duration of every write. */
int usslp_chunkmap_next_missing(const struct usslp_chunk_bitmap *m);

/*
 * The gossip dedupe filter: 4,096 bits (512 bytes) and three hashes.
 *
 * The sizing follows from the traffic. A zone's rollout generates on the order
 * of 200 distinct chunk announcements that any one label hears, so
 *
 *   p = (1 - e^(-kn/m))^k   with k=3, m=4096, n=200
 *     = (1 - e^(-0.1465))^3 = 0.1363^3 = 0.0025
 *
 * a quarter of a per cent. The obvious smaller choice, 1,024 bits, gives
 * (1 - e^(-0.586))^3 = 0.087 — nearly nine per cent of announcements suppressed
 * that should not have been, which at 384 bytes saved is not a trade worth
 * making on a device that has 256 KiB and is using this buffer for ninety
 * seconds.
 *
 * The filter is cleared between rollouts, which is what keeps n bounded, and
 * usslp_bloom_fp_ppm reports the *observed* fill so an operator can see a
 * saturated filter rather than infer it from a slow rollout.
 */
#define USSLP_BLOOM_BITS 4096
#define USSLP_BLOOM_BYTES (USSLP_BLOOM_BITS / 8)
#define USSLP_BLOOM_HASHES 3

struct usslp_bloom {
	uint8_t bits[USSLP_BLOOM_BYTES];
	uint16_t inserted;
};

void usslp_bloom_init(struct usslp_bloom *b);
/* Records a key. Returns true if the key was probably already present, in which
 * case the caller suppresses the re-broadcast. */
bool usslp_bloom_add(struct usslp_bloom *b, uint64_t key);
bool usslp_bloom_maybe_contains(const struct usslp_bloom *b, uint64_t key);
/* Estimated false-positive rate in parts per million, for telemetry: a rollout
 * whose filter has saturated is one whose gossip has stopped working, and the
 * operator wants to see that rather than infer it from a slow rollout. */
uint32_t usslp_bloom_fp_ppm(const struct usslp_bloom *b);

/* The announcement key: a rollout is identified by the low 32 bits of its target
 * digest, and a chunk by its index. Combining them means a filter left over from
 * a previous rollout cannot suppress announcements for the current one even if
 * the caller forgot to clear it. */
uint64_t usslp_bloom_chunk_key(uint32_t rollout_id, uint16_t chunk_index);

#endif /* USSLP_CHUNKMAP_H */
