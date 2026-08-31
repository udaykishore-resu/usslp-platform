#include "usslp_keyring.h"

#include "usslp_sha256.h"

#include <string.h>

static const char hexdigits[] = "0123456789abcdef";

void usslp_keyring_derive_kid(const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN],
			      char out[USSLP_KID_BUF])
{
	uint8_t sum[USSLP_SHA256_DIGEST_LEN];
	size_t p = sizeof(USSLP_KID_PREFIX) - 1u;

	usslp_sha256(pub, USSLP_ED25519_PUBLIC_KEY_LEN, sum);
	memcpy(out, USSLP_KID_PREFIX, p);
	for (unsigned i = 0; i < 8; i++) {
		out[p + i * 2u] = hexdigits[(sum[i] >> 4) & 0x0fu];
		out[p + i * 2u + 1u] = hexdigits[sum[i] & 0x0fu];
	}
	out[p + 16u] = '\0';
	memset(sum, 0, sizeof(sum));
}

void usslp_keyring_init(struct usslp_keyring *ring)
{
	memset(ring, 0, sizeof(*ring));
	for (unsigned i = 0; i < USSLP_KEYRING_SLOTS; i++) {
		ring->keys[i].status = (uint8_t)USSLP_KEY_EMPTY;
	}
	ring->count = 0;
}

static struct usslp_ring_key *slot_for(struct usslp_keyring *ring, const char *kid)
{
	struct usslp_ring_key *free_slot = NULL;

	for (unsigned i = 0; i < USSLP_KEYRING_SLOTS; i++) {
		struct usslp_ring_key *k = &ring->keys[i];

		if (k->status == (uint8_t)USSLP_KEY_EMPTY) {
			if (free_slot == NULL) {
				free_slot = k;
			}
			continue;
		}
		if (usslp_ct_streq(k->kid, kid, USSLP_KID_LEN)) {
			return k;
		}
	}
	return free_slot;
}

int usslp_keyring_add(struct usslp_keyring *ring, const char *kid,
		      const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN], int64_t not_before,
		      int64_t not_after, enum usslp_key_status status)
{
	char derived[USSLP_KID_BUF];
	struct usslp_ring_key *slot;
	bool was_empty;

	if (ring == NULL || pub == NULL) {
		return USSLP_ERR_INVAL;
	}
	if (status != USSLP_KEY_ACTIVE && status != USSLP_KEY_RETIRING) {
		return USSLP_ERR_INVAL;
	}
	usslp_keyring_derive_kid(pub, derived);
	if (kid != NULL && kid[0] != '\0' && !usslp_ct_streq(kid, derived, USSLP_KID_LEN)) {
		/* pki.ErrKeyIDMismatch. The entry claims an identifier its own bytes do
		 * not produce, which is the shape of a substituted key. */
		return USSLP_ERR_AUTH;
	}
	slot = slot_for(ring, derived);
	if (slot == NULL) {
		return USSLP_ERR_NOSPACE;
	}
	was_empty = slot->status == (uint8_t)USSLP_KEY_EMPTY;

	memcpy(slot->kid, derived, sizeof(derived));
	memcpy(slot->pub, pub, USSLP_ED25519_PUBLIC_KEY_LEN);
	slot->not_before = not_before;
	slot->not_after = not_after;
	slot->status = (uint8_t)status;
	if (was_empty) {
		ring->count++;
	}
	return USSLP_OK;
}

int usslp_keyring_remove(struct usslp_keyring *ring, const char *kid)
{
	if (ring == NULL || kid == NULL) {
		return USSLP_ERR_INVAL;
	}
	for (unsigned i = 0; i < USSLP_KEYRING_SLOTS; i++) {
		struct usslp_ring_key *k = &ring->keys[i];

		if (k->status == (uint8_t)USSLP_KEY_EMPTY) {
			continue;
		}
		if (usslp_ct_streq(k->kid, kid, USSLP_KID_LEN)) {
			memset(k, 0, sizeof(*k));
			k->status = (uint8_t)USSLP_KEY_EMPTY;
			ring->count--;
			return USSLP_OK;
		}
	}
	return USSLP_ERR_INVAL;
}

const struct usslp_ring_key *usslp_keyring_find(const struct usslp_keyring *ring, const char *kid)
{
	const struct usslp_ring_key *found = NULL;

	if (ring == NULL || kid == NULL) {
		return NULL;
	}
	/* The loop runs to completion rather than returning early. With four slots
	 * the saving from an early exit is nothing, and the timing of a lookup
	 * miss stays independent of which slot the key is in. */
	for (unsigned i = 0; i < USSLP_KEYRING_SLOTS; i++) {
		const struct usslp_ring_key *k = &ring->keys[i];

		if (k->status == (uint8_t)USSLP_KEY_EMPTY) {
			continue;
		}
		if (usslp_ct_streq(k->kid, kid, USSLP_KID_LEN)) {
			found = k;
		}
	}
	return found;
}

bool usslp_ring_key_valid_at(const struct usslp_ring_key *key, int64_t now_unix)
{
	if (key == NULL || key->status == (uint8_t)USSLP_KEY_EMPTY) {
		return false;
	}
	if (key->not_before != 0 && now_unix < key->not_before) {
		return false;
	}
	if (key->not_after != 0 && now_unix > key->not_after) {
		return false;
	}
	return true;
}

uint8_t usslp_keyring_len(const struct usslp_keyring *ring)
{
	return ring == NULL ? 0u : ring->count;
}
