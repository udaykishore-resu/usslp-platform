package pki

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Errors returned when resolving a price-attestation signing key.
var (
	// ErrUnknownKeyID means the attestation names a key the verifier has never
	// seen. On a Shelf Edge Controller this usually means the controller's key
	// ring is stale and needs a sync; it is not, on its own, evidence of an
	// attack, but it is always a refusal — an update whose signer cannot be
	// identified is never displayed.
	ErrUnknownKeyID = errors.New("pki: unknown price authority key id")
	// ErrKeyRetired means the key that signed an attestation is past the end of
	// its verification overlap. The signature may be perfectly valid; the key is
	// simply no longer trusted to authorise a price.
	ErrKeyRetired = errors.New("pki: price authority key is retired")
	// ErrKeyIDMismatch means a key was offered under an identifier that is not
	// the one derived from its own public bytes.
	ErrKeyIDMismatch = errors.New("pki: key id does not match the key it names")
)

// KeyStatus is a key's position in the rotation lifecycle.
type KeyStatus string

// The two states a price authority key can be in. There is deliberately no
// "revoked" state: a compromised signing key is removed from the ring entirely
// and every price it signed is re-signed, because leaving a compromised key
// listed in any state invites a verifier to accept it.
const (
	// KeyStatusActive means the key is the one currently signing.
	KeyStatusActive KeyStatus = "active"
	// KeyStatusRetiring means the key no longer signs but is still accepted for
	// verification during the rotation overlap.
	KeyStatusRetiring KeyStatus = "retiring"
)

// KeyIDFor derives the canonical key identifier of an Ed25519 public key.
//
// The identifier is a truncated SHA-256 of the key itself, which makes it
// self-authenticating in a way an arbitrary label is not: two different keys
// cannot share a kid, and an attacker cannot publish a key ring entry that
// claims an existing kid with a substituted key, because the identifier would
// not match the bytes. The Shelf Edge Controller therefore does not need to
// trust the ordering or provenance of ring entries — it can check the kid
// against the key it is about to use.
//
// Sixteen hex characters (64 bits) is enough: the identifier's job is to select
// among a handful of keys, and forging one requires a preimage, not a birthday
// collision.
func KeyIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "usslp-price-" + hex.EncodeToString(sum[:8])
}

// RingKey is one public key in a price authority key ring, with the window
// during which a verifier will accept signatures made by it.
type RingKey struct {
	// KeyID is the identifier carried in canon.Attestation.KeyID.
	KeyID string
	// PublicKey is the Ed25519 verification key.
	PublicKey ed25519.PublicKey
	// NotBefore is when the key started signing.
	NotBefore time.Time
	// NotAfter is the end of the verification overlap. Zero means the key is
	// active and has no scheduled end.
	NotAfter time.Time
	// Status is active or retiring.
	Status KeyStatus
}

// ValidAt reports whether the key may be used to verify at the given instant.
func (k RingKey) ValidAt(at time.Time) bool {
	if !k.NotBefore.IsZero() && at.Before(k.NotBefore) {
		return false
	}
	if !k.NotAfter.IsZero() && at.After(k.NotAfter) {
		return false
	}
	return true
}

// KeyRing verifies price attestations by resolving the signing key named in
// each one.
//
// It is the object a Shelf Edge Controller holds. Its whole purpose is to make
// key rotation survivable at the edge: a controller that last synced on Tuesday
// must still be able to verify a price signed on Thursday with a key that was
// generated on Wednesday, and — the harder half — must still be able to verify
// prices that were signed on Monday with the key that has since been rotated
// out. Holding several keys with explicit windows is what makes both work
// without a flag day across 50 million devices.
//
// Safe for concurrent use: one ring serves every verification on a controller.
type KeyRing struct {
	mu          sync.RWMutex
	keys        map[string]RingKey
	activeKeyID string
	generatedAt time.Time
}

// NewKeyRing returns an empty ring.
func NewKeyRing() *KeyRing {
	return &KeyRing{keys: make(map[string]RingKey)}
}

// Add inserts or replaces a key.
//
// The key identifier must be the one derived from the public key itself; a
// mismatch is rejected rather than corrected, because an entry whose label does
// not describe its contents is either a bug in a publisher or an attempt to
// shadow a legitimate kid, and neither should be silently repaired.
func (r *KeyRing) Add(k RingKey) error {
	if len(k.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("pki: key ring: public key for %q is %d bytes, want %d",
			k.KeyID, len(k.PublicKey), ed25519.PublicKeySize)
	}
	derived := KeyIDFor(k.PublicKey)
	if k.KeyID == "" {
		k.KeyID = derived
	}
	if k.KeyID != derived {
		return fmt.Errorf("%w: entry claims %q but its public key derives %q", ErrKeyIDMismatch, k.KeyID, derived)
	}
	if k.Status == "" {
		k.Status = KeyStatusRetiring
	}
	k.PublicKey = append(ed25519.PublicKey(nil), k.PublicKey...)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys = make(map[string]RingKey)
	}
	r.keys[k.KeyID] = k
	if k.Status == KeyStatusActive {
		r.activeKeyID = k.KeyID
	}
	return nil
}

// Remove deletes a key from the ring. It is the response to a compromised
// signing key: remove it, rotate, and re-sign every price that is still on a
// shelf.
func (r *KeyRing) Remove(keyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.keys, keyID)
	if r.activeKeyID == keyID {
		r.activeKeyID = ""
	}
}

// Key returns the ring entry for an identifier.
func (r *KeyRing) Key(keyID string) (RingKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.keys[keyID]
	return k, ok
}

// ActiveKeyID returns the identifier of the key currently signing, or the empty
// string if the ring contains only retiring keys.
func (r *KeyRing) ActiveKeyID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeKeyID
}

// Keys returns every key, active first and then by identifier, so that
// serialising a ring twice produces the same bytes.
func (r *KeyRing) Keys() []RingKey {
	r.mu.RLock()
	out := make([]RingKey, 0, len(r.keys))
	for _, k := range r.keys {
		out = append(out, k)
	}
	active := r.activeKeyID
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if (out[i].KeyID == active) != (out[j].KeyID == active) {
			return out[i].KeyID == active
		}
		return out[i].KeyID < out[j].KeyID
	})
	return out
}

// Len returns the number of keys in the ring.
func (r *KeyRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// Verify checks a price attestation against the key it names, using the current
// time for the key's validity window.
func (r *KeyRing) Verify(input canon.AttestationInput, att canon.Attestation) error {
	return r.VerifyAt(input, att, time.Now())
}

// VerifyAt checks a price attestation as of a given instant.
//
// The instant matters on a device whose clock may be wrong, which is why it is
// a parameter rather than a call to time.Now buried inside: a Shelf Edge
// Controller passes the time it got from its gateway, not the time its own RTC
// believes, so a drifted clock cannot silently widen or narrow the window in
// which a rotated key is accepted.
//
// The signature check itself is canon.Verify, which recomputes the digest from
// the price about to be displayed rather than trusting the one on the wire.
func (r *KeyRing) VerifyAt(input canon.AttestationInput, att canon.Attestation, at time.Time) error {
	r.mu.RLock()
	key, ok := r.keys[att.KeyID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q (ring holds %d keys; a stale key ring is the usual cause)",
			ErrUnknownKeyID, att.KeyID, r.Len())
	}
	if !key.ValidAt(at) {
		return fmt.Errorf("%w: %q was valid %s to %s, checked at %s",
			ErrKeyRetired, key.KeyID,
			formatTimeOrOpen(key.NotBefore), formatTimeOrOpen(key.NotAfter),
			at.UTC().Format(time.RFC3339))
	}
	return canon.Verify(input, att, key.PublicKey)
}

func formatTimeOrOpen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// JWKS-like publication
//
// The ring is distributed to the edge as a JSON document modelled on RFC 7517
// so that anything already able to consume a JWKS — a tenant's own integration,
// an auditor's tooling — can read it without bespoke code. The USSLP-specific
// fields carry the rotation windows, which a plain JWKS has no way to express
// and which are exactly what an offline verifier needs.
//
// The document carries no signature of its own. Its integrity comes from the
// mutually authenticated channel it is fetched over (see [ClientTLSConfig]) and
// from the content-derived key identifiers: an attacker who can substitute the
// document still cannot make a controller accept a price signed by a key it
// does not hold, because a substituted key changes its own kid and every
// attestation names the kid it was signed with.
// ---------------------------------------------------------------------------

// keyRingDocument is the wire form of a published ring.
type keyRingDocument struct {
	// Format is a version marker so an edge device can refuse a document
	// written by a future publisher rather than misparse it.
	Format      string        `json:"usslp_keyring_format"`
	GeneratedAt time.Time     `json:"generated_at"`
	ActiveKeyID string        `json:"active_kid,omitempty"`
	Keys        []jwkDocument `json:"keys"`
}

// jwkDocument is one key in RFC 8037 OKP form plus the USSLP rotation window.
type jwkDocument struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	// X is the base64url-encoded, unpadded public key, per RFC 8037 §2.
	X         string     `json:"x"`
	Status    KeyStatus  `json:"usslp_status"`
	NotBefore *time.Time `json:"usslp_not_before,omitempty"`
	NotAfter  *time.Time `json:"usslp_not_after,omitempty"`
}

const keyRingFormat = "usslp.keyring.v1"

// MarshalJSON renders the ring as the JWKS-like document distributed to the
// edge. Only public halves are ever written.
func (r *KeyRing) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.document())
}

// PublishJSON renders the ring as indented JSON, the form written to disk and
// served to the edge.
func (r *KeyRing) PublishJSON() ([]byte, error) {
	out, err := json.MarshalIndent(r.document(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pki: publish key ring: %w", err)
	}
	return append(out, '\n'), nil
}

// document builds the wire form. Field order comes from the struct rather than
// from map iteration, so two publications of the same ring are byte-identical
// and an operator diffing them sees only real changes.
func (r *KeyRing) document() keyRingDocument {
	keys := r.Keys()
	doc := keyRingDocument{
		Format:      keyRingFormat,
		GeneratedAt: r.generatedAtOrNow(),
		ActiveKeyID: r.ActiveKeyID(),
		Keys:        make([]jwkDocument, 0, len(keys)),
	}
	for _, k := range keys {
		entry := jwkDocument{
			KeyType:   "OKP",
			Curve:     "Ed25519",
			Use:       "sig",
			Algorithm: "EdDSA",
			KeyID:     k.KeyID,
			X:         base64.RawURLEncoding.EncodeToString(k.PublicKey),
			Status:    k.Status,
		}
		if !k.NotBefore.IsZero() {
			nb := k.NotBefore.UTC()
			entry.NotBefore = &nb
		}
		if !k.NotAfter.IsZero() {
			na := k.NotAfter.UTC()
			entry.NotAfter = &na
		}
		doc.Keys = append(doc.Keys, entry)
	}
	return doc
}

func (r *KeyRing) generatedAtOrNow() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.generatedAt.IsZero() {
		return time.Now().UTC()
	}
	return r.generatedAt.UTC()
}

// ParseKeyRing decodes a published key ring document.
//
// Every entry is checked against [KeyIDFor] as it is added, so a document that
// has been tampered with in transit fails to parse rather than producing a ring
// that verifies the wrong things.
func ParseKeyRing(data []byte) (*KeyRing, error) {
	var doc keyRingDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pki: parse key ring: %w", err)
	}
	if doc.Format != keyRingFormat {
		return nil, fmt.Errorf("pki: parse key ring: unsupported format %q, this build understands %q",
			doc.Format, keyRingFormat)
	}
	ring := NewKeyRing()
	ring.generatedAt = doc.GeneratedAt
	for _, entry := range doc.Keys {
		if entry.KeyType != "OKP" || entry.Curve != "Ed25519" {
			return nil, fmt.Errorf("pki: parse key ring: key %q is %s/%s, only OKP/Ed25519 is accepted",
				entry.KeyID, entry.KeyType, entry.Curve)
		}
		raw, err := base64.RawURLEncoding.DecodeString(entry.X)
		if err != nil {
			return nil, fmt.Errorf("pki: parse key ring: key %q: %w", entry.KeyID, err)
		}
		k := RingKey{
			KeyID:     entry.KeyID,
			PublicKey: ed25519.PublicKey(raw),
			Status:    entry.Status,
		}
		if entry.NotBefore != nil {
			k.NotBefore = *entry.NotBefore
		}
		if entry.NotAfter != nil {
			k.NotAfter = *entry.NotAfter
		}
		if err := ring.Add(k); err != nil {
			return nil, fmt.Errorf("pki: parse key ring: %w", err)
		}
	}
	if doc.ActiveKeyID != "" {
		if _, ok := ring.Key(doc.ActiveKeyID); !ok {
			return nil, fmt.Errorf("pki: parse key ring: active kid %q is not in the document", doc.ActiveKeyID)
		}
		ring.mu.Lock()
		ring.activeKeyID = doc.ActiveKeyID
		ring.mu.Unlock()
	}
	return ring, nil
}
