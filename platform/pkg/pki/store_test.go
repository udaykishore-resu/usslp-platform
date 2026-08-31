package pki

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saveTestHierarchy bootstraps a hierarchy and writes it to a fresh directory.
func saveTestHierarchy(t *testing.T) (*Hierarchy, string) {
	t.Helper()
	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	dir := t.TempDir()
	if err := h.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return h, dir
}

// TestHierarchyPersistenceRoundTrips proves a reloaded hierarchy is the same
// hierarchy: it issues certificates the original's trust anchors verify.
func TestHierarchyPersistenceRoundTrips(t *testing.T) {
	t.Parallel()
	original, dir := saveTestHierarchy(t)

	issuedBefore := mustIssueLabel(t, original, tenantA, store1, "label-before-save")
	if err := original.RevokeCertificate(issuedBefore.Certificate, ReasonCessationOfOperation, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := original.GenerateCRL(RoleManufacturing, CRLOptions{}); err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}
	if err := original.Save(dir); err != nil {
		t.Fatalf("re-save: %v", err)
	}

	reloaded, err := Load(dir, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The offline root stays offline unless the caller asks for it.
	if reloaded.HasRootKey() {
		t.Error("Load brought the root key into a service that did not ask for it")
	}
	for _, role := range AllRoles() {
		ca, err := reloaded.CA(role)
		if err != nil {
			t.Fatalf("CA(%s): %v", role, err)
		}
		originalCA, _ := original.CA(role)
		if !ca.Certificate.Equal(originalCA.Certificate) {
			t.Errorf("%s: reloaded certificate differs from the original", role)
		}
		if role != RoleRoot && !ca.HasKey() {
			t.Errorf("%s: reloaded without its signing key", role)
		}
	}

	// Revocation state survives, so a restart does not un-revoke a stolen
	// device.
	if _, err := reloaded.VerifyChain(issuedBefore.Certificate, issuedBefore.Chain, VerifyOptions{}); !errors.Is(err, ErrRevoked) {
		t.Errorf("revocation did not survive a reload: %v", err)
	}
	// And the CRL number continues rather than restarting.
	der, err := reloaded.GenerateCRL(RoleManufacturing, CRLOptions{})
	if err != nil {
		t.Fatalf("GenerateCRL after reload: %v", err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("parse crl: %v", err)
	}
	if crl.Number.Int64() < 2 {
		t.Errorf("crl number restarted at %s after a reload", crl.Number)
	}

	// A certificate issued by the reloaded hierarchy verifies against the
	// original's anchors, which is what makes a rolling restart safe.
	issuedAfter := mustIssueLabel(t, reloaded, tenantA, store1, "label-after-load")
	if _, err := original.VerifyChain(issuedAfter.Certificate, issuedAfter.Chain, VerifyOptions{}); err != nil {
		t.Errorf("certificate from the reloaded hierarchy does not verify against the original: %v", err)
	}

	// With the ceremony flag the root comes back and can sign again.
	ceremony, err := Load(dir, LoadOptions{IncludeRootKey: true})
	if err != nil {
		t.Fatalf("Load with root key: %v", err)
	}
	if !ceremony.HasRootKey() {
		t.Error("IncludeRootKey did not load the root key")
	}
}

// TestSavedFilePermissions checks the modes every audit of this platform will
// look at first.
func TestSavedFilePermissions(t *testing.T) {
	t.Parallel()
	_, dir := saveTestHierarchy(t)

	for _, role := range AllRoles() {
		keyPath := filepath.Join(dir, string(role), caKeyFile)
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat %s: %v", keyPath, err)
		}
		if perm := info.Mode().Perm(); perm != keyMode {
			t.Errorf("%s: key mode %04o, want %04o", role, perm, keyMode)
		}
		certPath := filepath.Join(dir, string(role), caCertFile)
		certInfo, err := os.Stat(certPath)
		if err != nil {
			t.Fatalf("stat %s: %v", certPath, err)
		}
		if perm := certInfo.Mode().Perm(); perm != publicMode {
			t.Errorf("%s: certificate mode %04o, want %04o", role, perm, publicMode)
		}
	}
}

// TestLoadRefusesWorldReadableKey is the rule that turns a permissions mistake
// into a service that will not start rather than a platform quietly running
// with a readable CA key.
func TestLoadRefusesWorldReadableKey(t *testing.T) {
	t.Parallel()
	_, dir := saveTestHierarchy(t)

	keyPath := filepath.Join(dir, string(RoleManufacturing), caKeyFile)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := Load(dir, LoadOptions{})
	if !errors.Is(err, ErrInsecureKeyPermissions) {
		t.Fatalf("world-readable key: got %v, want ErrInsecureKeyPermissions", err)
	}
	if !strings.Contains(err.Error(), "chmod 600") || !strings.Contains(err.Error(), keyPath) {
		t.Errorf("error %q does not tell the operator how to fix it", err)
	}

	// Group-readable is refused too: "only the owner" means only the owner.
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Load(dir, LoadOptions{}); !errors.Is(err, ErrInsecureKeyPermissions) {
		t.Fatalf("group-readable key: got %v, want ErrInsecureKeyPermissions", err)
	}

	// Restored, it loads again, so the test is about the mode and nothing else.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Load(dir, LoadOptions{}); err != nil {
		t.Fatalf("hierarchy with correct permissions failed to load: %v", err)
	}
}

// TestLoadDetectsATamperedStore covers a certificate swapped on disk.
func TestLoadDetectsATamperedStore(t *testing.T) {
	t.Parallel()
	_, dir := saveTestHierarchy(t)
	foreign := foreignHierarchy(t)

	// Replace the Manufacturing sub-CA certificate with one from a different
	// hierarchy, keeping the original private key in place.
	path := filepath.Join(dir, string(RoleManufacturing), caCertFile)
	swapped := encodeCertificatePEM(foreign.Manufacturing.Chain()...)
	if err := os.WriteFile(path, swapped, publicMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir, LoadOptions{}); err == nil {
		t.Fatal("Load accepted a certificate that is not signed by its parent")
	}
}

// TestLoadRequiresIssuingKeys stops a service from starting in a state where it
// believes it can issue and cannot.
func TestLoadRequiresIssuingKeys(t *testing.T) {
	t.Parallel()
	_, dir := saveTestHierarchy(t)

	// Removing the root key is normal and expected.
	if err := os.Remove(filepath.Join(dir, string(RoleRoot), caKeyFile)); err != nil {
		t.Fatalf("remove root key: %v", err)
	}
	if _, err := Load(dir, LoadOptions{}); err != nil {
		t.Fatalf("a hierarchy without its offline root key must still load: %v", err)
	}

	// Removing an issuing key is not.
	if err := os.Remove(filepath.Join(dir, string(RoleServices), caKeyFile)); err != nil {
		t.Fatalf("remove services key: %v", err)
	}
	if _, err := Load(dir, LoadOptions{}); err == nil {
		t.Fatal("Load accepted an issuing authority with no signing key")
	}
}

// TestIdentityPersistenceRoundTrips covers what a service reads at start-up.
func TestIdentityPersistenceRoundTrips(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)
	dir := t.TempDir()

	issued, key, err := h.IssueSGU(tenantA, store1, "sgu-persisted")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := SaveIdentity(dir, "gateway", issued, key); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}

	stored, err := LoadIdentity(dir, "gateway")
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !stored.Certificate.Equal(issued.Certificate) {
		t.Error("reloaded certificate differs from the issued one")
	}
	if len(stored.Chain) != len(issued.Chain) {
		t.Errorf("reloaded chain has %d certificates, want %d", len(stored.Chain), len(issued.Chain))
	}
	if stored.Identity.DeviceID != "sgu-persisted" || stored.Identity.Kind != KindSGU {
		t.Errorf("reloaded identity = %+v", stored.Identity)
	}
	cert, err := stored.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if len(cert.Certificate) != 1+len(issued.Chain) {
		t.Errorf("tls certificate carries %d entries, want %d", len(cert.Certificate), 1+len(issued.Chain))
	}
	if _, err := h.VerifyChain(stored.Certificate, stored.Chain, VerifyOptions{}); err != nil {
		t.Errorf("reloaded identity does not verify: %v", err)
	}

	// The key file is owner-only, and a loosened one is refused.
	keyPath := filepath.Join(dir, "gateway.key.pem")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadIdentity(dir, "gateway"); !errors.Is(err, ErrInsecureKeyPermissions) {
		t.Fatalf("got %v, want ErrInsecureKeyPermissions", err)
	}
}

// TestSaveIdentityRejectsMismatchedKey catches the pairing mistake at the point
// it is made rather than at first handshake.
func TestSaveIdentityRejectsMismatchedKey(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)
	dir := t.TempDir()

	issued, _, err := h.IssueSEC(tenantA, store1, "sec-save-a")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, otherKey, err := h.IssueSEC(tenantA, store1, "sec-save-b")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := SaveIdentity(dir, "mismatched", issued, otherKey); err == nil {
		t.Fatal("SaveIdentity wrote a certificate and a key that do not match")
	}
}

// TestPEMHelpersRejectMalformedInput covers the bundle-parsing edge cases a
// service hits when a config map has been hand-edited.
func TestPEMHelpersRejectMalformedInput(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	good := encodeCertificatePEM(h.Manufacturing.Chain()...)
	certs, err := ParseCertificatesPEM(good)
	if err != nil {
		t.Fatalf("ParseCertificatesPEM: %v", err)
	}
	if len(certs) != 3 {
		t.Errorf("parsed %d certificates, want 3 (sub-CA, intermediate, root)", len(certs))
	}

	cases := map[string][]byte{
		"empty":            nil,
		"not pem":          []byte("this is not a certificate"),
		"trailing garbage": append(append([]byte{}, good...), []byte("\nnot a pem block\n")...),
		"wrong block type": []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"),
	}
	for name, data := range cases {
		if _, err := ParseCertificatesPEM(data); err == nil {
			t.Errorf("%s: ParseCertificatesPEM accepted malformed input", name)
		}
	}

	if _, err := ParsePrivateKeyPEM(good); err == nil {
		t.Error("ParsePrivateKeyPEM accepted a certificate")
	}
	key, err := KeyECDSAP256.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pemKey, err := EncodePrivateKeyPEM(key)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	parsed, err := ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	if !publicKeysEqual(parsed.Public(), key.Public()) {
		t.Error("private key did not round-trip through PEM")
	}
}
