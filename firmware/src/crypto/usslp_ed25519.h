/*
 * usslp_ed25519.h - the one crypto primitive this firmware does not implement.
 *
 * Ed25519 verification is delegated to a backend rather than written here:
 *
 *   - On device the backend is src/crypto/psa_backend.c, which calls PSA Crypto
 *     as provided by Zephyr's mbedTLS integration. On the nRF52840 that resolves
 *     to the software implementation in mbedTLS; on a part with a CryptoCell
 *     (nRF5340, nRF54L) the same PSA call lands on the accelerator with no
 *     change here. Delegating is what makes that portable.
 *   - In the host tests the backend is tests/fake_ed25519.c, an oracle table of
 *     (public key, message, signature) triples produced by Go's crypto/ed25519.
 *
 * The consequence, stated plainly because it matters when reading the test
 * results: the host tests exercise every decision the verifier makes — key
 * resolution by kid, validity window, digest recomputation, constant-time
 * comparison, refuse-to-render — against signatures that a real Ed25519
 * implementation produced, but they do not exercise curve arithmetic. That is
 * PSA's job and PSA's test suite.
 */

#ifndef USSLP_ED25519_H
#define USSLP_ED25519_H

#include "../usslp_portable.h"

#define USSLP_ED25519_PUBLIC_KEY_LEN 32
#define USSLP_ED25519_SIGNATURE_LEN 64

/*
 * Verifies sig over msg under pub. Returns USSLP_OK when the signature is good,
 * USSLP_ERR_AUTH when it is not, and USSLP_ERR_UNSUPPORTED when the backend is
 * unavailable (a PSA driver that failed to initialise, for instance).
 *
 * A backend must treat every non-OK outcome as a rejection: there is no partial
 * success, and a caller is not permitted to render on USSLP_ERR_UNSUPPORTED.
 */
int usslp_ed25519_verify(const uint8_t pub[USSLP_ED25519_PUBLIC_KEY_LEN], const uint8_t *msg,
			 size_t msg_len, const uint8_t sig[USSLP_ED25519_SIGNATURE_LEN]);

#endif /* USSLP_ED25519_H */
