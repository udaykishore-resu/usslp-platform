package pki

import (
	"crypto/x509"
	"errors"
	"math/big"
	"testing"
	"time"
)

// TestRevocationBlocksVerification is the end-to-end revocation path: revoke,
// verification fails, the published CRL is a real DER list that parses and
// verifies, and a fresh verifier that only has the CRL reaches the same answer.
func TestRevocationBlocksVerification(t *testing.T) {
	t.Parallel()

	// A dedicated hierarchy: revocation is the one piece of shared mutable
	// state in a Hierarchy, so this test does not use the suite's.
	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	good := mustIssueLabel(t, h, tenantA, store1, "label-good")
	stolen := mustIssueLabel(t, h, tenantA, store1, "label-stolen")

	if _, err := h.VerifyChain(stolen.Certificate, stolen.Chain, VerifyOptions{}); err != nil {
		t.Fatalf("certificate does not verify before revocation: %v", err)
	}

	revokedAt := time.Now().Add(-time.Minute)
	if err := h.RevokeCertificate(stolen.Certificate, ReasonKeyCompromise, revokedAt); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = h.VerifyChain(stolen.Certificate, stolen.Chain, VerifyOptions{})
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked certificate: got %v, want ErrRevoked", err)
	}
	if _, err := h.VerifyChain(good.Certificate, good.Chain, VerifyOptions{}); err != nil {
		t.Fatalf("revoking one certificate broke another: %v", err)
	}

	der, err := h.GenerateCRL(RoleManufacturing, CRLOptions{})
	if err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("x509.ParseRevocationList: %v", err)
	}
	if err := crl.CheckSignatureFrom(h.Manufacturing.Certificate); err != nil {
		t.Fatalf("crl signature does not verify against its issuer: %v", err)
	}
	if crl.Number == nil || crl.Number.Sign() <= 0 {
		t.Errorf("crl number = %v, want a positive integer", crl.Number)
	}
	if !crl.NextUpdate.After(crl.ThisUpdate) {
		t.Errorf("crl NextUpdate %s is not after ThisUpdate %s", crl.NextUpdate, crl.ThisUpdate)
	}
	if got, want := len(crl.RevokedCertificateEntries), 1; got != want {
		t.Fatalf("crl lists %d entries, want %d", got, want)
	}
	entry := crl.RevokedCertificateEntries[0]
	if entry.SerialNumber.Cmp(stolen.Certificate.SerialNumber) != 0 {
		t.Errorf("crl lists serial %s, want %s", entry.SerialNumber.Text(16), stolen.Certificate.SerialNumber.Text(16))
	}
	if entry.ReasonCode != int(ReasonKeyCompromise) {
		t.Errorf("reason code = %d, want %d", entry.ReasonCode, ReasonKeyCompromise)
	}

	// The edge case: a Store Gateway that has only the CRL, not the registry,
	// must reach the same conclusion.
	edge := NewRevocationChecker()
	parsed, err := edge.LoadCRL(der, h.Manufacturing.Certificate)
	if err != nil {
		t.Fatalf("edge LoadCRL: %v", err)
	}
	if parsed.Number.Cmp(crl.Number) != 0 {
		t.Errorf("parsed crl number = %s, want %s", parsed.Number, crl.Number)
	}
	if edge.Len() != 1 {
		t.Errorf("edge registry holds %d entries, want 1", edge.Len())
	}
	if _, err := h.VerifyChain(stolen.Certificate, stolen.Chain, VerifyOptions{Revocation: edge}); !errors.Is(err, ErrRevoked) {
		t.Errorf("edge verifier: got %v, want ErrRevoked", err)
	}
	if _, err := h.VerifyChain(good.Certificate, good.Chain, VerifyOptions{Revocation: edge}); err != nil {
		t.Errorf("edge verifier rejected an unrevoked certificate: %v", err)
	}

	// The next list continues the number sequence, so a relying party can tell
	// a replayed old list from the current one.
	der2, err := h.GenerateCRL(RoleManufacturing, CRLOptions{})
	if err != nil {
		t.Fatalf("GenerateCRL (second): %v", err)
	}
	crl2, err := x509.ParseRevocationList(der2)
	if err != nil {
		t.Fatalf("parse second crl: %v", err)
	}
	if crl2.Number.Cmp(crl.Number) <= 0 {
		t.Errorf("second crl number %s does not exceed the first %s", crl2.Number, crl.Number)
	}
}

// TestRevokingSubCAInvalidatesEverythingBeneathIt is why revocation is checked
// on the whole chain rather than only the leaf.
func TestRevokingSubCAInvalidatesEverythingBeneathIt(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	label := mustIssueLabel(t, h, tenantA, store1, "label-under-subca")
	sec, _, err := h.IssueSEC(tenantA, store1, "sec-other-branch")
	if err != nil {
		t.Fatalf("issue sec: %v", err)
	}

	if err := h.RevokeCertificate(h.Manufacturing.Certificate, ReasonCACompromise, time.Now()); err != nil {
		t.Fatalf("revoke sub-CA: %v", err)
	}
	if _, err := h.VerifyChain(label.Certificate, label.Chain, VerifyOptions{}); !errors.Is(err, ErrRevoked) {
		t.Errorf("label under a revoked sub-CA: got %v, want ErrRevoked", err)
	}
	// The Shelf Controller branch is untouched.
	if _, err := h.VerifyChain(sec.Certificate, sec.Chain, VerifyOptions{}); err != nil {
		t.Errorf("revoking the manufacturing sub-CA broke the controller branch: %v", err)
	}
}

// TestRevocationRegistrySemantics covers the merge rules that stop an entry
// from being quietly downgraded.
func TestRevocationRegistrySemantics(t *testing.T) {
	t.Parallel()

	r := NewRevocationChecker()
	serial := big.NewInt(0xdeadbeef)
	early := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	late := early.Add(48 * time.Hour)

	if err := r.Add(RevocationEntry{SerialNumber: serial, Reason: ReasonKeyCompromise, RevokedAt: late}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := r.Add(RevocationEntry{SerialNumber: serial, Reason: ReasonSuperseded, RevokedAt: early}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	entry, ok := r.IsRevoked(serial)
	if !ok {
		t.Fatal("serial is not revoked after two Add calls")
	}
	if entry.Reason != ReasonKeyCompromise {
		t.Errorf("reason = %s, want key-compromise; a compromise must never be downgraded", entry.Reason)
	}
	if !entry.RevokedAt.Equal(early) {
		t.Errorf("revoked at %s, want the earliest recorded time %s", entry.RevokedAt, early)
	}

	bad := []struct {
		name  string
		entry RevocationEntry
	}{
		{"nil serial", RevocationEntry{Reason: ReasonSuperseded, RevokedAt: early}},
		{"zero serial", RevocationEntry{SerialNumber: big.NewInt(0), Reason: ReasonSuperseded, RevokedAt: early}},
		{"unknown reason", RevocationEntry{SerialNumber: big.NewInt(7), Reason: RevocationReason(77), RevokedAt: early}},
		{"no time", RevocationEntry{SerialNumber: big.NewInt(7), Reason: ReasonSuperseded}},
	}
	for _, tc := range bad {
		if err := r.Add(tc.entry); err == nil {
			t.Errorf("%s: Add accepted an invalid entry", tc.name)
		}
	}
}

// TestCRLSignatureIsCheckedOnLoad stops a forged distribution point from
// revoking a whole store's fleet.
func TestCRLSignatureIsCheckedOnLoad(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	victim := mustIssueLabel(t, h, tenantA, store1, "label-victim")
	if err := h.RevokeCertificate(victim.Certificate, ReasonCessationOfOperation, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	der, err := h.GenerateCRL(RoleManufacturing, CRLOptions{})
	if err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}

	edge := NewRevocationChecker()
	// The list is genuine but presented as if it came from the wrong authority.
	if _, err := edge.LoadCRL(der, h.ShelfController.Certificate); err == nil {
		t.Fatal("a CRL verified against the wrong issuer was accepted")
	}
	// And a tampered list is refused even against the right one.
	tampered := make([]byte, len(der))
	copy(tampered, der)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := edge.LoadCRL(tampered, h.Manufacturing.Certificate); err == nil {
		t.Fatal("a tampered CRL was accepted")
	}
	if edge.Len() != 0 {
		t.Errorf("rejected CRLs left %d entries in the registry", edge.Len())
	}
	if _, err := edge.LoadCRL(der, h.Manufacturing.Certificate); err != nil {
		t.Fatalf("genuine CRL rejected: %v", err)
	}
}

// TestRevokeBySerialAlone covers the audit-record path, where the certificate
// itself is long gone and only its serial survives.
func TestRevokeBySerialAlone(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	issued := mustIssueLabel(t, h, tenantA, store1, "label-by-serial")
	if err := h.Revoke(issued.Certificate.SerialNumber, ReasonPrivilegeWithdrawn, time.Now()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v, want ErrRevoked", err)
	}
	// With no recorded issuer the entry appears on every authority's list,
	// which is the safe direction to err.
	for _, role := range []CARole{RoleManufacturing, RoleShelfController} {
		der, err := h.GenerateCRL(role, CRLOptions{})
		if err != nil {
			t.Fatalf("GenerateCRL(%s): %v", role, err)
		}
		crl, err := x509.ParseRevocationList(der)
		if err != nil {
			t.Fatalf("parse %s crl: %v", role, err)
		}
		if len(crl.RevokedCertificateEntries) != 1 {
			t.Errorf("%s crl lists %d entries, want 1", role, len(crl.RevokedCertificateEntries))
		}
	}
}

// TestSkipRevocationIsExplicit makes sure the escape hatch does what it says
// and nothing more.
func TestSkipRevocationIsExplicit(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	issued := mustIssueLabel(t, h, tenantA, store1, "label-skip")
	if err := h.RevokeCertificate(issued.Certificate, ReasonSuperseded, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v, want ErrRevoked", err)
	}
	if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{SkipRevocation: true}); err != nil {
		t.Fatalf("SkipRevocation still applied the registry: %v", err)
	}
}
