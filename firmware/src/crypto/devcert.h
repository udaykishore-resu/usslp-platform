/*
 * usslp_devcert.h - the device's own identity, and the key ring it verifies
 * prices against.
 *
 * Two different kinds of key material, with two different threat models, and
 * conflating them is a classic way to lose a fleet:
 *
 *   - The *device key* is a per-label Ed25519 private key generated on the part
 *     at manufacture and never exported. It signs the label's provisioning
 *     request and its telemetry. Compromising one gives an attacker one label's
 *     voice; compromising the storage design gives them the fleet's.
 *
 *   - The *price authority keys* are public. They arrive in a key ring, they
 *     verify what the platform signed, and there is nothing secret about them.
 *     What matters for them is integrity, not confidentiality, and
 *     usslp_keyring_add's self-authenticating kid check provides most of it.
 *
 * Where the device key actually lives
 * -----------------------------------
 * The nRF52840 has no TrustZone. It has the SPU/ACL mechanism, which lets the
 * bootloader mark the identity flash page as read-protected until the next
 * reset, and that is what this build uses: MCUboot locks the page after handing
 * over, so application code — including a compromised application delivered by
 * a subverted OTA — cannot read the private key. It can still *use* it, because
 * the signing routine lives in the bootloader's shared region.
 *
 * That is meaningfully weaker than a real secure element, and it is worth being
 * exact about the difference: an attacker with physical possession of the label
 * and a debugger can defeat it if APPROTECT has been cleared, and APPROTECT on
 * revision C parts has known bypasses. The mitigation is at the platform level —
 * a per-device key is not a fleet key, and device.identity.revoked exists — not
 * at the silicon level.
 *
 * The same source compiles against TF-M on the nRF5340 and nRF54L, where the key
 * genuinely never leaves the secure world; usslp_devcert_protection() reports
 * which of the two this build got, and provisioning includes it in the device
 * record so the platform knows what it is trusting.
 */

#ifndef USSLP_DEVCERT_H
#define USSLP_DEVCERT_H

#include "../usslp_portable.h"
#include "usslp_keyring.h"

enum usslp_key_protection {
	/* Software only: the key is in ordinary flash. Development builds. */
	USSLP_PROT_NONE = 0,
	/* Flash ACL: readable by the bootloader, locked out for the application. */
	USSLP_PROT_FLASH_ACL = 1,
	/* A secure world the non-secure image cannot address. */
	USSLP_PROT_TRUSTZONE = 2,
};

/* The factory record, written at manufacture into the identity partition. */
struct usslp_device_identity {
	/* The 64-bit IEEE 802.15.4 extended address. canon.DeviceSerial derives
	 * the printed serial from it, so the sticker on the label and the mesh
	 * address cannot disagree. */
	uint64_t eui64;
	/* "USSLP-" + 16 hex characters, as canon.DeviceSerial renders it. */
	char serial[24];
	uint8_t device_pub[USSLP_ED25519_PUBLIC_KEY_LEN];
	uint8_t tier;
	uint16_t hw_revision;
	enum usslp_key_protection protection;
};

/* Reads the factory record and unlocks the identity page. Called once, early in
 * main(), before anything that could want to sign. */
int usslp_devcert_init(void);

const struct usslp_device_identity *usslp_devcert_identity(void);

enum usslp_key_protection usslp_devcert_protection(void);

/* Signs a message with the device key. The private key is never returned to the
 * caller, and on a TrustZone build never leaves the secure world. */
int usslp_devcert_sign(const uint8_t *msg, size_t len,
		       uint8_t sig[USSLP_ED25519_SIGNATURE_LEN]);

/* The live price-authority key ring, loaded from NVS at boot. Never NULL after
 * usslp_devcert_init; a label with an empty ring simply refuses every price,
 * which is the correct behaviour and is visible in telemetry within three
 * heartbeats. */
struct usslp_keyring *usslp_price_keyring(void);

/*
 * Replaces the ring from a signed bundle pushed by the platform.
 *
 * The bundle is verified against the *current* ring before it is applied, so a
 * label can only be moved to a new set of keys by somebody who can already sign
 * with the old ones. That is what stops a mesh-level attacker from installing
 * their own price authority; it also means a label whose ring has fallen too far
 * behind has to be re-provisioned by hand, which is the intended trade.
 */
int usslp_price_keyring_update(const uint8_t *bundle, size_t len);

#endif /* USSLP_DEVCERT_H */
