package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// CARole names one node of the USSLP certificate authority hierarchy. Roles are
// a closed set: adding a CA means adding a role here, which means the change
// shows up in review rather than in a configuration file nobody reads.
type CARole string

// The six authorities of the hierarchy.
const (
	// RoleRoot is the offline trust anchor. Its key signs nothing but the four
	// intermediates, and in production it lives in an HSM in a safe.
	RoleRoot CARole = "root"
	// RoleDeviceIssuance is the intermediate under which all hardware is
	// issued. It exists so that the two device sub-CAs can be rotated,
	// suspended or geographically separated without touching the root.
	RoleDeviceIssuance CARole = "device-issuance"
	// RoleManufacturing signs label certificates on the production line.
	RoleManufacturing CARole = "manufacturing"
	// RoleShelfController signs SEC and SGU certificates.
	RoleShelfController CARole = "shelf-controller"
	// RoleServices signs short-lived SPIFFE certificates for cloud workloads.
	RoleServices CARole = "services"
	// RoleTenantManagement signs tenant API client certificates and holds the
	// tenant JWT signing keys.
	RoleTenantManagement CARole = "tenant-management"
)

// AllRoles lists every role, root first, in hierarchy order. Persistence and
// enumeration both walk it so that neither can silently omit an authority.
func AllRoles() []CARole {
	return []CARole{
		RoleRoot, RoleDeviceIssuance, RoleManufacturing,
		RoleShelfController, RoleServices, RoleTenantManagement,
	}
}

// Errors returned by hierarchy construction and use.
var (
	// ErrNoSigningKey means the CA's private key is not loaded. For the root
	// this is the normal, desired state: the key is offline.
	ErrNoSigningKey = errors.New("pki: certificate authority has no signing key loaded")
	// ErrUnknownRole means a role that is not part of the hierarchy was named.
	ErrUnknownRole = errors.New("pki: unknown certificate authority role")
)

// CA is one certificate authority: its certificate, the chain that vouches for
// it, and — when loaded — its private key.
//
// The key is deliberately unexported and reachable only through [CA.Signer].
// Every caller that wants to sign therefore has to acknowledge that the key may
// be absent, which is what makes "the root is offline" a state the type system
// forces callers to handle rather than a convention they forget.
//
// A CA must not be copied; it is always used through a pointer.
type CA struct {
	// Role identifies which node of the hierarchy this is.
	Role CARole
	// Certificate is this authority's own certificate.
	Certificate *x509.Certificate
	// Parent is the authority that issued Certificate; nil for the root.
	Parent *CA
	// KeySpec records the algorithm the key was generated with.
	KeySpec KeySpec

	mu        sync.RWMutex
	key       crypto.Signer
	crlNumber *big.Int
}

// Signer returns the CA's private key, or ErrNoSigningKey if it is not loaded.
func (c *CA) Signer() (crypto.Signer, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.key == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSigningKey, c.Role)
	}
	return c.key, nil
}

// HasKey reports whether the private key is loaded. Operators use it to assert
// that a running service does *not* hold the root key.
func (c *CA) HasKey() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.key != nil
}

// dropKey discards the private key. The Go runtime gives no way to scrub the
// key material itself from memory — an rsa.PrivateKey is immutable and its
// precomputed values are spread across several allocations — so this removes
// the reference and relies on the process being short-lived, which is exactly
// why a real deployment keeps the root in an HSM instead.
func (c *CA) dropKey() {
	c.mu.Lock()
	c.key = nil
	c.mu.Unlock()
}

// Chain returns this authority's certificate followed by each ancestor up to
// and including the root.
func (c *CA) Chain() []*x509.Certificate {
	var out []*x509.Certificate
	for ca := c; ca != nil; ca = ca.Parent {
		out = append(out, ca.Certificate)
	}
	return out
}

// IssuingChain returns the certificates a leaf issued by this CA must present
// alongside itself: this authority and every ancestor except the root.
//
// The root is omitted on purpose. A peer that does not already hold the root
// out of band must not be talked into trusting it by the very connection being
// authenticated, and shipping it wastes bytes on every handshake — at 50M
// devices reconnecting after a store power cycle, that is real bandwidth.
func (c *CA) IssuingChain() []*x509.Certificate {
	chain := c.Chain()
	if len(chain) <= 1 {
		return nil
	}
	return chain[:len(chain)-1]
}

// nextCRLNumber returns a monotonically increasing CRL number for this issuer.
// RFC 5280 requires it to be strictly increasing so that a relying party can
// tell a replayed old list from the current one — which matters here, because
// an attacker who can suppress a CRL update keeps a revoked label alive.
func (c *CA) nextCRLNumber() *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crlNumber == nil {
		c.crlNumber = big.NewInt(0)
	}
	c.crlNumber = new(big.Int).Add(c.crlNumber, big.NewInt(1))
	return new(big.Int).Set(c.crlNumber)
}

// observeCRLNumber raises the counter to at least n, so that a hierarchy
// reloaded from disk does not reissue a CRL number it has already published.
func (c *CA) observeCRLNumber(n *big.Int) {
	if n == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crlNumber == nil || c.crlNumber.Cmp(n) < 0 {
		c.crlNumber = new(big.Int).Set(n)
	}
}

// Hierarchy is the platform's complete certificate authority tree together with
// the trust anchors and revocation state a verifier needs.
//
// It is safe for concurrent use: after bootstrap or load, the certificates and
// pools are immutable, and the two pieces of mutable state — each CA's private
// key and the revocation registry — carry their own locks. That matters because
// a single Hierarchy backs the VerifyPeerCertificate callback of every TLS
// listener in a service.
type Hierarchy struct {
	profile Profile
	// log is held atomically because SetLogger may be called while handshakes
	// are already running through VerifyPeerCertificate on another goroutine.
	log       atomic.Pointer[obs.Logger]
	createdAt time.Time

	// Root is the offline trust anchor.
	Root *CA
	// DeviceIssuance is the intermediate above the two device sub-CAs.
	DeviceIssuance *CA
	// Manufacturing issues label certificates.
	Manufacturing *CA
	// ShelfController issues SEC and SGU certificates.
	ShelfController *CA
	// Services issues cloud workload certificates.
	Services *CA
	// TenantManagement issues tenant API certificates.
	TenantManagement *CA

	byRole        map[CARole]*CA
	roots         *x509.CertPool
	intermediates *x509.CertPool
	revocations   *RevocationChecker
}

// BootstrapConfig parameterises the creation of a brand new hierarchy.
type BootstrapConfig struct {
	// Profile selects algorithms and lifetimes. Nil means ProductionProfile.
	Profile *Profile
	// Now fixes the issuance instant. Zero means time.Now. Supplying it lets a
	// test create a hierarchy whose certificates are already expired, and lets
	// a ceremony record an exact, witnessed timestamp.
	Now time.Time
	// Logger receives one audit line per authority created. Nil means silent.
	Logger *obs.Logger
}

// Bootstrap creates a complete hierarchy from nothing: six key pairs, six
// certificates, and the trust pools that verify them.
//
// This is the key ceremony. In production it is run once, on an air-gapped
// machine, and the result is split immediately: the root key goes to an HSM,
// the intermediate keys go to the issuing services, and the root certificate is
// baked into every piece of firmware the platform will ever ship. Everything
// after that point is a consequence of what happens here, which is why the
// function takes no shortcuts even though it is slow: generating an RSA-4096
// modulus is the single most expensive thing this package does, and it happens
// once per decade.
func Bootstrap(cfg BootstrapConfig) (*Hierarchy, error) {
	profile := ProductionProfile()
	if cfg.Profile != nil {
		profile = *cfg.Profile
	}
	if err := profile.validate(); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	log := cfg.Logger
	if log == nil {
		log = obs.NopLogger()
	}

	h := &Hierarchy{
		profile:     profile,
		createdAt:   now,
		byRole:      make(map[CARole]*CA, len(AllRoles())),
		revocations: NewRevocationChecker(),
	}
	h.log.Store(log)

	// Path lengths are the structural guarantee that a stolen key is worth only
	// what it directly signs. Two intermediates may follow the root (device
	// issuance, then a device sub-CA); one may follow device issuance; none may
	// follow any issuing CA, so no issuing CA can mint another CA.
	type spec struct {
		role       CARole
		commonName string
		parent     *CA
		key        KeySpec
		validity   time.Duration
		maxPathLen int
	}

	root, err := h.createCA(RoleRoot, profile.Organization+" Root CA", nil,
		profile.RootKey, profile.RootValidity, 2, now)
	if err != nil {
		return nil, err
	}
	h.Root = root

	specs := []spec{
		{RoleDeviceIssuance, profile.Organization + " Device Issuance CA", root, profile.IntermediateKey, profile.IntermediateValidity, 1},
		{RoleServices, profile.Organization + " Services CA", root, profile.IntermediateKey, profile.IntermediateValidity, 0},
		{RoleTenantManagement, profile.Organization + " Tenant Management CA", root, profile.IntermediateKey, profile.IntermediateValidity, 0},
	}
	for _, s := range specs {
		ca, err := h.createCA(s.role, s.commonName, s.parent, s.key, s.validity, s.maxPathLen, now)
		if err != nil {
			return nil, err
		}
		switch s.role {
		case RoleDeviceIssuance:
			h.DeviceIssuance = ca
		case RoleServices:
			h.Services = ca
		case RoleTenantManagement:
			h.TenantManagement = ca
		}
	}

	subSpecs := []spec{
		{RoleManufacturing, profile.Organization + " Manufacturing Sub-CA", h.DeviceIssuance, profile.SubCAKey, profile.SubCAValidity, 0},
		{RoleShelfController, profile.Organization + " Shelf Controller Sub-CA", h.DeviceIssuance, profile.SubCAKey, profile.SubCAValidity, 0},
	}
	for _, s := range subSpecs {
		ca, err := h.createCA(s.role, s.commonName, s.parent, s.key, s.validity, s.maxPathLen, now)
		if err != nil {
			return nil, err
		}
		if s.role == RoleManufacturing {
			h.Manufacturing = ca
		} else {
			h.ShelfController = ca
		}
	}

	h.buildPools()
	log.Info("pki hierarchy bootstrapped",
		"root_cn", h.Root.Certificate.Subject.CommonName,
		"root_serial", serialHex(h.Root.Certificate.SerialNumber),
		"root_not_after", h.Root.Certificate.NotAfter.Format(time.RFC3339),
		"authorities", len(h.byRole))
	return h, nil
}

// createCA generates a key and issues the certificate for one authority. A nil
// parent means self-signed: the root is its own issuer, which is what makes it
// an anchor rather than a link.
func (h *Hierarchy) createCA(role CARole, commonName string, parent *CA, keySpec KeySpec, validity time.Duration, maxPathLen int, now time.Time) (*CA, error) {
	key, err := keySpec.Generate()
	if err != nil {
		return nil, fmt.Errorf("pki: generate %s key (%s): %w", role, keySpec, err)
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	ski, err := subjectKeyID(key.Public())
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", role, err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         commonName,
			Organization:       []string{h.profile.Organization},
			OrganizationalUnit: []string{"Certificate Authority"},
		},
		NotBefore:             now.Add(-h.profile.Backdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            maxPathLen,
		// MaxPathLenZero must be set explicitly, otherwise encoding/asn1 cannot
		// tell "pathlen 0" (issue leaves only) from "no constraint at all".
		// Getting this wrong is the classic way an intermediate silently
		// becomes able to mint further CAs.
		MaxPathLenZero: maxPathLen == 0,
		SubjectKeyId:   ski,
	}
	if h.profile.Country != "" {
		tmpl.Subject.Country = []string{h.profile.Country}
	}

	issuerCert := tmpl
	issuerKey := key
	issuerSpec := keySpec
	if parent != nil {
		if parent.Certificate.NotAfter.Before(tmpl.NotAfter) {
			return nil, fmt.Errorf("pki: %s would outlive its issuer %s (%s > %s)",
				role, parent.Role,
				tmpl.NotAfter.Format(time.RFC3339), parent.Certificate.NotAfter.Format(time.RFC3339))
		}
		issuerCert = parent.Certificate
		if issuerKey, err = parent.Signer(); err != nil {
			return nil, err
		}
		issuerSpec = parent.KeySpec
	}
	tmpl.SignatureAlgorithm = issuerSpec.signatureAlgorithm()

	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuerCert, key.Public(), issuerKey)
	if err != nil {
		return nil, fmt.Errorf("pki: create %s certificate: %w", role, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse freshly created %s certificate: %w", role, err)
	}

	ca := &CA{Role: role, Certificate: cert, Parent: parent, KeySpec: keySpec, key: key}
	h.byRole[role] = ca
	h.logger().Info("pki authority created",
		"role", string(role), "cn", commonName, "key_spec", string(keySpec),
		"serial", serialHex(serial), "max_path_len", maxPathLen,
		"not_after", cert.NotAfter.Format(time.RFC3339))
	return ca, nil
}

// buildPools precomputes the trust anchor and intermediate pools. They are
// built once and read from every handshake; x509.CertPool is safe for
// concurrent reads, and rebuilding one per verification would put a
// several-hundred-microsecond allocation on the connection hot path.
func (h *Hierarchy) buildPools() {
	h.roots = x509.NewCertPool()
	h.roots.AddCert(h.Root.Certificate)
	h.intermediates = x509.NewCertPool()
	for _, role := range AllRoles() {
		if role == RoleRoot {
			continue
		}
		if ca := h.byRole[role]; ca != nil {
			h.intermediates.AddCert(ca.Certificate)
		}
	}
}

// Profile returns the algorithm and lifetime decisions this hierarchy was
// created with.
func (h *Hierarchy) Profile() Profile { return h.profile }

// TrustDomain returns the SPIFFE trust domain of this hierarchy.
func (h *Hierarchy) TrustDomain() string { return h.profile.TrustDomain }

// CreatedAt returns the instant the hierarchy was bootstrapped.
func (h *Hierarchy) CreatedAt() time.Time { return h.createdAt }

// SetLogger replaces the audit logger, e.g. after a service has built the
// logger it wants to correlate issuance with. Passing nil silences the
// hierarchy. It is safe to call while the hierarchy is already serving
// handshakes.
func (h *Hierarchy) SetLogger(l *obs.Logger) {
	if l == nil {
		l = obs.NopLogger()
	}
	h.log.Store(l)
}

// logger returns the current audit logger, never nil.
func (h *Hierarchy) logger() *obs.Logger {
	if l := h.log.Load(); l != nil {
		return l
	}
	return obs.NopLogger()
}

// CA returns the authority for a role.
func (h *Hierarchy) CA(role CARole) (*CA, error) {
	ca, ok := h.byRole[role]
	if !ok || ca == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, string(role))
	}
	return ca, nil
}

// Authorities returns every authority in hierarchy order.
func (h *Hierarchy) Authorities() []*CA {
	out := make([]*CA, 0, len(h.byRole))
	for _, role := range AllRoles() {
		if ca := h.byRole[role]; ca != nil {
			out = append(out, ca)
		}
	}
	return out
}

// RootPool returns the trust anchors: exactly one certificate, the root.
//
// The returned pool is the hierarchy's own and is safe to share because
// x509.CertPool is read-only after construction. Callers that need to add
// foreign anchors must clone it, which is deliberate friction — adding an
// anchor is the single most consequential change anyone can make to a
// zero-trust deployment.
func (h *Hierarchy) RootPool() *x509.CertPool { return h.roots }

// IntermediatePool returns every intermediate and sub-CA, so a verifier can
// complete a chain even when a peer presents a bare leaf.
func (h *Hierarchy) IntermediatePool() *x509.CertPool { return h.intermediates }

// Revocations returns the hierarchy's revocation registry.
func (h *Hierarchy) Revocations() *RevocationChecker { return h.revocations }

// HasRootKey reports whether the offline root's private key is currently
// loaded in this process.
func (h *Hierarchy) HasRootKey() bool { return h.Root.HasKey() }

// DropRootKey discards the root private key, modelling the moment the key
// ceremony ends and the key goes back in the safe.
//
// Everything the platform does day to day — issuing device certificates,
// issuing service certificates, publishing CRLs — is signed by an intermediate,
// so a hierarchy without its root key is fully operational. Only creating a new
// intermediate requires bringing the root back, and that is a planned ceremony
// with witnesses, not something a running service should be able to do.
func (h *Hierarchy) DropRootKey() { h.Root.dropKey() }

// issuerFor returns the authority that signs end-entity certificates of a kind.
func (h *Hierarchy) issuerFor(kind IdentityKind) (*CA, error) {
	switch kind {
	case KindLabel:
		return h.Manufacturing, nil
	case KindSEC, KindSGU:
		return h.ShelfController, nil
	case KindService:
		return h.Services, nil
	case KindTenant:
		return h.TenantManagement, nil
	default:
		return nil, fmt.Errorf("pki: no issuing authority for identity kind %q", string(kind))
	}
}

// newSerialNumber returns a 128-bit positive random serial.
//
// RFC 5280 caps serials at 20 octets and the CA/Browser Forum requires at least
// 64 bits of entropy; 128 bits is chosen for two reasons beyond compliance.
// First, an unpredictable serial denies an attacker the chosen-prefix control
// that made historical hash-collision forgeries practical. Second, and more
// operationally: the platform issues certificates from several regional
// clusters at once, and 128 random bits make a collision between two clusters
// impossible in practice without any coordination between them — a sequential
// counter would need exactly the kind of shared mutable state that a
// multi-region issuance path cannot afford.
func newSerialNumber() (*big.Int, error) {
	var b [16]byte
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("pki: read entropy for serial number: %w", err)
		}
		// Clear the top bit so the DER INTEGER encoding is unambiguously
		// positive; a negative serial is a RFC 5280 violation that some
		// verifiers reject and others silently reinterpret.
		b[0] &= 0x7f
		n := new(big.Int).SetBytes(b[:])
		if n.Sign() > 0 {
			return n, nil
		}
	}
	return nil, errors.New("pki: entropy source returned zero four times; refusing to issue a serial")
}

// serialHex renders a serial as lower-case hex, the form used in logs, in the
// revocation registry and in the audit stream. Decimal serials are unreadable
// at 128 bits and hex matches what OpenSSL prints.
func serialHex(n *big.Int) string {
	if n == nil {
		return ""
	}
	return hex.EncodeToString(n.Bytes())
}

// subjectKeyID derives a subject key identifier from a public key.
//
// RFC 5280 §4.2.1.2 suggests SHA-1 of the public key bit string but explicitly
// permits any method that yields unique values. SHA-256 truncated to 20 octets
// is used instead so that no part of the platform depends on SHA-1 being
// available — several of the FIPS-mode and hardened-kernel environments USSLP
// is deployed into refuse it outright, and an identifier is not the place to
// discover that.
func subjectKeyID(pub crypto.PublicKey) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return sum[:20], nil
}
