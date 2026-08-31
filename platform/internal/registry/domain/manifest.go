package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ManufacturingRecord is one line of a factory manifest: everything the
// platform knows about a device before it has ever been powered on.
//
// The manifest is the anchor of zero-touch provisioning. A device certificate
// proves that the Manufacturing Sub-CA signed *something*; the manifest is what
// says the platform expected that something to exist. Without it, anyone who
// obtained a signing capability could mint labels that the registry would
// happily enrol, because a chain that verifies is the only evidence a
// certificate carries about itself.
//
// The record is written when the production line flashes the unit and is
// immutable afterwards. Everything that changes — where the device ended up,
// what state it is in — lives on the [Device], not here.
type ManufacturingRecord struct {
	// Serial is the human-readable serial printed on the unit, the value a
	// technician scans off a shelf. It is the manifest's primary key.
	Serial string `json:"serial"`
	// EUI64 is the IEEE 802.15.4 extended address burned into the radio, 16
	// upper-case hex characters. It is unique across the fleet by construction
	// and is checked at provisioning against what the device announces, so a
	// certificate presented by a different radio is caught.
	EUI64 string `json:"eui64"`
	// HardwareTier names the display and radio generation. OTA jobs target it,
	// so a manifest that lies about it is how a cohort gets bricked.
	HardwareTier string `json:"hardware_tier"`
	// FirmwareVersion is what the line flashed.
	FirmwareVersion string `json:"firmware_version,omitempty"`

	// TenantID, StoreID and DeviceID are the identity the Manufacturing Sub-CA
	// bound into the certificate. Electronic shelf labels are ordered and
	// shipped per store, so the destination is known at flash time; the part
	// that is genuinely unknown until first power-on — which controller and
	// which zone the label ends up under — is not in the manifest and is
	// supplied by the provisioning request.
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	DeviceID string         `json:"device_id"`
	// Kind is the device tier this record describes.
	Kind DeviceKind `json:"kind"`

	// CertSerial is the lower-case hex serial of the certificate the line
	// issued.
	CertSerial string `json:"cert_serial"`
	// PublicKeySPKI is the DER-encoded SubjectPublicKeyInfo of the key inside
	// the device's secure element.
	//
	// Recording the key and not merely the certificate serial is what makes the
	// manifest resistant to a compromised issuance path: a second certificate
	// minted for the same identity carries a different key, so it fails the
	// check here even though it verifies perfectly against the hierarchy.
	PublicKeySPKI []byte `json:"public_key_spki"`

	// BatchID identifies the production run, which is the unit a recall or a
	// firmware advisory is scoped to.
	BatchID string `json:"batch_id,omitempty"`
	// ManufacturedAt is when the line flashed the unit.
	ManufacturedAt time.Time `json:"manufactured_at"`
}

// PublicKeyFingerprint returns the hex SHA-256 of the recorded SPKI. It is what
// logs and mismatch reports carry, because a DER blob in a log line is unusable
// and the fingerprint is enough to tell two keys apart.
func (r ManufacturingRecord) PublicKeyFingerprint() string {
	if len(r.PublicKeySPKI) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.PublicKeySPKI)
	return hex.EncodeToString(sum[:])
}

// Validate rejects a manifest line the registry could not act on. It runs at
// ingest rather than at provisioning so that a bad manifest is a rejected
// upload with a line number, not a store's worth of labels that mysteriously
// fail to enrol at 6 a.m. on opening day.
func (r ManufacturingRecord) Validate() error {
	switch {
	case r.Serial == "":
		return fmt.Errorf("%w: manifest record has no serial", ErrInvalid)
	case !canon.ValidID(r.DeviceID):
		return fmt.Errorf("%w: manifest record %s: device id %q", ErrInvalid, r.Serial, r.DeviceID)
	case !r.Kind.Valid():
		return fmt.Errorf("%w: manifest record %s: device kind %q", ErrInvalid, r.Serial, r.Kind)
	case !canon.ValidID(string(r.TenantID)):
		return fmt.Errorf("%w: manifest record %s: tenant id %q", ErrInvalid, r.Serial, r.TenantID)
	case !canon.ValidID(string(r.StoreID)):
		return fmt.Errorf("%w: manifest record %s: store id %q", ErrInvalid, r.Serial, r.StoreID)
	case r.HardwareTier == "":
		return fmt.Errorf("%w: manifest record %s: hardware tier is required", ErrInvalid, r.Serial)
	case r.CertSerial == "":
		return fmt.Errorf("%w: manifest record %s: certificate serial is required", ErrInvalid, r.Serial)
	case len(r.PublicKeySPKI) == 0:
		return fmt.Errorf("%w: manifest record %s: public key is required", ErrInvalid, r.Serial)
	}
	if err := validateEUI64(r.EUI64); err != nil {
		return fmt.Errorf("%w: manifest record %s: %s", ErrInvalid, r.Serial, err)
	}
	return nil
}

// Normalize returns the record with its case-insensitive fields canonicalised,
// so that a manifest exported by a factory MES in lower case and one exported
// by a spreadsheet in upper case produce identical registry state.
func (r ManufacturingRecord) Normalize() ManufacturingRecord {
	r.EUI64 = strings.ToUpper(strings.TrimSpace(r.EUI64))
	r.CertSerial = strings.ToLower(strings.TrimSpace(r.CertSerial))
	r.Serial = strings.TrimSpace(r.Serial)
	if r.Kind == "" {
		r.Kind = KindLabel
	}
	return r
}

// validateEUI64 enforces the 16-hex-character form. The address is used as a
// uniqueness key across the whole fleet, so a value that differs only in case
// or in a stray separator would create two records for one radio.
func validateEUI64(s string) error {
	if len(s) != 16 {
		return fmt.Errorf("eui64 %q is %d characters, expected 16", s, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
		default:
			return fmt.Errorf("eui64 %q must be upper-case hexadecimal", s)
		}
	}
	return nil
}

// Manifest is one ingested batch of manufacturing records.
type Manifest struct {
	// ManifestID is assigned at ingest and is what an operator quotes when a
	// batch has to be traced.
	ManifestID string `json:"manifest_id"`
	// TenantID scopes the batch. A manifest never spans tenants: the records
	// carry certificates issued into one tenant's namespace, and accepting a
	// mixed batch would mean one upload could enrol devices into a customer who
	// did not order them.
	TenantID canon.TenantID `json:"tenant_id"`
	// BatchID is the factory's own production-run identifier.
	BatchID string `json:"batch_id,omitempty"`
	// Records are the lines of the manifest.
	Records []ManufacturingRecord `json:"records"`
	// IngestedAt is when the platform accepted the batch.
	IngestedAt time.Time `json:"ingested_at"`
	// Source names who uploaded it, for the audit trail.
	Source string `json:"source,omitempty"`
}

// Validate checks the batch as a whole: every record individually, plus the two
// uniqueness properties that only make sense across a batch — no serial and no
// EUI-64 appears twice. A duplicate inside one file is a production-line fault
// and must be rejected before it becomes two registry entries fighting over one
// radio address.
func (m *Manifest) Validate() error {
	if !canon.ValidID(string(m.TenantID)) {
		return fmt.Errorf("%w: manifest tenant id %q", ErrInvalid, m.TenantID)
	}
	if len(m.Records) == 0 {
		return fmt.Errorf("%w: manifest carries no records", ErrInvalid)
	}
	serials := make(map[string]struct{}, len(m.Records))
	euis := make(map[string]struct{}, len(m.Records))
	devices := make(map[string]struct{}, len(m.Records))
	for i := range m.Records {
		rec := m.Records[i].Normalize()
		if err := rec.Validate(); err != nil {
			return err
		}
		if rec.TenantID != m.TenantID {
			return fmt.Errorf("%w: manifest record %s belongs to tenant %q, batch is %q",
				ErrInvalid, rec.Serial, rec.TenantID, m.TenantID)
		}
		if _, dup := serials[rec.Serial]; dup {
			return fmt.Errorf("%w: serial %s appears twice in the manifest", ErrInvalid, rec.Serial)
		}
		if _, dup := euis[rec.EUI64]; dup {
			return fmt.Errorf("%w: eui64 %s appears twice in the manifest", ErrInvalid, rec.EUI64)
		}
		if _, dup := devices[rec.DeviceID]; dup {
			return fmt.Errorf("%w: device id %s appears twice in the manifest", ErrInvalid, rec.DeviceID)
		}
		serials[rec.Serial] = struct{}{}
		euis[rec.EUI64] = struct{}{}
		devices[rec.DeviceID] = struct{}{}
		m.Records[i] = rec
	}
	return nil
}
