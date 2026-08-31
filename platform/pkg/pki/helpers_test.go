package pki

import (
	"encoding/base64"
	"math/big"
	"sync"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// base64RawURL encodes bytes the way a JWK member is encoded.
func base64RawURL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// bigOne is the serial number used by certificates the tests forge. Real
// issuance never produces a serial this small, which makes a forged certificate
// obvious in any log line that reaches a human.
func bigOne() *big.Int { return big.NewInt(1) }

// The test suite bootstraps two independent hierarchies and shares them across
// tests. Bootstrapping is the most expensive operation in the package — six key
// pairs and six certificates — and every test that needs a hierarchy needs the
// same one, so building it once keeps the suite fast enough to run on every
// save. Tests that mutate shared state (revocation) issue their own leaves and
// revoke only those, so sharing stays safe.
var (
	primaryOnce = sync.OnceValues(func() (*Hierarchy, error) {
		p := TestProfile()
		return Bootstrap(BootstrapConfig{Profile: &p})
	})
	foreignOnce = sync.OnceValues(func() (*Hierarchy, error) {
		p := TestProfile()
		p.Organization = "OTHERCORP"
		p.TrustDomain = "othercorp.example"
		return Bootstrap(BootstrapConfig{Profile: &p})
	})
)

// testHierarchy returns the shared hierarchy every test verifies against.
func testHierarchy(t *testing.T) *Hierarchy {
	t.Helper()
	h, err := primaryOnce()
	if err != nil {
		t.Fatalf("bootstrap test hierarchy: %v", err)
	}
	return h
}

// foreignHierarchy returns a completely unrelated hierarchy, used to prove that
// a certificate from somewhere else is rejected rather than merely mistrusted.
func foreignHierarchy(t *testing.T) *Hierarchy {
	t.Helper()
	h, err := foreignOnce()
	if err != nil {
		t.Fatalf("bootstrap foreign hierarchy: %v", err)
	}
	return h
}

// Fixed identifiers used across the suite. They are DNS-label safe, which the
// issuance path requires.
const (
	tenantA = canon.TenantID("tenant-a")
	tenantB = canon.TenantID("tenant-b")
	store1  = canon.StoreID("store-0001")
	store2  = canon.StoreID("store-0002")
)

func mustIssueLabel(t *testing.T, h *Hierarchy, tenant canon.TenantID, store canon.StoreID, label canon.LabelID) *Issued {
	t.Helper()
	issued, _, err := h.IssueLabel(tenant, store, label)
	if err != nil {
		t.Fatalf("issue label %s: %v", label, err)
	}
	return issued
}
