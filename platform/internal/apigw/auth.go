package apigw

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// ---------------------------------------------------------------------------
// Authentication
//
// Three credential types, one output. Whichever way a caller proves itself,
// the result is a [Principal] whose tenant came from the credential and from
// nowhere else. Handlers never see a token, a certificate or a key.
// ---------------------------------------------------------------------------

// Credential presentation.
const (
	// authSchemeBearer covers both API keys and JWTs. Which one is decided by
	// the USSLP key prefix, not by a second header, because a client that has
	// to know which header to use for which credential will eventually send
	// both and be surprised by the precedence.
	authSchemeBearer = "bearer"
	// wsCredentialProtocol carries a credential through a WebSocket handshake.
	//
	// A browser cannot set Authorization on `new WebSocket(...)`. The two
	// remaining channels are a query parameter — which lands in every access
	// log, proxy log and browser history between here and the client — and the
	// subprotocol header, which does not. So the console offers
	// ["usslp.v1", "usslp.credential.<key>"] and the gateway selects
	// "usslp.v1" in its response, never echoing the credential back.
	wsCredentialProtocol = "usslp.credential."
	// wsProtocol is the subprotocol the gateway agrees to.
	wsProtocol = "usslp.v1"
)

// scrubbedRequestHeaders are removed from every inbound request before it is
// authenticated or routed.
//
// This is the mechanical half of the tenant boundary. An upstream service
// trusts X-USSLP-Tenant completely; if a client could set it and the gateway
// merely overwrote it in the happy path, then any code path that forwarded a
// request without going through the rewrite — a future health probe, a
// mistaken direct handler, a proxy bug — would be a cross-tenant hole. Deleting
// the headers at the door means no such path can exist.
var scrubbedRequestHeaders = []string{
	HeaderTenant,
	HeaderSubject,
	HeaderRoles,
	HeaderStores,
	// A client-supplied upstream name would let a caller pick which internal
	// service sees their request.
	HeaderUpstream,
}

// Authenticator turns a request into a principal.
type Authenticator struct {
	keys *KeyIssuer

	jwtKeys     *pki.JWTKeySet
	jwtIssuer   string
	jwtAudience string
	jwtLeeway   time.Duration

	certPrincipal func(pki.Identity) (Principal, error)

	now func() time.Time
}

// AuthConfig configures authentication. Every field is optional; a scheme with
// no configuration is simply never accepted, which is how a deployment that
// does not issue API keys turns them off.
type AuthConfig struct {
	// Keys verifies API keys.
	Keys *KeyIssuer
	// JWTKeys is the verification key set. Rotation is a matter of adding the
	// new key and removing the old one from this set; the gateway resolves by
	// "kid" and holds no other state.
	JWTKeys *pki.JWTKeySet
	// JWTIssuer and JWTAudience, when set, are required to match.
	JWTIssuer   string
	JWTAudience string
	// JWTLeeway absorbs clock skew between the token service and here.
	JWTLeeway time.Duration
	// CertPrincipal maps a verified client certificate identity to a
	// principal. When nil, [DefaultCertPrincipal] is used.
	CertPrincipal func(pki.Identity) (Principal, error)
	// Now supplies the clock.
	Now func() time.Time
}

// NewAuthenticator builds an authenticator.
func NewAuthenticator(cfg AuthConfig) *Authenticator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CertPrincipal == nil {
		cfg.CertPrincipal = DefaultCertPrincipal
	}
	if cfg.JWTLeeway <= 0 {
		cfg.JWTLeeway = 30 * time.Second
	}
	return &Authenticator{
		keys: cfg.Keys, jwtKeys: cfg.JWTKeys,
		jwtIssuer: cfg.JWTIssuer, jwtAudience: cfg.JWTAudience, jwtLeeway: cfg.JWTLeeway,
		certPrincipal: cfg.CertPrincipal, now: cfg.Now,
	}
}

// Authenticate resolves the principal for a request.
//
// Client certificates are tried first and, when one is present, exclusively: a
// caller that has completed a mutual-TLS handshake has already proved a strong
// identity, and letting a bearer token presented on the same connection
// override it would let a compromised token borrow a machine's certificate
// standing.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, CredentialKind, error) {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		p, err := a.fromCertificate(r.TLS.PeerCertificates[0])
		return p, CredMTLS, err
	}
	raw, err := credentialFromRequest(r)
	if err != nil {
		return Principal{}, "", err
	}
	if strings.HasPrefix(raw, KeyPrefixLive) || strings.HasPrefix(raw, KeyPrefixTest) {
		p, err := a.fromAPIKey(ctx, raw)
		return p, CredAPIKey, err
	}
	p, err := a.fromJWT(raw)
	return p, CredJWT, err
}

// credentialFromRequest extracts the presented secret.
func credentialFromRequest(r *http.Request) (string, error) {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		scheme, value, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, authSchemeBearer) {
			return "", errUnauthorized("Authorization must use the Bearer scheme")
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", errUnauthorized("Authorization carries an empty Bearer credential")
		}
		return value, nil
	}
	for _, proto := range parseSubprotocols(r.Header.Get("Sec-WebSocket-Protocol")) {
		if strings.HasPrefix(proto, wsCredentialProtocol) {
			if v := strings.TrimPrefix(proto, wsCredentialProtocol); v != "" {
				return v, nil
			}
		}
	}
	return "", errUnauthorized("no credential presented")
}

func parseSubprotocols(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fromAPIKey verifies a presented API key.
func (a *Authenticator) fromAPIKey(ctx context.Context, presented string) (Principal, error) {
	if a.keys == nil {
		return Principal{}, errUnauthorized("API key authentication is not enabled")
	}
	rec, err := a.keys.Verify(ctx, presented)
	if err != nil {
		// Every failure — malformed, unknown, wrong secret, expired, revoked —
		// becomes the same 401 with the same message. The distinction is in
		// the wrapped error, which the access log records and the caller does
		// not see; telling a caller that a key id is real but expired tells
		// them which half of a leaked credential is still worth attacking.
		return Principal{}, &apiError{
			status: http.StatusUnauthorized, code: "unauthenticated",
			err:     fmt.Errorf("api key rejected: %w", err),
			headers: map[string]string{"WWW-Authenticate": `Bearer realm="usslp", charset="UTF-8"`},
		}
	}
	return Principal{
		TenantID:     rec.TenantID,
		Subject:      "key:" + rec.KeyID,
		Kind:         CredAPIKey,
		CredentialID: rec.KeyID,
		Roles:        append([]Role(nil), rec.Roles...),
		Stores:       append([]canon.StoreID(nil), rec.Stores...),
		Scopes:       append([]string(nil), rec.Scopes...),
		ExpiresAt:    rec.ExpiresAt,
		RateKey:      "key:" + rec.KeyID,
	}, nil
}

// Claim scope conventions.
//
// pkg/pki fixes the token payload — issuer, subject, audience, tenant, a
// string scope list and the timestamps — and this package may not extend it.
// Roles and store scoping therefore travel inside the scope list under two
// reserved prefixes. That is a real constraint expressed honestly rather than
// a second claims struct that would have to be kept in sync with the signer.
const (
	// scopeRolePrefix marks a scope entry as a role grant: "role:owner".
	scopeRolePrefix = "role:"
	// scopeStorePrefix marks a scope entry as a store restriction:
	// "store:store-42". A token with no store scope entries is unrestricted
	// within its tenant.
	scopeStorePrefix = "store:"
)

// fromJWT verifies a bearer token.
func (a *Authenticator) fromJWT(token string) (Principal, error) {
	if a.jwtKeys == nil || a.jwtKeys.Len() == 0 {
		return Principal{}, errUnauthorized("bearer token authentication is not enabled")
	}
	claims, err := a.jwtKeys.Verify(token, pki.JWTVerifyOptions{
		At: a.now(), Issuer: a.jwtIssuer, Audience: a.jwtAudience, Leeway: a.jwtLeeway,
	})
	if err != nil {
		return Principal{}, &apiError{
			status: http.StatusUnauthorized, code: "unauthenticated",
			err:     fmt.Errorf("bearer token rejected: %w", err),
			headers: map[string]string{"WWW-Authenticate": `Bearer realm="usslp", error="invalid_token"`},
		}
	}
	if claims.TenantID == "" {
		return Principal{}, errUnauthorized("bearer token carries no tenant")
	}
	if !canon.ValidID(string(claims.TenantID)) {
		return Principal{}, errUnauthorized("bearer token tenant is not a valid identifier")
	}

	p := Principal{
		TenantID:     claims.TenantID,
		Subject:      claims.Subject,
		Kind:         CredJWT,
		CredentialID: claims.TokenID,
		ExpiresAt:    time.Unix(claims.ExpiresAt, 0).UTC(),
	}
	for _, scope := range claims.Scopes {
		switch {
		case strings.HasPrefix(scope, scopeRolePrefix):
			role := Role(strings.TrimPrefix(scope, scopeRolePrefix))
			// An unrecognised role is dropped rather than refused. Tokens are
			// minted by a service that may be a version ahead of this gateway
			// during a rollout, and failing every request for a role this
			// binary has not heard of would make adding a role an outage.
			if role.Valid() {
				p.Roles = append(p.Roles, role)
			}
		case strings.HasPrefix(scope, scopeStorePrefix):
			store := canon.StoreID(strings.TrimPrefix(scope, scopeStorePrefix))
			if canon.ValidID(string(store)) {
				p.Stores = append(p.Stores, store)
			}
		default:
			p.Scopes = append(p.Scopes, scope)
		}
	}
	// The bucket is keyed on the subject rather than the token id: tokens are
	// fifteen minutes long, and a per-token bucket would reset every time a
	// client refreshed, which is precisely the client that most needs limiting.
	p.RateKey = "sub:" + string(claims.TenantID) + "/" + claims.Subject
	return p, nil
}

// fromCertificate derives a principal from a verified client certificate.
//
// The certificate has already been chain-verified by crypto/tls against the
// configured client CA pool; [pki.IdentityFromCertificate] does no chain
// building and must only ever be called on a leaf that reached this point.
func (a *Authenticator) fromCertificate(cert *x509.Certificate) (Principal, error) {
	id, err := pki.IdentityFromCertificate(cert)
	if err != nil {
		return Principal{}, &apiError{
			status: http.StatusUnauthorized, code: "unauthenticated",
			err: fmt.Errorf("client certificate rejected: %w", err),
		}
	}
	p, err := a.certPrincipal(id)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			return Principal{}, err
		}
		return Principal{}, &apiError{
			status: http.StatusForbidden, code: "permission_denied",
			err: fmt.Errorf("client certificate refused: %w", err),
		}
	}
	return p, nil
}

// DefaultCertPrincipal is the mapping from a USSLP certificate identity to a
// gateway principal.
//
// Only tenant client certificates are accepted. A service identity carries no
// tenant — it is platform-internal and cross-tenant by construction — and
// admitting one at the public front door would create a principal with no
// isolation boundary, which is the one thing this package exists to prevent.
// Service-to-service traffic reaches the internal services through the mesh,
// not through here. Device certificates (label, SEC, SGU) never speak HTTP to
// the cloud at all; they speak MQTT to a broker inside the building.
func DefaultCertPrincipal(id pki.Identity) (Principal, error) {
	if id.Kind != pki.KindTenant {
		return Principal{}, fmt.Errorf("%w: %s identities do not authenticate at the public gateway",
			pki.ErrIdentityRejected, id.Kind)
	}
	if id.TenantID == "" {
		return Principal{}, fmt.Errorf("%w: tenant certificate carries no tenant", pki.ErrIdentityRejected)
	}
	return Principal{
		TenantID:     id.TenantID,
		Subject:      id.SPIFFEID,
		Kind:         CredMTLS,
		CredentialID: id.SerialNumber,
		// A machine holding a tenant certificate is an integration. It can
		// push prices and read state; it cannot mint credentials or drive a
		// firmware rollout, both of which are decisions a human makes.
		Roles: []Role{RoleIntegration},
		// Certificates are not store-scoped: the tenant hierarchy issues one
		// per integration, not per store, and the store scope of a machine is
		// a policy decision that belongs in the key store where it can be
		// changed without a re-issue.
		ExpiresAt: id.NotAfter,
		RateKey:   "cert:" + string(id.TenantID) + "/" + id.DeviceID,
	}, nil
}

// authenticate is the middleware. It is installed on every non-public route by
// the router, which is the only place routes are constructed — so "did someone
// forget the auth middleware on this handler" is not a question that can be
// asked about this codebase.
func (g *Gateway) authenticate(route *Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range scrubbedRequestHeaders {
			r.Header.Del(h)
		}
		if route.Public {
			next.ServeHTTP(w, r)
			return
		}
		p, kind, err := g.auth.Authenticate(r.Context(), r)
		if err != nil {
			g.metrics.Auth.With(string(kind), "rejected").Inc()
			g.log.FromContext(r.Context()).Warn("authentication failed",
				"route", route.Pattern, "auth", string(kind), "error", err.Error(),
				"remote", clientIP(r), "request_id", RequestIDFrom(r.Context()))
			writeError(w, r, err)
			return
		}
		g.metrics.Auth.With(string(kind), "accepted").Inc()
		if slot := principalSlotFrom(r.Context()); slot != nil {
			*slot = p
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// authorize enforces RBAC and store scoping in one place.
func (g *Gateway) authorize(route *Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route.Public {
			next.ServeHTTP(w, r)
			return
		}
		p, err := principalOf(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !route.Permission.Zero() && !p.Can(route.Permission) {
			g.metrics.Denied.With(route.Operation, "permission").Inc()
			writeError(w, r, errForbidden("this credential does not grant %s", route.Permission))
			return
		}
		if route.StorePathValue != "" {
			store := canon.StoreID(r.PathValue(route.StorePathValue))
			if !canon.ValidID(string(store)) {
				writeError(w, r, errBadRequest("store id %q contains reserved characters", store))
				return
			}
			if !p.AllowsStore(store) {
				// 404 rather than 403, for the same reason a cross-tenant
				// label is a 404: a store manager probing store ids should not
				// be able to enumerate the estate.
				g.metrics.Denied.With(route.Operation, "store_scope").Inc()
				writeError(w, r, errNotFound("store %s not found", store))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
