package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestIdentityRoundTrips is the contract the MQTT broker's authorizer depends
// on: what goes into a certificate at issuance comes back out at verification,
// with no lookaside.
func TestIdentityRoundTrips(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	cases := []struct {
		name     string
		identity Identity
		wantCN   string
		wantURI  string
		wantDNS  []string
	}{
		{
			name:     "label",
			identity: NewLabelIdentity(tenantA, store1, "label-000042"),
			wantCN:   "USSLP-LABEL-label-000042",
			wantURI:  "spiffe://usslp.io/tenant/tenant-a/store/store-0001/label/label-000042",
			wantDNS: []string{
				"label-000042.store-0001.tenant-a.labels.usslp.io",
				"store-0001.tenant-a.stores.usslp.io",
				"tenant-a.tenants.usslp.io",
			},
		},
		{
			name:     "shelf edge controller",
			identity: NewSECIdentity(tenantB, store2, "sec-000007"),
			wantCN:   "USSLP-SEC-sec-000007",
			wantURI:  "spiffe://usslp.io/tenant/tenant-b/store/store-0002/sec/sec-000007",
		},
		{
			name:     "store gateway unit",
			identity: NewSGUIdentity(tenantB, store2, "sgu-000001"),
			wantCN:   "USSLP-SGU-sgu-000001",
			wantURI:  "spiffe://usslp.io/tenant/tenant-b/store/store-0002/sgu/sgu-000001",
		},
		{
			name:     "service",
			identity: NewServiceIdentity("usslp-prod", "label-service"),
			wantCN:   "USSLP-SVC-label-service.usslp-prod",
			wantURI:  "spiffe://usslp.io/ns/usslp-prod/sa/label-service",
			wantDNS:  []string{"label-service.usslp-prod.svc.usslp.io", "label-service.usslp-prod.svc"},
		},
		{
			name:     "tenant client",
			identity: NewTenantIdentity(tenantA, "pos-adapter"),
			wantCN:   "USSLP-TENANT-tenant-a-pos-adapter",
			wantURI:  "spiffe://usslp.io/tenant/tenant-a/client/pos-adapter",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issued, _, err := h.IssueLeaf(tc.identity, LeafOptions{})
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			cert := issued.Certificate

			if cert.Subject.CommonName != tc.wantCN {
				t.Errorf("CN = %q, want %q", cert.Subject.CommonName, tc.wantCN)
			}
			if len(cert.URIs) != 1 || cert.URIs[0].String() != tc.wantURI {
				t.Errorf("URI SANs = %v, want exactly [%s]", cert.URIs, tc.wantURI)
			}
			for _, want := range tc.wantDNS {
				if !slices.Contains(cert.DNSNames, want) {
					t.Errorf("DNS SANs %v do not contain %q", cert.DNSNames, want)
				}
			}
			if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != "USSLP" {
				t.Errorf("O = %v, want [USSLP]", cert.Subject.Organization)
			}

			got, err := IdentityFromCertificate(cert)
			if err != nil {
				t.Fatalf("IdentityFromCertificate: %v", err)
			}
			if got.Kind != tc.identity.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.identity.Kind)
			}
			if got.TenantID != tc.identity.TenantID {
				t.Errorf("TenantID = %q, want %q", got.TenantID, tc.identity.TenantID)
			}
			if got.StoreID != tc.identity.StoreID {
				t.Errorf("StoreID = %q, want %q", got.StoreID, tc.identity.StoreID)
			}
			if got.DeviceID != tc.identity.DeviceID {
				t.Errorf("DeviceID = %q, want %q", got.DeviceID, tc.identity.DeviceID)
			}
			if got.Namespace != tc.identity.Namespace || got.Service != tc.identity.Service {
				t.Errorf("namespace/service = %q/%q, want %q/%q",
					got.Namespace, got.Service, tc.identity.Namespace, tc.identity.Service)
			}
			if got.SPIFFEID != tc.wantURI {
				t.Errorf("SPIFFEID = %q, want %q", got.SPIFFEID, tc.wantURI)
			}
			if got.SerialNumber != serialHex(cert.SerialNumber) {
				t.Errorf("SerialNumber = %q, want %q", got.SerialNumber, serialHex(cert.SerialNumber))
			}
			// Nothing in the platform authorises on OU; it is checked here only
			// so that a human reading the certificate sees the right thing.
			wantOU := organizationalUnit(got.Kind)
			if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != wantOU {
				t.Errorf("OU = %v, want [%s]", cert.Subject.OrganizationalUnit, wantOU)
			}
		})
	}
}

// TestIdentityDrivesTopicScope proves the certificate alone is enough to build
// the MQTT namespace a device is confined to.
func TestIdentityDrivesTopicScope(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	issued := mustIssueLabel(t, h, tenantA, store1, "label-topic")
	id, err := IdentityFromCertificate(issued.Certificate)
	if err != nil {
		t.Fatalf("IdentityFromCertificate: %v", err)
	}
	scope := id.TopicScope("eu-west-1")
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope from certificate is invalid: %v", err)
	}
	want := "usslp/tenant-a/eu-west-1/store-0001/labels/label-topic/price"
	if got := scope.LabelTopic(id.LabelID(), canon.LeafPrice); got != want {
		t.Errorf("LabelTopic = %q, want %q", got, want)
	}
	if got, want := id.SubscribePattern(), "usslp/tenant-a/#"; got != want {
		t.Errorf("SubscribePattern = %q, want %q", got, want)
	}
}

// TestIdentityFromCertificateRejections covers the inputs an authorizer must
// refuse rather than interpret.
func TestIdentityFromCertificateRejections(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	t.Run("nil certificate", func(t *testing.T) {
		if _, err := IdentityFromCertificate(nil); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("got %v, want ErrNoIdentity", err)
		}
	})

	t.Run("ca certificate is not an end entity", func(t *testing.T) {
		if _, err := IdentityFromCertificate(h.Manufacturing.Certificate); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("got %v, want ErrNoIdentity", err)
		}
	})

	t.Run("certificate with no usslp sans", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          bigOne(),
			Subject:               pkix.Name{CommonName: "shop.example.com"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(time.Hour),
			DNSNames:              []string{"shop.example.com"},
			BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("create certificate: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse certificate: %v", err)
		}
		if _, err := IdentityFromCertificate(cert); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("got %v, want ErrNoIdentity", err)
		}
	})

	t.Run("common name disagreeing with the san is refused", func(t *testing.T) {
		issued := mustIssueLabel(t, h, tenantA, store1, "label-cn-swap")
		// Re-issue the same public key under a subject that claims a different
		// device, keeping the honest SANs: exactly the shape of a certificate
		// assembled by hand to confuse an authorizer that reads only the CN.
		mfgKey, err := h.Manufacturing.Signer()
		if err != nil {
			t.Fatalf("manufacturing signer: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          bigOne(),
			Subject:               pkix.Name{CommonName: "USSLP-LABEL-label-somebody-else", Organization: []string{"USSLP"}},
			NotBefore:             issued.Certificate.NotBefore,
			NotAfter:              issued.Certificate.NotAfter,
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			DNSNames:              issued.Certificate.DNSNames,
			URIs:                  issued.Certificate.URIs,
			BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, h.Manufacturing.Certificate, issued.Certificate.PublicKey, mfgKey)
		if err != nil {
			t.Fatalf("create mismatched certificate: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if _, err := IdentityFromCertificate(cert); !errors.Is(err, ErrMalformedIdentity) {
			t.Fatalf("got %v, want ErrMalformedIdentity", err)
		}
	})
}

// TestVerifyPeerRejectsForeignTrustDomain covers a certificate that is
// structurally a USSLP identity but belongs to a different trust domain.
func TestVerifyPeerRejectsForeignTrustDomain(t *testing.T) {
	t.Parallel()
	foreign := foreignHierarchy(t)

	issued, _, err := foreign.IssueSEC(tenantA, store1, "sec-otherdomain")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	id, err := IdentityFromCertificate(issued.Certificate)
	if err != nil {
		t.Fatalf("IdentityFromCertificate: %v", err)
	}
	if id.TrustDomain != "othercorp.example" {
		t.Fatalf("trust domain = %q, want othercorp.example", id.TrustDomain)
	}
	// Against its own hierarchy it is fine; against the platform's it is not,
	// and the check happens before any authorisation decision is made.
	if _, err := foreign.VerifyPeer(issued.Certificate, issued.Chain, VerifyOptions{}); err != nil {
		t.Fatalf("foreign hierarchy rejects its own peer: %v", err)
	}
	h := testHierarchy(t)
	if _, err := h.VerifyPeer(issued.Certificate, issued.Chain, VerifyOptions{}); err == nil {
		t.Fatal("platform accepted a peer from another trust domain")
	}
}

// TestIdentityPredicates covers the authorisation rules applied during a
// handshake, independent of the handshake itself.
func TestIdentityPredicates(t *testing.T) {
	t.Parallel()

	labelA := Identity{Kind: KindLabel, TenantID: tenantA, StoreID: store1, DeviceID: "label-1", SPIFFEID: "spiffe://usslp.io/a"}
	labelB := Identity{Kind: KindLabel, TenantID: tenantB, StoreID: store2, DeviceID: "label-2", SPIFFEID: "spiffe://usslp.io/b"}
	svc := Identity{Kind: KindService, Namespace: "usslp-prod", Service: "pricing-service", SPIFFEID: "spiffe://usslp.io/c"}

	cases := []struct {
		name      string
		predicate IdentityPredicate
		identity  Identity
		wantErr   bool
	}{
		{"same tenant accepted", RequireTenant(tenantA), labelA, false},
		{"cross tenant rejected", RequireTenant(tenantA), labelB, true},
		{"same store accepted", RequireStore(tenantA, store1), labelA, false},
		{"other store rejected", RequireStore(tenantA, store2), labelA, true},
		{"kind accepted", RequireKind(KindLabel, KindSEC), labelA, false},
		{"kind rejected", RequireKind(KindSEC, KindSGU), labelA, true},
		{"named service accepted", RequireService("usslp-prod", "pricing-service"), svc, false},
		{"other service rejected", RequireService("usslp-prod", "label-service"), svc, true},
		{"all requires every rule", AllPredicates(RequireTenant(tenantA), RequireKind(KindSEC)), labelA, true},
		{"all passes when every rule passes", AllPredicates(RequireTenant(tenantA), RequireKind(KindLabel)), labelA, false},
		{"any accepts one match", AnyPredicate(RequireTenant(tenantB), RequireTenant(tenantA)), labelA, false},
		{"any rejects when none match", AnyPredicate(RequireTenant(tenantB), RequireKind(KindService)), labelA, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.predicate(tc.identity)
			if tc.wantErr {
				if !errors.Is(err, ErrIdentityRejected) {
					t.Fatalf("got %v, want ErrIdentityRejected", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}
