package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// IdentityPredicate is an authorisation rule applied to a verified peer during
// the TLS handshake.
//
// Running the check here rather than in the application handler is a deliberate
// choice: a connection that should never have been accepted is closed before
// the first byte of protocol is exchanged, so no parser, no router and no
// broker subscription logic ever sees traffic from a peer that failed the rule.
// Returning an error aborts the handshake.
type IdentityPredicate func(Identity) error

// TLSOptions configures the mutual-TLS builders.
type TLSOptions struct {
	// Roots overrides the hierarchy's trust anchors. Leave nil in every normal
	// deployment; it exists for a migration in which two roots are trusted
	// simultaneously while the fleet re-enrols.
	Roots *x509.CertPool
	// Revocation overrides the hierarchy's revocation registry, e.g. an edge
	// verifier supplying the registry it loaded from a synced CRL.
	Revocation *RevocationChecker
	// Predicate is the authorisation rule applied to the verified peer. Nil
	// means any peer holding a valid USSLP certificate is accepted, which is
	// almost never what a zero-trust deployment wants — see [RequireTenant].
	Predicate IdentityPredicate
	// ServerName is the name a client verifies the server certificate against.
	// Required for clients; ignored for servers.
	ServerName string
	// NextProtos is the ALPN list.
	NextProtos []string
	// ClientAuth overrides the server's client-certificate policy. Zero means
	// tls.RequireAndVerifyClientCert. Weakening it is possible but has to be
	// written down at the call site, where a reviewer will see it.
	ClientAuth tls.ClientAuthType
	// Now overrides the clock, for tests and for devices whose time is being
	// supplied by a synchronised source rather than the system clock.
	Now func() time.Time
}

func (o TLSOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// ServerTLSConfig builds the TLS configuration for a USSLP listener.
//
// Every connection into the platform is mutually authenticated: TLS 1.3 as a
// floor, a client certificate required and verified against the USSLP root, the
// full chain re-verified with revocation checking, and the peer's identity run
// through the caller's authorisation predicate before the handshake completes.
//
// Session tickets are disabled. TLS 1.3 resumption would let a peer skip the
// full handshake — and therefore skip VerifyPeerCertificate — on subsequent
// connections, which means a device revoked five minutes ago could keep
// reconnecting until its ticket expired. USSLP connections are long-lived
// (a SEC holds one MQTT session for weeks), so the saved round trip is worth
// far less than re-evaluating revocation on every connection.
func (h *Hierarchy) ServerTLSConfig(cert tls.Certificate, opts TLSOptions) (*tls.Config, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("pki: server tls config: certificate is empty")
	}
	roots := opts.Roots
	if roots == nil {
		roots = h.roots
	}
	clientAuth := opts.ClientAuth
	if clientAuth == tls.NoClientCert {
		clientAuth = tls.RequireAndVerifyClientCert
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   clientAuth,
		ClientCAs:    roots,
		NextProtos:   opts.NextProtos,
		// The peer presenting a client certificate is a client, so it is the
		// client-authentication usage that must be present on its leaf.
		VerifyPeerCertificate:  h.verifyPeerFunc(x509.ExtKeyUsageClientAuth, opts),
		SessionTicketsDisabled: true,
	}
	if opts.Now != nil {
		cfg.Time = opts.Now
	}
	return cfg, nil
}

// ClientTLSConfig builds the TLS configuration for a USSLP client.
//
// The client verifies the server exactly as strictly as the server verifies it:
// same root, same revocation registry, same identity predicate. A Shelf Edge
// Controller connecting to its Store Gateway checks that the gateway holds a
// certificate for its own tenant, so a rogue gateway plugged into the store
// network cannot collect traffic even if it can win the DNS race.
func (h *Hierarchy) ClientTLSConfig(cert tls.Certificate, opts TLSOptions) (*tls.Config, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("pki: client tls config: certificate is empty")
	}
	if opts.ServerName == "" {
		return nil, errors.New("pki: client tls config: ServerName is required; " +
			"without it the client cannot tell which server it reached")
	}
	roots := opts.Roots
	if roots == nil {
		roots = h.roots
	}
	cfg := &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            tls.VersionTLS13,
		RootCAs:               roots,
		ServerName:            opts.ServerName,
		NextProtos:            opts.NextProtos,
		VerifyPeerCertificate: h.verifyPeerFunc(x509.ExtKeyUsageServerAuth, opts),
	}
	if opts.Now != nil {
		cfg.Time = opts.Now
	}
	return cfg, nil
}

// verifyPeerFunc builds the VerifyPeerCertificate callback.
//
// crypto/tls has already built and verified a chain by the time this runs; the
// callback exists to add the three things it cannot know about: revocation, the
// platform's own error taxonomy, and the caller's authorisation rule. It
// re-verifies from the raw certificates rather than trusting the verifiedChains
// argument so that the same code path serves both directions and so that a
// future configuration mistake (VerifyPeerCertificate wired up alongside
// InsecureSkipVerify, say) cannot leave the peer unverified.
func (h *Hierarchy) verifyPeerFunc(usage x509.ExtKeyUsage, opts TLSOptions) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("pki: peer presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for i, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("pki: parse peer certificate %d: %w", i, err)
			}
			certs = append(certs, cert)
		}
		id, err := h.VerifyPeer(certs[0], certs[1:], VerifyOptions{
			At:         opts.now(),
			KeyUsages:  []x509.ExtKeyUsage{usage},
			Revocation: opts.Revocation,
		})
		if err != nil {
			return err
		}
		if opts.Predicate != nil {
			if err := opts.Predicate(id); err != nil {
				h.logger().Warn("pki peer rejected by policy", append(id.LogAttrs(), "error", err.Error())...)
				return err
			}
		}
		return nil
	}
}

// PeerIdentity extracts the verified USSLP identity from a completed handshake.
//
// It is safe to call from a request handler: by the time a connection state is
// available the peer certificate has been through the full verification above,
// so the identity it returns is the one the platform authorised, not one the
// peer asserted at the application layer. Handlers should authorise on this and
// never on a tenant identifier taken from a header or a message body.
func PeerIdentity(state *tls.ConnectionState) (Identity, error) {
	if state == nil {
		return Identity{}, fmt.Errorf("%w: no TLS connection state", ErrNoIdentity)
	}
	if len(state.PeerCertificates) == 0 {
		return Identity{}, fmt.Errorf("%w: peer presented no certificate", ErrNoIdentity)
	}
	return IdentityFromCertificate(state.PeerCertificates[0])
}

// RequireTenant accepts only peers belonging to one tenant.
//
// This is the predicate a store-local listener uses. A Store Gateway in a
// Tesco store will not complete a handshake with a device issued to Carrefour,
// even though both certificates chain to the same USSLP root — shared trust
// anchor is not shared authority, and that distinction is the whole of tenant
// isolation at the transport layer.
func RequireTenant(tenant canon.TenantID) IdentityPredicate {
	return func(id Identity) error {
		if id.TenantID != tenant {
			return fmt.Errorf("%w: peer %s belongs to tenant %q, this endpoint serves %q",
				ErrIdentityRejected, id.SPIFFEID, id.TenantID, tenant)
		}
		return nil
	}
}

// RequireStore accepts only peers belonging to one tenant and store. It is the
// rule a Shelf Edge Controller applies to the labels in its zone: a label from
// the shop across the road is a valid USSLP device and still has no business on
// this mesh.
func RequireStore(tenant canon.TenantID, store canon.StoreID) IdentityPredicate {
	return func(id Identity) error {
		if id.TenantID != tenant || id.StoreID != store {
			return fmt.Errorf("%w: peer %s is %s/%s, this endpoint serves %s/%s",
				ErrIdentityRejected, id.SPIFFEID, id.TenantID, id.StoreID, tenant, store)
		}
		return nil
	}
}

// RequireKind accepts only peers of the listed kinds, e.g. a broker listener
// that accepts SECs and SGUs but not labels, because labels never connect to it
// directly.
func RequireKind(kinds ...IdentityKind) IdentityPredicate {
	allowed := make(map[IdentityKind]struct{}, len(kinds))
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	return func(id Identity) error {
		if _, ok := allowed[id.Kind]; !ok {
			return fmt.Errorf("%w: peer %s is a %s, which this endpoint does not serve",
				ErrIdentityRejected, id.SPIFFEID, id.Kind)
		}
		return nil
	}
}

// RequireService accepts only one named workload, the tightest rule available
// for service-to-service calls: the Label Service's price-authority endpoint
// accepts the Pricing Service and nothing else, not even another service in the
// same namespace.
func RequireService(namespace, service string) IdentityPredicate {
	return func(id Identity) error {
		if id.Kind != KindService || id.Namespace != namespace || id.Service != service {
			return fmt.Errorf("%w: peer %s is not %s/%s", ErrIdentityRejected, id.SPIFFEID, namespace, service)
		}
		return nil
	}
}

// AllPredicates accepts a peer only if every predicate accepts it. Predicates
// run in order and the first rejection is returned, so put the cheapest and
// most selective rule first.
func AllPredicates(preds ...IdentityPredicate) IdentityPredicate {
	return func(id Identity) error {
		for _, p := range preds {
			if p == nil {
				continue
			}
			if err := p(id); err != nil {
				return err
			}
		}
		return nil
	}
}

// AnyPredicate accepts a peer if at least one predicate accepts it — the rule
// for an endpoint serving two distinct populations, such as a gateway that
// accepts both its own store's controllers and the platform's field-service
// tooling.
func AnyPredicate(preds ...IdentityPredicate) IdentityPredicate {
	return func(id Identity) error {
		var last error
		for _, p := range preds {
			if p == nil {
				continue
			}
			err := p(id)
			if err == nil {
				return nil
			}
			last = err
		}
		if last == nil {
			return fmt.Errorf("%w: no predicate accepted peer %s", ErrIdentityRejected, id.SPIFFEID)
		}
		return last
	}
}
