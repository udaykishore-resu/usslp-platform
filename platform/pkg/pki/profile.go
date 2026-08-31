package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"time"
)

// KeySpec names an asymmetric key algorithm and strength.
//
// It is a string rather than a struct so that a bootstrapped hierarchy can
// record on disk exactly which algorithm each CA was created with. Ten years
// into a root's life, "what is this key?" must be answerable without loading
// the key itself — that question comes up during a crypto-agility migration,
// which is precisely when you cannot afford to guess.
type KeySpec string

// The key specifications used across the platform.
//
// RSA appears at the top of the hierarchy and ECDSA at the leaves for opposite
// reasons. The root and intermediates are RSA because their certificates must
// remain verifiable by whatever validates them in 2045, including firmware and
// hardware security modules that predate widespread ECDSA support; RSA-4096 is
// the conservative choice for a key that cannot be rotated cheaply. Label and
// service leaves are ECDSA P-256 because a Cortex-M4F label verifies a P-256
// signature in a fraction of the energy an RSA-2048 verification costs, and a
// service leaf that lives 90 days is rotated often enough that long-horizon
// algorithm risk does not apply to it.
const (
	// KeyRSA2048 is the intermediate and shelf-controller strength.
	KeyRSA2048 KeySpec = "rsa-2048"
	// KeyRSA4096 is reserved for the offline root, whose 20-year life makes
	// margin cheaper than performance.
	KeyRSA4096 KeySpec = "rsa-4096"
	// KeyECDSAP256 is the leaf strength for labels and services.
	KeyECDSAP256 KeySpec = "ecdsa-p256"
	// KeyECDSAP384 is available for deployments whose regulator requires a
	// 192-bit security level at the leaf.
	KeyECDSAP384 KeySpec = "ecdsa-p384"
)

// Generate produces a fresh private key matching the specification.
//
// It returns a crypto.Signer rather than a concrete type so that a future
// hardware-backed implementation (a PKCS#11 or KMS handle for the root) can be
// substituted without touching any certificate-building code.
func (k KeySpec) Generate() (crypto.Signer, error) {
	switch k {
	case KeyRSA2048:
		return rsa.GenerateKey(rand.Reader, 2048)
	case KeyRSA4096:
		return rsa.GenerateKey(rand.Reader, 4096)
	case KeyECDSAP256:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case KeyECDSAP384:
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	default:
		return nil, fmt.Errorf("pki: unknown key specification %q", string(k))
	}
}

// signatureAlgorithm returns the algorithm a certificate issued *by* a key of
// this specification should be signed with. Pairing a 4096-bit modulus with
// SHA-256 wastes the modulus; pairing P-256 with SHA-384 wastes the hash.
func (k KeySpec) signatureAlgorithm() x509.SignatureAlgorithm {
	switch k {
	case KeyRSA4096:
		return x509.SHA384WithRSA
	case KeyRSA2048:
		return x509.SHA256WithRSA
	case KeyECDSAP384:
		return x509.ECDSAWithSHA384
	default:
		return x509.ECDSAWithSHA256
	}
}

// Valid reports whether the specification is one the platform recognises.
func (k KeySpec) Valid() bool {
	switch k {
	case KeyRSA2048, KeyRSA4096, KeyECDSAP256, KeyECDSAP384:
		return true
	}
	return false
}

// Durations expressed in whole days. Certificate lifetimes are quoted in years
// in the blueprint and in days here; a "year" is 365 days, which is what every
// operator's calendar reminder assumes anyway.
const (
	day  = 24 * time.Hour
	year = 365 * day
)

// Profile is the complete set of algorithm and lifetime decisions a hierarchy
// is created with. Bundling them in one value means a deployment that must
// diverge — a national retailer whose regulator mandates a 3-year label
// certificate, say — changes one struct rather than hunting constants.
type Profile struct {
	// Organization is the O in every subject. It is the platform operator, not
	// the tenant: a tenant's identity lives in the SANs, because tenants come
	// and go far faster than certificates do.
	Organization string `json:"organization"`
	// Country is the optional C in every subject.
	Country string `json:"country,omitempty"`
	// TrustDomain is the SPIFFE trust domain and the DNS suffix of every SAN.
	TrustDomain string `json:"trust_domain"`

	// Key specifications, top of the hierarchy downwards.
	RootKey         KeySpec `json:"root_key"`
	IntermediateKey KeySpec `json:"intermediate_key"`
	SubCAKey        KeySpec `json:"sub_ca_key"`
	LabelKey        KeySpec `json:"label_key"`
	ControllerKey   KeySpec `json:"controller_key"`
	ServiceKey      KeySpec `json:"service_key"`
	TenantKey       KeySpec `json:"tenant_key"`

	// Certificate lifetimes.
	RootValidity         time.Duration `json:"root_validity"`
	IntermediateValidity time.Duration `json:"intermediate_validity"`
	SubCAValidity        time.Duration `json:"sub_ca_validity"`
	LabelValidity        time.Duration `json:"label_validity"`
	ControllerValidity   time.Duration `json:"controller_validity"`
	ServiceValidity      time.Duration `json:"service_validity"`
	TenantValidity       time.Duration `json:"tenant_validity"`

	// Backdate shifts every NotBefore into the past to absorb clock skew. A
	// label whose RTC has drifted four minutes must not reject a certificate
	// minted seconds ago, and a fleet of 50M devices contains a great many
	// drifted clocks.
	Backdate time.Duration `json:"backdate"`
}

// ProductionProfile returns the algorithm and lifetime decisions from the
// platform blueprint. It is what [Bootstrap] uses when no profile is supplied.
func ProductionProfile() Profile {
	return Profile{
		Organization:         "USSLP",
		Country:              "US",
		TrustDomain:          DefaultTrustDomain,
		RootKey:              KeyRSA4096,
		IntermediateKey:      KeyRSA2048,
		SubCAKey:             KeyRSA2048,
		LabelKey:             KeyECDSAP256,
		ControllerKey:        KeyRSA2048,
		ServiceKey:           KeyECDSAP256,
		TenantKey:            KeyECDSAP256,
		RootValidity:         20 * year,
		IntermediateValidity: 10 * year,
		SubCAValidity:        5 * year,
		LabelValidity:        2 * year,
		ControllerValidity:   1 * year,
		ServiceValidity:      90 * day,
		TenantValidity:       1 * year,
		Backdate:             5 * time.Minute,
	}
}

// TestProfile returns a profile with the same shape as [ProductionProfile] but
// with elliptic-curve keys throughout, so that a test suite or a developer
// laptop can bootstrap a complete six-CA hierarchy in milliseconds instead of
// spending tens of seconds generating an RSA-4096 modulus.
//
// Every key in it still meets the platform's own strength floor — P-256 is
// stronger than the RSA-2048 it replaces at the intermediate tier, and no
// weaker than it at the root, where the blueprint's RSA-4096 buys extra margin
// for a 20-year key that this profile does not. It is therefore fine for a test
// or a laptop and wrong for a hierarchy meant to outlive the decade, which
// should be created with [ProductionProfile]. Nothing enforces that; the choice
// is recorded on disk in the hierarchy metadata so an auditor can see which was
// used.
func TestProfile() Profile {
	p := ProductionProfile()
	p.RootKey = KeyECDSAP256
	p.IntermediateKey = KeyECDSAP256
	p.SubCAKey = KeyECDSAP256
	p.ControllerKey = KeyECDSAP256
	return p
}

// validate rejects a profile that would produce a hierarchy which cannot work:
// an unknown algorithm, or a child CA outliving its parent. The second is the
// subtle one — an intermediate valid past its root's expiry looks fine for
// years and then breaks the whole fleet on one morning with no deploy to blame.
func (p Profile) validate() error {
	specs := map[string]KeySpec{
		"root":         p.RootKey,
		"intermediate": p.IntermediateKey,
		"sub-ca":       p.SubCAKey,
		"label":        p.LabelKey,
		"controller":   p.ControllerKey,
		"service":      p.ServiceKey,
		"tenant":       p.TenantKey,
	}
	for name, spec := range specs {
		if !spec.Valid() {
			return fmt.Errorf("pki: profile %s key: unknown specification %q", name, string(spec))
		}
	}
	if p.Organization == "" {
		return fmt.Errorf("pki: profile organization must not be empty")
	}
	if p.TrustDomain == "" {
		return fmt.Errorf("pki: profile trust domain must not be empty")
	}
	if p.RootValidity <= p.IntermediateValidity {
		return fmt.Errorf("pki: profile root validity %s must exceed intermediate validity %s", p.RootValidity, p.IntermediateValidity)
	}
	if p.IntermediateValidity <= p.SubCAValidity {
		return fmt.Errorf("pki: profile intermediate validity %s must exceed sub-CA validity %s", p.IntermediateValidity, p.SubCAValidity)
	}
	leaves := map[string]time.Duration{
		"label":      p.LabelValidity,
		"controller": p.ControllerValidity,
		"service":    p.ServiceValidity,
		"tenant":     p.TenantValidity,
	}
	for name, d := range leaves {
		if d <= 0 {
			return fmt.Errorf("pki: profile %s validity must be positive", name)
		}
		if d > p.SubCAValidity {
			return fmt.Errorf("pki: profile %s validity %s exceeds sub-CA validity %s", name, d, p.SubCAValidity)
		}
	}
	if p.Backdate < 0 {
		return fmt.Errorf("pki: profile backdate must not be negative")
	}
	return nil
}

// leafValidity returns the default lifetime for an end-entity certificate of
// the given kind.
func (p Profile) leafValidity(kind IdentityKind) time.Duration {
	switch kind {
	case KindLabel:
		return p.LabelValidity
	case KindSEC, KindSGU:
		return p.ControllerValidity
	case KindService:
		return p.ServiceValidity
	default:
		return p.TenantValidity
	}
}

// leafKeySpec returns the key specification the convenience issuers generate
// for the given kind. Callers presenting their own CSR are not bound by it;
// [acceptableLeafKey] is what actually gates a foreign public key.
func (p Profile) leafKeySpec(kind IdentityKind) KeySpec {
	switch kind {
	case KindLabel:
		return p.LabelKey
	case KindSEC, KindSGU:
		return p.ControllerKey
	case KindService:
		return p.ServiceKey
	default:
		return p.TenantKey
	}
}
