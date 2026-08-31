package mqtt

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Action distinguishes the two things a client can ask to do with a topic. The
// distinction is not cosmetic: a store's POS bridge is allowed to publish price
// updates but must never be able to subscribe to another lane's traffic, and a
// SEC is the mirror image.
type Action int

// The authorizable actions.
const (
	// ActionPublish covers PUBLISH, and also the last-will topic, which is
	// checked at CONNECT time — otherwise a client could have the broker
	// publish to a topic on its behalf that it could not publish to itself.
	ActionPublish Action = iota
	// ActionSubscribe covers SUBSCRIBE. The topic passed is a filter and may
	// contain wildcards, so an authorizer must reason about what the filter
	// could match, not about one concrete topic.
	ActionSubscribe
)

// String names the action for authorization denials, which are the log lines an
// operator reads when a device is reaching outside its tenant.
func (a Action) String() string {
	if a == ActionPublish {
		return "publish"
	}
	return "subscribe"
}

// Authentication and authorization failures are distinct errors because they
// map to distinct CONNACK codes and to distinct operational responses: bad
// credentials means a device was mis-provisioned, not authorized means a device
// is reaching outside its tenant and someone should look at why.
var (
	// ErrNotAuthenticated rejects a CONNECT. It becomes CONNACK 0x04.
	ErrNotAuthenticated = errors.New("mqtt: authentication failed")
	// ErrNotAuthorized rejects a topic operation. On CONNECT (a will topic) it
	// becomes CONNACK 0x05; on SUBSCRIBE it becomes a 0x80 return code for that
	// filter alone; on PUBLISH the broker closes the connection, because MQTT
	// 3.1.1 gives it no way to report a refused publication.
	ErrNotAuthorized = errors.New("mqtt: not authorized")
)

// ConnInfo is everything the broker knows about a connecting peer. It is passed
// to the optional CertAuthenticator/CertAuthorizer extensions so that a device
// certificate can be the identity: in production a SEC has no password, it has
// an mTLS client certificate, and the tenant it belongs to is a field of that
// certificate's subject.
type ConnInfo struct {
	ClientID   string
	Username   string
	Password   []byte
	RemoteAddr net.Addr
	// TLS is nil on a plaintext listener.
	TLS *tls.ConnectionState
	// PeerCertificate is the leaf certificate the client presented, or nil.
	PeerCertificate *x509.Certificate
}

// Subject returns the peer certificate's distinguished name, or "" on a
// connection with no client certificate. It is the string an operator sees in
// an authorization denial, so it is the full DN rather than just the CN.
func (i ConnInfo) Subject() string {
	if i.PeerCertificate == nil {
		return ""
	}
	return i.PeerCertificate.Subject.String()
}

// Authorizer is the broker's security hook. USSLP is multi-tenant on shared
// infrastructure, so "who may touch which topic" cannot be a property of the
// deployment's network topology; it has to be enforced on every packet by the
// broker itself. TenantAuthorizer is the default implementation and is a real
// one, not a placeholder.
type Authorizer interface {
	// Authenticate accepts or rejects a CONNECT. Returning any error rejects
	// the connection; ErrNotAuthorized yields CONNACK 0x05 and anything else
	// yields 0x04.
	Authenticate(clientID, username string, password []byte) error
	// Authorize accepts or rejects one topic operation. topic is a filter when
	// action is ActionSubscribe.
	Authorize(clientID, username, topic string, action Action) error
}

// CertAuthenticator is the optional extension an Authorizer implements when it
// needs the TLS peer certificate. The broker prefers it over Authenticate when
// available, so mTLS identity never has to be smuggled through the username
// field.
type CertAuthenticator interface {
	AuthenticateConn(info ConnInfo) error
}

// CertAuthorizer is the optional extension for topic checks that depend on the
// certificate rather than on the username — the normal case once devices are
// provisioned with SVIDs.
type CertAuthorizer interface {
	AuthorizeConn(info ConnInfo, topic string, action Action) error
}

// AllowAll permits every connection and every topic. It exists for `make dev`
// and for tests of behaviour other than access control; a broker configured
// with it logs that fact on start-up because running it in front of real stores
// would put every tenant in one namespace.
type AllowAll struct{}

// Authenticate accepts every client.
func (AllowAll) Authenticate(string, string, []byte) error { return nil }

// Authorize accepts every topic operation.
func (AllowAll) Authorize(string, string, string, Action) error { return nil }

// TenantAuthorizer enforces USSLP's one structural security rule: a client may
// only publish and subscribe under usslp/{its-tenant}/#.
//
// The rule is checkable in constant time because canon puts the tenant in the
// second topic level for exactly this purpose, and it is airtight because a
// filter is required to name its tenant literally — "usslp/+/#" is refused,
// since a '+' there would match every tenant on the broker.
type TenantAuthorizer struct {
	// Credentials maps username to password. A nil map with AllowAnonymous
	// false accepts only certificate-authenticated clients, which is the
	// production posture; passwords exist for the developer stack and for POS
	// bridges that predate mTLS.
	Credentials map[string]string
	// AllowAnonymous accepts a CONNECT with no username. The tenant then has to
	// come from TenantOf, so this is only useful with a custom TenantOf or in
	// single-tenant development.
	AllowAnonymous bool
	// TenantOf derives the tenant from the connection. Nil means
	// DefaultTenantOf: the certificate's organization if there is a client
	// certificate, otherwise the part of the username before the first ':'.
	TenantOf func(info ConnInfo) (canon.TenantID, error)
}

// DefaultTenantOf is the identity-to-tenant mapping TenantAuthorizer uses when
// none is configured.
//
// A client certificate wins over a username, because a certificate is issued by
// the platform's CA and a username is whatever the device was told to send. The
// certificate's Organization holds the tenant; USSLP's device CA is configured
// to emit "O=acme-retail, CN=sec-0042.store-17.acme-retail". Failing that, the
// username is either the tenant itself or "{tenant}:{user}", which lets one
// tenant issue several broker logins without a second lookup table.
func DefaultTenantOf(info ConnInfo) (canon.TenantID, error) {
	if c := info.PeerCertificate; c != nil {
		for _, o := range c.Subject.Organization {
			if canon.ValidID(o) {
				return canon.TenantID(o), nil
			}
		}
		return "", fmt.Errorf("%w: certificate %q names no usable tenant organization", ErrNotAuthenticated, c.Subject.String())
	}
	name := info.Username
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	if !canon.ValidID(name) {
		return "", fmt.Errorf("%w: username %q does not name a tenant", ErrNotAuthenticated, info.Username)
	}
	return canon.TenantID(name), nil
}

func (a *TenantAuthorizer) tenantOf(info ConnInfo) (canon.TenantID, error) {
	if a.TenantOf != nil {
		return a.TenantOf(info)
	}
	return DefaultTenantOf(info)
}

// Authenticate implements Authorizer for password clients.
func (a *TenantAuthorizer) Authenticate(clientID, username string, password []byte) error {
	return a.AuthenticateConn(ConnInfo{ClientID: clientID, Username: username, Password: password})
}

// AuthenticateConn implements CertAuthenticator: it accepts a client that
// presents a usable certificate, or one whose username and password match
// Credentials, and requires that either way a tenant can be derived — an
// authenticated client with no tenant could not be authorized for any topic, so
// admitting it would only produce a confusing failure later.
func (a *TenantAuthorizer) AuthenticateConn(info ConnInfo) error {
	if info.PeerCertificate == nil {
		if info.Username == "" {
			if !a.AllowAnonymous {
				return fmt.Errorf("%w: no client certificate and no username", ErrNotAuthenticated)
			}
		} else {
			want, ok := a.Credentials[info.Username]
			if !ok {
				return fmt.Errorf("%w: unknown username %q", ErrNotAuthenticated, info.Username)
			}
			// Constant time: the broker is reachable from a store network, and
			// a timing oracle on the password of a tenant-wide login is worth
			// more to an attacker than one device's certificate.
			if subtle.ConstantTimeCompare([]byte(want), info.Password) != 1 {
				return fmt.Errorf("%w: bad password for %q", ErrNotAuthenticated, info.Username)
			}
		}
	}
	_, err := a.tenantOf(info)
	return err
}

// Authorize implements Authorizer for password clients.
func (a *TenantAuthorizer) Authorize(clientID, username, topic string, action Action) error {
	return a.AuthorizeConn(ConnInfo{ClientID: clientID, Username: username}, topic, action)
}

// AuthorizeConn implements CertAuthorizer: the topic or filter must sit inside
// the connection's tenant namespace.
func (a *TenantAuthorizer) AuthorizeConn(info ConnInfo, topic string, action Action) error {
	tenant, err := a.tenantOf(info)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotAuthorized, err)
	}
	if !withinTenant(topic, tenant) {
		return fmt.Errorf("%w: client %q (tenant %q) may not %s %q; permitted namespace is %q",
			ErrNotAuthorized, info.ClientID, tenant, action, topic, canon.SubscribeTenant(tenant))
	}
	return nil
}

// withinTenant reports whether a topic name or filter is confined to one
// tenant's namespace.
//
// Both of the first two levels must be literal. That is the whole check, and it
// is deliberately strict: "usslp/acme/#" passes, "usslp/+/us-east-1/#" does not
// even though it looks scoped, because '+' at the tenant level matches every
// tenant on the broker. A filter of "#" or "+/…" is likewise refused.
func withinTenant(topic string, tenant canon.TenantID) bool {
	levels := strings.SplitN(topic, levelSeparator, 3)
	if len(levels) < 2 {
		return false
	}
	return levels[0] == canon.MQTTRoot && levels[1] == string(tenant)
}
