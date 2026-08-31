package pki

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// DefaultTrustDomain is the SPIFFE trust domain and DNS suffix of the platform.
// It is a name the platform controls but never resolves: no USSLP certificate
// is ever validated by a public CA, and no SAN here is expected to appear in
// public DNS.
const DefaultTrustDomain = "usslp.io"

// IdentityKind is what a certificate holder *is*. Authorisation everywhere in
// USSLP starts by branching on it: a label may publish acknowledgements, a SEC
// may publish on behalf of the labels in its zone, a service may do neither.
type IdentityKind string

// The five kinds of end-entity identity the platform issues.
const (
	// KindLabel is a Tier 1 smart shelf label.
	KindLabel IdentityKind = "label"
	// KindSEC is a Tier 2 Shelf Edge Controller.
	KindSEC IdentityKind = "sec"
	// KindSGU is a Tier 3 Store Gateway Unit.
	KindSGU IdentityKind = "sgu"
	// KindService is a cloud microservice authenticating with a SPIFFE identity.
	KindService IdentityKind = "service"
	// KindTenant is a tenant-operated API client (a retailer's own integration
	// calling the Universal Integration Gateway with a client certificate).
	KindTenant IdentityKind = "tenant"
)

// IsDevice reports whether the kind denotes physical hardware in a store, which
// is the set of identities the Device Issuance branch of the hierarchy signs.
func (k IdentityKind) IsDevice() bool {
	return k == KindLabel || k == KindSEC || k == KindSGU
}

// Common-name prefixes. The prefix is redundant with the SPIFFE SAN by design:
// an operator reading `openssl x509 -text` output, a log line, or a broker's
// connection table sees the kind immediately, and a certificate whose CN and
// SAN disagree is rejected rather than silently resolved in favour of one.
const (
	cnPrefixLabel   = "USSLP-LABEL-"
	cnPrefixSEC     = "USSLP-SEC-"
	cnPrefixSGU     = "USSLP-SGU-"
	cnPrefixService = "USSLP-SVC-"
	cnPrefixTenant  = "USSLP-TENANT-"
)

// Errors returned when a certificate does not carry a usable USSLP identity.
var (
	// ErrNoIdentity means the certificate carries no USSLP SAN at all. It is
	// the error a foreign certificate produces — a public web PKI leaf, or one
	// from a different hierarchy.
	ErrNoIdentity = errors.New("pki: certificate carries no USSLP identity")
	// ErrMalformedIdentity means the certificate carries USSLP-shaped SANs
	// whose contents are inconsistent or unparseable. Unlike ErrNoIdentity this
	// is worth an alert: something in the issuance path is broken, or someone
	// is probing the parser.
	ErrMalformedIdentity = errors.New("pki: certificate identity is malformed")
	// ErrIdentityRejected is returned by an identity predicate that refuses a
	// well-formed peer, e.g. a client from another tenant.
	ErrIdentityRejected = errors.New("pki: peer identity rejected by policy")
)

// Identity is the structured meaning of an end-entity certificate: who the
// holder is, which tenant and store it belongs to, and what it may therefore be
// authorised to do.
//
// Every field is derived from the certificate, never from anything the peer
// says about itself at the application layer. That is the whole point: the MQTT
// broker's authorizer builds a topic ACL from this struct without consulting a
// database, so a device cannot claim a tenant it was not issued for even if the
// authorization service is unreachable.
type Identity struct {
	// Kind is label, sec, sgu, service or tenant.
	Kind IdentityKind
	// DeviceID is the label, SEC or SGU identifier for device certificates, the
	// client name for tenant certificates, and empty for services.
	DeviceID string
	// TenantID is the isolation boundary. Empty for service identities, which
	// are platform-internal and cross-tenant by construction.
	TenantID canon.TenantID
	// StoreID is the physical location. Empty for service and tenant
	// identities.
	StoreID canon.StoreID
	// Namespace and Service carry the SPIFFE workload identity of a service
	// certificate and are empty otherwise.
	Namespace string
	Service   string
	// SPIFFEID is the canonical URI form of this identity.
	SPIFFEID string
	// TrustDomain is the SPIFFE trust domain the identity was issued in.
	TrustDomain string
	// CommonName is the subject CN, retained so that callers logging an
	// identity print what an operator will see in the certificate.
	CommonName string
	// SerialNumber is the issued certificate's serial in lower-case hex. It is
	// the key a revocation check is made on, so it is carried alongside the
	// identity rather than requiring the caller to keep the certificate too.
	SerialNumber string
	// NotBefore and NotAfter are the certificate's validity window, zero for an
	// identity that has not yet been issued.
	NotBefore time.Time
	NotAfter  time.Time
}

// NewLabelIdentity describes a Tier 1 label in a given tenant and store.
func NewLabelIdentity(tenant canon.TenantID, store canon.StoreID, label canon.LabelID) Identity {
	return Identity{Kind: KindLabel, TenantID: tenant, StoreID: store, DeviceID: string(label)}
}

// NewSECIdentity describes a Shelf Edge Controller.
func NewSECIdentity(tenant canon.TenantID, store canon.StoreID, sec canon.SECID) Identity {
	return Identity{Kind: KindSEC, TenantID: tenant, StoreID: store, DeviceID: string(sec)}
}

// NewSGUIdentity describes a Store Gateway Unit.
func NewSGUIdentity(tenant canon.TenantID, store canon.StoreID, sgu canon.SGUID) Identity {
	return Identity{Kind: KindSGU, TenantID: tenant, StoreID: store, DeviceID: string(sgu)}
}

// NewServiceIdentity describes a cloud workload, e.g. namespace "usslp-prod",
// service "label-service".
func NewServiceIdentity(namespace, service string) Identity {
	return Identity{Kind: KindService, Namespace: namespace, Service: service}
}

// NewTenantIdentity describes a tenant-operated API client. The client name
// distinguishes a retailer's several integrations — "pos-adapter",
// "merch-portal" — so that one can be revoked without disconnecting the others.
func NewTenantIdentity(tenant canon.TenantID, client string) Identity {
	return Identity{Kind: KindTenant, TenantID: tenant, DeviceID: client}
}

// LabelID returns the identity's device identifier as a canon.LabelID. It is
// only meaningful when Kind is KindLabel.
func (i Identity) LabelID() canon.LabelID { return canon.LabelID(i.DeviceID) }

// SECID returns the identity's device identifier as a canon.SECID.
func (i Identity) SECID() canon.SECID { return canon.SECID(i.DeviceID) }

// SGUID returns the identity's device identifier as a canon.SGUID.
func (i Identity) SGUID() canon.SGUID { return canon.SGUID(i.DeviceID) }

// TopicScope builds the MQTT scope this identity is confined to. Region is not
// carried in the certificate — a device is re-homed between regions during a
// failover without being re-issued — so the caller supplies it from its own
// configuration.
func (i Identity) TopicScope(region canon.Region) canon.TopicScope {
	return canon.TopicScope{Tenant: i.TenantID, Region: region, Store: i.StoreID}
}

// SubscribePattern returns the single MQTT filter this identity's broker
// credential should be granted. It is deliberately the whole-tenant wildcard
// from canon: tenant isolation is the boundary the broker enforces, and
// per-store filtering is enforced above it by the authorizer using StoreID.
func (i Identity) SubscribePattern() string { return canon.SubscribeTenant(i.TenantID) }

// String renders the identity for logs. It is the SPIFFE ID when there is one,
// because that is the form that appears in every other system's audit trail.
func (i Identity) String() string {
	if i.SPIFFEID != "" {
		return i.SPIFFEID
	}
	n, err := i.normalize(DefaultTrustDomain)
	if err != nil {
		return string(i.Kind) + ":<invalid>"
	}
	return n.SPIFFEID
}

// LogAttrs returns the identity as alternating key/value pairs for
// obs.Logger.With, so every log line written while serving a peer carries the
// same field names the rest of the platform filters on.
func (i Identity) LogAttrs() []any {
	attrs := []any{"peer_kind", string(i.Kind), "peer_spiffe_id", i.SPIFFEID}
	if i.TenantID != "" {
		attrs = append(attrs, "tenant_id", string(i.TenantID))
	}
	if i.StoreID != "" {
		attrs = append(attrs, "store_id", string(i.StoreID))
	}
	if i.DeviceID != "" {
		attrs = append(attrs, "device_id", i.DeviceID)
	}
	if i.SerialNumber != "" {
		attrs = append(attrs, "cert_serial", i.SerialNumber)
	}
	return attrs
}

// normalize fills in the derived fields (trust domain, common name, SPIFFE ID)
// and rejects an identity that cannot be issued. It returns a copy: callers
// hold Identity by value everywhere so that a predicate cannot mutate the
// identity a later predicate will see.
func (i Identity) normalize(trustDomain string) (Identity, error) {
	if i.TrustDomain == "" {
		i.TrustDomain = trustDomain
	}
	if i.TrustDomain == "" {
		i.TrustDomain = DefaultTrustDomain
	}
	switch i.Kind {
	case KindLabel, KindSEC, KindSGU:
		if err := validateDNSComponent("tenant id", string(i.TenantID)); err != nil {
			return Identity{}, err
		}
		if err := validateDNSComponent("store id", string(i.StoreID)); err != nil {
			return Identity{}, err
		}
		if err := validateDNSComponent("device id", i.DeviceID); err != nil {
			return Identity{}, err
		}
		i.Namespace, i.Service = "", ""
		i.CommonName = devicePrefix(i.Kind) + i.DeviceID
		i.SPIFFEID = fmt.Sprintf("spiffe://%s/tenant/%s/store/%s/%s/%s",
			i.TrustDomain, i.TenantID, i.StoreID, i.Kind, i.DeviceID)
	case KindService:
		if err := validateDNSComponent("namespace", i.Namespace); err != nil {
			return Identity{}, err
		}
		if err := validateDNSComponent("service name", i.Service); err != nil {
			return Identity{}, err
		}
		i.TenantID, i.StoreID, i.DeviceID = "", "", ""
		i.CommonName = cnPrefixService + i.Service + "." + i.Namespace
		i.SPIFFEID = fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", i.TrustDomain, i.Namespace, i.Service)
	case KindTenant:
		if err := validateDNSComponent("tenant id", string(i.TenantID)); err != nil {
			return Identity{}, err
		}
		if err := validateDNSComponent("client name", i.DeviceID); err != nil {
			return Identity{}, err
		}
		i.StoreID, i.Namespace, i.Service = "", "", ""
		i.CommonName = cnPrefixTenant + string(i.TenantID) + "-" + i.DeviceID
		i.SPIFFEID = fmt.Sprintf("spiffe://%s/tenant/%s/client/%s", i.TrustDomain, i.TenantID, i.DeviceID)
	default:
		return Identity{}, fmt.Errorf("pki: unknown identity kind %q", string(i.Kind))
	}
	return i, nil
}

func devicePrefix(k IdentityKind) string {
	switch k {
	case KindLabel:
		return cnPrefixLabel
	case KindSEC:
		return cnPrefixSEC
	default:
		return cnPrefixSGU
	}
}

// organizationalUnit is the OU the subject carries. It exists for humans and
// for legacy directory tooling; nothing in the platform authorises on it.
func organizationalUnit(k IdentityKind) string {
	switch k {
	case KindLabel:
		return "Labels"
	case KindSEC:
		return "Controllers"
	case KindSGU:
		return "Gateways"
	case KindService:
		return "Services"
	default:
		return "Tenants"
	}
}

// subject builds the X.501 subject for an identity.
func (i Identity) subject(p Profile) pkix.Name {
	n := pkix.Name{
		CommonName:         i.CommonName,
		Organization:       []string{p.Organization},
		OrganizationalUnit: []string{organizationalUnit(i.Kind)},
	}
	if p.Country != "" {
		n.Country = []string{p.Country}
	}
	return n
}

// dnsNames returns the DNS SANs for the identity.
//
// The device form is {device}.{store}.{tenant}.{kind}s.{trust-domain}, most
// specific label first, so that an MQTT broker or an nginx-style matcher can
// authorise a whole tenant with one suffix comparison and a whole store with a
// slightly longer one. The bare tenant name is repeated as its own SAN because
// the commonest ACL question by far — "is this peer in tenant X?" — should not
// require string surgery in the broker's hot path.
func (i Identity) dnsNames() []string {
	switch i.Kind {
	case KindLabel, KindSEC, KindSGU:
		return []string{
			fmt.Sprintf("%s.%s.%s.%ss.%s", i.DeviceID, i.StoreID, i.TenantID, i.Kind, i.TrustDomain),
			fmt.Sprintf("%s.%s.stores.%s", i.StoreID, i.TenantID, i.TrustDomain),
			fmt.Sprintf("%s.tenants.%s", i.TenantID, i.TrustDomain),
		}
	case KindService:
		return []string{
			fmt.Sprintf("%s.%s.svc.%s", i.Service, i.Namespace, i.TrustDomain),
			fmt.Sprintf("%s.%s.svc", i.Service, i.Namespace),
		}
	default:
		return []string{
			fmt.Sprintf("%s.%s.clients.%s", i.DeviceID, i.TenantID, i.TrustDomain),
			fmt.Sprintf("%s.tenants.%s", i.TenantID, i.TrustDomain),
		}
	}
}

// uris returns the URI SANs — always exactly the SPIFFE ID. A certificate with
// two URI SANs would let two authorizers disagree about who the peer is.
func (i Identity) uris() ([]*url.URL, error) {
	u, err := url.Parse(i.SPIFFEID)
	if err != nil {
		return nil, fmt.Errorf("pki: identity %s: %w", i.CommonName, err)
	}
	return []*url.URL{u}, nil
}

// extKeyUsage returns the extended key usages for the identity.
//
// Devices get client authentication only: a label or a SEC never terminates a
// TLS server socket, so a certificate that would let it impersonate one is
// authority nobody needs. The Store Gateway Unit is the exception — it runs the
// store's local MQTT listener, which the SECs connect *to* — so it gets both.
// Services get both because every service is a client of some other service.
func (i Identity) extKeyUsage() []x509.ExtKeyUsage {
	switch i.Kind {
	case KindLabel, KindSEC, KindTenant:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	default:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	}
}

// IdentityFromCertificate extracts the USSLP identity a certificate asserts.
//
// It reads the SPIFFE URI SAN first because that form is unambiguous and
// canonical, falls back to the DNS SAN encoding for certificates issued before
// a URI SAN was mandatory, and then cross-checks the subject common name. A
// certificate whose CN and SANs disagree is rejected outright rather than
// resolved in favour of either: disagreement means the certificate was not
// produced by this package's issuance path, and guessing which half to believe
// is how confused-deputy bugs are born.
//
// It performs no chain validation whatsoever. Call it only on a certificate
// that has already been verified — from a VerifyPeerCertificate callback, or
// after [Hierarchy.VerifyChain] — never on an unverified leaf off the wire.
func IdentityFromCertificate(cert *x509.Certificate) (Identity, error) {
	if cert == nil {
		return Identity{}, fmt.Errorf("%w: nil certificate", ErrNoIdentity)
	}
	if cert.IsCA {
		return Identity{}, fmt.Errorf("%w: %q is a CA certificate, not an end entity", ErrNoIdentity, cert.Subject.CommonName)
	}

	id, err := identityFromURIs(cert.URIs)
	if err != nil {
		return Identity{}, err
	}
	if id.Kind == "" {
		if id, err = identityFromDNS(cert.DNSNames); err != nil {
			return Identity{}, err
		}
	}
	if id.Kind == "" {
		return Identity{}, fmt.Errorf("%w: subject %q has no SPIFFE URI SAN and no USSLP DNS SAN",
			ErrNoIdentity, cert.Subject.CommonName)
	}

	normalized, err := id.normalize(id.TrustDomain)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %s", ErrMalformedIdentity, err)
	}
	if cert.Subject.CommonName != normalized.CommonName {
		return Identity{}, fmt.Errorf("%w: subject CN %q does not match SAN identity %q",
			ErrMalformedIdentity, cert.Subject.CommonName, normalized.CommonName)
	}
	normalized.SerialNumber = serialHex(cert.SerialNumber)
	normalized.NotBefore = cert.NotBefore
	normalized.NotAfter = cert.NotAfter
	return normalized, nil
}

// identityFromURIs decodes the SPIFFE SAN. A certificate with more than one URI
// SAN, or with a non-SPIFFE URI, is malformed rather than merely unidentified.
func identityFromURIs(uris []*url.URL) (Identity, error) {
	if len(uris) == 0 {
		return Identity{}, nil
	}
	if len(uris) > 1 {
		return Identity{}, fmt.Errorf("%w: %d URI SANs present, exactly one SPIFFE ID is required",
			ErrMalformedIdentity, len(uris))
	}
	u := uris[0]
	if u.Scheme != "spiffe" {
		return Identity{}, fmt.Errorf("%w: URI SAN scheme %q is not spiffe", ErrMalformedIdentity, u.Scheme)
	}
	if u.Host == "" {
		return Identity{}, fmt.Errorf("%w: SPIFFE ID %q has no trust domain", ErrMalformedIdentity, u.String())
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	id := Identity{TrustDomain: u.Host}
	switch {
	case len(parts) == 4 && parts[0] == "ns" && parts[2] == "sa":
		id.Kind = KindService
		id.Namespace, id.Service = parts[1], parts[3]
	case len(parts) == 6 && parts[0] == "tenant" && parts[2] == "store":
		kind := IdentityKind(parts[4])
		if !kind.IsDevice() {
			return Identity{}, fmt.Errorf("%w: SPIFFE ID %q names unknown device kind %q",
				ErrMalformedIdentity, u.String(), parts[4])
		}
		id.Kind = kind
		id.TenantID = canon.TenantID(parts[1])
		id.StoreID = canon.StoreID(parts[3])
		id.DeviceID = parts[5]
	case len(parts) == 4 && parts[0] == "tenant" && parts[2] == "client":
		id.Kind = KindTenant
		id.TenantID = canon.TenantID(parts[1])
		id.DeviceID = parts[3]
	default:
		return Identity{}, fmt.Errorf("%w: SPIFFE ID %q does not match any USSLP identity path",
			ErrMalformedIdentity, u.String())
	}
	return id, nil
}

// identityFromDNS decodes the DNS SAN encoding. The kind marker sits at a fixed
// offset from the front, so the trust domain may be any number of labels long
// without making the parse ambiguous.
func identityFromDNS(names []string) (Identity, error) {
	for _, name := range names {
		parts := strings.Split(name, ".")
		if len(parts) >= 5 {
			switch parts[3] {
			case "labels", "secs", "sgus":
				return Identity{
					Kind:        IdentityKind(strings.TrimSuffix(parts[3], "s")),
					DeviceID:    parts[0],
					StoreID:     canon.StoreID(parts[1]),
					TenantID:    canon.TenantID(parts[2]),
					TrustDomain: strings.Join(parts[4:], "."),
				}, nil
			}
		}
		if len(parts) >= 4 && parts[2] == "svc" {
			return Identity{
				Kind:        KindService,
				Service:     parts[0],
				Namespace:   parts[1],
				TrustDomain: strings.Join(parts[3:], "."),
			}, nil
		}
		if len(parts) >= 4 && parts[2] == "clients" {
			return Identity{
				Kind:        KindTenant,
				DeviceID:    parts[0],
				TenantID:    canon.TenantID(parts[1]),
				TrustDomain: strings.Join(parts[3:], "."),
			}, nil
		}
	}
	return Identity{}, nil
}

// validateDNSComponent enforces the intersection of three separate rule sets:
// canon's identifier rules (no MQTT wildcards or separators, because these
// strings become topic segments), the DNS label rules from RFC 1035 (because
// they become SAN labels), and a ban on the dot (because the SAN encoding uses
// it as its own separator and an identifier containing one would let a device
// claim a store it was not issued for).
func validateDNSComponent(what, s string) error {
	if s == "" {
		return fmt.Errorf("pki: %s must not be empty", what)
	}
	if len(s) > 63 {
		return fmt.Errorf("pki: %s %q is %d characters, DNS labels are limited to 63", what, s, len(s))
	}
	if !canon.ValidID(s) {
		return fmt.Errorf("pki: %s %q contains characters that are not legal in a topic segment", what, s)
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return fmt.Errorf("pki: %s %q must not start or end with a hyphen", what, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return fmt.Errorf("pki: %s %q contains %q; only letters, digits and hyphens are allowed "+
				"because the value is encoded as a DNS SAN label", what, s, string(rune(c)))
		}
	}
	return nil
}
