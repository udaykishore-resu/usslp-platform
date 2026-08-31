package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

func priceInput(price int64, seq int64) canon.AttestationInput {
	return canon.AttestationInput{
		TenantID:    tenantA,
		StoreID:     store1,
		LabelID:     "label-000042",
		SKU:         "sku-99887766",
		Price:       canon.NewMoney(price, "GBP"),
		EffectiveAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		Sequence:    seq,
		PromotionID: "promo-summer",
	}
}

// TestPriceAttestationSignAndVerify is the shelf-edge path in miniature: the
// Label Service signs, the controller verifies, and a price that differs by one
// penny from the signed one does not verify.
func TestPriceAttestationSignAndVerify(t *testing.T) {
	t.Parallel()

	pa, err := NewPriceAuthority(PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	input := priceInput(249, 17)

	att, err := pa.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if att.Algorithm != canon.AttestationAlg {
		t.Errorf("algorithm = %q, want %q", att.Algorithm, canon.AttestationAlg)
	}
	if att.KeyID != pa.KeyID() {
		t.Errorf("kid = %q, want %q", att.KeyID, pa.KeyID())
	}

	// Directly against canon, as the label firmware does.
	if err := canon.Verify(input, att, pa.PublicKey()); err != nil {
		t.Fatalf("canon.Verify: %v", err)
	}

	ring, err := pa.KeyRing()
	if err != nil {
		t.Fatalf("KeyRing: %v", err)
	}
	if err := ring.Verify(input, att); err != nil {
		t.Fatalf("KeyRing.Verify: %v", err)
	}

	// One penny cheaper, same signature: the controller recomputes the digest
	// from the price it is about to display and refuses.
	tampered := priceInput(248, 17)
	if err := ring.Verify(tampered, att); !errors.Is(err, canon.ErrAttestationInvalid) {
		t.Fatalf("tampered price: got %v, want canon.ErrAttestationInvalid", err)
	}
	// So does a replay at a different sequence number.
	if err := ring.Verify(priceInput(249, 18), att); !errors.Is(err, canon.ErrAttestationInvalid) {
		t.Fatalf("replayed sequence: got %v, want canon.ErrAttestationInvalid", err)
	}
}

// TestKeyIDIsDerivedFromTheKey covers the property that stops a published ring
// entry from shadowing a legitimate key identifier.
func TestKeyIDIsDerivedFromTheKey(t *testing.T) {
	t.Parallel()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if KeyIDFor(pub) == KeyIDFor(other) {
		t.Fatal("two different keys derived the same identifier")
	}

	ring := NewKeyRing()
	// An entry claiming an identifier that belongs to a different key.
	err = ring.Add(RingKey{KeyID: KeyIDFor(other), PublicKey: pub, Status: KeyStatusActive})
	if !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("got %v, want ErrKeyIDMismatch", err)
	}
	if ring.Len() != 0 {
		t.Errorf("rejected entry was added anyway; ring holds %d keys", ring.Len())
	}
}

// TestRotationKeepsOldAttestationsVerifiable is the requirement that makes
// rotation survivable at the edge.
func TestRotationKeepsOldAttestationsVerifiable(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	pa, err := NewPriceAuthority(PriceAuthorityConfig{Now: start, Overlap: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	oldKID := pa.KeyID()

	before := priceInput(1099, 1)
	oldAtt, err := pa.Sign(before)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	rotateAt := start.Add(90 * 24 * time.Hour)
	newKID, err := pa.Rotate(rotateAt)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newKID == oldKID {
		t.Fatal("rotation produced the same key identifier")
	}
	if pa.KeyID() != newKID {
		t.Errorf("active kid = %q, want %q", pa.KeyID(), newKID)
	}

	after := priceInput(999, 2)
	newAtt, err := pa.Sign(after)
	if err != nil {
		t.Fatalf("Sign after rotation: %v", err)
	}
	if newAtt.KeyID != newKID {
		t.Errorf("new attestation kid = %q, want %q", newAtt.KeyID, newKID)
	}

	ring, err := pa.KeyRing()
	if err != nil {
		t.Fatalf("KeyRing: %v", err)
	}
	if ring.Len() != 2 {
		t.Fatalf("ring holds %d keys, want 2 (active plus the one in overlap)", ring.Len())
	}
	if ring.ActiveKeyID() != newKID {
		t.Errorf("ring active kid = %q, want %q", ring.ActiveKeyID(), newKID)
	}

	// A controller that synced after the rotation verifies both the price
	// signed before it and the price signed after it.
	duringOverlap := rotateAt.Add(24 * time.Hour)
	if err := ring.VerifyAt(before, oldAtt, duringOverlap); err != nil {
		t.Fatalf("price signed before rotation no longer verifies: %v", err)
	}
	if err := ring.VerifyAt(after, newAtt, duringOverlap); err != nil {
		t.Fatalf("price signed after rotation does not verify: %v", err)
	}

	// Past the overlap the retired key stops being accepted, and the failure is
	// distinguishable from a bad signature.
	afterOverlap := rotateAt.Add(31 * 24 * time.Hour)
	if err := ring.VerifyAt(before, oldAtt, afterOverlap); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("past the overlap: got %v, want ErrKeyRetired", err)
	}
	if err := ring.VerifyAt(after, newAtt, afterOverlap); err != nil {
		t.Fatalf("active key rejected past the old key's overlap: %v", err)
	}
}

// TestUnknownKeyIDRejected covers the stale-controller case: an attestation
// signed by a key the ring has never seen is refused, never guessed at.
func TestUnknownKeyIDRejected(t *testing.T) {
	t.Parallel()

	signer, err := NewPriceAuthority(PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	stranger, err := NewPriceAuthority(PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}

	input := priceInput(499, 3)
	att, err := stranger.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ring, err := signer.KeyRing()
	if err != nil {
		t.Fatalf("KeyRing: %v", err)
	}
	if err := ring.Verify(input, att); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("got %v, want ErrUnknownKeyID", err)
	}

	// A made-up identifier is refused the same way.
	att.KeyID = "usslp-price-0000000000000000"
	if err := ring.Verify(input, att); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("got %v, want ErrUnknownKeyID", err)
	}
}

// TestKeyRingPublicationRoundTrips covers the document a Shelf Edge Controller
// actually downloads.
func TestKeyRingPublicationRoundTrips(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa, err := NewPriceAuthority(PriceAuthorityConfig{Now: start})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	input := priceInput(1250, 9)
	oldAtt, err := pa.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := pa.Rotate(start.Add(24 * time.Hour)); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	newAtt, err := pa.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	doc, err := pa.PublishKeyRing()
	if err != nil {
		t.Fatalf("PublishKeyRing: %v", err)
	}

	// The published document contains no private material.
	var generic map[string]any
	if err := json.Unmarshal(doc, &generic); err != nil {
		t.Fatalf("published ring is not JSON: %v", err)
	}
	keys, ok := generic["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("published ring has %v keys, want 2", generic["keys"])
	}
	for _, k := range keys {
		entry := k.(map[string]any)
		if _, present := entry["d"]; present {
			t.Fatal("published ring contains a private key component")
		}
		if entry["kty"] != "OKP" || entry["crv"] != "Ed25519" || entry["alg"] != "EdDSA" {
			t.Errorf("unexpected JWK header fields: %v", entry)
		}
	}

	edge, err := ParseKeyRing(doc)
	if err != nil {
		t.Fatalf("ParseKeyRing: %v", err)
	}
	if edge.Len() != 2 {
		t.Fatalf("parsed ring holds %d keys, want 2", edge.Len())
	}
	if edge.ActiveKeyID() != pa.KeyID() {
		t.Errorf("parsed active kid = %q, want %q", edge.ActiveKeyID(), pa.KeyID())
	}
	at := start.Add(48 * time.Hour)
	if err := edge.VerifyAt(input, oldAtt, at); err != nil {
		t.Errorf("parsed ring cannot verify the pre-rotation attestation: %v", err)
	}
	if err := edge.VerifyAt(input, newAtt, at); err != nil {
		t.Errorf("parsed ring cannot verify the post-rotation attestation: %v", err)
	}

	// A document from a future publisher is refused rather than misparsed.
	bad := []byte(`{"usslp_keyring_format":"usslp.keyring.v2","keys":[]}`)
	if _, err := ParseKeyRing(bad); err == nil {
		t.Error("ParseKeyRing accepted an unknown format version")
	}
	// So is one whose key bytes have been substituted under an existing kid.
	tamperedDoc := tamperKeyRing(t, doc)
	if _, err := ParseKeyRing(tamperedDoc); !errors.Is(err, ErrKeyIDMismatch) {
		t.Errorf("tampered ring: got %v, want ErrKeyIDMismatch", err)
	}
}

// tamperKeyRing substitutes a fresh public key into the first entry of a
// published ring while leaving its kid alone.
func tamperKeyRing(t *testing.T, doc []byte) []byte {
	t.Helper()
	var generic map[string]any
	if err := json.Unmarshal(doc, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	entry := generic["keys"].([]any)[0].(map[string]any)
	entry["x"] = base64RawURL(pub)
	out, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// TestPriceAuthorityPersistence covers a Label Service restart: the keys that
// signed prices still on shelves must come back.
func TestPriceAuthorityPersistence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	pa, err := NewPriceAuthority(PriceAuthorityConfig{Now: start})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	input := priceInput(325, 11)
	oldAtt, err := pa.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := pa.Rotate(start.Add(time.Hour)); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := pa.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	keyPath := filepath.Join(dir, priceKeysDir, pa.KeyID()+".key.pem")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != keyMode {
		t.Errorf("key file mode %04o, want %04o", perm, keyMode)
	}

	reloaded, err := LoadPriceAuthority(dir, PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("LoadPriceAuthority: %v", err)
	}
	if reloaded.KeyID() != pa.KeyID() {
		t.Errorf("reloaded active kid = %q, want %q", reloaded.KeyID(), pa.KeyID())
	}
	ring, err := reloaded.KeyRing()
	if err != nil {
		t.Fatalf("KeyRing: %v", err)
	}
	if err := ring.VerifyAt(input, oldAtt, start.Add(2*time.Hour)); err != nil {
		t.Errorf("reloaded authority cannot verify a pre-restart attestation: %v", err)
	}
	newAtt, err := reloaded.Sign(input)
	if err != nil {
		t.Fatalf("reloaded Sign: %v", err)
	}
	if err := ring.Verify(input, newAtt); err != nil {
		t.Errorf("reloaded authority signs unverifiably: %v", err)
	}
}

// TestPriceAuthorityRefusesWorldReadableKey is the same rule the CA keys are
// held to, applied to the platform's most consequential secret.
func TestPriceAuthorityRefusesWorldReadableKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pa, err := NewPriceAuthority(PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	if err := pa.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	keyPath := filepath.Join(dir, priceKeysDir, pa.KeyID()+".key.pem")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err = LoadPriceAuthority(dir, PriceAuthorityConfig{})
	if !errors.Is(err, ErrInsecureKeyPermissions) {
		t.Fatalf("got %v, want ErrInsecureKeyPermissions", err)
	}
}

// TestRetireRemovesAKeyImmediately covers the compromised-key response.
func TestRetireRemovesAKeyImmediately(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pa, err := NewPriceAuthority(PriceAuthorityConfig{Now: now})
	if err != nil {
		t.Fatalf("NewPriceAuthority: %v", err)
	}
	compromised := pa.KeyID()
	input := priceInput(750, 4)
	att, err := pa.Sign(input)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := pa.Rotate(now); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := pa.Retire(pa.KeyID()); err == nil {
		t.Error("Retire removed the active signing key")
	}
	if err := pa.Retire(compromised); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	ring, err := pa.KeyRing()
	if err != nil {
		t.Fatalf("KeyRing: %v", err)
	}
	if ring.Len() != 1 {
		t.Errorf("ring holds %d keys after retiring one, want 1", ring.Len())
	}
	if err := ring.Verify(input, att); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("attestation from a retired key: got %v, want ErrUnknownKeyID", err)
	}
	if err := pa.Retire("usslp-price-not-a-key"); !errors.Is(err, ErrUnknownKeyID) {
		t.Errorf("retiring an unknown key: got %v, want ErrUnknownKeyID", err)
	}
}
