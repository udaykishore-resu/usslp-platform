#include "devcert.h"

#include "usslp_sha256.h"

#include <psa/crypto.h>
#include <string.h>
#include <zephyr/drivers/flash.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/settings/settings.h>
#include <zephyr/storage/flash_map.h>

LOG_MODULE_DECLARE(usslp_crypto, CONFIG_USSLP_LOG_LEVEL);

#define IDENTITY_PARTITION identity_partition
#define IDENTITY_ID FIXED_PARTITION_ID(IDENTITY_PARTITION)

/* The on-flash factory record. Written by the production programmer, never by
 * the application. */
#define IDENTITY_MAGIC 0x55534944u /* "USID" */
#define IDENTITY_VERSION 1

struct identity_record {
	uint32_t magic;
	uint16_t version;
	uint16_t hw_revision;
	uint64_t eui64;
	uint8_t tier;
	uint8_t protection;
	uint8_t reserved[2];
	uint8_t device_pub[USSLP_ED25519_PUBLIC_KEY_LEN];
	/* The private key occupies the next 32 bytes on flash. The application
	 * never maps them: the field is absent from this struct on purpose, so that
	 * there is no expression anywhere in the non-secure image that names the
	 * private key's address. */
	uint32_t crc;
};

static struct usslp_device_identity identity;
static struct usslp_keyring keyring;
static bool identity_valid;

/* The device key handle. On a TF-M build this is a key in the secure world and
 * the id is all the non-secure image ever sees. On the nRF52840 it is a volatile
 * key imported once by the bootloader-shared routine and held for the life of
 * the boot. */
static psa_key_id_t device_key_id = PSA_KEY_ID_NULL;

static void derive_serial(uint64_t eui64, char out[24])
{
	static const char hexd[] = "0123456789ABCDEF";
	size_t p = 0;

	/* canon.DeviceSerial: "USSLP-" followed by the big-endian EUI-64 in upper
	 * case hex. This is what a technician scans, so it has to be byte-identical
	 * with the Go helper or a scan will not resolve. */
	memcpy(out, "USSLP-", 6);
	p = 6;
	for (int i = 7; i >= 0; i--) {
		uint8_t b = (uint8_t)(eui64 >> (i * 8));

		out[p++] = hexd[b >> 4];
		out[p++] = hexd[b & 0x0fu];
	}
	out[p] = '\0';
}

/* ------------------------------------------------------------------------- */
/* Key ring persistence                                                      */
/* ------------------------------------------------------------------------- */

#define KEYRING_SETTINGS_KEY "usslp/keyring"

static int keyring_settings_set(const char *name, size_t len, settings_read_cb read_cb,
				void *cb_arg)
{
	struct usslp_keyring loaded;
	ssize_t n;

	if (!settings_name_steq(name, "ring", NULL)) {
		return -ENOENT;
	}
	if (len != sizeof(loaded)) {
		LOG_WRN("stored key ring is %u bytes, expected %u; ignoring it",
			(unsigned)len, (unsigned)sizeof(loaded));
		return 0;
	}
	n = read_cb(cb_arg, &loaded, sizeof(loaded));
	if (n != (ssize_t)sizeof(loaded)) {
		return -EIO;
	}
	/* Re-derive every kid rather than trusting the stored one. Flash is not a
	 * trust boundary, and the whole value of a self-authenticating identifier is
	 * lost if it is only checked on the path in. */
	{
		struct usslp_keyring rebuilt;
		unsigned kept = 0;

		usslp_keyring_init(&rebuilt);
		for (unsigned i = 0; i < USSLP_KEYRING_SLOTS; i++) {
			const struct usslp_ring_key *k = &loaded.keys[i];

			if (k->status == (uint8_t)USSLP_KEY_EMPTY) {
				continue;
			}
			if (usslp_keyring_add(&rebuilt, k->kid, k->pub, k->not_before,
					      k->not_after,
					      (enum usslp_key_status)k->status) == USSLP_OK) {
				kept++;
			} else {
				LOG_ERR("dropping a stored price authority key whose id does "
					"not match its bytes");
			}
		}
		keyring = rebuilt;
		LOG_INF("loaded %u price authority keys", kept);
	}
	return 0;
}

static struct settings_handler keyring_handler = {
	.name = "usslp/keyring",
	.h_set = keyring_settings_set,
};

static int keyring_save(void)
{
	return settings_save_one(KEYRING_SETTINGS_KEY "/ring", &keyring, sizeof(keyring));
}

/* ------------------------------------------------------------------------- */

int usslp_devcert_init(void)
{
	const struct flash_area *fa;
	struct identity_record rec;
	int rc;

	usslp_keyring_init(&keyring);

	rc = flash_area_open(IDENTITY_ID, &fa);
	if (rc != 0) {
		LOG_ERR("identity partition unavailable (%d)", rc);
		return USSLP_ERR_IO;
	}
	rc = flash_area_read(fa, 0, &rec, sizeof(rec));
	flash_area_close(fa);
	if (rc != 0) {
		LOG_ERR("reading the identity record failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	if (rec.magic != IDENTITY_MAGIC || rec.version != IDENTITY_VERSION) {
		/* An unprovisioned part. The label will advertise for commissioning
		 * over BLE and NFC and will not join a mesh or accept a price. */
		LOG_WRN("no factory identity record; this label is unprovisioned");
		return USSLP_ERR_MALFORMED;
	}

	identity.eui64 = rec.eui64;
	identity.tier = rec.tier;
	identity.hw_revision = rec.hw_revision;
	identity.protection = (enum usslp_key_protection)rec.protection;
	memcpy(identity.device_pub, rec.device_pub, sizeof(identity.device_pub));
	derive_serial(rec.eui64, identity.serial);
	identity_valid = true;

	if (identity.tier != CONFIG_USSLP_DISPLAY_TIER) {
		/* The image and the hardware disagree about which panel is fitted. The
		 * waveform tables are per panel and driving the wrong one can mark a
		 * panel permanently, so this is fatal rather than a warning. The OTA
		 * path checks the same thing before a swap; this check catches a
		 * mis-flashed part at the factory. */
		LOG_ERR("image is built for display tier %d, board reports tier %u",
			CONFIG_USSLP_DISPLAY_TIER, identity.tier);
		identity_valid = false;
		return USSLP_ERR_UNSUPPORTED;
	}

	rc = settings_subsys_init();
	if (rc != 0 && rc != -EALREADY) {
		LOG_ERR("settings init failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	rc = settings_register(&keyring_handler);
	if (rc != 0 && rc != -EEXIST) {
		LOG_ERR("registering the key ring handler failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	(void)settings_load_subtree(KEYRING_SETTINGS_KEY);

	if (usslp_keyring_len(&keyring) == 0u) {
		LOG_WRN("the price authority key ring is empty; every price update will "
			"be refused until a ring is synced");
	}

	LOG_INF("identity %s, tier %u, hw rev %u, key protection %u", identity.serial,
		identity.tier, identity.hw_revision, (unsigned)identity.protection);
	return USSLP_OK;
}

const struct usslp_device_identity *usslp_devcert_identity(void)
{
	return identity_valid ? &identity : NULL;
}

enum usslp_key_protection usslp_devcert_protection(void)
{
	return identity_valid ? identity.protection : USSLP_PROT_NONE;
}

int usslp_devcert_sign(const uint8_t *msg, size_t len, uint8_t sig[USSLP_ED25519_SIGNATURE_LEN])
{
	psa_status_t st;
	size_t out_len = 0;

	if (!identity_valid || device_key_id == PSA_KEY_ID_NULL) {
		return USSLP_ERR_UNSUPPORTED;
	}
	st = psa_sign_message(device_key_id, PSA_ALG_PURE_EDDSA, msg, len, sig,
			      USSLP_ED25519_SIGNATURE_LEN, &out_len);
	if (st != PSA_SUCCESS || out_len != USSLP_ED25519_SIGNATURE_LEN) {
		LOG_ERR("device signature failed (%d)", (int)st);
		return USSLP_ERR_AUTH;
	}
	return USSLP_OK;
}

struct usslp_keyring *usslp_price_keyring(void)
{
	return &keyring;
}

/*
 * The signed key-ring bundle.
 *
 *   0   magic "USKR"
 *   4   version (1)
 *   5   key count
 *   6   issued_at, int64 big endian
 *  14   entries: 32-byte public key, int64 not_before, int64 not_after, status
 *       byte = 49 bytes each
 *  ...  kid of the signing key, 28 bytes
 *  ...  Ed25519 signature over everything before the signature
 *
 * Verified against the ring already installed, so moving a label to a new set of
 * keys requires the ability to sign with the old ones.
 */
#define BUNDLE_MAGIC "USKR"
#define BUNDLE_ENTRY_BYTES 49
#define BUNDLE_FIXED_BYTES 14

static int64_t rd_i64(const uint8_t *p)
{
	uint64_t v = 0;

	for (unsigned i = 0; i < 8; i++) {
		v = (v << 8) | p[i];
	}
	return (int64_t)v;
}

int usslp_price_keyring_update(const uint8_t *bundle, size_t len)
{
	struct usslp_keyring staged;
	const struct usslp_ring_key *signer;
	uint8_t count;
	size_t body, off;
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	char kid[USSLP_KID_BUF];
	int rc;

	if (bundle == NULL || len < BUNDLE_FIXED_BYTES + USSLP_KID_LEN +
					  USSLP_ED25519_SIGNATURE_LEN) {
		return USSLP_ERR_MALFORMED;
	}
	if (memcmp(bundle, BUNDLE_MAGIC, 4) != 0 || bundle[4] != 1u) {
		return USSLP_ERR_MALFORMED;
	}
	count = bundle[5];
	if (count == 0u || count > USSLP_KEYRING_SLOTS) {
		return USSLP_ERR_MALFORMED;
	}
	body = BUNDLE_FIXED_BYTES + (size_t)count * BUNDLE_ENTRY_BYTES;
	if (len != body + USSLP_KID_LEN + USSLP_ED25519_SIGNATURE_LEN) {
		return USSLP_ERR_MALFORMED;
	}

	memcpy(kid, &bundle[body], USSLP_KID_LEN);
	kid[USSLP_KID_LEN] = '\0';
	signer = usslp_keyring_find(&keyring, kid);
	if (signer == NULL) {
		/* Nobody the label already trusts vouched for this ring. Refusing means
		 * a label that has fallen far behind needs hands on it; accepting would
		 * mean anyone on the mesh could install their own price authority. */
		LOG_ERR("key ring bundle signed by an unknown key %s", kid);
		return USSLP_ERR_AUTH;
	}
	usslp_sha256(bundle, body + USSLP_KID_LEN, digest);
	rc = usslp_ed25519_verify(signer->pub, digest, sizeof(digest), &bundle[body + USSLP_KID_LEN]);
	if (rc != USSLP_OK) {
		LOG_ERR("key ring bundle signature does not verify");
		return USSLP_ERR_AUTH;
	}

	/* Build the new ring completely before installing it, so a malformed entry
	 * halfway through cannot leave the label trusting a partial set. */
	usslp_keyring_init(&staged);
	off = BUNDLE_FIXED_BYTES;
	for (unsigned i = 0; i < count; i++) {
		const uint8_t *e = &bundle[off];
		enum usslp_key_status status =
			e[48] == 1u ? USSLP_KEY_RETIRING : USSLP_KEY_ACTIVE;

		rc = usslp_keyring_add(&staged, NULL, e, rd_i64(&e[32]), rd_i64(&e[40]), status);
		if (rc != USSLP_OK) {
			LOG_ERR("key ring bundle entry %u rejected (%d)", i, rc);
			return USSLP_ERR_MALFORMED;
		}
		off += BUNDLE_ENTRY_BYTES;
	}

	keyring = staged;
	rc = keyring_save();
	if (rc != 0) {
		/* The in-memory ring is live either way; a failed save means the label
		 * reverts on reboot, which is recoverable and visible. */
		LOG_ERR("persisting the new key ring failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	LOG_INF("installed a price authority key ring with %u keys", count);
	return USSLP_OK;
}
