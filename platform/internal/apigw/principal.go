package apigw

import (
	"context"
	"net/http"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// CredentialKind names how a request proved who it was.
//
// It is recorded on every access log line and returned by /v1/me because the
// first question in any credential-compromise investigation is "which kind of
// credential was used", and the second is "which one" — both of which have to
// be answerable without joining against an authentication service that may be
// the thing that is broken.
type CredentialKind string

// The three ways a caller authenticates to USSLP.
const (
	// CredAPIKey is a tenant-scoped bearer secret. Convenient, revocable, and
	// the credential most likely to end up in a git repository — which is why
	// it is prefix-identifiable (see [KeyPrefixLive]).
	CredAPIKey CredentialKind = "api_key"
	// CredJWT is a short-lived ES256 token minted by the platform's token
	// service and verified against a published key set.
	CredJWT CredentialKind = "jwt"
	// CredMTLS is a client certificate from the USSLP hierarchy. This is the
	// production path for machine-to-machine traffic: nothing bearer-shaped
	// crosses the wire, so nothing bearer-shaped can be replayed from a log.
	CredMTLS CredentialKind = "mtls"
)

// Principal is the authenticated caller.
//
// It is built once, by the authentication middleware, from the credential
// alone. Nothing downstream may widen it: a handler that wants a different
// tenant has to obtain a different credential, because there is no setter and
// the struct is passed by value into the request context.
type Principal struct {
	// TenantID is the isolation boundary. Every proxied request is stamped
	// with it and every stream subscription is filtered by it.
	TenantID canon.TenantID
	// Subject identifies the human or machine, e.g. "key:9f2c…" or a JWT sub.
	Subject string
	// Kind is how the caller authenticated.
	Kind CredentialKind
	// CredentialID is the specific credential: an API key id, a JWT jti, or a
	// certificate serial. It is what an operator revokes.
	CredentialID string
	// Roles grant permissions. A principal with no roles can authenticate and
	// do nothing, which is the correct state for a key whose roles were
	// removed rather than an implicit escalation to some default.
	Roles []Role
	// Stores restricts the principal to a subset of its tenant's stores. Empty
	// means every store in the tenant — the distinction is deliberate: nil is
	// "unscoped", and an explicitly empty-but-non-nil scope cannot be
	// constructed by any of the credential paths, so "no stores at all" is not
	// a state a caller can end up in by accident.
	Stores []canon.StoreID
	// Scopes are free-form additional grants carried by the credential. They
	// are not consulted by RBAC; they exist so a tenant can attach its own
	// labels to a key and read them back from /v1/me.
	Scopes []string
	// ExpiresAt is when the credential stops working, zero for one that does
	// not expire on its own (an mTLS principal expires with its certificate,
	// which the TLS stack has already enforced by the time we see it).
	ExpiresAt time.Time
	// RateKey is the token-bucket identity of this credential. Two keys
	// belonging to one tenant get separate buckets so that a runaway
	// integration does not starve a human operator, while both are also
	// charged against the shared per-tenant bucket.
	RateKey string
}

// AllowsStore reports whether the principal may act on a store.
//
// An unscoped principal (Stores empty) may act on every store in its tenant;
// the tenant boundary itself is enforced separately and unconditionally, so
// "every store" never means "every store in the platform".
func (p Principal) AllowsStore(store canon.StoreID) bool {
	if len(p.Stores) == 0 {
		return true
	}
	for _, s := range p.Stores {
		if s == store {
			return true
		}
	}
	return false
}

// Can reports whether any of the principal's roles grants a permission.
func (p Principal) Can(perm Permission) bool {
	for _, r := range p.Roles {
		if r.Grants(perm) {
			return true
		}
	}
	return false
}

// LogAttrs renders the principal for a structured log line. The credential id
// is included and the credential itself never is.
func (p Principal) LogAttrs() []any {
	return []any{
		"tenant_id", string(p.TenantID),
		"subject", p.Subject,
		"auth", string(p.Kind),
		"credential_id", p.CredentialID,
	}
}

// principalKey is the unexported context key. Unexported so that no package
// outside this one can inject a principal into a context and thereby
// manufacture an authorisation.
type principalKey struct{}

// WithPrincipal returns a context carrying the authenticated caller.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated caller in ctx.
//
// The boolean is not decoration: a handler that reads a principal from an
// unauthenticated context would otherwise silently act as the zero tenant,
// which is a tenant identifier that no record in the platform carries and so
// would fail open into an empty result set rather than an error.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// principalOf is the internal accessor for handlers that the authentication
// middleware has already guarded. A missing principal there is a routing bug,
// not a client error, so it returns a 500-shaped error rather than a 401.
func principalOf(r *http.Request) (Principal, error) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		return Principal{}, errInternal("authenticated route reached without a principal")
	}
	return p, nil
}
