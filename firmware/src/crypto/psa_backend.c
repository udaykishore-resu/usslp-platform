/*
 * The device Ed25519 backend: PSA Crypto, as provided by Zephyr's mbedTLS
 * integration.
 *
 * Verification of one signature over a 32-byte digest takes about 13 ms on a
 * Cortex-M4F at 64 MHz with the mbedTLS software implementation, which is why
 * canon.AttestationAlg is Ed25519 rather than ECDSA P-256: it is inside the
 * label's power budget (13 ms at ~3 mA is 11 nAh, about a thousandth of what
 * the E-Ink refresh that follows will cost) and it is constant time without
 * special care.
 *
 * The key is passed in as raw bytes rather than as a PSA key handle because the
 * price-authority public keys are not secrets: they arrive in a signed key ring
 * and live in NVS. The device's *own* private key is different and never leaves
 * secure storage; see devcert.c.
 */

#include "usslp_ed25519.h"

#include <psa/crypto.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>

LOG_MODULE_REGISTER(usslp_crypto, CONFIG_USSLP_LOG_LEVEL);

static bool psa_ready;

/*
 * psa_crypto_init is idempotent but not free, and the price path calls the
 * verifier from a thread that may run before any other PSA user. Initialising
 * lazily under a mutex keeps the ordering out of main() without making every
 * verification pay for it.
 */
static K_MUTEX_DEFINE(psa_init_lock);

static int ensure_psa(void)
{
	psa_status_t st;

	if (psa_ready) {
		return USSLP_OK;
	}
	k_mutex_lock(&psa_init_lock, K_FOREVER);
	if (!psa_ready) {
		st = psa_crypto_init();
		if (st != PSA_SUCCESS) {
			k_mutex_unlock(&psa_init_lock);
			LOG_ERR("psa_crypto_init failed (%d); the label cannot verify a "
				"price and will not display one",
				(int)st);
			return USSLP_ERR_UNSUPPORTED;
		}
		psa_ready = true;
	}
	k_mutex_unlock(&psa_init_lock);
	return USSLP_OK;
}

int usslp_ed25519_verify(const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN], const uint8_t *msg,
			 size_t msg_len, const uint8_t sig[USSLP_ED25519_SIGNATURE_LEN])
{
	psa_key_attributes_t attr = PSA_KEY_ATTRIBUTES_INIT;
	psa_key_id_t key = PSA_KEY_ID_NULL;
	psa_status_t st;
	int rc;

	if (pub == NULL || msg == NULL || sig == NULL) {
		return USSLP_ERR_INVAL;
	}
	rc = ensure_psa();
	if (rc != USSLP_OK) {
		return rc;
	}

	psa_set_key_usage_flags(&attr, PSA_KEY_USAGE_VERIFY_MESSAGE);
	psa_set_key_algorithm(&attr, PSA_ALG_PURE_EDDSA);
	psa_set_key_type(&attr, PSA_KEY_TYPE_ECC_PUBLIC_KEY(PSA_ECC_FAMILY_TWISTED_EDWARDS));
	psa_set_key_bits(&attr, 255);
	psa_set_key_lifetime(&attr, PSA_KEY_LIFETIME_VOLATILE);

	st = psa_import_key(&attr, pub, USSLP_ED25519_PUBLIC_KEY_LEN, &key);
	if (st != PSA_SUCCESS) {
		LOG_WRN("importing a price authority key failed (%d)", (int)st);
		return USSLP_ERR_AUTH;
	}

	/* PSA_ALG_PURE_EDDSA hashes the message internally, which is correct here:
	 * canon.Attest signs the 32-byte SHA-256 digest *as a message*, not as a
	 * pre-hash. Using PSA_ALG_ED25519PH would compute Ed25519ph over the digest
	 * and disagree with every signature the platform has ever produced. */
	st = psa_verify_message(key, PSA_ALG_PURE_EDDSA, msg, msg_len, sig,
				USSLP_ED25519_SIGNATURE_LEN);
	psa_destroy_key(key);

	if (st == PSA_SUCCESS) {
		return USSLP_OK;
	}
	if (st != PSA_ERROR_INVALID_SIGNATURE) {
		/* A driver failure is not a bad signature, and conflating them would
		 * have an operator chasing a tampering alert while the real problem is
		 * an accelerator that did not initialise. It is still a refusal. */
		LOG_ERR("Ed25519 verification failed for a reason other than the "
			"signature (%d)",
			(int)st);
		return USSLP_ERR_UNSUPPORTED;
	}
	return USSLP_ERR_AUTH;
}
