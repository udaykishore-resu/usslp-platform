package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestBootstrapChainVerifies is the load-bearing test of the package: a leaf
// issued by the deepest sub-CA must verify all the way to the offline root.
func TestBootstrapChainVerifies(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	cases := []struct {
		name        string
		issue       func() (*Issued, error)
		wantIssuer  CARole
		wantChain   int // leaf + intermediates + root
		wantUsage   x509.ExtKeyUsage
		wantNoUsage x509.ExtKeyUsage
	}{
		{
			name: "label via manufacturing sub-ca",
			issue: func() (*Issued, error) {
				iss, _, err := h.IssueLabel(tenantA, store1, "label-0001")
				return iss, err
			},
			wantIssuer:  RoleManufacturing,
			wantChain:   4,
			wantUsage:   x509.ExtKeyUsageClientAuth,
			wantNoUsage: x509.ExtKeyUsageServerAuth,
		},
		{
			name: "sec via shelf controller sub-ca",
			issue: func() (*Issued, error) {
				iss, _, err := h.IssueSEC(tenantA, store1, "sec-0001")
				return iss, err
			},
			wantIssuer:  RoleShelfController,
			wantChain:   4,
			wantUsage:   x509.ExtKeyUsageClientAuth,
			wantNoUsage: x509.ExtKeyUsageServerAuth,
		},
		{
			name: "service via services intermediate",
			issue: func() (*Issued, error) {
				iss, _, err := h.IssueService("usslp-prod", "label-service")
				return iss, err
			},
			wantIssuer: RoleServices,
			wantChain:  3,
			wantUsage:  x509.ExtKeyUsageServerAuth,
		},
		{
			name: "tenant client via tenant management intermediate",
			issue: func() (*Issued, error) {
				iss, _, err := h.IssueTenantClient(tenantA, "pos-adapter")
				return iss, err
			},
			wantIssuer:  RoleTenantManagement,
			wantChain:   3,
			wantUsage:   x509.ExtKeyUsageClientAuth,
			wantNoUsage: x509.ExtKeyUsageServerAuth,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issued, err := tc.issue()
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			if issued.Issuer != tc.wantIssuer {
				t.Errorf("issuer = %s, want %s", issued.Issuer, tc.wantIssuer)
			}
			chains, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{
				KeyUsages: []x509.ExtKeyUsage{tc.wantUsage},
			})
			if err != nil {
				t.Fatalf("VerifyChain: %v", err)
			}
			if len(chains) != 1 {
				t.Fatalf("got %d chains, want exactly 1", len(chains))
			}
			if got := len(chains[0]); got != tc.wantChain {
				var names []string
				for _, c := range chains[0] {
					names = append(names, c.Subject.CommonName)
				}
				t.Errorf("chain length = %d, want %d: %v", got, tc.wantChain, names)
			}
			if last := chains[0][len(chains[0])-1]; last.Subject.CommonName != h.Root.Certificate.Subject.CommonName {
				t.Errorf("chain does not terminate at the root, got %q", last.Subject.CommonName)
			}
			if issued.Certificate.IsCA {
				t.Error("leaf certificate is marked as a CA")
			}
			if !issued.Certificate.BasicConstraintsValid {
				t.Error("leaf certificate does not assert basic constraints")
			}
			if tc.wantNoUsage != 0 {
				if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{
					KeyUsages: []x509.ExtKeyUsage{tc.wantNoUsage},
				}); !errors.Is(err, ErrWrongUsage) {
					t.Errorf("verifying with a usage the leaf lacks: got %v, want ErrWrongUsage", err)
				}
			}
		})
	}
}

// TestHierarchyShape asserts the structural constraints that make a stolen key
// worth only what it directly signs.
func TestHierarchyShape(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	want := map[CARole]struct {
		pathLen    int
		pathLenSet bool
		parent     CARole
	}{
		RoleRoot:             {pathLen: 2, parent: ""},
		RoleDeviceIssuance:   {pathLen: 1, parent: RoleRoot},
		RoleManufacturing:    {pathLen: 0, pathLenSet: true, parent: RoleDeviceIssuance},
		RoleShelfController:  {pathLen: 0, pathLenSet: true, parent: RoleDeviceIssuance},
		RoleServices:         {pathLen: 0, pathLenSet: true, parent: RoleRoot},
		RoleTenantManagement: {pathLen: 0, pathLenSet: true, parent: RoleRoot},
	}

	for role, w := range want {
		ca, err := h.CA(role)
		if err != nil {
			t.Fatalf("CA(%s): %v", role, err)
		}
		cert := ca.Certificate
		if !cert.IsCA {
			t.Errorf("%s: IsCA = false", role)
		}
		if cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
			t.Errorf("%s: key usage %b lacks certSign or cRLSign", role, cert.KeyUsage)
		}
		if cert.MaxPathLen != w.pathLen || cert.MaxPathLenZero != w.pathLenSet {
			t.Errorf("%s: MaxPathLen = %d (zero=%v), want %d (zero=%v)",
				role, cert.MaxPathLen, cert.MaxPathLenZero, w.pathLen, w.pathLenSet)
		}
		if w.parent == "" {
			if ca.Parent != nil {
				t.Errorf("%s: has a parent, want self-signed root", role)
			}
			continue
		}
		if ca.Parent == nil || ca.Parent.Role != w.parent {
			t.Errorf("%s: parent = %v, want %s", role, ca.Parent, w.parent)
		}
		if err := cert.CheckSignatureFrom(ca.Parent.Certificate); err != nil {
			t.Errorf("%s: not signed by %s: %v", role, w.parent, err)
		}
	}

	// The root of a device chain is deep enough that the path length matters:
	// root(2) -> device-issuance(1) -> manufacturing(0) -> leaf.
	if h.Manufacturing.Parent != h.DeviceIssuance {
		t.Error("manufacturing sub-CA is not under device issuance")
	}
	if got := len(h.Manufacturing.IssuingChain()); got != 2 {
		t.Errorf("manufacturing issuing chain has %d certs, want 2 (sub-CA + intermediate, root excluded)", got)
	}
	for _, c := range h.Manufacturing.IssuingChain() {
		if c.Subject.CommonName == h.Root.Certificate.Subject.CommonName {
			t.Error("issuing chain includes the root; peers must obtain the anchor out of band")
		}
	}
}

// TestDeviceCertCannotSignAnotherCert is the concrete form of the platform's
// central promise: a stolen label key is worth exactly one label.
func TestDeviceCertCannotSignAnotherCert(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	label, labelKey, err := h.IssueLabel(tenantA, store1, "label-forger")
	if err != nil {
		t.Fatalf("issue label: %v", err)
	}
	if label.Certificate.IsCA {
		t.Fatal("label certificate is a CA; the rest of this test is moot")
	}

	victimKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate victim key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          bigOne(),
		Subject:               pkix.Name{CommonName: "USSLP-LABEL-victim"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, label.Certificate, &victimKey.PublicKey, labelKey)
	if err != nil {
		// Some toolchains refuse to sign with a non-CA parent at all, which is
		// an even stronger result than the verification failure below.
		t.Logf("x509.CreateCertificate refused to sign with a leaf as issuer: %v", err)
		return
	}
	forged, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse forged certificate: %v", err)
	}

	intermediates := append([]*x509.Certificate{label.Certificate}, label.Chain...)
	_, err = h.VerifyChain(forged, intermediates, VerifyOptions{})
	if err == nil {
		t.Fatal("a certificate signed by a label certificate verified; the fleet has no isolation")
	}
	if !errors.Is(err, ErrUnknownAuthority) && !errors.Is(err, ErrWrongUsage) {
		t.Errorf("got %v, want ErrUnknownAuthority or ErrWrongUsage", err)
	}
}

// TestSubCAPathLengthEnforced proves the pathlen 0 on an issuing sub-CA stops a
// stolen sub-CA key from being used to hide behind a further CA.
func TestSubCAPathLengthEnforced(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	mfgKey, err := h.Manufacturing.Signer()
	if err != nil {
		t.Fatalf("manufacturing signer: %v", err)
	}
	rogueKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate rogue key: %v", err)
	}
	// The Manufacturing sub-CA still has a usable key, so nothing prevents it
	// from *creating* this certificate. The constraint bites at verification.
	rogueTmpl := &x509.Certificate{
		SerialNumber:          bigOne(),
		Subject:               pkix.Name{CommonName: "USSLP Rogue Sub-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	rogueDER, err := x509.CreateCertificate(rand.Reader, rogueTmpl, h.Manufacturing.Certificate, &rogueKey.PublicKey, mfgKey)
	if err != nil {
		t.Fatalf("create rogue sub-CA: %v", err)
	}
	rogue, err := x509.ParseCertificate(rogueDER)
	if err != nil {
		t.Fatalf("parse rogue sub-CA: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          bigOne(),
		Subject:               pkix.Name{CommonName: "USSLP-LABEL-rogue"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rogue, &leafKey.PublicKey, rogueKey)
	if err != nil {
		t.Fatalf("create leaf under rogue sub-CA: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	_, err = h.VerifyChain(leaf, []*x509.Certificate{rogue}, VerifyOptions{})
	if err == nil {
		t.Fatal("a chain with an extra CA under a pathlen-0 sub-CA verified")
	}
	if !errors.Is(err, ErrWrongUsage) && !errors.Is(err, ErrUnknownAuthority) {
		t.Errorf("got %v, want ErrWrongUsage (path length) or ErrUnknownAuthority", err)
	}
}

// TestCSRWithBrokenSignatureRejected covers the case the doc comment on
// ErrCSRSignature describes: a request asking the CA to bind an identity to a
// key the requester cannot prove it holds.
func TestCSRWithBrokenSignatureRejected(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	id := NewLabelIdentity(tenantA, store1, "label-badcsr")
	key, err := KeyECDSAP256.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	csr, err := CreateCSR(id, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("freshly created csr does not verify: %v", err)
	}

	// The signature is the last field of the DER, so flipping its final byte
	// leaves a structurally valid request whose proof of possession is broken.
	tampered := make([]byte, len(csr.Raw))
	copy(tampered, csr.Raw)
	tampered[len(tampered)-1] ^= 0xff
	broken, err := x509.ParseCertificateRequest(tampered)
	if err != nil {
		t.Fatalf("tampered csr no longer parses, adjust the test: %v", err)
	}

	if _, err := h.IssueDeviceCert(CSRRequest{CSR: broken, Identity: id}); !errors.Is(err, ErrCSRSignature) {
		t.Fatalf("got %v, want ErrCSRSignature", err)
	}

	// And the untampered request is still accepted, so the test is testing the
	// tampering rather than some unrelated rejection.
	if _, err := h.IssueDeviceCert(CSRRequest{CSR: csr, Identity: id}); err != nil {
		t.Fatalf("valid csr rejected: %v", err)
	}
}

// TestIssueDeviceCertRefusesNonDeviceKinds proves the provisioning path cannot
// be talked into minting a service or tenant identity.
func TestIssueDeviceCertRefusesNonDeviceKinds(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	id := NewServiceIdentity("usslp-prod", "pricing-service")
	key, err := KeyECDSAP256.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	csr, err := CreateCSR(id, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	if _, err := h.IssueDeviceCert(CSRRequest{CSR: csr, Identity: id}); err == nil {
		t.Fatal("IssueDeviceCert minted a service identity")
	}
}

// TestWeakKeyRejected proves the strength floor is enforced at the only place
// that can enforce it.
func TestWeakKeyRejected(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate weak key: %v", err)
	}
	id := NewSECIdentity(tenantA, store1, "sec-weak")
	csr, err := CreateCSR(id, weak)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	if _, err := h.IssueDeviceCert(CSRRequest{CSR: csr, Identity: id}); !errors.Is(err, ErrWeakKey) {
		t.Fatalf("got %v, want ErrWeakKey", err)
	}
}

// TestExpiredAndNotYetValidAreDistinguishable is what lets an operator tell a
// missed renewal from a wrong clock without opening a packet capture.
func TestExpiredAndNotYetValidAreDistinguishable(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// A dedicated hierarchy created three days ago, so that the "inside its own
	// window" checks below fall inside the issuing CA's window too rather than
	// tripping over a sub-CA that did not yet exist.
	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p, Now: now.Add(-72 * time.Hour)})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	expired, _, err := h.IssueLeaf(NewLabelIdentity(tenantA, store1, "label-expired"), LeafOptions{
		NotBefore: now.Add(-48 * time.Hour),
		NotAfter:  now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("issue expired label: %v", err)
	}
	future, _, err := h.IssueLeaf(NewLabelIdentity(tenantA, store1, "label-future"), LeafOptions{
		NotBefore: now.Add(24 * time.Hour),
		NotAfter:  now.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("issue future label: %v", err)
	}

	_, err = h.VerifyChain(expired.Certificate, expired.Chain, VerifyOptions{At: now})
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expired leaf: got %v, want ErrExpired", err)
	}
	if errors.Is(err, ErrNotYetValid) {
		t.Error("expired leaf also reported as not-yet-valid; the two must be distinguishable")
	}

	_, err = h.VerifyChain(future.Certificate, future.Chain, VerifyOptions{At: now})
	if !errors.Is(err, ErrNotYetValid) {
		t.Errorf("future leaf: got %v, want ErrNotYetValid", err)
	}
	if errors.Is(err, ErrExpired) {
		t.Error("not-yet-valid leaf also reported as expired; the two must be distinguishable")
	}

	// Both verify inside their own windows, so the errors above are about time
	// and nothing else.
	if _, err := h.VerifyChain(expired.Certificate, expired.Chain, VerifyOptions{At: now.Add(-36 * time.Hour)}); err != nil {
		t.Errorf("expired leaf inside its window: %v", err)
	}
	if _, err := h.VerifyChain(future.Certificate, future.Chain, VerifyOptions{At: now.Add(36 * time.Hour)}); err != nil {
		t.Errorf("future leaf inside its window: %v", err)
	}
}

// TestForeignCertificateRejected covers a certificate from a valid but
// unrelated PKI: shared shape is not shared trust.
func TestForeignCertificateRejected(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)
	foreign := foreignHierarchy(t)

	issued, _, err := foreign.IssueSEC(tenantA, store1, "sec-foreign")
	if err != nil {
		t.Fatalf("issue foreign sec: %v", err)
	}
	if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{}); !errors.Is(err, ErrUnknownAuthority) {
		t.Fatalf("got %v, want ErrUnknownAuthority", err)
	}
}

// TestValidityCannotOutliveIssuer stops the failure mode where a fleet expires
// on one morning because its certificates outlived the CA that signed them.
func TestValidityCannotOutliveIssuer(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	_, _, err := h.IssueLeaf(NewLabelIdentity(tenantA, store1, "label-immortal"), LeafOptions{
		NotAfter: h.Manufacturing.Certificate.NotAfter.Add(time.Hour),
	})
	if !errors.Is(err, ErrValidityExceedsIssuer) {
		t.Fatalf("got %v, want ErrValidityExceedsIssuer", err)
	}
}

// TestSerialNumbersAreRandomAndUnique guards the property that lets several
// regional issuance clusters mint certificates without coordinating.
func TestSerialNumbersAreRandomAndUnique(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	const n = 24
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		issued, _, err := h.IssueService("usslp-prod", "serial-probe")
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		serial := issued.Certificate.SerialNumber
		if serial.Sign() <= 0 {
			t.Fatalf("serial %s is not positive", serial)
		}
		if bits := serial.BitLen(); bits < 96 {
			t.Errorf("serial has only %d bits; 128 bits of entropy were requested", bits)
		}
		hexSerial := serialHex(serial)
		if seen[hexSerial] {
			t.Fatalf("serial %s issued twice", hexSerial)
		}
		seen[hexSerial] = true
	}
}

// TestDropRootKey models the end of the key ceremony: the platform keeps
// working, and the root can no longer be used from this process.
func TestDropRootKey(t *testing.T) {
	t.Parallel()
	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !h.HasRootKey() {
		t.Fatal("freshly bootstrapped hierarchy has no root key")
	}
	h.DropRootKey()
	if h.HasRootKey() {
		t.Fatal("root key still loaded after DropRootKey")
	}
	if _, err := h.Root.Signer(); !errors.Is(err, ErrNoSigningKey) {
		t.Errorf("root signer: got %v, want ErrNoSigningKey", err)
	}
	// Day-to-day issuance is unaffected, which is the whole point of an
	// intermediate tier.
	if _, _, err := h.IssueLabel(tenantA, store1, "label-after-ceremony"); err != nil {
		t.Errorf("issuance broken after dropping the root key: %v", err)
	}
	if _, err := h.GenerateCRL(RoleManufacturing, CRLOptions{}); err != nil {
		t.Errorf("crl generation broken after dropping the root key: %v", err)
	}
}

// TestProfileValidationRejectsImpossibleHierarchies covers the checks that stop
// a hierarchy which cannot work from being created at all.
func TestProfileValidationRejectsImpossibleHierarchies(t *testing.T) {
	t.Parallel()

	cases := map[string]func(p *Profile){
		"leaf outlives sub-ca":         func(p *Profile) { p.LabelValidity = p.SubCAValidity + day },
		"intermediate outlives root":   func(p *Profile) { p.IntermediateValidity = p.RootValidity },
		"unknown key spec":             func(p *Profile) { p.LabelKey = "rsa-512" },
		"empty organization":           func(p *Profile) { p.Organization = "" },
		"empty trust domain":           func(p *Profile) { p.TrustDomain = "" },
		"non-positive leaf validity":   func(p *Profile) { p.ServiceValidity = 0 },
		"sub-ca outlives intermediate": func(p *Profile) { p.SubCAValidity = p.IntermediateValidity },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := TestProfile()
			mutate(&p)
			if _, err := Bootstrap(BootstrapConfig{Profile: &p}); err == nil {
				t.Fatal("bootstrap accepted an impossible profile")
			}
		})
	}
}

// TestBootstrapProductionProfile exercises the real blueprint algorithms —
// RSA-4096 root, RSA-2048 intermediates — which the rest of the suite skips
// because generating them costs seconds rather than milliseconds.
func TestBootstrapProductionProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping RSA-4096 key generation in short mode")
	}
	t.Parallel()

	h, err := Bootstrap(BootstrapConfig{})
	if err != nil {
		t.Fatalf("bootstrap production profile: %v", err)
	}
	rootKey, err := h.Root.Signer()
	if err != nil {
		t.Fatalf("root signer: %v", err)
	}
	rsaKey, ok := rootKey.Public().(*rsa.PublicKey)
	if !ok {
		t.Fatalf("root key is %T, want RSA", rootKey.Public())
	}
	if bits := rsaKey.N.BitLen(); bits != 4096 {
		t.Errorf("root key is RSA-%d, want RSA-4096", bits)
	}
	if got := h.Root.Certificate.NotAfter.Sub(h.Root.Certificate.NotBefore); got < 19*year {
		t.Errorf("root validity is %s, want about 20 years", got)
	}
	issued, _, err := h.IssueLabel(tenantA, store1, "label-prod")
	if err != nil {
		t.Fatalf("issue label: %v", err)
	}
	if _, err := h.VerifyChain(issued.Certificate, issued.Chain, VerifyOptions{
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("verify production chain: %v", err)
	}
	if _, ok := issued.Certificate.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("label key is %T, want ECDSA", issued.Certificate.PublicKey)
	}
}

// TestIdentifiersWithTopicSeparatorsRejected keeps a device from being issued a
// certificate whose tenant or store would break out of the MQTT namespace.
func TestIdentifiersWithTopicSeparatorsRejected(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	bad := []struct {
		name   string
		tenant canon.TenantID
		store  canon.StoreID
		label  canon.LabelID
	}{
		{"wildcard in tenant", "tenant-#", store1, "label-0001"},
		{"separator in store", tenantA, "store/0001", "label-0001"},
		{"plus in label", tenantA, store1, "label+1"},
		{"dot in store would forge a SAN label", tenantA, "store.evil", "label-0001"},
		{"empty tenant", "", store1, "label-0001"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := h.IssueLabel(tc.tenant, tc.store, tc.label); err == nil {
				t.Fatal("issuance accepted an identifier that is illegal in a topic segment or DNS label")
			}
		})
	}
}
