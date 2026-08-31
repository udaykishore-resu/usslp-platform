package labelsim

import (
	"errors"
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// VerdictFor classifies a verification failure into the verdict the ack
// carries.
//
// The Go verifier reports failures as wrapped errors and the firmware reports
// them as an enum, so something has to map between them; doing it here, once,
// keeps every caller from pattern-matching on error strings. The sentinel
// errors are matched first because they are structural; only the cases
// canon.Verify distinguishes by message alone fall through to a string match,
// and those are commented individually.
//
// A nil error is VerdictOK, which is what the flags byte carries on every ack
// that is not an attestation refusal.
func VerdictFor(err error) AttestVerdict {
	switch {
	case err == nil:
		return VerdictOK
	case errors.Is(err, pki.ErrUnknownKeyID):
		return VerdictUnknownKeyID
	case errors.Is(err, pki.ErrKeyRetired):
		return VerdictKeyExpired
	case errors.Is(err, ErrMalformedFrame):
		// An identifier the platform could not have issued: the price tuple
		// cannot be canonicalised at all, so there is nothing to verify.
		return VerdictMalformedPrice
	case errors.Is(err, ErrNoKeyRing):
		// A label holding no ring reports the same thing a label holding an
		// empty one would: the key that signed this price is not one it knows.
		// The firmware cannot distinguish the two — usslp_price_keyring always
		// returns a ring, possibly empty — and the operator action is identical
		// either way, which is to push this label a ring.
		return VerdictUnknownKeyID
	case !errors.Is(err, canon.ErrAttestationInvalid):
		// Something outside the verifier failed. Failing closed and saying the
		// crypto was unavailable is honest: no verdict was reached.
		return VerdictCryptoUnavailable
	}

	// canon.Verify distinguishes its remaining cases by message. Matching on
	// them is unlovely, and the alternative — duplicating canon's checks here to
	// classify them — would mean two implementations of what a valid price is,
	// which is exactly what the firmware port went to some trouble to avoid.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unsupported algorithm"):
		return VerdictBadAlgorithm
	case strings.Contains(msg, "digest mismatch"):
		return VerdictDigestMismatch
	case strings.Contains(msg, "signature does not verify"):
		return VerdictBadSignature
	case strings.Contains(msg, "malformed digest"), strings.Contains(msg, "malformed signature"):
		// Unreachable across the mesh: the digest and signature are fixed-width
		// binary fields on the wire and cannot fail to decode. It is here for
		// the JSON path, where they are hex and base64.
		return VerdictMalformedPrice
	case strings.Contains(msg, "bad public key size"):
		return VerdictCryptoUnavailable
	}
	return VerdictBadSignature
}
