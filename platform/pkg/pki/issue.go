package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Errors returned by the issuance path.
var (
	// ErrCSRSignature means the certificate signing request is not signed by
	// the private key matching the public key it carries.
	//
	// This is never a mistake. A CSR is self-signed precisely so that the CA
	// can prove the requester holds the key it is asking to have certified; a
	// request that fails the check is either a corrupted transfer — in which
	// case retrying costs nothing — or someone asking the CA to bind an
	// identity to a public key they do not control, which is the setup for
	// impersonating whoever does control it.
	ErrCSRSignature = errors.New("pki: certificate request signature does not verify")
	// ErrWeakKey means the public key in the request is below the platform's
	// floor. A 1024-bit RSA label would be indistinguishable from a compliant
	// one in every log and dashboard while being forgeable, so it is refused at
	// the only place that can refuse it: issuance.
	ErrWeakKey = errors.New("pki: public key does not meet the platform minimum")
	// ErrValidityExceedsIssuer means the requested certificate would still be
	// valid after the CA that signed it has expired. Issuing it would produce a
	// fleet that fails all at once on a day with no deploy to blame.
	ErrValidityExceedsIssuer = errors.New("pki: requested validity outlives the issuing authority")
)

// Issued is a freshly signed certificate together with everything a caller
// needs to install and present it.
type Issued struct {
	// Certificate is the end-entity certificate.
	Certificate *x509.Certificate
	// Chain is the issuing chain the holder must present alongside the leaf:
	// sub-CA first, then intermediate. The root is excluded — see
	// [CA.IssuingChain].
	Chain []*x509.Certificate
	// Identity is the normalised identity bound into the certificate.
	Identity Identity
	// Issuer names the authority that signed it.
	Issuer CARole
}

// CertificatePEM returns the leaf certificate in PEM form.
func (i *Issued) CertificatePEM() []byte {
	return encodeCertificatePEM(i.Certificate)
}

// ChainPEM returns the leaf followed by its issuing chain, in the order a TLS
// peer must send them (RFC 8446 §4.4.2: each certificate certifies the one
// before it).
func (i *Issued) ChainPEM() []byte {
	certs := append([]*x509.Certificate{i.Certificate}, i.Chain...)
	return encodeCertificatePEM(certs...)
}

// TLSCertificate assembles a tls.Certificate from the issued chain and the
// matching private key, ready to hand to [ServerTLSConfig] or
// [ClientTLSConfig].
func (i *Issued) TLSCertificate(key crypto.PrivateKey) (tls.Certificate, error) {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("pki: private key of type %T cannot sign", key)
	}
	if !publicKeysEqual(signer.Public(), i.Certificate.PublicKey) {
		return tls.Certificate{}, errors.New("pki: private key does not match the issued certificate")
	}
	out := tls.Certificate{
		Certificate: make([][]byte, 0, 1+len(i.Chain)),
		PrivateKey:  key,
		Leaf:        i.Certificate,
	}
	out.Certificate = append(out.Certificate, i.Certificate.Raw)
	for _, c := range i.Chain {
		out.Certificate = append(out.Certificate, c.Raw)
	}
	return out, nil
}

// CSRRequest asks the platform to bind an already-authorised identity to the
// public key in a certificate signing request.
//
// The identity is supplied by the caller, not read out of the CSR. A CSR
// arrives from a device on a factory line or from a workload in a cluster; both
// are attacker-adjacent, and both would happily ask for CN=USSLP-LABEL-anything
// if the CA read their subject. The only thing this package takes from the CSR
// is the public key and the proof that the requester holds the matching private
// key. Deciding that this requester deserves this identity happens upstream, in
// the provisioning service, against the manufacturing record.
type CSRRequest struct {
	// CSR is the parsed request. Its signature is verified before use.
	CSR *x509.CertificateRequest
	// Identity is the authorised identity to bind.
	Identity Identity
	// NotBefore and NotAfter override the profile's default validity window.
	// Zero values mean "now, backdated for clock skew" and "now plus the
	// profile lifetime for this kind of identity".
	NotBefore time.Time
	NotAfter  time.Time
	// Now fixes the issuance instant; zero means time.Now.
	Now time.Time
}

// LeafOptions tunes the convenience issuers that generate the key themselves.
type LeafOptions struct {
	// KeySpec overrides the profile's default key for this kind of identity.
	KeySpec KeySpec
	// NotBefore and NotAfter override the default validity window.
	NotBefore time.Time
	NotAfter  time.Time
	// Now fixes the issuance instant; zero means time.Now.
	Now time.Time
}

// IssueDeviceCert issues a label, SEC or SGU certificate from a certificate
// signing request produced on the device itself.
//
// This is the path a real device takes: the key pair is generated inside the
// device's secure element and the private key never leaves it, so the CSR is
// the only thing that crosses the factory network. Issuance is restricted to
// device kinds here so that a compromised provisioning service — which by
// design can reach this function — cannot mint a service or tenant identity
// with it.
func (h *Hierarchy) IssueDeviceCert(req CSRRequest) (*Issued, error) {
	if !req.Identity.Kind.IsDevice() {
		return nil, fmt.Errorf("pki: IssueDeviceCert: identity kind %q is not a device; "+
			"use IssueServiceCert or IssueTenantCert", string(req.Identity.Kind))
	}
	return h.issueFromCSR(req)
}

// IssueServiceCert issues a 90-day SPIFFE certificate for a cloud workload from
// its CSR. The short lifetime is the mitigation for a service key that lives in
// a container's memory rather than in hardware: an exfiltrated key is useful
// for at most a quarter, and in practice for far less, because the workload
// re-requests on every deploy.
func (h *Hierarchy) IssueServiceCert(req CSRRequest) (*Issued, error) {
	if req.Identity.Kind != KindService {
		return nil, fmt.Errorf("pki: IssueServiceCert: identity kind %q is not a service", string(req.Identity.Kind))
	}
	return h.issueFromCSR(req)
}

// IssueTenantCert issues a client certificate for a retailer's own integration
// calling the Universal Integration Gateway.
func (h *Hierarchy) IssueTenantCert(req CSRRequest) (*Issued, error) {
	if req.Identity.Kind != KindTenant {
		return nil, fmt.Errorf("pki: IssueTenantCert: identity kind %q is not a tenant client", string(req.Identity.Kind))
	}
	return h.issueFromCSR(req)
}

// issueFromCSR is the single code path every certificate in the platform goes
// through. Keeping it single is deliberate: the CSR signature check, the weak
// key check and the validity clamp are the three things that must never be
// skipped, and a second issuance path is how one of them eventually gets
// skipped.
func (h *Hierarchy) issueFromCSR(req CSRRequest) (*Issued, error) {
	if req.CSR == nil {
		return nil, errors.New("pki: issue: certificate request is nil")
	}
	// Proof of possession. Checked before anything else is read out of the
	// request so that a forged CSR is rejected without its contents ever
	// reaching the identity or key policy code.
	if err := req.CSR.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCSRSignature, err)
	}
	if err := acceptableLeafKey(req.CSR.PublicKey); err != nil {
		return nil, err
	}

	id, err := req.Identity.normalize(h.profile.TrustDomain)
	if err != nil {
		return nil, err
	}
	issuer, err := h.issuerFor(id.Kind)
	if err != nil {
		return nil, err
	}
	issuerKey, err := issuer.Signer()
	if err != nil {
		return nil, err
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	notBefore := req.NotBefore
	if notBefore.IsZero() {
		notBefore = now.Add(-h.profile.Backdate)
	}
	notAfter := req.NotAfter
	if notAfter.IsZero() {
		notAfter = now.Add(h.profile.leafValidity(id.Kind))
	}
	if !notAfter.After(notBefore) {
		return nil, fmt.Errorf("pki: issue %s: NotAfter %s is not after NotBefore %s",
			id.CommonName, notAfter.Format(time.RFC3339), notBefore.Format(time.RFC3339))
	}
	if notAfter.After(issuer.Certificate.NotAfter) {
		return nil, fmt.Errorf("%w: %s expires %s, issuer %s expires %s",
			ErrValidityExceedsIssuer, id.CommonName, notAfter.Format(time.RFC3339),
			issuer.Role, issuer.Certificate.NotAfter.Format(time.RFC3339))
	}

	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	uris, err := id.uris()
	if err != nil {
		return nil, err
	}
	ski, err := subjectKeyID(req.CSR.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("pki: issue %s: %w", id.CommonName, err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:       serial,
		Subject:            id.subject(h.profile),
		NotBefore:          notBefore.UTC(),
		NotAfter:           notAfter.UTC(),
		KeyUsage:           leafKeyUsage(req.CSR.PublicKey),
		ExtKeyUsage:        id.extKeyUsage(),
		DNSNames:           id.dnsNames(),
		URIs:               uris,
		SubjectKeyId:       ski,
		SignatureAlgorithm: issuer.KeySpec.signatureAlgorithm(),
		// cA=FALSE is asserted explicitly rather than left absent. An end-entity
		// certificate with no basicConstraints extension at all is treated as a
		// CA by a handful of old verifiers, and "a handful" across 50M devices
		// is not a rounding error.
		BasicConstraintsValid: true,
		IsCA:                  false,
		MaxPathLen:            -1,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.Certificate, req.CSR.PublicKey, issuerKey)
	if err != nil {
		return nil, fmt.Errorf("pki: create certificate for %s: %w", id.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse freshly created certificate for %s: %w", id.CommonName, err)
	}
	id.SerialNumber = serialHex(serial)
	id.NotBefore = cert.NotBefore
	id.NotAfter = cert.NotAfter

	h.logger().Info("pki certificate issued", append(id.LogAttrs(),
		"issuer_role", string(issuer.Role),
		"not_before", cert.NotBefore.Format(time.RFC3339),
		"not_after", cert.NotAfter.Format(time.RFC3339))...)

	return &Issued{
		Certificate: cert,
		Chain:       issuer.IssuingChain(),
		Identity:    id,
		Issuer:      issuer.Role,
	}, nil
}

// IssueLeaf generates a key pair for an identity, builds the CSR and issues the
// certificate in one step, returning the private key to the caller.
//
// It exists for two callers: tests, and the factory-provisioning simulator that
// stands in for a production line with no secure elements attached. Real
// hardware must never take this path — the whole point of generating the key
// inside the device is that no other process ever sees it — so production
// provisioning uses [Hierarchy.IssueDeviceCert] with a CSR from the device.
func (h *Hierarchy) IssueLeaf(id Identity, opts LeafOptions) (*Issued, crypto.Signer, error) {
	normalized, err := id.normalize(h.profile.TrustDomain)
	if err != nil {
		return nil, nil, err
	}
	spec := opts.KeySpec
	if spec == "" {
		spec = h.profile.leafKeySpec(normalized.Kind)
	}
	key, err := spec.Generate()
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate %s key for %s: %w", spec, normalized.CommonName, err)
	}
	csr, err := CreateCSR(normalized, key)
	if err != nil {
		return nil, nil, err
	}
	issued, err := h.issueFromCSR(CSRRequest{
		CSR:       csr,
		Identity:  normalized,
		NotBefore: opts.NotBefore,
		NotAfter:  opts.NotAfter,
		Now:       opts.Now,
	})
	if err != nil {
		return nil, nil, err
	}
	return issued, key, nil
}

// IssueLabel issues a certificate for a Tier 1 shelf label, generating the key.
func (h *Hierarchy) IssueLabel(tenant canon.TenantID, store canon.StoreID, label canon.LabelID) (*Issued, crypto.Signer, error) {
	return h.IssueLeaf(NewLabelIdentity(tenant, store, label), LeafOptions{})
}

// IssueSEC issues a certificate for a Shelf Edge Controller, generating the key.
func (h *Hierarchy) IssueSEC(tenant canon.TenantID, store canon.StoreID, sec canon.SECID) (*Issued, crypto.Signer, error) {
	return h.IssueLeaf(NewSECIdentity(tenant, store, sec), LeafOptions{})
}

// IssueSGU issues a certificate for a Store Gateway Unit, generating the key.
func (h *Hierarchy) IssueSGU(tenant canon.TenantID, store canon.StoreID, sgu canon.SGUID) (*Issued, crypto.Signer, error) {
	return h.IssueLeaf(NewSGUIdentity(tenant, store, sgu), LeafOptions{})
}

// IssueService issues a 90-day SPIFFE certificate for a cloud workload,
// generating the key.
func (h *Hierarchy) IssueService(namespace, service string) (*Issued, crypto.Signer, error) {
	return h.IssueLeaf(NewServiceIdentity(namespace, service), LeafOptions{})
}

// IssueTenantClient issues a client certificate for a tenant integration,
// generating the key.
func (h *Hierarchy) IssueTenantClient(tenant canon.TenantID, client string) (*Issued, crypto.Signer, error) {
	return h.IssueLeaf(NewTenantIdentity(tenant, client), LeafOptions{})
}

// CreateCSR builds and self-signs a certificate signing request for an
// identity.
//
// The subject and SANs it writes are advisory: the CA ignores them and uses the
// identity it was given out of band. They are included anyway so that an
// operator inspecting a queued request with standard tooling can see what the
// device believes it is, which is exactly the information needed when a
// provisioning mismatch has to be diagnosed.
func CreateCSR(id Identity, key crypto.Signer) (*x509.CertificateRequest, error) {
	normalized, err := id.normalize(id.TrustDomain)
	if err != nil {
		return nil, err
	}
	uris, err := normalized.uris()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.CertificateRequest{
		Subject:  normalized.subject(ProductionProfile()),
		DNSNames: normalized.dnsNames(),
		URIs:     uris,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("pki: create certificate request for %s: %w", normalized.CommonName, err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse freshly created certificate request: %w", err)
	}
	return csr, nil
}

// leafKeyUsage returns the key usage bits appropriate to a leaf's key type.
//
// digitalSignature is what TLS 1.3 actually needs: the handshake authenticates
// with a signature over the transcript and never encrypts to the certificate
// key. keyEncipherment is added for RSA leaves only, because an SGU also fronts
// legacy in-store POS hardware that still negotiates RSA key transport on a
// separate, non-USSLP listener; adding the bit to an ECDSA certificate would be
// meaningless and some verifiers reject the combination outright.
func leafKeyUsage(pub crypto.PublicKey) x509.KeyUsage {
	if _, ok := pub.(*rsa.PublicKey); ok {
		return x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}
	return x509.KeyUsageDigitalSignature
}

// acceptableLeafKey enforces the platform's minimum key strength.
//
// The floors are the ones a 2035-horizon deployment can defend: 2048-bit RSA,
// or a NIST curve of at least P-256. Anything weaker is refused rather than
// warned about, because a warning at issuance time is read by nobody and the
// resulting certificate outlives the person who ignored it by two years.
func acceptableLeafKey(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if bits := k.N.BitLen(); bits < 2048 {
			return fmt.Errorf("%w: RSA-%d is below the 2048-bit minimum", ErrWeakKey, bits)
		}
		return nil
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		default:
			return fmt.Errorf("%w: elliptic curve %s is not permitted", ErrWeakKey, k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		return nil
	default:
		return fmt.Errorf("%w: key type %T is not permitted", ErrWeakKey, pub)
	}
}

// publicKeysEqual reports whether two public keys are the same key. It is used
// to catch a certificate and private key that have been paired by mistake — a
// mismatch that otherwise surfaces as an opaque TLS handshake failure in
// production rather than as an error at load time.
func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if ea, ok := a.(equaler); ok {
		return ea.Equal(b)
	}
	return false
}
