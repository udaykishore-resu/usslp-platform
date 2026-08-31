package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// DefaultRotationOverlap is how long a rotated price authority key stays
// acceptable for verification after it stops signing.
//
// Thirty days is sized from the worst realistic edge case rather than from
// cryptographic hygiene. A price signed today may still be on a shelf a month
// from now — nothing forces a promotion to end — and a Shelf Edge Controller
// re-verifies its cached state from scratch every time it reboots, which for a
// store that loses power during a refit can be weeks after the price was set.
// An overlap shorter than the longest plausible gap between signing and
// verification turns a routine key rotation into a store full of labels that
// refuse to redisplay what they are already showing.
const DefaultRotationOverlap = 30 * 24 * time.Hour

// DefaultRetainedKeys is how many superseded keys the authority keeps. Three
// covers two unscheduled rotations inside one overlap window without unbounded
// growth of the published ring, which every device downloads.
const DefaultRetainedKeys = 3

// ErrNoActiveKey means the price authority has no key able to sign. It should
// be impossible in a running service and indicates a load that produced an
// empty or entirely retired key set.
var ErrNoActiveKey = errors.New("pki: price authority has no active signing key")

// priceKey is one Ed25519 keypair with its rotation window.
type priceKey struct {
	kid       string
	priv      ed25519.PrivateKey
	notBefore time.Time
	notAfter  time.Time
	status    KeyStatus
}

func (k priceKey) ring() RingKey {
	return RingKey{
		KeyID:     k.kid,
		PublicKey: k.priv.Public().(ed25519.PublicKey),
		NotBefore: k.notBefore,
		NotAfter:  k.notAfter,
		Status:    k.status,
	}
}

// PriceAuthority holds the Ed25519 keys with which the Label Service signs
// every authorised price.
//
// This is the platform's most consequential secret, and it is deliberately not
// the same secret as any TLS key. A device certificate lets a device speak; the
// price authority key lets the platform *authorise*. Separating them is what
// makes the guarantee at the top of this package true: an attacker who steals
// every device key in a store, or who owns the store's MQTT broker outright,
// can suppress updates but cannot author one, because the Shelf Edge Controller
// checks a signature made by this key before driving a single E-Ink waveform.
//
// Safe for concurrent use: one authority serves every signing goroutine in the
// Label Service, and rotation happens underneath them without a pause.
type PriceAuthority struct {
	mu       sync.RWMutex
	active   priceKey
	previous []priceKey
	overlap  time.Duration
	retained int
	log      *obs.Logger
}

// PriceAuthorityConfig parameterises a new price authority.
type PriceAuthorityConfig struct {
	// Now fixes the creation instant; zero means time.Now.
	Now time.Time
	// Overlap is how long a rotated key remains valid for verification. Zero
	// means DefaultRotationOverlap.
	Overlap time.Duration
	// MaxRetained caps the number of superseded keys kept. Zero means
	// DefaultRetainedKeys.
	MaxRetained int
	// Logger receives an audit line on creation and on every rotation. Nil
	// means silent.
	Logger *obs.Logger
}

// NewPriceAuthority generates a fresh Ed25519 keypair and returns an authority
// ready to sign.
//
// Ed25519 is not a choice made here: canon fixes it (see canon.AttestationAlg),
// because a Cortex-M4F label verifies an Ed25519 signature inside its power
// budget and an RSA one does not.
func NewPriceAuthority(cfg PriceAuthorityConfig) (*PriceAuthority, error) {
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	overlap := cfg.Overlap
	if overlap <= 0 {
		overlap = DefaultRotationOverlap
	}
	retained := cfg.MaxRetained
	if retained <= 0 {
		retained = DefaultRetainedKeys
	}
	log := cfg.Logger
	if log == nil {
		log = obs.NopLogger()
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate price authority key: %w", err)
	}
	active := newPriceKey(priv, now)
	log.Info("pki price authority created", "kid", active.kid,
		"not_before", active.notBefore.Format(time.RFC3339),
		"overlap", overlap.String())
	return &PriceAuthority{active: active, overlap: overlap, retained: retained, log: log}, nil
}

func newPriceKey(priv ed25519.PrivateKey, at time.Time) priceKey {
	pub := priv.Public().(ed25519.PublicKey)
	return priceKey{
		kid:       KeyIDFor(pub),
		priv:      priv,
		notBefore: at.UTC(),
		status:    KeyStatusActive,
	}
}

// SetLogger replaces the audit logger. Nil silences the authority.
func (a *PriceAuthority) SetLogger(l *obs.Logger) {
	if l == nil {
		l = obs.NopLogger()
	}
	a.mu.Lock()
	a.log = l
	a.mu.Unlock()
}

// KeyID returns the identifier of the key currently signing. It is what appears
// in canon.Attestation.KeyID and what an edge verifier resolves against its
// key ring.
func (a *PriceAuthority) KeyID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active.kid
}

// PublicKey returns the active verification key.
func (a *PriceAuthority) PublicKey() ed25519.PublicKey {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active.priv == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), a.active.priv.Public().(ed25519.PublicKey)...)
}

// Overlap returns the configured rotation overlap.
func (a *PriceAuthority) Overlap() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.overlap
}

// Sign produces the attestation that authorises a price.
//
// It signs canon's canonical digest, not a rendering or a message envelope, so
// re-templating a label or re-routing an update through a different broker does
// not invalidate the authorisation — while changing a single minor unit of the
// price does.
func (a *PriceAuthority) Sign(input canon.AttestationInput) (canon.Attestation, error) {
	a.mu.RLock()
	kid, priv := a.active.kid, a.active.priv
	a.mu.RUnlock()
	if priv == nil {
		return canon.Attestation{}, ErrNoActiveKey
	}
	return canon.Attest(input, kid, priv)
}

// Rotate generates a new signing key and moves the current one into the
// verification overlap.
//
// Rotation is routine — quarterly by policy, and immediately on any suspicion —
// and it must not require a fleet-wide coordination. It does not, because
// nothing needs to change on any device: a controller that has not yet synced
// keeps verifying with the old key, which remains in the ring; a controller
// that has synced verifies with either. The returned identifier is the new
// active kid.
func (a *PriceAuthority) Rotate(at time.Time) (string, error) {
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("pki: rotate price authority key: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	retiring := a.active
	if retiring.priv != nil {
		retiring.status = KeyStatusRetiring
		retiring.notAfter = at.Add(a.overlap)
		a.previous = append(a.previous, retiring)
	}
	a.active = newPriceKey(priv, at)
	a.pruneLocked(at)

	if a.log != nil {
		a.log.Warn("pki price authority key rotated",
			"new_kid", a.active.kid, "retired_kid", retiring.kid,
			"retired_valid_until", retiring.notAfter.Format(time.RFC3339),
			"retained_keys", len(a.previous))
	}
	return a.active.kid, nil
}

// pruneLocked drops superseded keys that are past their overlap, then trims to
// the retention cap, newest first. Must be called with the lock held.
func (a *PriceAuthority) pruneLocked(at time.Time) {
	kept := a.previous[:0]
	for _, k := range a.previous {
		if k.notAfter.IsZero() || at.Before(k.notAfter) {
			kept = append(kept, k)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].notBefore.After(kept[j].notBefore) })
	if len(kept) > a.retained {
		kept = kept[:a.retained]
	}
	a.previous = append([]priceKey(nil), kept...)
}

// Retire removes a key from the authority immediately, without waiting for its
// overlap to elapse. It is the response to a key believed compromised: the key
// stops being published, and any price still signed by it must be re-signed.
func (a *PriceAuthority) Retire(keyID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active.kid == keyID {
		return fmt.Errorf("pki: cannot retire %q: it is the active signing key; rotate first", keyID)
	}
	for i, k := range a.previous {
		if k.kid == keyID {
			a.previous = append(a.previous[:i], a.previous[i+1:]...)
			if a.log != nil {
				a.log.Warn("pki price authority key retired early", "kid", keyID)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownKeyID, keyID)
}

// KeyRing returns the public half of every key the authority still vouches for:
// the active signer and each key inside its verification overlap.
//
// This is what gets published to the edge. It contains no private material, so
// it can be served from a CDN, cached in a store, and inspected by an auditor
// without any of them gaining the ability to authorise a price.
func (a *PriceAuthority) KeyRing() (*KeyRing, error) {
	a.mu.RLock()
	keys := make([]priceKey, 0, 1+len(a.previous))
	if a.active.priv != nil {
		keys = append(keys, a.active)
	}
	keys = append(keys, a.previous...)
	a.mu.RUnlock()

	ring := NewKeyRing()
	ring.generatedAt = time.Now().UTC()
	for _, k := range keys {
		if err := ring.Add(k.ring()); err != nil {
			return nil, err
		}
	}
	return ring, nil
}

// PublishKeyRing renders the public key ring as the JWKS-like document
// distributed to Shelf Edge Controllers.
func (a *PriceAuthority) PublishKeyRing() ([]byte, error) {
	ring, err := a.KeyRing()
	if err != nil {
		return nil, err
	}
	return ring.PublishJSON()
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

const (
	priceRingFile = "ring.json"
	priceKeysDir  = "keys"
)

// Save writes the authority to a directory: the public ring document, and one
// PKCS#8 PEM file per private key at mode 0600.
//
// Every key still inside its overlap is written, not just the active one. A
// Label Service that restarts after a rotation must be able to answer for
// prices it signed before the restart, and the only way to do that is to still
// hold the key that signed them.
func (a *PriceAuthority) Save(dir string) error {
	if dir == "" {
		return errors.New("pki: save price authority: directory is required")
	}
	keysDir := filepath.Join(dir, priceKeysDir)
	if err := os.MkdirAll(keysDir, dirMode); err != nil {
		return fmt.Errorf("pki: save price authority: %w", err)
	}

	a.mu.RLock()
	keys := make([]priceKey, 0, 1+len(a.previous))
	if a.active.priv != nil {
		keys = append(keys, a.active)
	}
	keys = append(keys, a.previous...)
	a.mu.RUnlock()

	if len(keys) == 0 {
		return ErrNoActiveKey
	}
	for _, k := range keys {
		path := filepath.Join(keysDir, k.kid+".key.pem")
		if err := writePrivateKeyPEM(path, k.priv); err != nil {
			return err
		}
	}
	ring, err := a.KeyRing()
	if err != nil {
		return err
	}
	doc, err := ring.PublishJSON()
	if err != nil {
		return err
	}
	if err := writeFileMode(filepath.Join(dir, priceRingFile), doc, publicMode); err != nil {
		return err
	}
	return nil
}

// LoadPriceAuthority reads an authority previously written by
// [PriceAuthority.Save].
//
// Private key files are refused if their permissions allow any access beyond
// the owner — see [ErrInsecureKeyPermissions]. Each loaded key is checked
// against the identifier it is filed under, so a key file swapped on disk is
// caught at load rather than at the moment a store full of labels rejects a
// price.
func LoadPriceAuthority(dir string, cfg PriceAuthorityConfig) (*PriceAuthority, error) {
	doc, err := os.ReadFile(filepath.Join(dir, priceRingFile))
	if err != nil {
		return nil, fmt.Errorf("pki: load price authority: %w", err)
	}
	ring, err := ParseKeyRing(doc)
	if err != nil {
		return nil, err
	}
	overlap := cfg.Overlap
	if overlap <= 0 {
		overlap = DefaultRotationOverlap
	}
	retained := cfg.MaxRetained
	if retained <= 0 {
		retained = DefaultRetainedKeys
	}
	log := cfg.Logger
	if log == nil {
		log = obs.NopLogger()
	}

	a := &PriceAuthority{overlap: overlap, retained: retained, log: log}
	for _, rk := range ring.Keys() {
		path := filepath.Join(dir, priceKeysDir, rk.KeyID+".key.pem")
		signer, err := readPrivateKeyPEM(path)
		if err != nil {
			return nil, err
		}
		priv, ok := signer.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("pki: load price authority: %s holds a %T, want an Ed25519 key", path, signer)
		}
		if got := KeyIDFor(priv.Public().(ed25519.PublicKey)); got != rk.KeyID {
			return nil, fmt.Errorf("%w: %s holds key %q", ErrKeyIDMismatch, path, got)
		}
		k := priceKey{
			kid:       rk.KeyID,
			priv:      priv,
			notBefore: rk.NotBefore,
			notAfter:  rk.NotAfter,
			status:    rk.Status,
		}
		if rk.Status == KeyStatusActive {
			a.active = k
		} else {
			a.previous = append(a.previous, k)
		}
	}
	if a.active.priv == nil {
		return nil, ErrNoActiveKey
	}
	return a, nil
}
