package pki

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Revocation
//
// Why CRLs and not OCSP
//
// The textbook answer for a modern PKI is OCSP with stapling. USSLP uses signed
// certificate revocation lists instead, for reasons specific to a fleet of 50
// million battery-powered devices sitting behind retail WAN links:
//
//   - Devices are offline more than they are online. A shelf label wakes for a
//     few hundred milliseconds, verifies an update and sleeps; a store whose
//     broadband is down keeps running for hours on the Store Gateway's local
//     cache. OCSP asks a relying party to make a network call to a third party
//     at the exact moment it is least able to. A CRL is a file: the gateway
//     syncs one during its normal maintenance window and can enforce revocation
//     for the rest of the week with no connectivity at all.
//
//   - OCSP's failure mode is soft-fail, and soft-fail is not zero trust. Every
//     production OCSP deployment eventually accepts certificates when the
//     responder times out, because the alternative is an outage triggered by
//     someone else's server. A design whose security property evaporates under
//     load is not a security property. A locally cached CRL either contains the
//     serial or it does not, and the answer is the same whether the WAN is up.
//
//   - Volume. At 50M devices and a 0.1% annual revocation rate, the platform's
//     steady state is on the order of 50,000 entries — roughly 1.5 MB of DER,
//     which an SGU stores without noticing, and which reduces to a 50,000-entry
//     hash set of 128-bit serials that a Shelf Edge Controller can hold in a
//     megabyte of RAM. The same fleet under OCSP generates a query per
//     handshake: after a regional power cut, tens of millions of devices
//     reconnect inside a few minutes and every one of them wants an answer from
//     the same responder.
//
//   - Privacy and traffic analysis. An OCSP responder learns which device
//     talked to which service and when. That log is a map of a retailer's
//     store estate, held by the platform operator, for no operational benefit.
//
//   - Verifiability with what the device already has. A CRL is signed by the
//     same CA whose certificate is already provisioned on the device. No new
//     trust anchor, no responder certificate, no delegated signing key, and no
//     extra thing to get wrong in firmware that ships to 50M units.
//
// The cost is latency: revocation takes effect when the next list is
// distributed, not instantly. That is bounded by the CRL's NextUpdate and by
// how often the edge syncs — hours, not days, for a compromise. It is a
// deliberate trade, and it is acceptable because revoking a *label* does not
// need to be instant: a label cannot change a price whatever its certificate
// says, since prices are separately signed by the price authority. Revocation
// that must be instant — a compromised sub-CA — is handled by rotating the
// authority, not by waiting for a list.
// ---------------------------------------------------------------------------

// ErrRevoked means the certificate appears on the platform's revocation list.
var ErrRevoked = errors.New("pki: certificate has been revoked")

// RevocationReason is an RFC 5280 §5.3.1 CRLReason code.
type RevocationReason int

// The revocation reasons the platform uses. The set is deliberately small: an
// operator choosing between ten near-synonyms picks badly, and the reason code
// drives real behaviour — key compromise triggers a fleet-wide audit, cessation
// of operation does not.
const (
	// ReasonUnspecified is the fallback; prefer a specific reason so that the
	// audit stream can be triaged without reading free text.
	ReasonUnspecified RevocationReason = 0
	// ReasonKeyCompromise means the private key is believed to be in someone
	// else's hands. This is the one that pages people.
	ReasonKeyCompromise RevocationReason = 1
	// ReasonCACompromise means an authority key is compromised.
	ReasonCACompromise RevocationReason = 2
	// ReasonAffiliationChanged covers a device moved between tenants or stores,
	// which invalidates the tenant and store bound into its SANs.
	ReasonAffiliationChanged RevocationReason = 3
	// ReasonSuperseded covers routine re-issuance, e.g. a service certificate
	// replaced before its 90 days elapse.
	ReasonSuperseded RevocationReason = 4
	// ReasonCessationOfOperation covers a device decommissioned, returned or
	// scrapped — by volume, the commonest reason in a retail fleet.
	ReasonCessationOfOperation RevocationReason = 5
	// ReasonPrivilegeWithdrawn covers a tenant integration whose access has
	// been terminated.
	ReasonPrivilegeWithdrawn RevocationReason = 9
)

// String renders the reason for logs and audit records.
func (r RevocationReason) String() string {
	switch r {
	case ReasonUnspecified:
		return "unspecified"
	case ReasonKeyCompromise:
		return "key-compromise"
	case ReasonCACompromise:
		return "ca-compromise"
	case ReasonAffiliationChanged:
		return "affiliation-changed"
	case ReasonSuperseded:
		return "superseded"
	case ReasonCessationOfOperation:
		return "cessation-of-operation"
	case ReasonPrivilegeWithdrawn:
		return "privilege-withdrawn"
	default:
		return fmt.Sprintf("reason(%d)", int(r))
	}
}

// Valid reports whether the reason is one the platform issues.
func (r RevocationReason) Valid() bool {
	switch r {
	case ReasonUnspecified, ReasonKeyCompromise, ReasonCACompromise,
		ReasonAffiliationChanged, ReasonSuperseded, ReasonCessationOfOperation,
		ReasonPrivilegeWithdrawn:
		return true
	}
	return false
}

// RevocationEntry is one revoked certificate.
type RevocationEntry struct {
	// SerialNumber is the revoked certificate's serial.
	SerialNumber *big.Int
	// Reason is the RFC 5280 reason code.
	Reason RevocationReason
	// RevokedAt is when the revocation took effect. It is supplied rather than
	// taken from the clock so that a revocation backdated to the moment a
	// device was known to be stolen appears correctly on the list.
	RevokedAt time.Time
	// IssuerKeyID is the hex subject key identifier of the CA that issued the
	// certificate, when known. It scopes the entry to that CA's CRL.
	IssuerKeyID string
}

// RevocationChecker is the revocation registry a verifier consults.
//
// It is the object the TLS VerifyPeerCertificate callback holds. On the cloud
// side it is the authoritative registry the platform revokes into; on a Store
// Gateway it is populated from a synced CRL and is entirely offline. Both sides
// use the same type so that the edge and the cloud cannot drift into answering
// the question differently.
//
// It is safe for concurrent use by many handshakes at once.
type RevocationChecker struct {
	mu        sync.RWMutex
	entries   map[string]RevocationEntry
	updatedAt time.Time
}

// NewRevocationChecker returns an empty registry.
func NewRevocationChecker() *RevocationChecker {
	return &RevocationChecker{entries: make(map[string]RevocationEntry)}
}

// Add records a revocation. Re-revoking a serial keeps the earliest revocation
// time and the most serious reason already recorded is not downgraded: an entry
// that once said key-compromise must never quietly become superseded.
func (r *RevocationChecker) Add(entry RevocationEntry) error {
	if entry.SerialNumber == nil || entry.SerialNumber.Sign() <= 0 {
		return errors.New("pki: revoke: serial number must be a positive integer")
	}
	if !entry.Reason.Valid() {
		return fmt.Errorf("pki: revoke: unknown revocation reason %d", int(entry.Reason))
	}
	if entry.RevokedAt.IsZero() {
		return errors.New("pki: revoke: revocation time must be set")
	}
	key := serialHex(entry.SerialNumber)
	entry.SerialNumber = new(big.Int).Set(entry.SerialNumber)
	entry.RevokedAt = entry.RevokedAt.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.entries[key]; ok {
		if existing.RevokedAt.Before(entry.RevokedAt) {
			entry.RevokedAt = existing.RevokedAt
		}
		if existing.Reason == ReasonKeyCompromise || existing.Reason == ReasonCACompromise {
			entry.Reason = existing.Reason
		}
		if entry.IssuerKeyID == "" {
			entry.IssuerKeyID = existing.IssuerKeyID
		}
	}
	r.entries[key] = entry
	r.updatedAt = time.Now().UTC()
	return nil
}

// Check returns ErrRevoked if the certificate has been revoked.
func (r *RevocationChecker) Check(cert *x509.Certificate) error {
	if cert == nil {
		return nil
	}
	entry, ok := r.IsRevoked(cert.SerialNumber)
	if !ok {
		return nil
	}
	return fmt.Errorf("%w: %q serial %s revoked at %s (%s)",
		ErrRevoked, cert.Subject.CommonName, serialHex(entry.SerialNumber),
		entry.RevokedAt.Format(time.RFC3339), entry.Reason)
}

// IsRevoked reports whether a serial appears in the registry.
func (r *RevocationChecker) IsRevoked(serial *big.Int) (RevocationEntry, bool) {
	if serial == nil {
		return RevocationEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[serialHex(serial)]
	return entry, ok
}

// Len returns the number of revoked serials.
func (r *RevocationChecker) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// UpdatedAt returns when the registry last changed, which is what an edge
// verifier exposes as a staleness metric.
func (r *RevocationChecker) UpdatedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt
}

// Entries returns a copy of the registry sorted by serial number.
//
// The sort makes CRL generation deterministic: two CRLs produced from identical
// state are byte-identical apart from their number and timestamps, so an
// operator diffing published lists sees only real changes.
func (r *RevocationChecker) Entries() []RevocationEntry {
	r.mu.RLock()
	out := make([]RevocationEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].SerialNumber.Cmp(out[j].SerialNumber) < 0
	})
	return out
}

// LoadCRL verifies a DER-encoded CRL against its issuer and merges its entries
// into the registry.
//
// This is how a Store Gateway Unit learns about revocations: it fetches the
// list published by the CA, checks the signature against the CA certificate it
// was provisioned with, and merges. The signature check is not optional and not
// skippable — an unsigned CRL from a compromised distribution point could
// otherwise be used to revoke a store's entire fleet, which is a denial of
// service against the shelf edge.
//
// It returns the parsed list so the caller can record ThisUpdate/NextUpdate and
// alarm on a list that has gone stale.
func (r *RevocationChecker) LoadCRL(der []byte, issuer *x509.Certificate) (*x509.RevocationList, error) {
	if issuer == nil {
		return nil, errors.New("pki: load crl: issuer certificate is required")
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse crl: %w", err)
	}
	if err := crl.CheckSignatureFrom(issuer); err != nil {
		return nil, fmt.Errorf("pki: crl signature does not verify against %q: %w",
			issuer.Subject.CommonName, err)
	}
	issuerKeyID := hex.EncodeToString(issuer.SubjectKeyId)
	for _, e := range crl.RevokedCertificateEntries {
		entry := RevocationEntry{
			SerialNumber: e.SerialNumber,
			Reason:       RevocationReason(e.ReasonCode),
			RevokedAt:    e.RevocationTime,
			IssuerKeyID:  issuerKeyID,
		}
		if !entry.Reason.Valid() {
			entry.Reason = ReasonUnspecified
		}
		if entry.RevokedAt.IsZero() {
			entry.RevokedAt = crl.ThisUpdate
		}
		if err := r.Add(entry); err != nil {
			return nil, fmt.Errorf("pki: crl entry for serial %s: %w", e.SerialNumber.Text(16), err)
		}
	}
	return crl, nil
}

// Revoke marks the certificate with the given serial as revoked from the given
// instant.
//
// The revocation takes effect for every verifier sharing this hierarchy
// immediately, and for the edge at the next CRL distribution. Prefer
// [Hierarchy.RevokeCertificate] when the certificate itself is to hand: it
// records which authority issued the serial, which is what keeps the entry on
// the right CRL.
func (h *Hierarchy) Revoke(serial *big.Int, reason RevocationReason, at time.Time) error {
	if err := h.revocations.Add(RevocationEntry{
		SerialNumber: serial,
		Reason:       reason,
		RevokedAt:    at,
	}); err != nil {
		return err
	}
	h.logger().Warn("pki certificate revoked",
		"serial", serialHex(serial), "reason", reason.String(),
		"revoked_at", at.UTC().Format(time.RFC3339))
	return nil
}

// RevokeCertificate revokes a certificate, recording its issuing authority so
// the serial is published on that authority's CRL and only that one.
func (h *Hierarchy) RevokeCertificate(cert *x509.Certificate, reason RevocationReason, at time.Time) error {
	if cert == nil {
		return errors.New("pki: revoke: certificate is nil")
	}
	issuerKeyID := hex.EncodeToString(cert.AuthorityKeyId)
	if err := h.revocations.Add(RevocationEntry{
		SerialNumber: cert.SerialNumber,
		Reason:       reason,
		RevokedAt:    at,
		IssuerKeyID:  issuerKeyID,
	}); err != nil {
		return err
	}
	h.logger().Warn("pki certificate revoked",
		"serial", serialHex(cert.SerialNumber), "cn", cert.Subject.CommonName,
		"reason", reason.String(), "revoked_at", at.UTC().Format(time.RFC3339))
	return nil
}

// CRLOptions tunes CRL generation.
type CRLOptions struct {
	// At is the list's ThisUpdate; zero means time.Now.
	At time.Time
	// ValidFor sets NextUpdate relative to At. Zero means DefaultCRLValidity.
	ValidFor time.Duration
}

// DefaultCRLValidity is how long a published list is considered current.
//
// Seven days is chosen against the connectivity reality of the fleet, not
// against best practice for a web PKI: a store whose WAN is out for a long
// weekend must still be able to enforce revocation on Monday morning rather
// than fail closed on every handshake in the building. The platform republishes
// far more often than weekly; NextUpdate is the point at which an edge verifier
// starts alarming that its list has gone stale, not the republication interval.
const DefaultCRLValidity = 7 * 24 * time.Hour

// GenerateCRL produces a signed DER-encoded certificate revocation list for one
// authority.
//
// It includes every revocation recorded against that authority, plus any
// revocation recorded without a known issuer — a serial revoked by number alone
// through [Hierarchy.Revoke], where the caller had the serial from an audit
// record rather than the certificate. Listing such an entry on every CRL is the
// safe direction to err: a verifier keys on the serial, serials are unique
// across the whole hierarchy by construction, and the alternative is a
// compromised key that nobody can revoke because its certificate has been lost.
func (h *Hierarchy) GenerateCRL(issuer CARole, opts CRLOptions) ([]byte, error) {
	ca, err := h.CA(issuer)
	if err != nil {
		return nil, err
	}
	key, err := ca.Signer()
	if err != nil {
		return nil, err
	}
	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	validFor := opts.ValidFor
	if validFor <= 0 {
		validFor = DefaultCRLValidity
	}

	issuerKeyID := hex.EncodeToString(ca.Certificate.SubjectKeyId)
	var listed []x509.RevocationListEntry
	for _, e := range h.revocations.Entries() {
		if e.IssuerKeyID != "" && e.IssuerKeyID != issuerKeyID {
			continue
		}
		listed = append(listed, x509.RevocationListEntry{
			SerialNumber:   e.SerialNumber,
			RevocationTime: e.RevokedAt,
			ReasonCode:     int(e.Reason),
		})
	}

	tmpl := &x509.RevocationList{
		SignatureAlgorithm:        ca.KeySpec.signatureAlgorithm(),
		RevokedCertificateEntries: listed,
		Number:                    ca.nextCRLNumber(),
		ThisUpdate:                at,
		NextUpdate:                at.Add(validFor),
	}
	der, err := x509.CreateRevocationList(rand.Reader, tmpl, ca.Certificate, key)
	if err != nil {
		return nil, fmt.Errorf("pki: create crl for %s: %w", issuer, err)
	}
	h.logger().Info("pki crl published",
		"issuer_role", string(issuer), "crl_number", tmpl.Number.String(),
		"entries", len(listed), "next_update", tmpl.NextUpdate.Format(time.RFC3339))
	return der, nil
}
