package adapter

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// SignatureEncoding is how a vendor renders an HMAC digest in a header.
type SignatureEncoding string

const (
	// EncodingBase64 is Shopify's and Square's convention.
	EncodingBase64 SignatureEncoding = "base64"
	// EncodingHex is NCR's and the convention most mapping-driven sources use.
	EncodingHex SignatureEncoding = "hex"
)

// SignHMACSHA256 returns the raw HMAC-SHA256 of msg under key.
func SignHMACSHA256(key string, msg []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(msg)
	return mac.Sum(nil)
}

// EncodeSignature renders a raw digest the way a vendor's header carries it.
func EncodeSignature(sum []byte, enc SignatureEncoding) string {
	if enc == EncodingHex {
		return hex.EncodeToString(sum)
	}
	return base64.StdEncoding.EncodeToString(sum)
}

// VerifyHMACSHA256 checks a vendor signature over msg.
//
// Three details matter and all three are the reason this is one shared function
// rather than four similar ones in four adapters:
//
//   - The comparison is over the raw digest bytes, using hmac.Equal, so it is
//     constant time. Comparing the encoded strings with == leaks the position
//     of the first differing byte through timing, which is enough to forge a
//     signature given enough attempts against an endpoint that is, by design,
//     reachable from the public internet.
//   - An empty configured key fails closed. Treating "no secret configured" as
//     "no signature required" turns a configuration mistake into an open price
//     endpoint, and the retailer would never know.
//   - A provided signature that does not decode fails as unauthorized rather
//     than as malformed, because an attacker choosing the encoding of their own
//     forged header should not be able to pick which error path they take.
func VerifyHMACSHA256(key string, msg []byte, provided string, enc SignatureEncoding, prefix string) error {
	if key == "" {
		return Unauthorized("no_secret", "binding has no signing key configured")
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return Unauthorized("missing_signature", "request carried no signature header")
	}
	if prefix != "" {
		if !strings.HasPrefix(provided, prefix) {
			return Unauthorized("bad_signature", "signature header is not in the expected form")
		}
		provided = provided[len(prefix):]
	}
	var got []byte
	var err error
	switch enc {
	case EncodingHex:
		got, err = hex.DecodeString(provided)
	default:
		got, err = base64.StdEncoding.DecodeString(provided)
	}
	if err != nil {
		return Unauthorized("bad_signature", "signature header is not valid "+string(enc))
	}
	if !hmac.Equal(got, SignHMACSHA256(key, msg)) {
		return Unauthorized("bad_signature", "signature does not match the request body")
	}
	return nil
}

// VerifySharedToken compares a bare secret from a header against the configured
// one in constant time. Clover's X-Clover-Auth and Oracle's WS-Security
// password are both this shape: no digest, just a secret the caller repeats.
func VerifySharedToken(expected, provided, reason string) error {
	if expected == "" {
		return Unauthorized("no_secret", "binding has no shared secret configured")
	}
	if provided == "" {
		return Unauthorized("missing_"+reason, "request carried no "+reason)
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		return Unauthorized("bad_"+reason, reason+" does not match")
	}
	return nil
}

// VerifyPeerIdentity accepts a delivery whose mTLS subject is in the binding's
// allow-list. It returns false when the binding does not use peer
// authentication, letting the adapter fall through to its signature check.
//
// The identity comes from the TLS handshake the server performed, never from a
// header: a proxy-supplied X-Client-CN is a claim, not a credential, and the
// distinction is the whole security property.
func VerifyPeerIdentity(b *Binding, peer string) (accepted, configured bool) {
	if b == nil || len(b.Secrets.PeerCommonNames) == 0 {
		return false, false
	}
	if peer == "" {
		return false, true
	}
	for _, cn := range b.Secrets.PeerCommonNames {
		if subtle.ConstantTimeCompare([]byte(cn), []byte(peer)) == 1 {
			return true, true
		}
	}
	return false, true
}
