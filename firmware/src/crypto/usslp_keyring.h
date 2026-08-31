/*
 * usslp_keyring.h - the label's on-device price-authority key ring.
 *
 * The platform-side ring is pki.KeyRing (platform/pkg/pki/keyring.go). This is
 * the four-slot subset a label carries, and the reasons it is four:
 *
 *   - One active key.
 *   - One retiring key, because pki.PriceAuthority.Rotate keeps the previous
 *     key valid for an overlap window, and an update signed a minute before a
 *     rotation must still verify a minute after it.
 *   - One incoming key, so a ring update can be staged and committed atomically
 *     rather than leaving a window in which the label trusts nothing.
 *   - One spare, which is what a tenant migration between price authorities
 *     needs and what nobody remembers to leave room for.
 *
 * A key identifier is self-authenticating: pki.KeyIDFor derives it as
 * "usslp-price-" followed by the first eight bytes of SHA-256 over the public
 * key, hex encoded. usslp_keyring_add recomputes it and refuses an entry whose
 * claimed kid does not match its bytes, which means a label does not have to
 * trust the provenance or the ordering of the ring it was handed — an attacker
 * who can rewrite the ring in NVS still cannot make the label accept a key
 * under an identifier the platform is signing with.
 */

#ifndef USSLP_KEYRING_H
#define USSLP_KEYRING_H

#include "../usslp_portable.h"
#include "usslp_ed25519.h"

/* "usslp-price-" (12) + 16 hex characters + NUL. */
#define USSLP_KID_LEN 28
#define USSLP_KID_BUF (USSLP_KID_LEN + 1)
#define USSLP_KID_PREFIX "usslp-price-"

#define USSLP_KEYRING_SLOTS 4

enum usslp_key_status {
	USSLP_KEY_ACTIVE = 0,
	USSLP_KEY_RETIRING = 1,
	/* An empty slot. Distinct from "retired": a retired key is removed. */
	USSLP_KEY_EMPTY = 0xff,
};

struct usslp_ring_key {
	char kid[USSLP_KID_BUF];
	uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN];
	/* Validity window in seconds since the epoch. not_after == 0 means open
	 * ended, matching pki.RingKey's zero-time convention. */
	int64_t not_before;
	int64_t not_after;
	uint8_t status;
};

struct usslp_keyring {
	struct usslp_ring_key keys[USSLP_KEYRING_SLOTS];
	uint8_t count;
};

void usslp_keyring_init(struct usslp_keyring *ring);

/*
 * Adds or replaces a key. The kid is recomputed from pub and the entry is
 * refused with USSLP_ERR_AUTH if it does not match what the caller claimed.
 * Passing an empty kid means "derive it", which is what a factory provisioning
 * record does.
 *
 * Replacing an existing kid in place is deliberate and is how a status or
 * validity-window change arrives; it never silently changes the key bytes under
 * an identifier, because that would fail the derivation check.
 */
int usslp_keyring_add(struct usslp_keyring *ring, const char *kid,
		      const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN], int64_t not_before,
		      int64_t not_after, enum usslp_key_status status);

int usslp_keyring_remove(struct usslp_keyring *ring, const char *kid);

/*
 * Resolves a kid. Returns NULL when the ring has never seen it. The comparison
 * is constant time with respect to content so that the rejection latency does
 * not reveal how many leading characters of a guessed kid were right.
 */
const struct usslp_ring_key *usslp_keyring_find(const struct usslp_keyring *ring, const char *kid);

/* Reports whether a key may be used to verify a signature at a given instant. */
bool usslp_ring_key_valid_at(const struct usslp_ring_key *key, int64_t now_unix);

/*
 * Derives the canonical identifier of a public key into out, which must be at
 * least USSLP_KID_BUF bytes. This is pki.KeyIDFor.
 */
void usslp_keyring_derive_kid(const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN],
			      char out[USSLP_KID_BUF]);

/* Number of occupied slots. */
uint8_t usslp_keyring_len(const struct usslp_keyring *ring);

#endif /* USSLP_KEYRING_H */
