package pki

import (
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

// Verification failures, distinguished so that a caller can act on them
// differently.
//
// The distinction is not cosmetic. An expired label certificate means a device
// missed its renewal window and needs a truck roll or an OTA nudge; a
// not-yet-valid one almost always means the device's real-time clock is wrong,
// which is fixed by an NTP sync and never by re-issuing; an unknown authority
// means something from outside the platform is knocking, which is a security
// event; and a usage mismatch means a certificate is being used for something
// it was not issued for, which is a bug in whichever service presented it.
// Four different runbook entries, so four different errors.
var (
	// ErrExpired means a certificate in the chain has passed its NotAfter.
	ErrExpired = errors.New("pki: certificate has expired")
	// ErrNotYetValid means a certificate in the chain has not reached its
	// NotBefore — almost always a wrong clock on the presenting device.
	ErrNotYetValid = errors.New("pki: certificate is not yet valid")
	// ErrUnknownAuthority means the chain does not lead to a USSLP trust
	// anchor.
	ErrUnknownAuthority = errors.New("pki: certificate is not issued by a USSLP authority")
	// ErrWrongUsage means the certificate is structurally unfit for the
	// requested use: the wrong extended key usage, or a path-length or name
	// constraint violated somewhere in the chain.
	ErrWrongUsage = errors.New("pki: certificate is not authorised for this use")
	// ErrHostnameMismatch means the peer certificate does not cover the name
	// the caller asked for.
	ErrHostnameMismatch = errors.New("pki: certificate does not cover the requested name")
)

// VerifyOptions tunes chain verification.
type VerifyOptions struct {
	// At is the instant to validate against; zero means time.Now.
	At time.Time
	// KeyUsages restricts which extended key usages the leaf may satisfy. Nil
	// means any: VerifyChain is called for client certificates as often as for
	// server certificates, and crypto/x509's own default of ServerAuth would
	// reject every device certificate the platform issues. The TLS builders in
	// this package always pass the correct usage explicitly.
	KeyUsages []x509.ExtKeyUsage
	// DNSName, when set, additionally requires the leaf to cover that name.
	DNSName string
	// Revocation overrides the hierarchy's own revocation registry — an edge
	// verifier uses this to supply the registry it loaded from a synced CRL.
	Revocation *RevocationChecker
	// SkipRevocation disables the revocation check. It exists for the narrow
	// case of validating a chain whose revocation status is being determined by
	// some other means (a CRL being parsed, for example); leaving it false is
	// what any online caller should do.
	SkipRevocation bool
}

// VerifyChain verifies a leaf certificate against the platform's trust anchors.
//
// The intermediates argument holds whatever the peer presented; the hierarchy's
// own intermediates are added to it, so a peer that sends a bare leaf still
// verifies while a peer that sends a foreign intermediate gains nothing —
// chain building only ever terminates at the USSLP root.
//
// Revocation is checked on every certificate in the resulting chain, not only
// the leaf. Revoking a compromised sub-CA has to invalidate everything beneath
// it in one action; if only leaves were checked, a compromised Manufacturing
// key would keep minting acceptable label certificates until each one was
// individually listed.
func (h *Hierarchy) VerifyChain(leaf *x509.Certificate, intermediates []*x509.Certificate, opts VerifyOptions) ([][]*x509.Certificate, error) {
	if leaf == nil {
		return nil, errors.New("pki: verify: leaf certificate is nil")
	}
	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	// Check the leaf's own window first. crypto/x509 reports "expired" for both
	// ends of the window and for any certificate in the chain, so doing this
	// here is what lets the common case carry a precise error.
	if err := checkValidityWindow(leaf, at); err != nil {
		return nil, err
	}

	// The common case — a peer presenting only its leaf, or presenting exactly
	// the intermediates this hierarchy already holds — reuses the precomputed
	// pool. Only a peer sending something extra pays for a clone.
	pool := h.intermediates
	if extra := unknownIntermediates(h, intermediates); len(extra) > 0 {
		pool = h.intermediates.Clone()
		for _, c := range extra {
			pool.AddCert(c)
		}
	}

	keyUsages := opts.KeyUsages
	if len(keyUsages) == 0 {
		keyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	}

	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         h.roots,
		Intermediates: pool,
		CurrentTime:   at,
		KeyUsages:     keyUsages,
		DNSName:       opts.DNSName,
	})
	if err != nil {
		return nil, classifyVerifyError(err, at)
	}

	if !opts.SkipRevocation {
		checker := opts.Revocation
		if checker == nil {
			checker = h.revocations
		}
		if checker != nil {
			for _, chain := range chains {
				for _, cert := range chain {
					if err := checker.Check(cert); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return chains, nil
}

// VerifyPeer verifies a chain and returns the peer's USSLP identity, which is
// the pair of operations every authorizer in the platform actually wants. The
// identity is only extracted after verification succeeds — reading an identity
// out of an unverified certificate is reading attacker-supplied data.
func (h *Hierarchy) VerifyPeer(leaf *x509.Certificate, intermediates []*x509.Certificate, opts VerifyOptions) (Identity, error) {
	if _, err := h.VerifyChain(leaf, intermediates, opts); err != nil {
		return Identity{}, err
	}
	id, err := IdentityFromCertificate(leaf)
	if err != nil {
		return Identity{}, err
	}
	if id.TrustDomain != h.profile.TrustDomain {
		return Identity{}, fmt.Errorf("%w: peer asserts trust domain %q, this hierarchy is %q",
			ErrMalformedIdentity, id.TrustDomain, h.profile.TrustDomain)
	}
	return id, nil
}

// unknownIntermediates returns the certificates a peer presented that are not
// already among the hierarchy's own authorities. Chain building can only ever
// terminate at the USSLP root, so adding a peer's certificates to the pool
// grants it nothing; the filter exists purely to keep the hot path allocation
// free for the overwhelmingly common case.
func unknownIntermediates(h *Hierarchy, presented []*x509.Certificate) []*x509.Certificate {
	if len(presented) == 0 {
		return nil
	}
	known := h.Authorities()
	var extra []*x509.Certificate
	for _, c := range presented {
		if c == nil {
			continue
		}
		found := false
		for _, ca := range known {
			if ca.Certificate.Equal(c) {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, c)
		}
	}
	return extra
}

// checkValidityWindow distinguishes the two ends of a certificate's lifetime.
func checkValidityWindow(cert *x509.Certificate, at time.Time) error {
	if at.Before(cert.NotBefore) {
		return fmt.Errorf("%w: %q is valid from %s, which is %s from now",
			ErrNotYetValid, cert.Subject.CommonName,
			cert.NotBefore.Format(time.RFC3339), cert.NotBefore.Sub(at).Round(time.Second))
	}
	if at.After(cert.NotAfter) {
		return fmt.Errorf("%w: %q expired at %s, %s ago",
			ErrExpired, cert.Subject.CommonName,
			cert.NotAfter.Format(time.RFC3339), at.Sub(cert.NotAfter).Round(time.Second))
	}
	return nil
}

// classifyVerifyError translates crypto/x509's verification errors into the
// package's sentinel errors, preserving the original text so the operator still
// sees which certificate and which constraint were at fault.
func classifyVerifyError(err error, at time.Time) error {
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return fmt.Errorf("%w: %v", ErrUnknownAuthority, err)
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return fmt.Errorf("%w: %v", ErrHostnameMismatch, err)
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		switch invalid.Reason {
		case x509.Expired:
			if invalid.Cert != nil {
				if werr := checkValidityWindow(invalid.Cert, at); werr != nil {
					return werr
				}
			}
			return fmt.Errorf("%w: %v", ErrExpired, err)
		case x509.TooManyIntermediates:
			// The path-length constraint did its job: something in the chain
			// tried to act as a CA when the authority above it said it may not.
			return fmt.Errorf("%w: path length constraint violated: %v", ErrWrongUsage, err)
		case x509.NotAuthorizedToSign, x509.IncompatibleUsage, x509.CANotAuthorizedForThisName,
			x509.CANotAuthorizedForExtKeyUsage, x509.TooManyConstraints, x509.NameConstraintsWithoutSANs:
			return fmt.Errorf("%w: %v", ErrWrongUsage, err)
		default:
			return fmt.Errorf("%w: %v", ErrUnknownAuthority, err)
		}
	}
	if errors.Is(err, ErrRevoked) {
		return err
	}
	return fmt.Errorf("pki: certificate verification failed: %w", err)
}
