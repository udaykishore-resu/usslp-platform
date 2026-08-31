package labelsim

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// Frame type 4: the attested update.
//
// Frame type 1 carries a sequence number, a price and pixels, and relies on the
// Shelf Edge Controller having verified the attestation on the label's behalf.
// That is what INTERFACE-CONTRACTS section 5 specifies, and against the threat
// model it states — an attacker with write access to the store's broker — it
// holds: the controller recomputes the digest and checks the signature, so a
// forged publication is refused before a waveform runs.
//
// It leaves one gap, and this frame closes it. The contract's guarantee is
// "a compromised controller ... cannot change a displayed price", but a
// controller that has been rooted or physically replaced is *inside* the trust
// boundary that claim rests on: it is the thing doing the verifying. A shelf
// label is a device a member of the public can reach, in a building with a
// service door, and the controller is a Linux box in the ceiling void above it.
//
// So the attested frame carries the whole signed tuple end to end — the five
// identifiers, the price, the effective instant, the sequence, the key
// identifier, the digest and the 64-byte Ed25519 signature — and the label
// rebuilds the canonical string and verifies for itself before driving a single
// pixel. The controller still verifies too; the two checks are independent, and
// an attacker now has to hold the platform's signing key rather than a
// controller.
//
// The layout is fixed by firmware/src/radio/usslp_wire.h, whose host tests
// decode frames this encoder produces. The first 33 bytes are byte-identical to
// a type 1 update so that a controller can build one frame and truncate it for
// a label that has not been upgraded, and so that both decoders share a head.
//
//	  0  version (1)                    33  effective_at   int64 (seconds, UTC)
//	  1  type (4)                       41  alg            uint8 (1 = Ed25519)
//	  2  sequence            int64      42  kid            28 bytes, not NUL terminated
//	 10  price_minor         int64      70  digest         32 bytes
//	 18  currency            3 bytes   102  signature      64 bytes
//	 21  flags               uint8     166  tenant_len     uint8
//	 22  template            uint8     167  store_len      uint8
//	 23  image_crc           uint32    168  label_len      uint8
//	 27  origin_x            uint16    169  sku_len        uint8
//	 29  origin_y            uint16    170  promotion_len  uint8
//	 31  image_len           uint16    171  identifiers, then the image
//
// What it costs is not nothing, and section 4's budget does not have room to
// spare: see AttestedOverheadBytes.
// ---------------------------------------------------------------------------

// Sizes fixed by the firmware's frame layout. They are constants rather than
// derived from Go types because the firmware asserts them numerically and a
// disagreement would only show up as a fleet that silently stops taking prices.
const (
	// AttestedHeaderBytes is the fixed portion of a type 4 frame, before the
	// identifiers and the image.
	AttestedHeaderBytes = 171
	// KeyIDLen is the width of the key-identifier field. It is fixed rather
	// than length-prefixed, so every key identifier the platform issues must be
	// exactly this long — which pki.KeyIDFor's "usslp-price-" plus sixteen hex
	// characters is.
	KeyIDLen = 28
	// DigestLen and SignatureLen are SHA-256 and Ed25519.
	DigestLen    = 32
	SignatureLen = 64
	// MaxIdentifierLen mirrors canon.ValidID's cap. The wire's length prefixes
	// are single bytes, so an identifier longer than this could not be framed
	// even if canon allowed it.
	MaxIdentifierLen = 128
)

// AttestAlgEd25519 is the on-wire algorithm code for canon.AttestationAlg.
//
// A byte rather than the string the JSON envelope carries, because a label has
// no use for a negotiation it is not allowed to lose: anything other than this
// value is refused before a key is touched.
const AttestAlgEd25519 = 1

// AttestedOverheadBytes is what end-to-end attestation adds to a frame, before
// the identifiers.
//
// It is exported because it is a number a deployment has to plan with rather
// than discover. 138 bytes of fixed header plus typically 40 to 60 bytes of
// identifiers is roughly two extra 802.15.4 fragments per hop, and on a shared
// 250 kbps channel that is paid on every hop of every update. See the package
// tests for what it does to the measured latency percentiles.
const AttestedOverheadBytes = AttestedHeaderBytes - updateHeaderBytes

// AttestedUpdate is a price update carrying its own proof.
type AttestedUpdate struct {
	Update
	// EffectiveAtUnix is whole seconds since the epoch, UTC. Whole seconds
	// because canon's canonical string formats the instant with Go's
	// time.RFC3339 layout, which carries no fractional part: sub-second
	// precision is dropped by the signer, so carrying it would be carrying
	// something that cannot affect the digest.
	EffectiveAtUnix int64
	Alg             uint8
	// KeyID is the price-authority key identifier, exactly KeyIDLen bytes.
	KeyID     string
	Digest    [DigestLen]byte
	Signature [SignatureLen]byte
	// The five identifiers of the signed tuple, in the order the canonical
	// string uses them.
	TenantID    canon.TenantID
	StoreID     canon.StoreID
	LabelID     canon.LabelID
	SKU         canon.SKU
	PromotionID canon.PromotionID
}

// AttestationInput rebuilds the tuple that was signed, from the frame as
// received.
//
// From the frame as received, and never from anything the controller asserted
// separately: that is the entire point. The digest this drives is computed over
// the price the label is about to render, so a signature lifted from a
// different price does not verify.
func (a AttestedUpdate) AttestationInput() canon.AttestationInput {
	return canon.AttestationInput{
		TenantID:    a.TenantID,
		StoreID:     a.StoreID,
		LabelID:     a.LabelID,
		SKU:         a.SKU,
		Price:       canon.Money{Amount: a.PriceMinor, Currency: a.Currency},
		EffectiveAt: time.Unix(a.EffectiveAtUnix, 0).UTC(),
		Sequence:    a.Sequence,
		PromotionID: a.PromotionID,
	}
}

// Attestation rebuilds the detached signature in the form canon.Verify takes.
func (a AttestedUpdate) Attestation() canon.Attestation {
	return canon.Attestation{
		Algorithm: canon.AttestationAlg,
		KeyID:     a.KeyID,
		Digest:    hex.EncodeToString(a.Digest[:]),
		Signature: base64.StdEncoding.EncodeToString(a.Signature[:]),
	}
}

// ValidateIdentifiers rejects a frame whose identifiers could not have come
// from the platform.
//
// An identifier carrying '/' or '#' is not a typo, it is an attempt to address
// outside a tenant's MQTT namespace, and hashing it as though it were
// legitimate would let a forged tuple be canonicalised. The firmware's
// usslp_attested_price_input applies the same rule before it hashes anything.
func (a AttestedUpdate) ValidateIdentifiers() error {
	for _, f := range []struct {
		name  string
		value string
		empty bool
	}{
		{"tenant", string(a.TenantID), false},
		{"store", string(a.StoreID), false},
		{"label", string(a.LabelID), false},
		{"sku", string(a.SKU), false},
		{"promotion", string(a.PromotionID), true},
	} {
		if f.value == "" {
			if f.empty {
				continue
			}
			return fmt.Errorf("%w: %s identifier is empty", ErrMalformedFrame, f.name)
		}
		if len(f.value) > MaxIdentifierLen || !canon.ValidID(f.value) {
			return fmt.Errorf("%w: %s identifier %q is not a legal USSLP identifier",
				ErrMalformedFrame, f.name, f.value)
		}
	}
	return nil
}

// EncodeAttestedUpdate serialises a type 4 frame.
func EncodeAttestedUpdate(a AttestedUpdate) ([]byte, error) {
	if len(a.Currency) != 3 {
		return nil, fmt.Errorf("labelsim: currency %q must be three characters", a.Currency)
	}
	if len(a.KeyID) != KeyIDLen {
		// The field is fixed width on the wire, so this is a hard limit rather
		// than a preference. Failing here is better than truncating: a truncated
		// key identifier resolves to no key and every label in the fleet would
		// refuse every price with an unhelpful "unknown kid".
		return nil, fmt.Errorf("labelsim: key id %q is %d bytes, the frame field is exactly %d",
			a.KeyID, len(a.KeyID), KeyIDLen)
	}
	if a.Alg == 0 {
		a.Alg = AttestAlgEd25519
	}
	if err := a.ValidateIdentifiers(); err != nil {
		return nil, err
	}
	if len(a.Image) > 0xFFFF {
		return nil, fmt.Errorf("labelsim: image of %d bytes exceeds the 65535-byte frame limit", len(a.Image))
	}
	ids := []string{string(a.TenantID), string(a.StoreID), string(a.LabelID), string(a.SKU), string(a.PromotionID)}
	idBytes := 0
	for _, id := range ids {
		idBytes += len(id)
	}

	b := make([]byte, AttestedHeaderBytes+idBytes+len(a.Image))
	b[0] = WireVersion
	b[1] = FrameAttestedUpdate
	binary.BigEndian.PutUint64(b[2:], uint64(a.Sequence))
	binary.BigEndian.PutUint64(b[10:], uint64(a.PriceMinor))
	copy(b[18:21], a.Currency)
	b[21] = a.Flags
	b[22] = a.Template
	crc := a.ImageCRC
	if crc == 0 {
		crc = crc32.Checksum(a.Image, crcTable)
	}
	binary.BigEndian.PutUint32(b[23:], crc)
	binary.BigEndian.PutUint16(b[27:], a.OriginX)
	binary.BigEndian.PutUint16(b[29:], a.OriginY)
	binary.BigEndian.PutUint16(b[31:], uint16(len(a.Image)))
	binary.BigEndian.PutUint64(b[33:], uint64(a.EffectiveAtUnix))
	b[41] = a.Alg
	copy(b[42:42+KeyIDLen], a.KeyID)
	copy(b[70:70+DigestLen], a.Digest[:])
	copy(b[102:102+SignatureLen], a.Signature[:])
	for i, id := range ids {
		b[166+i] = byte(len(id))
	}
	off := AttestedHeaderBytes
	for _, id := range ids {
		off += copy(b[off:], id)
	}
	copy(b[off:], a.Image)
	return b, nil
}

// DecodeAttestedUpdate parses a type 4 frame, verifying the image checksum.
//
// It performs no cryptography: decoding and verifying are separate steps
// because the sequence rule sits between them. A duplicated frame is the normal
// case under at-least-once mesh delivery, and discarding it on the free
// invariant before spending thirteen milliseconds of a coin cell on a signature
// is worth doing — the firmware's price handler orders it the same way and says
// so.
func DecodeAttestedUpdate(b []byte) (AttestedUpdate, error) {
	var a AttestedUpdate
	if len(b) < AttestedHeaderBytes {
		return a, fmt.Errorf("%w: %d bytes is shorter than an attested header", ErrMalformedFrame, len(b))
	}
	if b[0] != WireVersion {
		return a, fmt.Errorf("%w: protocol version %d, this device speaks %d", ErrMalformedFrame, b[0], WireVersion)
	}
	if b[1] != FrameAttestedUpdate {
		return a, fmt.Errorf("%w: frame type %d is not an attested update", ErrMalformedFrame, b[1])
	}
	a.Sequence = int64(binary.BigEndian.Uint64(b[2:]))
	a.PriceMinor = int64(binary.BigEndian.Uint64(b[10:]))
	a.Currency = string(b[18:21])
	a.Flags = b[21]
	a.Template = b[22]
	a.ImageCRC = binary.BigEndian.Uint32(b[23:])
	a.OriginX = binary.BigEndian.Uint16(b[27:])
	a.OriginY = binary.BigEndian.Uint16(b[29:])
	imageLen := int(binary.BigEndian.Uint16(b[31:]))
	a.EffectiveAtUnix = int64(binary.BigEndian.Uint64(b[33:]))
	a.Alg = b[41]
	a.KeyID = string(b[42 : 42+KeyIDLen])
	copy(a.Digest[:], b[70:70+DigestLen])
	copy(a.Signature[:], b[102:102+SignatureLen])

	lens := [5]int{int(b[166]), int(b[167]), int(b[168]), int(b[169]), int(b[170])}
	idBytes := lens[0] + lens[1] + lens[2] + lens[3] + lens[4]
	if len(b) < AttestedHeaderBytes+idBytes+imageLen {
		return AttestedUpdate{}, fmt.Errorf(
			"%w: header claims %d bytes of identifiers and %d of image, frame holds %d beyond the header",
			ErrMalformedFrame, idBytes, imageLen, len(b)-AttestedHeaderBytes)
	}
	off := AttestedHeaderBytes
	take := func(n int) string {
		s := string(b[off : off+n])
		off += n
		return s
	}
	a.TenantID = canon.TenantID(take(lens[0]))
	a.StoreID = canon.StoreID(take(lens[1]))
	a.LabelID = canon.LabelID(take(lens[2]))
	a.SKU = canon.SKU(take(lens[3]))
	a.PromotionID = canon.PromotionID(take(lens[4]))
	a.Image = append([]byte(nil), b[off:off+imageLen]...)

	if got := crc32.Checksum(a.Image, crcTable); got != a.ImageCRC {
		return AttestedUpdate{}, fmt.Errorf("%w: image checksum %08x, frame claims %08x",
			ErrMalformedFrame, got, a.ImageCRC)
	}
	return a, nil
}
