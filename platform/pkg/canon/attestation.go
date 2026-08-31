package canon

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Price attestation
//
// Weights-and-measures regulation (NTEP in the US, OIML elsewhere) requires the
// price a shopper sees on the shelf to be the price charged at the till. USSLP
// discharges that obligation cryptographically rather than procedurally: the
// Label Service signs a canonical digest of every price it authorises, the SEC
// verifies the signature before driving a single E-Ink waveform, and the
// signature is retained in the audit stream for the statutory period.
//
// A compromised SEC, a corrupted mesh frame, or a malicious actor with write
// access to the local MQTT broker therefore cannot change a displayed price:
// they can only prevent one from being displayed, which is detected within
// three missed heartbeats.
// ---------------------------------------------------------------------------

// AttestationAlg is the only signature algorithm accepted. Ed25519 is chosen
// over ECDSA P-256 because verification is constant time without special care,
// signatures are 64 bytes, and a Cortex-M4F can verify one in ~13ms — inside
// the label's power budget.
const AttestationAlg = "Ed25519"

// ErrAttestationInvalid means the price on the wire is not the price that was
// authorised. It is always fatal for the update: the label keeps showing the
// last verified price rather than displaying an unattested one.
var ErrAttestationInvalid = errors.New("canon: price attestation invalid")

// AttestationInput is the exact tuple that gets signed. Every field that could
// change what a shopper is charged is included; nothing else is, so that
// re-rendering a label with a different template does not invalidate the
// attestation.
type AttestationInput struct {
	TenantID    TenantID
	StoreID     StoreID
	LabelID     LabelID
	SKU         SKU
	Price       Money
	EffectiveAt time.Time
	Sequence    int64
	PromotionID PromotionID
}

// CanonicalString renders the input in the one form all four tiers agree on.
//
// The encoding is deliberately dull: fixed field order, explicit separators,
// integer minor units, RFC 3339 UTC to the second, no optional whitespace, no
// map iteration. The label firmware implements the same 9 lines in C; any
// cleverness here would be a bug there.
func (a AttestationInput) CanonicalString() string {
	var sb strings.Builder
	sb.Grow(160)
	sb.WriteString("usslp.price.v1")
	sb.WriteByte('|')
	sb.WriteString(string(a.TenantID))
	sb.WriteByte('|')
	sb.WriteString(string(a.StoreID))
	sb.WriteByte('|')
	sb.WriteString(string(a.LabelID))
	sb.WriteByte('|')
	sb.WriteString(string(a.SKU))
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(a.Price.Amount, 10))
	sb.WriteByte('|')
	sb.WriteString(a.Price.Currency)
	sb.WriteByte('|')
	sb.WriteString(a.EffectiveAt.UTC().Format(time.RFC3339))
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(a.Sequence, 10))
	sb.WriteByte('|')
	sb.WriteString(string(a.PromotionID))
	return sb.String()
}

// Digest returns the SHA-256 of the canonical string.
func (a AttestationInput) Digest() [32]byte {
	return sha256.Sum256([]byte(a.CanonicalString()))
}

// Attest signs the input with the platform's price authority key.
func Attest(input AttestationInput, keyID string, priv ed25519.PrivateKey) (Attestation, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Attestation{}, fmt.Errorf("canon: attest: bad private key size %d", len(priv))
	}
	digest := input.Digest()
	sig := ed25519.Sign(priv, digest[:])
	return Attestation{
		Algorithm: AttestationAlg,
		KeyID:     keyID,
		Digest:    hex.EncodeToString(digest[:]),
		Signature: base64.StdEncoding.EncodeToString(sig),
		SignedAt:  time.Now().UTC(),
	}, nil
}

// Verify checks an attestation against the price actually about to be shown.
//
// It re-derives the digest from the local view of the update rather than
// trusting the digest field, so substituting a valid signature from a different
// price fails: the recomputed digest will not match what was signed.
func Verify(input AttestationInput, att Attestation, pub ed25519.PublicKey) error {
	if att.Algorithm != AttestationAlg {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrAttestationInvalid, att.Algorithm)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key size %d", ErrAttestationInvalid, len(pub))
	}
	want := input.Digest()
	got, err := hex.DecodeString(att.Digest)
	if err != nil || len(got) != len(want) {
		return fmt.Errorf("%w: malformed digest", ErrAttestationInvalid)
	}
	// Compare the transmitted digest with the locally recomputed one first so a
	// mismatch is reported as tampering rather than as a signature failure —
	// the two have different runbook entries.
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("%w: digest mismatch (transmitted price differs from signed price)", ErrAttestationInvalid)
		}
	}
	sig, err := base64.StdEncoding.DecodeString(att.Signature)
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrAttestationInvalid)
	}
	if !ed25519.Verify(pub, want[:], sig) {
		return fmt.Errorf("%w: signature does not verify under key %s", ErrAttestationInvalid, att.KeyID)
	}
	return nil
}

// AttestationInputFrom rebuilds the signed tuple from an update as received.
// The SEC calls this on the message it is holding, never on the sender's claim
// about what it signed.
func AttestationInputFrom(tenant TenantID, u PriceUpdated) AttestationInput {
	return AttestationInput{
		TenantID:    tenant,
		StoreID:     u.StoreID,
		LabelID:     u.LabelID,
		SKU:         u.SKU,
		Price:       u.Price,
		EffectiveAt: u.EffectiveAt,
		Sequence:    u.Sequence,
		PromotionID: u.PromotionID,
	}
}
